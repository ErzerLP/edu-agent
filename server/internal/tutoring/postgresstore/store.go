package postgresstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("tutoring record not found")

// DBTX is the caller-owned database handle used by the tutoring owner store.
// Persist never starts or commits a transaction.
type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type WriteSet struct {
	Session         *tutoring.Session
	FocusFrame      *tutoring.FocusFrame
	InvalidateFrame bool
	ResumeFrame     bool
	FreeQuestion    *tutoring.FreeQuestion
	FreeAnswer      *tutoring.FreeAnswer
	ReceivedAt      time.Time
}

func (s *Store) Persist(ctx context.Context, db DBTX, write WriteSet) error {
	if db == nil {
		return fmt.Errorf("persist tutoring records: database handle is required")
	}
	if value := write.Session; value != nil {
		completedAt := any(nil)
		if value.State == tutoring.StateCompleted {
			completedAt = write.ReceivedAt
		}
		if _, err := db.Exec(ctx, `INSERT INTO tutoring_sessions(id,aggregate_version,state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,attached_quiz,started_at,updated_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12,$13) ON CONFLICT(id) DO UPDATE SET aggregate_version=EXCLUDED.aggregate_version,state=EXCLUDED.state,goal_revision_id=EXCLUDED.goal_revision_id,route_revision_id=EXCLUDED.route_revision_id,route_step_id=EXCLUDED.route_step_id,knowledge_revision_id=EXCLUDED.knowledge_revision_id,focus_node_revision_id=EXCLUDED.focus_node_revision_id,activity_id=EXCLUDED.activity_id,attempt_id=EXCLUDED.attempt_id,attached_quiz=EXCLUDED.attached_quiz,updated_at=EXCLUDED.updated_at,completed_at=EXCLUDED.completed_at`, value.ID, value.AggregateVer, value.State, nullable(value.Context.GoalRevisionID), nullable(value.Context.RouteRevisionID), nullable(value.Context.RouteStepID), nullable(value.Context.KnowledgeRevisionID), nullable(value.Context.FocusNodeRevisionID), value.Context.ActivityID, value.Context.AttemptID, value.AttachedQuiz, write.ReceivedAt, completedAt); err != nil {
			return fmt.Errorf("persist tutoring session: %w", err)
		}
	}
	if value := write.FocusFrame; value != nil {
		if _, err := db.Exec(ctx, `INSERT INTO tutoring_focus_frames(id,session_id,saved_state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,saved_aggregate_version,created_event_seq,invalidated_at,invalidation_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT(id) DO NOTHING`, value.ID, value.SessionID, value.SavedState, nullable(value.Context.GoalRevisionID), nullable(value.Context.RouteRevisionID), nullable(value.Context.RouteStepID), nullable(value.Context.KnowledgeRevisionID), nullable(value.Context.FocusNodeRevisionID), value.Context.ActivityID, value.Context.AttemptID, value.SavedAggregateVersion, value.CreatedEventSequence, timeIf(value.Invalidated, write.ReceivedAt), nullable(value.InvalidationReason)); err != nil {
			return fmt.Errorf("insert tutoring focus frame: %w", err)
		}
	}
	if write.InvalidateFrame && write.Session != nil {
		if _, err := db.Exec(ctx, `UPDATE tutoring_focus_frames SET invalidated_at=$2,invalidation_reason=COALESCE(NULLIF(invalidation_reason,''),'command') WHERE session_id=$1 AND invalidated_at IS NULL AND resumed_at IS NULL`, write.Session.ID, write.ReceivedAt); err != nil {
			return fmt.Errorf("invalidate tutoring focus frame: %w", err)
		}
	}
	if write.ResumeFrame && write.Session != nil {
		if _, err := db.Exec(ctx, `UPDATE tutoring_focus_frames SET resumed_at=$2 WHERE session_id=$1 AND invalidated_at IS NULL AND resumed_at IS NULL`, write.Session.ID, write.ReceivedAt); err != nil {
			return fmt.Errorf("resume tutoring focus frame: %w", err)
		}
	}
	if value := write.FreeQuestion; value != nil {
		references, err := json.Marshal(value.References)
		if err != nil {
			return fmt.Errorf("encode free question references: %w", err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO tutoring_free_questions(id,session_id,focus_frame_id,session_aggregate_version,question_text,knowledge_revision_id,references_snapshot,actor_device_id,occurred_at,received_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, value.ID, value.SessionID, value.FocusFrameID, value.SessionAggregateVer, value.Text, value.KnowledgeRevisionID, references, value.ActorDeviceID, value.OccurredAt, value.ReceivedAt); err != nil {
			return fmt.Errorf("insert free question: %w", err)
		}
	}
	if value := write.FreeAnswer; value != nil {
		references, err := json.Marshal(value.References)
		if err != nil {
			return fmt.Errorf("encode free answer references: %w", err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO tutoring_free_answers(id,session_id,focus_frame_id,free_question_id,answer_text,knowledge_revision_id,references_snapshot,source_proposal_id,received_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, value.ID, value.SessionID, value.FocusFrameID, value.FreeQuestionID, value.Text, value.KnowledgeRevisionID, references, nullable(value.SourceProposalID), value.ReceivedAt); err != nil {
			return fmt.Errorf("insert free answer: %w", err)
		}
	}
	return nil
}

func (s *Store) LoadSession(ctx context.Context, id string) (tutoring.Session, error) {
	return withPrivacyRead(ctx, s, func(db DBTX) (tutoring.Session, error) {
		return s.LoadSessionWith(ctx, db, id)
	})
}

func (s *Store) LockReadWith(ctx context.Context, db DBTX) (int64, error) {
	return privacy.LockOwnerRead(ctx, db, privacy.OwnerTutoring)
}

func (s *Store) LoadSessionWith(ctx context.Context, db DBTX, id string) (tutoring.Session, error) {
	if _, err := s.LockReadWith(ctx, db); err != nil {
		return tutoring.Session{}, err
	}
	return s.LoadSessionLockedWith(ctx, db, id)
}

func (s *Store) LoadSessionLockedWith(ctx context.Context, db DBTX, id string) (tutoring.Session, error) {
	var value tutoring.Session
	var state string
	var goal, route, step, knowledgeRevision, node *string
	var activity, attempt *string
	err := db.QueryRow(ctx, `SELECT id,aggregate_version,state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,attached_quiz FROM tutoring_sessions WHERE id=$1`, id).Scan(&value.ID, &value.AggregateVer, &state, &goal, &route, &step, &knowledgeRevision, &node, &activity, &attempt, &value.AttachedQuiz)
	if errors.Is(err, pgx.ErrNoRows) {
		return tutoring.Session{}, ErrNotFound
	}
	if err != nil {
		return tutoring.Session{}, fmt.Errorf("load tutoring session: %w", err)
	}
	value.State = tutoring.State(state)
	value.Context = tutoring.FocusContext{GoalRevisionID: deref(goal), RouteRevisionID: deref(route), RouteStepID: deref(step), KnowledgeRevisionID: deref(knowledgeRevision), FocusNodeRevisionID: deref(node), ActivityID: activity, AttemptID: attempt}

	var frame tutoring.FocusFrame
	var saved string
	var frameGoal, frameRoute, frameStep, frameKnowledge, frameNode *string
	var frameActivity, frameAttempt *string
	err = db.QueryRow(ctx, `SELECT id,session_id,saved_state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,saved_aggregate_version,created_event_seq FROM tutoring_focus_frames WHERE session_id=$1 AND invalidated_at IS NULL AND resumed_at IS NULL`, id).Scan(&frame.ID, &frame.SessionID, &saved, &frameGoal, &frameRoute, &frameStep, &frameKnowledge, &frameNode, &frameActivity, &frameAttempt, &frame.SavedAggregateVersion, &frame.CreatedEventSequence)
	if err == nil {
		frame.SavedState = tutoring.State(saved)
		frame.Context = tutoring.FocusContext{GoalRevisionID: deref(frameGoal), RouteRevisionID: deref(frameRoute), RouteStepID: deref(frameStep), KnowledgeRevisionID: deref(frameKnowledge), FocusNodeRevisionID: deref(frameNode), ActivityID: frameActivity, AttemptID: frameAttempt}
		value.ActiveFrame = &frame
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return tutoring.Session{}, fmt.Errorf("load active focus frame: %w", err)
	} else {
		var invalidated, notResumed bool
		err = db.QueryRow(ctx, `SELECT invalidated_at IS NOT NULL,resumed_at IS NULL FROM tutoring_focus_frames WHERE session_id=$1 ORDER BY created_event_seq DESC,id DESC LIMIT 1`, id).Scan(&invalidated, &notResumed)
		if err == nil {
			value.FocusFrameInvalidated = invalidated && notResumed
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return tutoring.Session{}, fmt.Errorf("load latest focus frame: %w", err)
		}
	}
	return value, nil
}

func (s *Store) LoadFreeQuestion(ctx context.Context, id string) (tutoring.FreeQuestion, error) {
	return withPrivacyRead(ctx, s, func(db DBTX) (tutoring.FreeQuestion, error) {
		return s.LoadFreeQuestionWith(ctx, db, id)
	})
}

func (s *Store) LoadFreeQuestionWith(ctx context.Context, db DBTX, id string) (tutoring.FreeQuestion, error) {
	if _, err := s.LockReadWith(ctx, db); err != nil {
		return tutoring.FreeQuestion{}, err
	}
	return s.LoadFreeQuestionLockedWith(ctx, db, id)
}

func (s *Store) LoadFreeQuestionLockedWith(ctx context.Context, db DBTX, id string) (tutoring.FreeQuestion, error) {
	var value tutoring.FreeQuestion
	var refs []byte
	err := db.QueryRow(ctx, `SELECT id,session_id,focus_frame_id,session_aggregate_version,question_text,knowledge_revision_id,references_snapshot,actor_device_id,occurred_at,received_at FROM tutoring_free_questions WHERE id=$1`, id).Scan(&value.ID, &value.SessionID, &value.FocusFrameID, &value.SessionAggregateVer, &value.Text, &value.KnowledgeRevisionID, &refs, &value.ActorDeviceID, &value.OccurredAt, &value.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, fmt.Errorf("load free question: %w", err)
	}
	if err := json.Unmarshal(refs, &value.References); err != nil {
		return value, fmt.Errorf("decode free question references: %w", err)
	}
	return value, nil
}

func (s *Store) LoadFreeAnswer(ctx context.Context, id string) (tutoring.FreeAnswer, error) {
	return withPrivacyRead(ctx, s, func(db DBTX) (tutoring.FreeAnswer, error) {
		return s.LoadFreeAnswerWith(ctx, db, id)
	})
}

func (s *Store) LoadFreeAnswerWith(ctx context.Context, db DBTX, id string) (tutoring.FreeAnswer, error) {
	if _, err := s.LockReadWith(ctx, db); err != nil {
		return tutoring.FreeAnswer{}, err
	}
	return s.LoadFreeAnswerLockedWith(ctx, db, id)
}

func (s *Store) LoadFreeAnswerLockedWith(ctx context.Context, db DBTX, id string) (tutoring.FreeAnswer, error) {
	var value tutoring.FreeAnswer
	var refs []byte
	var proposal *string
	err := db.QueryRow(ctx, `SELECT id,session_id,focus_frame_id,free_question_id,answer_text,knowledge_revision_id,references_snapshot,source_proposal_id,received_at FROM tutoring_free_answers WHERE id=$1`, id).Scan(&value.ID, &value.SessionID, &value.FocusFrameID, &value.FreeQuestionID, &value.Text, &value.KnowledgeRevisionID, &refs, &proposal, &value.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, fmt.Errorf("load free answer: %w", err)
	}
	value.SourceProposalID = deref(proposal)
	if err := json.Unmarshal(refs, &value.References); err != nil {
		return value, fmt.Errorf("decode free answer references: %w", err)
	}
	return value, nil
}

func (s *Store) LatestFreeQuestion(ctx context.Context, sessionID string) (string, error) {
	return withPrivacyRead(ctx, s, func(db DBTX) (string, error) {
		return s.LatestFreeQuestionWith(ctx, db, sessionID)
	})
}

func (s *Store) LatestFreeQuestionWith(ctx context.Context, db DBTX, sessionID string) (string, error) {
	if _, err := s.LockReadWith(ctx, db); err != nil {
		return "", err
	}
	var id string
	err := db.QueryRow(ctx, `SELECT id FROM tutoring_free_questions WHERE session_id=$1 ORDER BY session_aggregate_version DESC,id DESC LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load latest free question: %w", err)
	}
	return id, nil
}

func (s *Store) LatestFreeQuestionForFrame(ctx context.Context, sessionID, frameID string) (string, error) {
	return withPrivacyRead(ctx, s, func(db DBTX) (string, error) {
		if _, err := s.LockReadWith(ctx, db); err != nil {
			return "", err
		}
		question, err := s.LatestFreeQuestionForFrameLockedWith(ctx, db, sessionID, frameID, 0)
		return question.ID, err
	})
}

func (s *Store) LatestFreeQuestionForFrameLockedWith(ctx context.Context, db DBTX, sessionID, frameID string, throughVersion int64) (tutoring.FreeQuestion, error) {
	var id *string
	var matches int64
	err := db.QueryRow(ctx, `
		WITH eligible AS (
			SELECT id,session_aggregate_version
			FROM tutoring_free_questions
			WHERE session_id=$1
			  AND focus_frame_id=$2
			  AND ($3::bigint=0 OR session_aggregate_version<=$3)
		), latest AS (
			SELECT max(session_aggregate_version) AS session_aggregate_version
			FROM eligible
		)
		SELECT min(eligible.id::text),count(*)
		FROM eligible
		JOIN latest USING(session_aggregate_version)`, sessionID, frameID, throughVersion).Scan(&id, &matches)
	if err != nil {
		return tutoring.FreeQuestion{}, fmt.Errorf("load latest frame free question: %w", err)
	}
	if matches == 0 || id == nil {
		return tutoring.FreeQuestion{}, ErrNotFound
	}
	if matches != 1 {
		return tutoring.FreeQuestion{}, fmt.Errorf("load latest frame free question: ambiguous current version")
	}
	return s.LoadFreeQuestionLockedWith(ctx, db, *id)
}

func (s *Store) LoadFreeAnswerForQuestionLockedWith(ctx context.Context, db DBTX, questionID string) (tutoring.FreeAnswer, bool, error) {
	var id string
	if err := db.QueryRow(ctx, `SELECT id FROM tutoring_free_answers WHERE free_question_id=$1`, questionID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return tutoring.FreeAnswer{}, false, nil
		}
		return tutoring.FreeAnswer{}, false, fmt.Errorf("load free answer for question: %w", err)
	}
	answer, err := s.LoadFreeAnswerLockedWith(ctx, db, id)
	return answer, err == nil, err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func timeIf(condition bool, value time.Time) any {
	if !condition {
		return nil
	}
	return value
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
