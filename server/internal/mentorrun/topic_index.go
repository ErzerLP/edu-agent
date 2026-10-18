package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Service) mapTopicMessages(ctx context.Context, tx pgx.Tx, conversation, run string, generation, ordinal int64, body Body) error {
	var through int64
	for _, a := range displayAnchors(run, ordinal, body) {
		through = a.Ordinal
		text := body.Output
		if a.Slot >= 0 {
			text = body.Messages[a.Slot].Content
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_conversation_messages(message_id,conversation_id,run_id,message_slot,ordinal,privacy_generation,scope_fingerprint,estimated_tokens) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(run_id,message_slot) DO NOTHING`, a.MessageID, conversation, run, a.Slot, a.Ordinal, generation, topicScope(body), (len(text)+2)/3); err != nil {
			return err
		}
	}
	if through > 0 {
		_, err := tx.Exec(ctx, `INSERT INTO learning_topic_index_scopes(conversation_id,privacy_generation,scope_fingerprint,scanned_through) VALUES($1,$2,$3,$4) ON CONFLICT(conversation_id,privacy_generation,scope_fingerprint) DO UPDATE SET scanned_through=GREATEST(learning_topic_index_scopes.scanned_through,excluded.scanned_through)`, conversation, generation, topicScope(body), through)
		return err
	}
	return nil
}

func (s *Service) RequestTopicIndex(ctx context.Context, actor identity.Credential, space, id string, request TopicIndexRequest) (Receipt, error) {
	if !validID(space) || !validID(id) || !validID(request.OperationID) || request.Generation < 1 || request.ExpectedRevision < 0 || request.DirectionVersion < 0 || request.SourceSelection != nil && request.SourceSelection.Validate() != nil {
		return Receipt{}, ErrInvalid
	}
	hash := requestHash("topic_index", space, id, request)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, true)
	if err != nil {
		return Receipt{}, err
	}
	if g != request.Generation {
		return Receipt{}, ErrInactive
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return Receipt{}, err
	}
	if r, found, e := operation(ctx, tx, actor.Device.ID, request.OperationID, hash); e != nil || found {
		return r, e
	}
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return Receipt{}, err
	}
	c, err := conversationRow(ctx, tx, actor, space, id, g, true)
	if err != nil {
		return Receipt{}, err
	}
	if c.deleted {
		return Receipt{}, ErrNotFound
	}
	if !c.Saved {
		return Receipt{}, ErrTemporary
	}
	if err = s.describeConversation(ctx, tx, &c); err != nil {
		return Receipt{}, err
	}
	if !c.Writable {
		return Receipt{}, ErrInactive
	}
	if c.ConfirmationRequired && request.ConfirmDestination != c.Destination {
		return Receipt{}, ErrDestination
	}
	body := Body{}
	if c.Mode == "study" {
		if request.SourceSelection == nil || request.DirectionVersion < 1 {
			return Receipt{}, ErrInvalid
		}
		d, e := s.directions.EnsureTx(ctx, tx, actor.Device.ID, space, g, request.DirectionVersion)
		if e != nil {
			return Receipt{}, e
		}
		body.Study = &d
	} else if request.DirectionVersion != 0 {
		return Receipt{}, ErrInvalid
	}
	if request.SourceSelection != nil {
		if len(request.SourceSelection.Items) > 0 {
			if err = sourceActor(ctx, tx, actor.TokenID); err != nil {
				return Receipt{}, err
			}
		}
		scoped, _ := learningspace.WithScope(ctx, space)
		body.SourceManifest, err = knowledgepg.New(s.pool).FreezeSourceSelectionTx(scoped, tx, *request.SourceSelection)
		if err != nil {
			return Receipt{}, err
		}
	}
	item, err := s.enqueueTopicsTx(ctx, tx, c, actor.TokenID, body, request.ExpectedRevision)
	if err != nil {
		return Receipt{}, err
	}
	r, err := receipt(ctx, tx, item, request.OperationID, hash)
	if err != nil {
		return Receipt{}, err
	}
	return r, tx.Commit(ctx)
}

func (s *Service) enqueueTopicsTx(ctx context.Context, tx pgx.Tx, c Conversation, token string, body Body, expected int64) (row, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO learning_topic_index_heads(conversation_id,privacy_generation) VALUES($1,$2) ON CONFLICT DO NOTHING`, c.ID, c.Generation); err != nil {
		return row{}, err
	}
	var revision int64
	var status string
	if err := tx.QueryRow(ctx, `SELECT revision,status FROM learning_topic_index_heads WHERE conversation_id=$1 FOR UPDATE`, c.ID).Scan(&revision, &status); err != nil {
		return row{}, err
	}
	if revision != expected || status == "queued" || status == "running" {
		return row{}, ErrConflict
	}
	_, _, configuration, err := s.settings.MentorClient()
	if err != nil {
		return row{}, ErrModel
	}
	var target int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(t.ordinal),0) FROM learning_tutor_turns t JOIN learning_mentor_runs r ON r.id=t.run_id WHERE t.conversation_id=$1 AND r.state->>'status' IN ('succeeded','partial','failed','cancelled')`, c.ID).Scan(&target); err != nil {
		return row{}, err
	}
	body.TopicIndex = &TopicIndexState{Revision: revision, TargetTurn: target, Scope: topicScope(body)}
	if _, err = tx.Exec(ctx, `UPDATE learning_tutor_conversations SET topic_index_scope=$2 WHERE id=$1`, c.ID, body.TopicIndex.Scope); err != nil {
		return row{}, err
	}
	item := row{Meta: Meta{RunID: uuid.NewString(), SessionID: c.ID, ConversationID: c.ID, SpaceID: c.SpaceID, GoalID: runtimeGoal(c.GoalID), GoalVersion: c.GoalVersion, Generation: c.Generation, Kind: "topic_index", Status: "queued", Stage: "queued", Saved: true, BodyAvailable: true, Configuration: configuration, ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour)}, token: token, process: s.process, body: body}
	// A topic run shares ownership with the conversation, but never replaces its
	// current_run_id or increments its send version.
	if err = tx.QueryRow(ctx, `SELECT device_id::text FROM learning_tutor_conversations WHERE id=$1`, c.ID).Scan(&item.device); err != nil {
		return row{}, err
	}
	raw, _ := json.Marshal(item.Meta)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_runs(id,session_id,device_id,token_id,space_id,goal_id,goal_version,privacy_generation,state,process_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, item.RunID, item.SessionID, item.device, item.token, item.SpaceID, item.GoalID, item.GoalVersion, item.Generation, raw, s.process, item.ExpiresAt); err != nil {
		return row{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_topic_index_heads SET run_id=$2,status='queued',lease_id=NULL,lease_until=NULL,last_failure='' WHERE conversation_id=$1`, c.ID, item.RunID); err != nil {
		return row{}, err
	}
	err = s.save(ctx, tx, &item, "accepted")
	return item, err
}

