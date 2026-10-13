package postgresstore

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func registerAssessmentTaskTx(ctx context.Context, tx pgx.Tx, a learning.Attempt, exercise, token string, version int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO learning_assessment_tasks(attempt_id,exercise_id,device_id,token_id,session_version,request_id,operation_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, a.ID, exercise, a.ActorDeviceID, token, version, uuid.NewString(), uuid.NewString())
	return err
}

const assessmentTaskColumns = `t.attempt_id,t.exercise_id,t.device_id,t.token_id,e.space_id,e.session_id,t.session_version,t.request_id,t.operation_id,COALESCE(t.run_id::text,''),t.failure_reason`

func scanAssessmentTask(row pgx.Row) (learning.AssessmentTask, error) {
	var t learning.AssessmentTask
	err := row.Scan(&t.AttemptID, &t.ExerciseID, &t.DeviceID, &t.TokenID, &t.SpaceID, &t.SessionID, &t.SessionVersion, &t.RequestID, &t.OperationID, &t.RunID, &t.FailureReason)
	return t, err
}

// PendingAssessmentTx is a worker owner port; the caller binds a run in this
// same transaction. SKIP LOCKED lets workers dispatch different attempts.
func (s *Store) PendingAssessmentTx(ctx context.Context, tx pgx.Tx) (*learning.AssessmentTask, error) {
	g, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return nil, err
	}
	t, err := scanAssessmentTask(tx.QueryRow(ctx, `SELECT `+assessmentTaskColumns+` FROM learning_assessment_tasks t JOIN learning_exercises e ON e.id=t.exercise_id WHERE t.run_id IS NULL AND t.failure_reason='' AND e.privacy_generation=$1 ORDER BY t.created_at,t.attempt_id FOR UPDATE OF t SKIP LOCKED LIMIT 1`, g))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
func (s *Store) AssessmentTaskTx(ctx context.Context, tx pgx.Tx, attempt string) (learning.AssessmentTask, error) {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return learning.AssessmentTask{}, err
	}
	return scanAssessmentTask(tx.QueryRow(ctx, `SELECT `+assessmentTaskColumns+` FROM learning_assessment_tasks t JOIN learning_exercises e ON e.id=t.exercise_id WHERE t.attempt_id=$1 AND e.space_id=$2`, attempt, learningspace.Scope(ctx)))
}
func (s *Store) BindAssessmentRunTx(ctx context.Context, tx pgx.Tx, attempt, run string) error {
	tag, err := tx.Exec(ctx, `UPDATE learning_assessment_tasks SET run_id=$2 WHERE attempt_id=$1 AND run_id IS NULL`, attempt, run)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeVersionConflict}
	}
	return nil
}

func (s *Store) RejectAssessmentTaskTx(ctx context.Context, tx pgx.Tx, attempt string) error {
	_, err := tx.Exec(ctx, `UPDATE learning_assessment_tasks SET failure_reason='context_changed' WHERE attempt_id=$1 AND run_id IS NULL`, attempt)
	return err
}

// Resume a budget-paused claim using its original request ID and hash. This is
// only called after the worker has verified that no unknown call is in flight.
func (s *Store) ResumeAssessmentProposalTx(ctx context.Context, tx pgx.Tx, task learning.AssessmentTask) error {
	_, err := tx.Exec(ctx, `UPDATE tutoring_proposal_requests SET status='processing',lease_token=$3,lease_expires_at=clock_timestamp()-interval '1 second',error_category=NULL,attempt_categories='{}' WHERE device_id=$1 AND request_id=$2 AND status='failed'`, task.DeviceID, task.RequestID, uuid.NewString())
	return err
}

// InspectAssessmentTx reads both durable layers before a worker decides whether
// it can resume. It does not acquire a new proposal lease or start a model call.
func (s *Store) InspectAssessmentTx(ctx context.Context, tx pgx.Tx, t learning.AssessmentTask) (learning.AssessmentRecovery, error) {
	var r learning.AssessmentRecovery
	err := tx.QueryRow(ctx, `SELECT CASE WHEN i.terminal_status='succeeded' AND NOT EXISTS (
 SELECT 1 FROM learning_events e JOIN learning_event_payloads p ON p.id=e.payload_id
 WHERE e.device_id=i.device_id AND e.operation_id=i.operation_id AND e.event_type='AssessmentRecorded'
 AND e.aggregate_id=$3 AND p.payload->>'attempt_id'=$4
 ) THEN 'conflict' ELSE i.terminal_status END
 FROM learning_inbox i WHERE i.device_id=$1 AND i.operation_id=$2 AND i.aggregate_type='session' AND i.aggregate_id=$3`, t.DeviceID, t.OperationID, t.SessionID, t.AttemptID).Scan(&r.OperationStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	err = tx.QueryRow(ctx, `SELECT status,COALESCE(result_proposal_id::text,''),lease_expires_at FROM tutoring_proposal_requests WHERE device_id=$1 AND request_id=$2 AND aggregate_id=$3 AND proposal_type='assessment' AND aggregate_type='session' AND aggregate_version=$4 FOR SHARE`, t.DeviceID, t.RequestID, t.SessionID, t.SessionVersion).Scan(&r.ProposalStatus, &r.ProposalID, &r.ProposalLeaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return r, err
}
