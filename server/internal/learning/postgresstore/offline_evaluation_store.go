package postgresstore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
)

func (s *Store) OfflineEvaluationCanApply(ctx context.Context, message outbox.Message) (outbox.ApplyDecision, error) {
	task, err := decodeOfflineEvaluationTask(message)
	if err != nil {
		return outbox.ApplyDecision{}, evaluationStoreError{"invalid_task", true, err}
	}
	var status string
	var generation int64
	err = s.pool.QueryRow(ctx, `
		SELECT status,learner_generation
		FROM offline_evaluation_jobs
		WHERE id=$1 AND submission_id=$2 AND outbox_id=$3`,
		task.JobID, task.SubmissionID, message.ID).Scan(&status, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return outbox.ApplyDecision{}, evaluationStoreError{"job_not_found", true, err}
	}
	if err != nil {
		return outbox.ApplyDecision{}, fmt.Errorf("read offline evaluation applicability: %w", err)
	}
	if generation != task.LearnerGeneration || generation != message.Generation {
		return outbox.ApplyDecision{Apply: false, TerminalDisposition: outbox.DispositionSuperseded}, nil
	}
	switch learning.OfflineAssessmentStatus(status) {
	case learning.OfflineAssessmentCompleted, learning.OfflineAssessmentFailed:
		return outbox.ApplyDecision{Apply: false, TerminalDisposition: outbox.DispositionSuperseded}, nil
	case learning.OfflineAssessmentQueued, learning.OfflineAssessmentProcessing, learning.OfflineAssessmentPendingRetry:
		return outbox.ApplyDecision{Apply: true}, nil
	default:
		return outbox.ApplyDecision{}, evaluationStoreError{"invalid_job_status", true, nil}
	}
}