func (s *Service) saveTopicState(ctx context.Context, tx pgx.Tx, item *row) error {
	status, failure := item.Status, ""
	switch status {
	case "succeeded":
		status = "ready"
	case "queued", "running":
	default:
		status = "failed"
		failure = "model_failed"
		switch item.Reason {
		case "context_changed", "conversation_deleted", "privacy_cleared":
			failure = "context_changed"
		case "lease_expired":
			failure = "lease_expired"
		case "history_context_limit":
			failure = "input_too_large"
		}
	}
	_, err := tx.Exec(ctx, `UPDATE learning_topic_index_heads SET status=$3,lease_id=$4,lease_until=$5,last_failure=$6 WHERE conversation_id=$1 AND run_id=$2`, item.ConversationID, item.RunID, status, item.lease, item.leaseUntil, failure)
	return err
}

// This runs after a successful ordinary turn transaction; indexing failures
// cannot roll back or terminate the user's answer. Reads never call this path.
func (s *Service) scheduleTopics(ctx context.Context, completed row) {
	if completed.ConversationID == "" || !completed.Saved || completed.Kind == "topic_index" {
		return
	}
	err := s.scheduleTopicsAfterTurn(ctx, completed)
	if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInactive) {
		slog.WarnContext(ctx, "topic_index_schedule_failed", "error_category", "index_unavailable")
	}
}

