package postgresstore

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 一次可重复读覆盖原答案、原活动、处置与 Evidence，避免拼接不同版本的事实。
func withFeedbackRead[T any](ctx context.Context, s *Store, read func(learningLoaderDB) (T, error)) (T, error) {
	var zero T
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(context.Background())
	var generation int64
	for _, owner := range []privacy.OwnerKind{privacy.OwnerKnowledge, privacy.OwnerLearning, privacy.OwnerTutoring} {
		current, err := privacy.LockOwnerRead(ctx, tx, owner)
		if err != nil {
			return zero, err
		}
		if generation != 0 && current != generation {
			return zero, &learning.Error{Code: learning.CodeContentRedacted}
		}
		generation = current
	}
	result, err := read(tx)
	if err != nil {
		return zero, err
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, err
	}
	return result, nil
}

func (s *Store) ListFeedback(ctx context.Context, q learning.FeedbackQuery) (learning.FeedbackPage, error) {
	if q.Page.Limit < 1 || q.Page.Limit > 100 || len(q.Page.Cursor) > 4096 ||
		(q.Status != "all" && q.Status != "pending" && q.Status != "provisional") ||
		(q.SessionID != "" && uuid.Validate(q.SessionID) != nil) {
		return learning.FeedbackPage{}, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	return withFeedbackRead(ctx, s, func(db learningLoaderDB) (learning.FeedbackPage, error) {
		page := learning.FeedbackPage{Items: []learning.FeedbackSummary{}}
		metadata, _, _, _, err := metadataFrom(ctx, db)
		if err != nil {
			return page, err
		}
		kind := "feedback:" + learningspace.Scope(ctx) + ":" + q.Status + ":" + q.SessionID
		keys, err := decodeCursor(q.Page.Cursor, kind, metadata.GenerationID, metadata.AsOfEventSequence, 2)
		if err != nil {
			return page, err
		}
		after, afterID := time.Time{}, uuid.Nil.String()
		if len(keys) > 0 {
			after, err = time.Parse(time.RFC3339Nano, keys[0])
			if err != nil || uuid.Validate(keys[1]) != nil {
				return page, &learning.Error{Code: learning.CodeStaleCursor}
			}
			afterID = keys[1]
		}
		rows, err := db.Query(ctx, `SELECT a.id,a.activity_id,a.session_id,COALESCE(e.id::text,''),a.received_at,COALESCE(d.disposition,''),CASE WHEN e.id IS NOT NULL THEN 'settled' ELSE COALESCE(pr.status,'received') END
 FROM learning_attempts a JOIN learning_activities act ON act.id=a.activity_id
 JOIN learning_goal_revisions g ON g.id=act.goal_revision_id
 JOIN learning_attempt_payloads p ON p.id=a.answer_payload_id
	LEFT JOIN learning_assessments e ON e.attempt_id=a.id
 LEFT JOIN LATERAL (SELECT disposition FROM learning_assessment_decisions WHERE assessment_id=e.id ORDER BY version DESC LIMIT 1) d ON true
 LEFT JOIN LATERAL (SELECT CASE WHEN status='processing' AND lease_expires_at<=clock_timestamp() THEN 'unknown' ELSE status END AS status FROM tutoring_proposal_requests WHERE proposal_type='assessment' AND aggregate_id=a.session_id AND input->>'attempt_id'=a.id::text ORDER BY updated_at DESC LIMIT 1) pr ON true
 WHERE g.space_id=$1 AND a.offline_submission_id IS NULL AND act.prompt<>'[redacted]'
 AND ($2='' OR a.session_id::text=$2)
 AND ($3='all' OR ($3='pending' AND e.id IS NULL) OR ($3='provisional' AND d.disposition='provisional'))
 AND (a.received_at,a.id)>($4,$5::uuid)
 ORDER BY a.received_at,a.id LIMIT $6`, learningspace.Scope(ctx), q.SessionID, q.Status, after, afterID, q.Page.Limit+1)
		if err != nil {
			return page, err
		}
		defer rows.Close()
		for rows.Next() {
			var item learning.FeedbackSummary
			if err := rows.Scan(&item.AttemptID, &item.ActivityID, &item.SessionID, &item.AssessmentID, &item.ReceivedAt, &item.Disposition, &item.Status); err != nil {
				return page, err
			}
			page.Items = append(page.Items, item)
		}
		if err := rows.Err(); err != nil {
			return page, err
		}
		if len(page.Items) > q.Page.Limit {
			page.Items = page.Items[:q.Page.Limit]
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeCursor(kind, metadata.GenerationID, metadata.AsOfEventSequence, last.ReceivedAt.UTC().Format(time.RFC3339Nano), last.AttemptID)
		}
		return page, nil
	})
}

func (s *Store) Feedback(ctx context.Context, id string, byAssessment bool) (learning.FeedbackView, error) {
	if uuid.Validate(id) != nil {
		return learning.FeedbackView{}, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	return withFeedbackRead(ctx, s, func(db learningLoaderDB) (learning.FeedbackView, error) {
		view := learning.FeedbackView{LearningSpaceID: learningspace.Scope(ctx), Status: "received", Decisions: []learning.AssessmentDecision{}, Evidence: []learning.AcceptedEvidence{}, Reasons: []string{}, AllowedDecisions: []string{}}
		var attemptID, activityID, goalID string
		var artifactID *string
		var artifactVersion *int64
		var contextID *string
		var redacted bool
		err := db.QueryRow(ctx, `SELECT a.id,a.activity_id,act.goal_revision_id,a.artifact_id,a.artifact_version,act.knowledge_context_revision_id,act.prompt='[redacted]'
 FROM learning_attempts a JOIN learning_activities act ON act.id=a.activity_id
 JOIN learning_goal_revisions g ON g.id=act.goal_revision_id
 JOIN learning_attempt_payloads p ON p.id=a.answer_payload_id
 LEFT JOIN learning_assessments e ON e.attempt_id=a.id
 WHERE g.space_id=$1 AND a.offline_submission_id IS NULL AND
 (($3 AND e.id=$2::uuid) OR (NOT $3 AND a.id=$2::uuid))`, view.LearningSpaceID, id, byAssessment).Scan(&attemptID, &activityID, &goalID, &artifactID, &artifactVersion, &contextID, &redacted)
		if errors.Is(err, pgx.ErrNoRows) {
			return view, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return view, err
		}
		if redacted {
			return view, &learning.Error{Code: learning.CodeContentRedacted}
		}
		view.Activity, err = loadActivityForView(ctx, db, activityID)
		if err != nil {
			return view, err
		}
		view.Attempt, err = loadAttemptForView(ctx, db, attemptID)
		if err != nil {
			return view, err
		}
		view.Goal, err = loadGoalRevisionForView(ctx, db, goalID)
		if err != nil {
			return view, err
		}
		view.KnowledgeContextID = deref(contextID)
		if artifactID != nil && artifactVersion != nil {
			view.Content = &learning.FeedbackContent{ArtifactID: *artifactID, Version: *artifactVersion}
		}
		err = db.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='session' AND aggregate_id=$1`, view.Attempt.SessionID).Scan(&view.SessionVersion)
		if err != nil {
			return view, err
		}
		// 接收回执来自原事件，不采用当前会话的最近一次操作。
		err = db.QueryRow(ctx, `SELECT e.operation_id,e.event_seq,e.received_at FROM learning_events e
 JOIN learning_event_payloads p ON p.id=e.payload_id
 WHERE e.event_type='AttemptSubmitted' AND e.aggregate_id=$1 AND p.redacted_at IS NULL AND p.payload->>'attempt_id'=$2
 ORDER BY e.event_seq LIMIT 1`, view.Attempt.SessionID, attemptID).Scan(&view.Receipt.OperationID, &view.Receipt.EventSequence, &view.Receipt.ReceivedAt)
		if err != nil {
			return view, err
		}
		loaded, err := loadAssessmentWith(ctx, db, attemptID, true)
		if learning.ErrorCode(err) == learning.CodeNotFound {
			var status string
			err = db.QueryRow(ctx, `SELECT CASE WHEN status='processing' AND lease_expires_at<=clock_timestamp() THEN 'unknown' ELSE status END
 FROM tutoring_proposal_requests WHERE proposal_type='assessment' AND aggregate_id=$1 AND input->>'attempt_id'=$2
 ORDER BY updated_at DESC LIMIT 1`, view.Attempt.SessionID, attemptID).Scan(&status)
			if errors.Is(err, pgx.ErrNoRows) {
				return view, nil
			}
			if err != nil {
				return view, err
			}
			view.Status = status
			return view, nil
		}
		if err != nil {
			return view, err
		}
		view.Status = "settled"
		if loaded.artifact.Items == nil {
			loaded.artifact.Items = []learning.AssessmentItem{}
		}
		if loaded.artifact.RiskFlags == nil {
			loaded.artifact.RiskFlags = []learning.RiskFlag{}
		}
		view.Assessment = &loaded.artifact
		view.Decisions = loaded.decisions
		for i := range view.Decisions {
			if view.Decisions[i].Items == nil {
				view.Decisions[i].Items = []learning.AssessmentItem{}
			}
		}
		acceptance, err := learning.EvaluateAssessment(view.Activity, view.Attempt, loaded.artifact)
		if err != nil {
			return view, err
		}
		view.Reasons = append(view.Reasons, acceptance.Reasons...)
		if view.Attempt.EvidenceIneligibleReason != "" {
			view.Reasons = append(view.Reasons, view.Attempt.EvidenceIneligibleReason)
		}
		view.AllowedDecisions = learning.FeedbackDecisions(view.Activity, view.Attempt, loaded.artifact, loaded.decision)
		evidence, err := loadValidEvidenceWith(ctx, db, view.Activity.TargetNodeRevisionID)
		if err != nil {
			return view, err
		}
		for _, e := range evidence {
			if e.AssessmentID == loaded.artifact.ID {
				view.Evidence = append(view.Evidence, e)
			}
		}
		return view, nil
	})
}