func (s *Store) BeginOfflineEvaluation(ctx context.Context, message outbox.Message) (learning.OfflineEvaluationSnapshot, error) {
	task, err := decodeOfflineEvaluationTask(message)
	if err != nil {
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"invalid_task", true, err}
	}
	if message.LeaseToken == "" || message.LeaseExpiresAt == nil {
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"missing_outbox_lease", true, nil}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return learning.OfflineEvaluationSnapshot{}, fmt.Errorf("begin offline evaluation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var status string
	var attemptID string
	var frozenJSON []byte
	var frozenHash []byte
	var retryDeadline time.Time
	var attempts int
	var aggregateVersion int64
	var artifactJSON []byte
	var artifactHash []byte
	var lastErrorCategory string
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return learning.OfflineEvaluationSnapshot{}, fmt.Errorf("read offline evaluation clock: %w", err)
	}
	err = tx.QueryRow(ctx, `
		SELECT j.status,j.attempt_id::text,j.frozen_request,j.frozen_request_hash,j.retry_deadline,j.attempt_count,
		       h.aggregate_version,j.model_artifact,j.model_artifact_hash,COALESCE(j.last_error_category,'')
		FROM offline_evaluation_jobs j
		JOIN offline_attempt_heads h ON h.submission_id=j.submission_id
		WHERE j.id=$1 AND j.submission_id=$2 AND j.outbox_id=$3
		FOR UPDATE OF j,h`, task.JobID, task.SubmissionID, message.ID).Scan(
		&status, &attemptID, &frozenJSON, &frozenHash, &retryDeadline, &attempts, &aggregateVersion,
		&artifactJSON, &artifactHash, &lastErrorCategory,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"job_not_found", true, err}
	}
	if err != nil {
		return learning.OfflineEvaluationSnapshot{}, fmt.Errorf("lock offline evaluation job: %w", err)
	}
	canonicalFrozen, canonicalErr := canonicalJSON(frozenJSON)
	frozenDigest := sha256.Sum256(canonicalFrozen)
	legacyFrozenDigest := sha256.Sum256(frozenJSON)
	frozenHashValid := len(frozenHash) == sha256.Size &&
		(subtle.ConstantTimeCompare(frozenDigest[:], frozenHash) == 1 || subtle.ConstantTimeCompare(legacyFrozenDigest[:], frozenHash) == 1)
	var frozen offlineEvaluationFrozenSnapshot
	if canonicalErr != nil || !frozenHashValid {
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"invalid_frozen_request", true, canonicalErr}
	}
	if err := decodeStrictEvaluationJSON(canonicalFrozen, &frozen); err != nil || frozen.FutureAssessmentID != task.FutureAssessmentID ||
		frozen.Activity.ID == "" || frozen.Attempt == nil || frozen.Attempt.ID != attemptID {
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"invalid_frozen_request", true, err}
	}
	retryExpired := now.After(retryDeadline)
	switch learning.OfflineAssessmentStatus(status) {
	case learning.OfflineAssessmentCompleted, learning.OfflineAssessmentFailed:
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"job_finalized", true, nil}
	case learning.OfflineAssessmentQueued, learning.OfflineAssessmentProcessing, learning.OfflineAssessmentPendingRetry:
	default:
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"invalid_job_status", true, nil}
	}
	activity, err := loadActivityForView(ctx, tx, frozen.Activity.ID)
	if err != nil {
		return learning.OfflineEvaluationSnapshot{}, err
	}
	attempt, err := loadAttemptForView(ctx, tx, attemptID)
	if err != nil {
		return learning.OfflineEvaluationSnapshot{}, err
	}
	if activity.Revision != frozen.Activity.Revision || attempt.ActivityID != activity.ID ||
		attempt.ActivityRevision != activity.Revision || attempt.ID != frozen.Attempt.ID {
		return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"frozen_authority_mismatch", true, nil}
	}
	attempts++
	if _, err := tx.Exec(ctx, `
		UPDATE offline_evaluation_jobs
		SET status='processing',attempt_count=$2,lease_token=$3,lease_expires_at=$4,
		    last_error_category=NULL,last_error_at=NULL,updated_at=$5
		WHERE id=$1`, task.JobID, attempts, message.LeaseToken, message.LeaseExpiresAt, now); err != nil {
		return learning.OfflineEvaluationSnapshot{}, fmt.Errorf("start offline evaluation job: %w", err)
	}
	if err := appendOfflineEvaluationStatus(ctx, tx, task.SubmissionID, learning.OfflineAssessmentProcessing, learning.OfflineEvidencePendingEvaluation, nil, "", "", nil, now); err != nil {
		return learning.OfflineEvaluationSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.OfflineEvaluationSnapshot{}, fmt.Errorf("commit offline evaluation start: %w", err)
	}

	var artifact *learning.AssessmentArtifact
	if len(artifactJSON) > 0 {
		canonical, canonicalErr := canonicalJSON(artifactJSON)
		digest := sha256.Sum256(canonical)
		if canonicalErr != nil || len(artifactHash) != sha256.Size || subtle.ConstantTimeCompare(digest[:], artifactHash) != 1 {
			return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"model_artifact_corrupt", true, canonicalErr}
		}
		var value learning.AssessmentArtifact
		if err := decodeStrictEvaluationJSON(canonical, &value); err != nil || value.ID != task.FutureAssessmentID || value.AttemptID != attemptID {
			return learning.OfflineEvaluationSnapshot{}, evaluationStoreError{"model_artifact_corrupt", true, err}
		}
		artifact = &value
	}
	return learning.OfflineEvaluationSnapshot{
		Task: task, Attempt: attempt, Activity: activity, AggregateVersion: aggregateVersion,
		AttemptCount: attempts, RetryDeadline: retryDeadline, RetryExpired: retryExpired,
		LastErrorCategory: lastErrorCategory, Now: now.UTC().Truncate(time.Microsecond),
		LeaseToken: message.LeaseToken, ModelArtifact: artifact,
	}, nil
}

func (s *Store) SaveOfflineEvaluationArtifact(ctx context.Context, snapshot learning.OfflineEvaluationSnapshot, artifact learning.AssessmentArtifact) error {
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return fmt.Errorf("encode offline model artifact: %w", err)
	}
	canonical, err := canonicalJSON(encoded)
	if err != nil {
		return fmt.Errorf("canonicalize offline model artifact: %w", err)
	}
	digest := sha256.Sum256(canonical)
	command, err := s.pool.Exec(ctx, `
		UPDATE offline_evaluation_jobs
		SET model_artifact=$4,model_artifact_hash=$5,updated_at=clock_timestamp()
		WHERE id=$1 AND submission_id=$2 AND status='processing' AND lease_token=$3
		  AND (model_artifact IS NULL OR model_artifact_hash=$5)`,
		snapshot.Task.JobID, snapshot.Task.SubmissionID, snapshot.LeaseToken, canonical, digest[:])
	if err != nil {
		return fmt.Errorf("save offline model artifact: %w", err)
	}
	if command.RowsAffected() != 1 {
		return evaluationStoreError{"model_artifact_conflict", true, nil}
	}
	return nil
}

