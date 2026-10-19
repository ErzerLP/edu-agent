package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

func validateStudioConversationScope(q learningcontent.StudioConversationScope) error {
	if !validID(q.ConversationID) || !validID(q.ThroughMessage) || q.IndexRevision < 1 || len(q.TopicIDs) < 1 || len(q.TopicIDs) > 64 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range q.TopicIDs {
		if !validID(id) || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	return nil
}

type StudioTopicOptionsRequest struct {
	SourceSelection  knowledge.SourceSelection `json:"source_selection"`
	DirectionVersion int64                     `json:"direction_version"`
	Cursor           string                    `json:"cursor,omitempty"`
	IndexRevision    int64                     `json:"index_revision,omitempty"`
}
type StudioTopicOptions struct {
	ConversationID string  `json:"conversation_id"`
	Generation     int64   `json:"privacy_generation"`
	Revision       int64   `json:"index_revision"`
	ThroughMessage string  `json:"through_message,omitempty"`
	Status         string  `json:"status"`
	Partial        bool    `json:"partial"`
	Items          []Topic `json:"items"`
	NextCursor     string  `json:"next_cursor,omitempty"`
}

// Only metadata is inspected to detect gaps; turn/checkpoint ciphertext is never read.
func studioConversationPartialTx(ctx context.Context, tx pgx.Tx, id string, through int64) (bool, error) {
	var partial bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_conversation_messages WHERE conversation_id=$1 AND (NOT covered OR ordinal>$2)) OR EXISTS(SELECT 1 FROM learning_tutor_turns t JOIN learning_mentor_runs r ON r.id=t.run_id WHERE t.conversation_id=$1 AND t.ciphertext IS NOT NULL AND r.state->>'status' IN ('succeeded','partial','failed','cancelled') AND NOT EXISTS(SELECT 1 FROM learning_conversation_messages m WHERE m.run_id=t.run_id))`, id, through).Scan(&partial)
	return partial, err
}

func (s *Service) StudioConversationTopics(ctx context.Context, actor identity.Credential, id string, q StudioTopicOptionsRequest, send func(StudioTopicOptions) error) error {
	if !validID(id) || q.SourceSelection.Validate() != nil || q.DirectionVersion < 0 || q.IndexRevision < 0 || q.Cursor != "" && (!validID(q.Cursor) || q.IndexRevision < 1) {
		return ErrInvalid
	}
	tx, g, err := s.studioJobBegin(ctx, actor, false)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	c, err := conversationRow(ctx, tx, actor, learningspace.Scope(ctx), id, g, false)
	if err != nil {
		return err
	}
	if c.deleted {
		return ErrNotFound
	}
	if !c.Saved {
		return ErrTemporary
	}
	d, err := s.directions.ReadTx(ctx, tx, c.SpaceID, 0)
	if err != nil {
		return err
	}
	if d.Version != q.DirectionVersion {
		return ErrConflict
	}
	manifest, err := knowledgepg.New(s.pool).FreezeSourceSelectionTx(ctx, tx, q.SourceSelection)
	if err != nil {
		return err
	}
	p := StudioTopicOptions{ConversationID: id, Generation: g, Status: "not_started", Items: []Topic{}}
	err = tx.QueryRow(ctx, `SELECT revision,status FROM learning_topic_index_heads WHERE conversation_id=$1 FOR SHARE`, id).Scan(&p.Revision, &p.Status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if q.IndexRevision > 0 && q.IndexRevision != p.Revision {
		return ErrConflict
	}
	var through int64
	err = tx.QueryRow(ctx, `SELECT message_id::text,ordinal FROM learning_conversation_messages WHERE conversation_id=$1 ORDER BY ordinal DESC LIMIT 1`, id).Scan(&p.ThroughMessage, &through)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	p.Partial, err = studioConversationPartialTx(ctx, tx, id, through)
	if err != nil {
		return err
	}
	scope := scopeBody(manifest, d.ID)
	rows, err := tx.Query(ctx, `SELECT id::text FROM learning_conversation_topics WHERE conversation_id=$1 AND scope_fingerprint=$2 AND ($3='' OR id>NULLIF($3,'')::uuid) ORDER BY id LIMIT 51`, id, topicScope(scope), q.Cursor)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var topic string
		if err = rows.Scan(&topic); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, topic)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(ids) > 50 {
		ids = ids[:50]
		p.NextCursor = ids[49]
	}
	if len(ids) > 0 {
		projection, e := s.scanTopicScopeTx(ctx, tx, c, actor.TokenID, scope, ids, p.Revision, through, func(t Topic) { p.Items = append(p.Items, t) })
		if e != nil {
			return e
		}
		p.Partial = p.Partial || len(projection.Gaps) > 0
	}
	// The no-source state remains readable so the UI can offer a source picker.
	return send(p)
}

func (s *Service) freezeStudioConversationTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, j learningcontent.StudioJob, q learningcontent.StudioConversationScope) (*learningcontent.StudioConversationSnapshot, error) {
	c, err := conversationRow(ctx, tx, actor, j.SpaceID, q.ConversationID, j.Generation, false)
	if err != nil {
		return nil, err
	}
	if c.deleted {
		return nil, ErrNotFound
	}
	if !c.Saved {
		return nil, ErrTemporary
	}
	p, err := s.readTopicScopeTx(ctx, tx, c, actor.TokenID, TopicScopeRequest{TopicIDs: q.TopicIDs, IndexRevision: q.IndexRevision, ThroughMessage: q.ThroughMessage, CurrentManifest: &j.Input.Manifest, DirectionRevision: j.Input.DirectionID, MaxTokens: 4096})
	if err != nil {
		return nil, err
	}
	if len(p.Topics) == 0 {
		return nil, ErrInvalid
	}
	partial, err := studioConversationPartialTx(ctx, tx, c.ID, p.Through)
	if err != nil {
		return nil, err
	}
	if partial {
		p.Gaps = append(p.Gaps, "未覆盖最新内容：本次仅使用已整理的话题范围。")
	}
	if partial && !q.AcceptPartial {
		return nil, ErrConflict
	}
	raw, err := json.Marshal(p)
	return &learningcontent.StudioConversationSnapshot{StudioConversationScope: q, Projection: raw}, err
}

func (s *Service) studioConversationGateTx(ctx context.Context, tx pgx.Tx, j learningcontent.StudioJob, token string) error {
	if j.Input.Conversation == nil {
		return nil
	}
	var available bool
	// Do not invert the deletion lock order (conversation -> run -> job).
	// Deletion locks all associated runs before removing the dependent jobs.
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_tutor_conversations WHERE id=$1 AND space_id=$2 AND device_id=$3 AND privacy_generation=$4 AND NOT deleted)`, j.Input.Conversation.ConversationID, j.SpaceID, j.DeviceID, j.Generation).Scan(&available); err != nil {
		return err
	}
	if !available {
		return ErrInactive
	}
	var p TopicScopeProjection
	if err := json.Unmarshal(j.Input.Conversation.Projection, &p); err != nil {
		return err
	}
	for _, topic := range p.Topics {
		evidence := []TopicEvidence{}
		for _, point := range topic.Points {
			evidence = append(evidence, point.TopicEvidence)
		}
		for _, observation := range topic.Observations {
			evidence = append(evidence, observation.TopicEvidence)
		}
		for _, e := range evidence {
			if err := s.topicReadable(ctx, tx, j.SpaceID, token, j.Generation, topicBody{MemoryGeneration: e.MemoryGeneration, MemorySources: e.MemorySources}); err != nil {
				return err
			}
		}
	}
	return nil
}