// Only completions written by this version carry the trigger. Restarting a
// service never synthesizes triggers for pre-upgrade conversations.
func (s *Service) schedulePendingTopics(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM learning_mentor_runs r WHERE r.state->>'topic_index_pending'='true' AND r.state->>'status'='succeeded' AND (r.state->>'body_available')::boolean AND r.expires_at>clock_timestamp() AND NOT EXISTS(SELECT 1 FROM learning_topic_index_heads h WHERE h.conversation_id=r.session_id AND h.status IN ('queued','running')) ORDER BY r.created_at DESC LIMIT 16`)
	if err != nil {
		return
	}
	items := []row{}
	for rows.Next() {
		item, e := scan(rows)
		if e != nil {
			rows.Close()
			return
		}
		items = append(items, item)
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	for _, item := range items {
		s.scheduleTopics(ctx, item)
	}
}

func (s *Service) scheduleTopicsAfterTurn(ctx context.Context, completed row) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, true)
	if err != nil {
		return err
	}
	if g != completed.Generation {
		return ErrInactive
	}
	if err = actorGate(ctx, tx, completed.device, completed.token, true); err != nil {
		return err
	}
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return err
	}
	var actor identity.Credential
	actor.Device.ID = completed.device
	actor.TokenID = completed.token
	c, err := conversationRow(ctx, tx, actor, completed.SpaceID, completed.ConversationID, g, true)
	if err != nil {
		return err
	}
	if c.deleted {
		return ErrInactive
	}
	if err = s.describeConversation(ctx, tx, &c); err != nil {
		return err
	}
	if c.ConfirmationRequired || !c.Writable {
		return ErrInactive
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1`, completed.RunID))
	if err != nil {
		return err
	}
	if item.Status != "succeeded" || !item.TopicIndexPending {
		return nil
	}
	consume := func() error {
		_, e := tx.Exec(ctx, `UPDATE learning_mentor_runs SET state=state-'topic_index_pending' WHERE id=$1`, item.RunID)
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if err = s.decode(&item); err != nil {
		return err
	}
	var scope string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(topic_index_scope,'') FROM learning_tutor_conversations WHERE id=$1`, c.ID).Scan(&scope); err != nil {
		return err
	}
	if scope != topicScope(item.body) {
		return consume()
	}
	var revision int64
	var status string
	err = tx.QueryRow(ctx, `SELECT revision,status FROM learning_topic_index_heads WHERE conversation_id=$1`, c.ID).Scan(&revision, &status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if status == "queued" || status == "running" {
		return nil
	}
	var turns, tokens, unmapped int
	if err = tx.QueryRow(ctx, `SELECT count(DISTINCT run_id),COALESCE(sum(estimated_tokens),0) FROM learning_conversation_messages WHERE conversation_id=$1 AND scope_fingerprint=$2 AND NOT covered`, c.ID, topicScope(item.body)).Scan(&turns, &tokens); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM learning_tutor_turns t WHERE conversation_id=$1 AND ciphertext IS NOT NULL AND NOT EXISTS(SELECT 1 FROM learning_conversation_messages m WHERE m.run_id=t.run_id)`, c.ID).Scan(&unmapped); err != nil {
		return err
	}
	if turns+unmapped < 4 && tokens < s.settings.View().Limits.ContextTokens*15/100 && status != "failed" {
		return consume()
	}
	_, err = s.enqueueTopicsTx(ctx, tx, c, item.token, Body{SourceManifest: item.body.SourceManifest, Study: item.body.Study}, revision)
	if err != nil {
		return err
	}
	return consume()
}