func (s *Store) MarkOfflineEvaluationRetry(ctx context.Context, snapshot learning.OfflineEvaluationSnapshot, category string) error {
	return s.markOfflineEvaluation(ctx, snapshot, learning.OfflineAssessmentPendingRetry, learning.OfflineEvidencePendingEvaluation, category)
}

func (s *Store) MarkOfflineEvaluationFailed(ctx context.Context, snapshot learning.OfflineEvaluationSnapshot, category string) error {
	return s.markOfflineEvaluation(ctx, snapshot, learning.OfflineAssessmentFailed, learning.OfflineEvidenceUnchanged, category)
}

func (s *Store) markOfflineEvaluation(ctx context.Context, snapshot learning.OfflineEvaluationSnapshot, status learning.OfflineAssessmentStatus, evidence learning.OfflineEvidenceStatus, category string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin offline evaluation status: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read offline evaluation status clock: %w", err)
	}
	if err := updateEvaluationJobStatus(ctx, tx, snapshot.Task.SubmissionID, status, category, now, snapshot.LeaseToken); err != nil {
		return err
	}
	if err := appendOfflineEvaluationStatus(ctx, tx, snapshot.Task.SubmissionID, status, evidence, []string{category}, "", "", nil, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit offline evaluation status: %w", err)
	}
	return nil
}

func (s *Store) CompleteOfflineEvaluation(ctx context.Context, snapshot learning.OfflineEvaluationSnapshot, completion learning.OfflineEvaluationCompletion, result learning.OperationResult) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin offline evaluation completion: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var current string
	var assessmentID *string
	if err := tx.QueryRow(ctx, `SELECT status,result_assessment_id::text FROM offline_evaluation_jobs WHERE id=$1 FOR UPDATE`, snapshot.Task.JobID).Scan(&current, &assessmentID); err != nil {
		return fmt.Errorf("lock offline evaluation completion: %w", err)
	}
	if current == string(learning.OfflineAssessmentCompleted) {
		if assessmentID != nil && *assessmentID == completion.Artifact.ID {
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit offline evaluation replay: %w", err)
			}
			return nil
		}
		return evaluationStoreError{"completion_conflict", true, nil}
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read offline evaluation completion clock: %w", err)
	}
	var evidenceID any
	if completion.Evidence != nil {
		evidenceID = completion.Evidence.ID
	}
	command, err := tx.Exec(ctx, `
		UPDATE offline_evaluation_jobs
		SET status='completed',result_assessment_id=$3,result_decision_id=$4,result_evidence_id=$5,
		    lease_token=NULL,lease_expires_at=NULL,last_error_category=NULL,last_error_at=NULL,updated_at=$6
		WHERE id=$1 AND submission_id=$2 AND status='processing' AND lease_token=$7`,
		snapshot.Task.JobID, snapshot.Task.SubmissionID, completion.Artifact.ID,
		completion.Decision.ID, evidenceID, now, snapshot.LeaseToken)
	if err != nil {
		return fmt.Errorf("complete offline evaluation job: %w", err)
	}
	if command.RowsAffected() != 1 {
		return evaluationStoreError{"evaluation_lease_lost", false, nil}
	}
	evidenceStatus := learning.OfflineEvidenceProvisional
	if completion.Evidence != nil {
		evidenceStatus = learning.OfflineEvidenceAccepted
	} else if snapshot.Attempt.EvidenceIneligibleReason != "" {
		evidenceStatus = learning.OfflineEvidenceNotEligible
	}
	evidenceText := ""
	if completion.Evidence != nil {
		evidenceText = completion.Evidence.ID
	}
	if err := appendOfflineEvaluationStatus(ctx, tx, snapshot.Task.SubmissionID, learning.OfflineAssessmentCompleted, evidenceStatus, completion.Reasons, completion.Artifact.ID, evidenceText, &result, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit offline evaluation completion: %w", err)
	}
	return nil
}

func decodeOfflineEvaluationTask(message outbox.Message) (learning.OfflineEvaluationTask, error) {
	var task learning.OfflineEvaluationTask
	if message.BusinessType != "learning.offline-evaluation" || message.Revision != 1 ||
		!validEvaluationUUID(message.ID) || !validEvaluationUUID(message.AggregateID) || message.Generation < 1 {
		return task, fmt.Errorf("invalid offline evaluation message envelope")
	}
	if err := decodeStrictEvaluationJSON(message.Payload, &task); err != nil {
		return task, err
	}
	if !validEvaluationUUID(task.JobID) || !validEvaluationUUID(task.SubmissionID) || !validEvaluationUUID(task.FutureAssessmentID) ||
		task.SubmissionID != message.AggregateID || task.LearnerGeneration != message.Generation {
		return task, fmt.Errorf("offline evaluation task binding mismatch")
	}
	return task, nil
}

