package mentorrun

import (
	"context"
	"errors"
	"slices"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

func (s *Service) topicRead(ctx context.Context, actor identity.Credential, space, id string, send func(pgx.Tx, Conversation) error) error {
	if !validID(space) || !validID(id) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return err
	}
	c, err := conversationRow(ctx, tx, actor, space, id, g, false)
	if err != nil {
		return err
	}
	if c.deleted {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `SELECT revision FROM learning_topic_index_heads WHERE conversation_id=$1 FOR SHARE`, id); err != nil {
		return err
	}
	return send(tx, c)
}
func (s *Service) topicReadable(ctx context.Context, tx pgx.Tx, space, token string, generation int64, b topicBody) error {
	if _, err := s.sourceMaterialTx(ctx, tx, space, token, Body{SourceManifest: b.SourceManifest}); err != nil {
		return err
	}
	if b.MemoryGeneration > 0 {
		var current int64
		if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('memory','read',NULL)`).Scan(&current); err != nil {
			return err
		}
		if current != b.MemoryGeneration {
			return ErrInactive
		}
	}
	if len(b.MemorySources) > 0 {
		h := executionHost{service: s, owned: row{Meta: Meta{Generation: generation}, token: token}, body: Body{MemorySources: b.MemorySources}}
		return h.validateMemorySources(ctx, tx)
	}
	return nil
}
func (s *Service) ReadTopics(ctx context.Context, actor identity.Credential, space, id, cursor string, limit int, send func(TopicPage) error) error {
	if limit < 1 || limit > 50 || cursor != "" && !validID(cursor) {
		return ErrInvalid
	}
	return s.topicRead(ctx, actor, space, id, func(tx pgx.Tx, c Conversation) error {
		p := TopicPage{ConversationID: id, Generation: c.Generation, Status: "not_started", Items: []Topic{}}
		err := tx.QueryRow(ctx, `SELECT revision,covered_through,status FROM learning_topic_index_heads WHERE conversation_id=$1`, id).Scan(&p.Revision, &p.CoveredThrough, &p.Status)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE covered),COALESCE(max(ordinal) FILTER(WHERE covered),0) FROM learning_conversation_messages WHERE conversation_id=$1`, id).Scan(&p.TotalMessages, &p.CoveredMessages, &p.CoveredTail); err != nil {
			return err
		}
		if c.Saved {
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM learning_tutor_turns t JOIN learning_mentor_runs r ON r.id=t.run_id WHERE t.conversation_id=$1 AND t.ciphertext IS NOT NULL AND r.state->>'status' IN ('succeeded','partial','failed','cancelled') AND NOT EXISTS(SELECT 1 FROM learning_conversation_messages m WHERE m.run_id=t.run_id)`, id).Scan(&p.UnmappedTurns); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT t.id::text,t.ciphertext,(SELECT count(*) FROM learning_topic_anchors a WHERE a.topic_id=t.id) FROM learning_conversation_topics t WHERE conversation_id=$1 AND ($2='' OR t.id>NULLIF($2,'')::uuid) ORDER BY id LIMIT $3`, id, cursor, limit+1)
		if err != nil {
			return err
		}
		type stored struct {
			id    string
			raw   []byte
			count int
		}
		items := []stored{}
		for rows.Next() {
			var v stored
			if err = rows.Scan(&v.id, &v.raw, &v.count); err != nil {
				rows.Close()
				return err
			}
			items = append(items, v)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		if len(items) > limit {
			items = items[:limit]
			p.NextCursor = items[limit-1].id
		}
		for _, v := range items {
			var b topicBody
			if err = openHistory(s.aead, historyAAD(id, c.Generation, "topic:"+v.id), v.raw, &b); err != nil {
				return err
			}
			if err = s.topicReadable(ctx, tx, space, actor.TokenID, c.Generation, b); err != nil {
				if sourceUnavailable(err) || errors.Is(err, ErrInactive) {
					continue
				}
				return err
			}
			b.AnchorCount = v.count
			p.Items = append(p.Items, b.Topic)
		}
		return send(p)
	})
}
func (s *Service) ReadTopicAnchors(ctx context.Context, actor identity.Credential, space, id, topic string, cursor int64, before bool, limit int, send func(TopicAnchorPage) error) error {
	if !validID(topic) || cursor < 0 || limit < 1 || limit > 50 || before && cursor == 0 {
		return ErrInvalid
	}
	return s.topicRead(ctx, actor, space, id, func(tx pgx.Tx, c Conversation) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT ciphertext FROM learning_conversation_topics WHERE conversation_id=$1 AND id=$2`, id, topic).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var b topicBody
		if err = openHistory(s.aead, historyAAD(id, c.Generation, "topic:"+topic), raw, &b); err != nil {
			return err
		}
		if err = s.topicReadable(ctx, tx, space, actor.TokenID, c.Generation, b); err != nil {
			return err
		}
		order, comparison := "ASC", ">"
		if before {
			order, comparison = "DESC", "<"
		}
		rows, err := tx.Query(ctx, `SELECT m.message_id::text,m.run_id::text,m.message_slot,m.ordinal FROM learning_topic_anchors a JOIN learning_conversation_messages m ON m.message_id=a.message_id WHERE a.topic_id=$1 AND a.conversation_id=$2 AND m.ordinal`+comparison+`$3 ORDER BY m.ordinal `+order+` LIMIT $4`, topic, id, cursor, limit+1)
		if err != nil {
			return err
		}
		p := TopicAnchorPage{ConversationID: id, Generation: c.Generation, Items: []MessageAnchor{}}
		for rows.Next() {
			var a MessageAnchor
			if err = rows.Scan(&a.MessageID, &a.RunID, &a.Slot, &a.Ordinal); err != nil {
				rows.Close()
				return err
			}
			p.Items = append(p.Items, a)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		if len(p.Items) > limit {
			p.Items = p.Items[:limit]
			p.NextCursor = p.Items[limit-1].Ordinal
		}
		if before {
			slices.Reverse(p.Items)
		}
		return send(p)
	})
}
func (s *Service) ReadMessage(ctx context.Context, actor identity.Credential, space, id, message string, send func(TurnPage) error) error {
	if !validID(message) {
		return ErrInvalid
	}
	return s.topicRead(ctx, actor, space, id, func(tx pgx.Tx, c Conversation) error {
		var ordinal int64
		var run string
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT t.ordinal,t.run_id::text,t.ciphertext FROM learning_conversation_messages m JOIN learning_tutor_turns t ON t.run_id=m.run_id WHERE m.conversation_id=$1 AND m.message_id=$2`, id, message).Scan(&ordinal, &run, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var b Body
		if err = openHistory(s.aead, historyAAD(id, c.Generation, run), raw, &b); err != nil {
			return err
		}
		if err = s.topicReadable(ctx, tx, space, actor.TokenID, c.Generation, topicBody{SourceManifest: b.SourceManifest, MemorySources: b.MemorySources}); err != nil {
			return err
		}
		if err = s.describeConversation(ctx, tx, &c); err != nil {
			return err
		}
		return s.readConversationPage(ctx, tx, actor, space, c, ordinal-1, 1, func(p TurnPage) error {
			if len(p.Items) != 1 || p.Items[0].RunID != run || !p.Items[0].BodyAvailable {
				return ErrNotFound
			}
			found := false
			for _, a := range p.Items[0].MessageAnchors {
				found = found || a.MessageID == message
			}
			if !found {
				return ErrNotFound
			}
			return send(p)
		})
	})
}
