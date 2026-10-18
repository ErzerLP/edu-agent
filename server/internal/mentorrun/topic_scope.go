package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningdirection"
	"github.com/jackc/pgx/v5"
)

// Topic selection is frozen by revision and message identity. This port returns
// derived points and evidence identities only, never raw messages or citations.
type TopicScopeRequest struct {
	TopicIDs          []string
	IndexRevision     int64
	ThroughMessage    string
	CurrentManifest   *knowledge.SourceManifest
	DirectionRevision string
	MaxTokens         int
}
type TopicScopeProjection struct {
	Revision       int64    `json:"index_revision"`
	Through        int64    `json:"through_message_ordinal"`
	CoveredThrough int64    `json:"covered_through"`
	Topics         []Topic  `json:"topics"`
	Gaps           []string `json:"coverage_gaps"`
}

func scopeBody(manifest *knowledge.SourceManifest, direction string) Body {
	body := Body{SourceManifest: manifest}
	if direction != "" {
		body.Study = &learningdirection.Revision{ID: direction}
	}
	return body
}
func (s *Service) ReadTopicScope(ctx context.Context, actor identity.Credential, space, id string, q TopicScopeRequest, send func(TopicScopeProjection) error) error {
	if len(q.TopicIDs) < 1 || len(q.TopicIDs) > 64 || q.IndexRevision < 1 || !validID(q.ThroughMessage) || q.MaxTokens < 128 || q.MaxTokens > 8192 || q.DirectionRevision != "" && !validID(q.DirectionRevision) {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range q.TopicIDs {
		if !validID(id) || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	return s.topicRead(ctx, actor, space, id, func(tx pgx.Tx, c Conversation) error {
		var revision, through int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM learning_topic_index_heads WHERE conversation_id=$1`, id).Scan(&revision); err != nil {
			return err
		}
		if revision != q.IndexRevision {
			return ErrConflict
		}
		if err := tx.QueryRow(ctx, `SELECT ordinal FROM learning_conversation_messages WHERE conversation_id=$1 AND message_id=$2 AND privacy_generation=$3`, id, q.ThroughMessage, c.Generation).Scan(&through); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalid
			}
			return err
		}
		body := scopeBody(q.CurrentManifest, q.DirectionRevision)
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM learning_conversation_topics WHERE conversation_id=$1 AND id=ANY($2::uuid[]) AND scope_fingerprint=$3 AND revision<=$4`, id, q.TopicIDs, topicScope(body), revision).Scan(&count); err != nil {
			return err
		}
		if count != len(q.TopicIDs) {
			return ErrInvalid
		}
		p, err := s.topicProjectionTx(ctx, tx, c, actor.TokenID, body, q.TopicIDs, revision, through, "", q.MaxTokens)
		if err != nil {
			return err
		}
		return send(p)
	})
}

func (s *Service) topicProjectionTx(ctx context.Context, tx pgx.Tx, c Conversation, token string, scope Body, ids []string, revision, through int64, question string, budget int) (TopicScopeProjection, error) {
	type candidate struct {
		topic Topic
		score int
	}
	best := []candidate{}
	omitted := false
	p, err := s.scanTopicScopeTx(ctx, tx, c, token, scope, ids, revision, through, func(topic Topic) {
		text := topic.Title
		for _, point := range topic.Points {
			text += " " + point.Text
		}
		for _, obs := range topic.Observations {
			text += " " + obs.Text
		}
		best = append(best, candidate{topic, relevance(question, text)})
		sort.SliceStable(best, func(i, j int) bool {
			if best[i].score != best[j].score {
				return best[i].score > best[j].score
			}
			return best[i].topic.Revision > best[j].topic.Revision
		})
		if len(best) > 32 {
			best = best[:32]
			omitted = true
		}
	})
	if err != nil {
		return p, err
	}
	for _, b := range best {
		next := append(append([]Topic(nil), p.Topics...), b.topic)
		raw, _ := json.Marshal(next)
		if messageCost([]modelclient.Message{{Role: "user", Content: string(raw)}}) > budget {
			omitted = true
			continue
		}
		p.Topics = next
	}
	if omitted {
		p.Gaps = append(p.Gaps, "仅纳入本次选取的有界话题要点，其他话题或证据未纳入。")
	}
	return p, nil
}

// Share the owner and per-evidence gates between the public evidence projection
// and the compact model projection. Only readable, same-scope items reach visit.
func (s *Service) scanTopicScopeTx(ctx context.Context, tx pgx.Tx, c Conversation, token string, scope Body, ids []string, revision, through int64, visit func(Topic)) (TopicScopeProjection, error) {
	p := TopicScopeProjection{Revision: revision, Through: through, Topics: []Topic{}, Gaps: []string{}}
	fingerprint := topicScope(scope)
	if _, err := s.sourceMaterialTx(ctx, tx, c.SpaceID, token, Body{SourceManifest: scope.SourceManifest}); err != nil {
		return p, err
	}
	var pending bool
	if err := tx.QueryRow(ctx, `WITH scoped AS (SELECT ordinal,covered FROM learning_conversation_messages WHERE conversation_id=$1 AND scope_fingerprint=$2 AND ordinal<=$3)
 SELECT COALESCE(max(ordinal) FILTER(WHERE covered AND ordinal<COALESCE((SELECT min(ordinal) FROM scoped WHERE NOT covered),9223372036854775807)),0),COALESCE(bool_or(NOT covered),false) FROM scoped`, c.ID, fingerprint, through).Scan(&p.CoveredThrough, &pending); err != nil {
		return p, err
	}
	var unmapped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_tutor_turns t WHERE t.conversation_id=$1 AND t.ordinal<$2 AND t.ciphertext IS NOT NULL AND NOT EXISTS(SELECT 1 FROM learning_conversation_messages m WHERE m.run_id=t.run_id))`, c.ID, through/MaxBody+1).Scan(&unmapped); err != nil {
		return p, err
	}
	if pending || unmapped {
		p.Gaps = append(p.Gaps, "较早内容未纳入：话题整理尚未覆盖全部消息。")
	}
	cursor := ""
	omitted := false
	for {
		rows, err := tx.Query(ctx, `SELECT id::text,ciphertext FROM learning_conversation_topics WHERE conversation_id=$1 AND scope_fingerprint=$2 AND revision<=$3 AND ($4='' OR id>NULLIF($4,'')::uuid) AND ($5::uuid[] IS NULL OR id=ANY($5)) ORDER BY id LIMIT 32`, c.ID, fingerprint, revision, cursor, ids)
		if err != nil {
			return p, err
		}
		type entry struct {
			id  string
			raw []byte
		}
		batch := []entry{}
		for rows.Next() {
			var e entry
			if err = rows.Scan(&e.id, &e.raw); err != nil {
				rows.Close()
				return p, err
			}
			batch = append(batch, e)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return p, err
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			cursor = e.id
			var body topicBody
			if err = openHistory(s.aead, historyAAD(c.ID, c.Generation, "topic:"+e.id), e.raw, &body); err != nil {
				return p, err
			}
			if err = s.topicReadable(ctx, tx, c.SpaceID, token, c.Generation, body); err != nil {
				if sourceUnavailable(err) || errors.Is(err, ErrInactive) {
					omitted = true
					continue
				}
				return p, err
			}
			topic := body.Topic
			topic.Points = nil
			topic.Observations = nil
			topic.AnchorCount = 0
			topicHasGap := false
			valid := func(ev TopicEvidence) (bool, error) {
				if ev.Scope != fingerprint || ev.Generation != c.Generation {
					return false, nil
				}
				if scope.Study == nil && ev.DirectionRevision != "" || scope.Study != nil && ev.DirectionRevision != scope.Study.ID {
					return false, nil
				}
				if scope.SourceManifest == nil && ev.SourceFingerprint != "" || scope.SourceManifest != nil && (ev.SourceFingerprint != scope.SourceManifest.SHA256 || ev.KnowledgeGeneration != scope.SourceManifest.Generation) {
					return false, nil
				}
				var count int
				err := tx.QueryRow(ctx, `SELECT count(*) FROM learning_conversation_messages WHERE conversation_id=$1 AND message_id=ANY($2::uuid[]) AND ordinal<=$3 AND privacy_generation=$4`, c.ID, ev.AnchorIDs, through, c.Generation).Scan(&count)
				return len(ev.AnchorIDs) > 0 && count == len(ev.AnchorIDs), err
			}
			for _, point := range body.Points {
				ok, e := valid(point.TopicEvidence)
				if e != nil {
					return p, e
				}
				if ok {
					topic.Points = append(topic.Points, point)
				} else {
					omitted = true
					topicHasGap = true
				}
			}
			for _, obs := range body.Observations {
				ok, e := valid(obs.TopicEvidence)
				if e != nil {
					return p, e
				}
				if ok {
					topic.Observations = append(topic.Observations, obs)
				} else {
					omitted = true
					topicHasGap = true
				}
			}
			if len(topic.Points)+len(topic.Observations) == 0 {
				continue
			}
			if topicHasGap {
				topic.Title = "所选话题（部分证据未纳入）"
			}
			visit(topic)
		}
	}
	if omitted {
		p.Gaps = append(p.Gaps, "仅纳入本次选取的有界话题要点，其他话题或证据未纳入。")
	}
	return p, nil
}

func (h *executionHost) contextTopics(ctx context.Context, budget int) ([]modelclient.Message, bool, error) {
	if h.body.Context == nil || h.owned.ConversationID == "" || !h.owned.Saved || budget <= 0 {
		return nil, true, nil
	}
	tx, err := h.service.pool.Begin(ctx)
	if err != nil {
		return nil, true, err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, false)
	if err != nil {
		return nil, true, err
	}
	if g != h.owned.Generation {
		return nil, true, ErrInactive
	}
	if err = actorGate(ctx, tx, h.owned.device, h.owned.token, true); err != nil {
		return nil, true, err
	}
	var revision int64
	var status string
	err = tx.QueryRow(ctx, `SELECT revision,status FROM learning_topic_index_heads WHERE conversation_id=$1 FOR SHARE`, h.owned.ConversationID).Scan(&revision, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, true, err
	}
	// Committed points stay usable while a later batch is pending or has failed;
	// failed output is never committed. The projection still checks each point's
	// scope/evidence and reports pending coverage.
	if revision < 1 {
		return nil, true, nil
	}
	if h.body.Context.TopicRevision == 0 {
		h.body.Context.TopicRevision = revision
	} else {
		revision = h.body.Context.TopicRevision
	}
	question := ""
	for _, m := range h.body.Messages {
		if m.Role == "user" {
			question = m.Content
		}
	}
	c := Conversation{ID: h.owned.ConversationID, SpaceID: h.owned.SpaceID, Generation: g}
	return h.projectContextTopicsTx(ctx, tx, c, revision, question, budget)
}