func updateEvaluationJobStatus(ctx context.Context, tx pgx.Tx, submissionID string, status learning.OfflineAssessmentStatus, category string, now time.Time, leaseToken string) error {
	query := `
		UPDATE offline_evaluation_jobs
		SET status=$2,lease_token=NULL,lease_expires_at=NULL,last_error_category=$3,last_error_at=$4,updated_at=$4
		WHERE submission_id=$1`
	arguments := []any{submissionID, status, category, now}
	if leaseToken != "" {
		query += ` AND status='processing' AND lease_token=$5`
		arguments = append(arguments, leaseToken)
	}
	command, err := tx.Exec(ctx, query, arguments...)
	if err != nil {
		return fmt.Errorf("update offline evaluation job status: %w", err)
	}
	if command.RowsAffected() != 1 {
		return evaluationStoreError{"evaluation_lease_lost", false, nil}
	}
	return nil
}

func appendOfflineEvaluationStatus(ctx context.Context, tx pgx.Tx, submissionID string, assessment learning.OfflineAssessmentStatus, evidence learning.OfflineEvidenceStatus, reasons []string, assessmentID, evidenceID string, result *learning.OperationResult, now time.Time) error {
	var ticketID string
	var currentRevision int64
	var archive string
	var aggregateVersion, firstSequence, lastSequence, projectionAsOf int64
	if err := tx.QueryRow(ctx, `
		SELECT s.ticket_id::text,h.current_revision,r.archive_status,r.aggregate_version,
		       r.first_event_seq,r.last_event_seq,r.projection_as_of_event_seq
		FROM offline_operation_statuses s
		JOIN offline_operation_status_heads h ON h.ticket_id=s.ticket_id
		JOIN offline_operation_status_revisions r ON r.id=h.current_revision_id
		WHERE s.submission_id=$1
		FOR UPDATE OF h`, submissionID).Scan(&ticketID, &currentRevision, &archive, &aggregateVersion, &firstSequence, &lastSequence, &projectionAsOf); err != nil {
		return fmt.Errorf("lock offline operation status head: %w", err)
	}
	if result != nil {
		aggregateVersion = result.AggregateVersion
		if result.LastEventSequence > 0 {
			lastSequence = result.LastEventSequence
			projectionAsOf = result.ProjectionAsOf
		}
	}
	revision := currentRevision + 1
	revisionID := uuid.NewSHA1(eventNamespace, []byte(fmt.Sprintf("offline-status\n%s\n%d", ticketID, revision))).String()
	var assessmentValue, evidenceValue any
	if assessmentID != "" {
		assessmentValue = assessmentID
	}
	if evidenceID != "" {
		evidenceValue = evidenceID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_operation_status_revisions(
			id,ticket_id,revision,archive_status,assessment_status,evidence_status,reason_codes,
			aggregate_version,first_event_seq,last_event_seq,projection_as_of_event_seq,
			assessment_id,evidence_id,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		revisionID, ticketID, revision, archive, assessment, evidence, normalizeEvaluationReasons(reasons),
		aggregateVersion, firstSequence, lastSequence, projectionAsOf, assessmentValue, evidenceValue, now); err != nil {
		return fmt.Errorf("insert offline operation status revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE offline_operation_status_heads
		SET current_revision_id=$2,current_revision=$3,updated_at=$4
		WHERE ticket_id=$1`, ticketID, revisionID, revision, now); err != nil {
		return fmt.Errorf("advance offline operation status head: %w", err)
	}
	return nil
}

func normalizeEvaluationReasons(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

type offlineEvaluationFrozenSnapshot struct {
	Activity           learning.Activity `json:"activity"`
	Attempt            *learning.Attempt `json:"attempt"`
	FutureAssessmentID string            `json:"future_assessment_id"`
}

func decodeStrictEvaluationJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validEvaluationUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

type evaluationStoreError struct {
	category  string
	permanent bool
	cause     error
}

func (e evaluationStoreError) Error() string {
	if e.cause == nil {
		return e.category
	}
	return fmt.Sprintf("%s: %v", e.category, e.cause)
}
func (e evaluationStoreError) Unwrap() error    { return e.cause }
func (e evaluationStoreError) Category() string { return e.category }
func (e evaluationStoreError) Permanent() bool  { return e.permanent }