type studioModelTopic struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Points       []string `json:"points"`
	Observations []string `json:"observations"`
}

func (s *Service) removeConversationStudioJobsTx(ctx context.Context, tx pgx.Tx, conversation string) error {
	rows, err := tx.Query(ctx, `SELECT id::text FROM learning_studio_jobs WHERE conversation_id=$1 ORDER BY id FOR UPDATE`, conversation)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	// Artifacts and immutable published sections remain independent of these jobs.
	for _, id := range ids {
		if err = removeStudioJobTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func studioConversationTopics(j learningcontent.StudioJob) []studioModelTopic {
	if j.Input.Conversation == nil {
		return nil
	}
	var projection TopicScopeProjection
	_ = json.Unmarshal(j.Input.Conversation.Projection, &projection)
	result := []studioModelTopic{}
	for _, t := range projection.Topics {
		item := studioModelTopic{ID: t.ID, Title: t.Title, Points: []string{}, Observations: []string{}}
		for _, p := range t.Points {
			item.Points = append(item.Points, p.Text)
		}
		for _, o := range t.Observations {
			item.Observations = append(item.Observations, o.Text)
		}
		result = append(result, item)
	}
	return result
}
func studioConversationQuery(j learningcontent.StudioJob) string {
	parts := []string{}
	for _, t := range studioConversationTopics(j) {
		parts = append(parts, t.Title)
		parts = append(parts, t.Points...)
		parts = append(parts, t.Observations...)
	}
	return strings.Join(parts, " ")
}

// Space-wide records may inform difficulty without supplying unselected concepts.
func studioConversationLearningState(j learningcontent.StudioJob) map[string]any {
	s := j.Input.LearningState
	return map[string]any{"coverage": s.Coverage, "needs_review_count": len(s.NeedsReview), "help_used_count": len(s.HelpUsed), "pending_count": len(s.PendingAssessments), "mastered_count": len(s.MasteredEvidence)}
}

const studioConversationInstruction = `conversation_topics 是用户勾选的话题和疑问，只限定内容范围，不是事实依据或资料引用。只围绕这些话题安排部分和题目；方向、侧重点、学习状态及修改请求不得引入未选话题。每部分 topic_ids 必须引用给定话题 ID，无法由资料支持的话题列入未覆盖范围，不得根据摘要补写事实。`

func studioConversationPlanSchema(base json.RawMessage, j learningcontent.StudioJob) json.RawMessage {
	var schema map[string]any
	_ = json.Unmarshal(base, &schema)
	item := schema["properties"].(map[string]any)["sections"].(map[string]any)["items"].(map[string]any)
	item["required"] = append(item["required"].([]any), "topic_ids")
	ids := []string{}
	for _, t := range studioConversationTopics(j) {
		ids = append(ids, t.ID)
	}
	item["properties"].(map[string]any)["topic_ids"] = map[string]any{"type": "array", "minItems": 1, "maxItems": len(ids), "uniqueItems": true, "items": map[string]any{"type": "string", "enum": ids}}
	raw, _ := json.Marshal(schema)
	return raw
}

// Units inherit only the selected topics attached to their confirmed section.
func studioUnitConversation(j learningcontent.StudioJob, section string) []studioModelTopic {
	selected := map[string]bool{}
	for _, part := range j.Plan.Sections {
		if part.ID == section {
			for _, id := range part.TopicIDs {
				selected[id] = true
			}
		}
	}
	result := []studioModelTopic{}
	for _, topic := range studioConversationTopics(j) {
		if selected[topic.ID] {
			result = append(result, topic)
		}
	}
	return result
}
