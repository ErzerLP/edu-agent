package postgresstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var _ learning.OfflineIngestStore = (*Store)(nil)

var offlineNamespace = uuid.MustParse("f70db2fa-5bb9-4dd4-bdce-902d15c261b1")

type storedOfflineAuthorization struct {
	SubmissionID      string
	PackID            string
	DeviceID          string
	OperationID       string
	DeviceSequence    int64
	ActivityID        string
	ActivityRevision  int64
	LearnerGeneration int64
	CredentialEpoch   int64
	ExpectedVersion   int64
	AuthorizationHash []byte
	Authorization     []byte
	SignerKeyID       string
	Signature         []byte
	EligibleUntil     time.Time
	ArchiveUntil      time.Time
	HeadState         learning.OfflineSubmissionState
	AggregateVersion  int64
}

type storedOfflineActivity struct {
	Activity          learning.Activity
	LearnerGeneration int64
	PracticeKind      string
	PayloadHash       []byte
	IssuedAt          time.Time
	EligibleUntil     time.Time
	ArchiveUntil      time.Time
}

type offlinePlan struct {
	activity            storedOfflineActivity
	materialize         bool
	attempt             *learning.Attempt
	assessment          *learning.AssessmentArtifact
	decision            *learning.AssessmentDecision
	evidence            *learning.AcceptedEvidence
	assessmentStatus    learning.OfflineAssessmentStatus
	evidenceStatus      learning.OfflineEvidenceStatus
	reasons             []string
	jobID               string
	futureAssessmentID  string
	outboxMessage       *outbox.Message
	evidenceClaim       bool
	evidenceClaimSource string
}

func (s *Store) IngestOffline(ctx context.Context, request learning.OfflineIngestRequest) (learning.OfflineIngestResult, error) {
	operation := request.Operation
	if err := operation.Validate(); err != nil {
		return learning.OfflineIngestResult{}, err
	}
	requestHash, err := operation.CanonicalHash()
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	requestHashBytes, _ := hex.DecodeString(requestHash)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("begin offline ingest: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "offline-operation:"+operation.DeviceID+":"+operation.OperationID); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline operation: %w", err)
	}

	var revokedAt *time.Time
	var credentialEpoch int64
	err = tx.QueryRow(ctx, `
		SELECT device.revoked_at,credential.credential_epoch
		FROM devices device
		JOIN offline_device_credentials credential ON credential.device_id=device.id
		WHERE device.id=$1
		FOR UPDATE OF device,credential`, operation.DeviceID).Scan(&revokedAt, &credentialEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return blockedOfflineResult(operation, learning.OfflineReasonDeviceRevoked), nil
	}
	if err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline device credential: %w", err)
	}
	if revokedAt != nil || credentialEpoch != operation.CredentialEpoch {
		return blockedOfflineResult(operation, learning.OfflineReasonDeviceRevoked), nil
	}
	learningGeneration, err := lockLearningWriteGeneration(ctx, tx, operation.LearnerGeneration)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	tutoringGeneration, err := s.tutoring.LockReadWith(ctx, tx)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if s.knowledge == nil {
		return learning.OfflineIngestResult{}, errors.New("offline knowledge owner is required")
	}
	knowledgeGeneration, err := s.knowledge.LockReadWith(ctx, tx)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if tutoringGeneration != learningGeneration || knowledgeGeneration != learningGeneration {
		return blockedOfflineResult(operation, learning.OfflineReasonContentRedacted), nil
	}

	if replay, found, err := lookupOfflineReplay(ctx, tx, operation, requestHash); err != nil || found {
		if err == nil {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return learning.OfflineIngestResult{}, fmt.Errorf("commit offline replay: %w", commitErr)
			}
		}
		return replay, err
	}

	authorization, conflict, err := lockOfflineAuthorization(ctx, tx, operation)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if conflict != nil {
		return *conflict, nil
	}
	if !equalHash(authorization.AuthorizationHash, operation.AuthorizationHash) ||
		!bytes.Equal(authorization.Authorization, operation.Authorization) ||
		!bytes.Equal(authorization.Signature, operation.AuthorizationSig) ||
		authorization.ExpectedVersion != operation.ExpectedVersion ||
		authorization.CredentialEpoch != operation.CredentialEpoch ||
		authorization.LearnerGeneration != operation.LearnerGeneration {
		return blockedOfflineResult(operation, learning.OfflineReasonAuthorizationInvalid), nil
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:offline_attempt:"+operation.SubmissionID); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline attempt aggregate: %w", err)
	}
	var aggregateVersion int64
	err = tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='offline_attempt' AND aggregate_id=$1 FOR UPDATE`, operation.SubmissionID).Scan(&aggregateVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		aggregateVersion = 0
		if _, err := tx.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('offline_attempt',$1,0,0,clock_timestamp())`, operation.SubmissionID); err != nil {
			return learning.OfflineIngestResult{}, fmt.Errorf("create offline attempt aggregate: %w", err)
		}
	} else if err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline attempt aggregate head: %w", err)
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-activity-evidence:"+operation.OfflineActivityID+fmt.Sprintf(":%d", operation.ActivityRevision)); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline activity evidence slot: %w", err)
	}
	var existingWinner string
	err = tx.QueryRow(ctx, `SELECT winning_attempt_id::text FROM learning_activity_evidence_claims WHERE activity_id=$1 AND activity_revision=$2 FOR UPDATE`, operation.OfflineActivityID, operation.ActivityRevision).Scan(&existingWinner)
	if errors.Is(err, pgx.ErrNoRows) {
		existingWinner = ""
	} else if err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline activity evidence claim: %w", err)
	}

	var receivedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("read offline ingest database clock: %w", err)
	}
	receivedAt = receivedAt.UTC().Truncate(time.Microsecond)

	if authorization.HeadState != learning.OfflineSubmissionReserved || aggregateVersion != 0 {
		return s.commitOfflineRejection(ctx, tx, operation, authorization, requestHashBytes, receivedAt, learning.OfflineReasonVersionConflict)
	}
	activity, err := loadOfflineActivityLocked(ctx, tx, authorization.ActivityID, authorization.ActivityRevision)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if activity.LearnerGeneration != operation.LearnerGeneration || activity.Activity.ID != operation.OfflineActivityID || receivedAt.After(authorization.ArchiveUntil) || receivedAt.After(activity.ArchiveUntil) {
		return s.commitOfflineRejection(ctx, tx, operation, authorization, requestHashBytes, receivedAt, learning.OfflineReasonAuthorizationExpired)
	}

	redacted, knowledgeHeadValue, err := s.knowledge.RevisionHeadLockedWith(ctx, tx, activity.Activity.KnowledgeRevisionID)
	if err != nil {
		return blockedOfflineResult(operation, learning.OfflineReasonActivityInvalid), nil
	}
	if redacted {
		return blockedOfflineResult(operation, learning.OfflineReasonContentRedacted), nil
	}
	var knowledgeHead *string
	if knowledgeHeadValue != "" {
		knowledgeHead = &knowledgeHeadValue
	}

	materialized, err := offlineActivityMaterialized(ctx, tx, activity.Activity)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if !materialized {
		if err := materializeOfflineActivity(ctx, tx, activity.Activity); err != nil {
			return learning.OfflineIngestResult{}, err
		}
	}

	if operation.Type == learning.OfflineActivitySkipped {
		return s.commitOfflineSuccess(ctx, tx, operation, authorization, requestHashBytes, receivedAt, offlinePlan{
			activity: activity, materialize: !materialized,
			assessmentStatus: learning.OfflineAssessmentNotRequested,
			evidenceStatus:   learning.OfflineEvidenceNotApplicable,
		})
	}

	plan, err := s.planOfflineAttempt(ctx, tx, operation, activity, receivedAt, existingWinner, knowledgeHead, !materialized)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	return s.commitOfflineSuccess(ctx, tx, operation, authorization, requestHashBytes, receivedAt, plan)
}

func lockLearningWriteGeneration(ctx context.Context, tx pgx.Tx, expected int64) (int64, error) {
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',$1)`, expected).Scan(&generation); err != nil {
		message := databaseMessage(err)
		switch message {
		case "content_redacted":
			return 0, &privacy.Error{Code: privacy.CodeContentRedacted, Reason: "learning_write_gate_closed", Cause: err}
		case "privacy_clear_in_progress", "privacy owner generation changed":
			return 0, &privacy.Error{Code: privacy.CodePrivacyClearInProgress, Reason: "learning_write_gate_closed", Cause: err}
		default:
			return 0, fmt.Errorf("lock learning privacy write generation: %w", err)
		}
	}
	return generation, nil
}

func databaseMessage(err error) string {
	var pgErr interface{ Error() string }
	if errors.As(err, &pgErr) {
		text := pgErr.Error()
		for _, message := range []string{"content_redacted", "privacy_clear_in_progress", "privacy owner generation changed"} {
			if strings.Contains(text, message) {
				return message
			}
		}
	}
	return ""
}

func lookupOfflineReplay(ctx context.Context, tx pgx.Tx, operation learning.OfflineOperation, requestHash string) (learning.OfflineIngestResult, bool, error) {
	var storedHash []byte
	var resultJSON []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,result FROM learning_inbox WHERE device_id=$1 AND operation_id=$2 FOR UPDATE`, operation.DeviceID, operation.OperationID).Scan(&storedHash, &resultJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.OfflineIngestResult{}, false, nil
	}
	if err != nil {
		return learning.OfflineIngestResult{}, false, fmt.Errorf("read offline inbox: %w", err)
	}
	if hex.EncodeToString(storedHash) != requestHash {
		result := conflictOfflineResult(operation, learning.OfflineIdempotencyConflict, learning.OfflineReasonIdempotencyConflict)
		return result, true, nil
	}
	var result learning.OfflineIngestResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		return learning.OfflineIngestResult{}, true, fmt.Errorf("decode offline inbox result: %w", err)
	}
	result.Replayed = true
	return result, true, nil
}

func lockOfflineAuthorization(ctx context.Context, tx pgx.Tx, operation learning.OfflineOperation) (storedOfflineAuthorization, *learning.OfflineIngestResult, error) {
	var value storedOfflineAuthorization
	err := tx.QueryRow(ctx, `
		SELECT a.submission_id,a.pack_id,a.device_id,
		       a.operation_id,a.device_seq,a.offline_activity_id,
		       a.activity_revision,a.learner_generation,
		       a.credential_epoch,a.expected_version,
		       a.authorization_hash,a.authorization_payload,a.signer_key_id,a.signature,
		       a.eligible_until,a.archive_until,
		       h.state,h.aggregate_version
		FROM offline_device_sequence_reservations r
		JOIN offline_submission_authorizations a
		  ON a.device_id=r.device_id
		 AND a.device_seq=r.device_seq
		 AND a.operation_id=r.operation_id
		 AND a.submission_id=r.submission_id
		JOIN offline_attempt_heads h ON h.submission_id=a.submission_id
		WHERE r.device_id=$1 AND r.device_seq=$2
		FOR UPDATE OF r,a,h`, operation.DeviceID, int64(operation.DeviceSequence)).Scan(
		&value.SubmissionID, &value.PackID, &value.DeviceID, &value.OperationID, &value.DeviceSequence,
		&value.ActivityID, &value.ActivityRevision, &value.LearnerGeneration, &value.CredentialEpoch,
		&value.ExpectedVersion, &value.AuthorizationHash, &value.Authorization, &value.SignerKeyID,
		&value.Signature, &value.EligibleUntil, &value.ArchiveUntil,
		&value.HeadState, &value.AggregateVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		var reservedOperation, reservedSubmission string
		err = tx.QueryRow(ctx, `SELECT operation_id::text,submission_id::text FROM offline_submission_authorizations WHERE device_id=$1 AND operation_id=$2`, operation.DeviceID, operation.OperationID).Scan(&reservedOperation, &reservedSubmission)
		if err == nil {
			result := conflictOfflineResult(operation, learning.OfflineIdempotencyConflict, learning.OfflineReasonIdempotencyConflict)
			return value, &result, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return value, nil, err
		}
		result := conflictOfflineResult(operation, learning.OfflineSequenceConflict, learning.OfflineReasonSequenceConflict)
		return value, &result, nil
	}
	if err != nil {
		return value, nil, fmt.Errorf("lock offline sequence reservation: %w", err)
	}
	canonicalAuthorization, err := learning.CanonicalizeJCS(value.Authorization)
	if err != nil {
		return value, nil, fmt.Errorf("canonicalize stored offline authorization: %w", err)
	}
	value.Authorization = canonicalAuthorization
	if value.OperationID != operation.OperationID || value.SubmissionID != operation.SubmissionID || value.ActivityID != operation.OfflineActivityID || value.ActivityRevision != operation.ActivityRevision {
		result := conflictOfflineResult(operation, learning.OfflineSequenceConflict, learning.OfflineReasonSequenceConflict)
		return value, &result, nil
	}
	return value, nil, nil
}

func loadOfflineActivityLocked(ctx context.Context, tx pgx.Tx, id string, revision int64) (storedOfflineActivity, error) {
	var value storedOfflineActivity
	var activityType string
	var rubricJSON []byte
	var allowed []string
	var reviewKind string
	err := tx.QueryRow(ctx, `
		SELECT id,revision,parent_session_id,goal_revision_id,route_revision_id,route_step_id,
		       knowledge_revision_id,target_node_id,target_node_revision_id,prompt,activity_type,
		       rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,
		       assessment_policy_version,review_policy_version,practice_kind,learner_generation,
		       payload_hash,issued_at,eligible_until,archive_until,created_at
		FROM offline_activities WHERE id=$1 AND revision=$2 FOR SHARE`, id, revision).Scan(
		&value.Activity.ID, &value.Activity.Revision, &value.Activity.SessionID,
		&value.Activity.GoalRevisionID, &value.Activity.RouteRevisionID, &value.Activity.RouteStepID,
		&value.Activity.KnowledgeRevisionID, &value.Activity.TargetNodeID, &value.Activity.TargetNodeRevisionID,
		&value.Activity.Prompt, &activityType, &value.Activity.Rubric.Revision, &rubricJSON,
		&value.Activity.Difficulty, &allowed, &value.Activity.ActivityPolicyVersion,
		&value.Activity.AssessmentPolicyVersion, &value.Activity.ReviewPolicyVersion, &reviewKind,
		&value.LearnerGeneration, &value.PayloadHash, &value.IssuedAt, &value.EligibleUntil,
		&value.ArchiveUntil, &value.Activity.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &learning.Error{Code: learning.CodeNotFound, Reason: learning.OfflineReasonActivityInvalid}
	}
	if err != nil {
		return value, fmt.Errorf("load offline activity: %w", err)
	}
	value.Activity.Type = learning.ActivityType(activityType)
	value.PracticeKind = reviewKind
	value.Activity.Review = reviewKind == "review"
	if err := json.Unmarshal(rubricJSON, &value.Activity.Rubric); err != nil {
		return value, fmt.Errorf("decode offline activity rubric: %w", err)
	}
	for _, item := range allowed {
		value.Activity.AllowedHelp = append(value.Activity.AllowedHelp, learning.HelpLevel(item))
	}
	rows, err := tx.Query(ctx, `SELECT knowledge_revision_id,node_id,node_revision_id,document_revision_id,source_start,source_end,slice_text,slice_hash FROM offline_activity_references WHERE activity_id=$1 AND activity_revision=$2 ORDER BY ordinal`, id, revision)
	if err != nil {
		return value, fmt.Errorf("load offline activity references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reference learning.KnowledgeReference
		var hash []byte
		if err := rows.Scan(&reference.KnowledgeRevisionID, &reference.NodeID, &reference.NodeRevisionID, &reference.DocumentRevisionID, &reference.Range.Start, &reference.Range.End, &reference.Slice, &hash); err != nil {
			return value, err
		}
		reference.SliceSHA256 = hex.EncodeToString(hash)
		value.Activity.References = append(value.Activity.References, reference)
	}
	if err := rows.Err(); err != nil {
		return value, err
	}
	if len(value.Activity.References) == 0 {
		return value, &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: learning.OfflineReasonActivityInvalid}
	}
	return value, nil
}

func offlineActivityMaterialized(ctx context.Context, tx pgx.Tx, activity learning.Activity) (bool, error) {
	var identical bool
	err := tx.QueryRow(ctx, `
		SELECT revision=$2 AND session_id=$3 AND goal_revision_id=$4 AND route_revision_id=$5
		   AND route_step_id=$6 AND knowledge_revision_id=$7 AND target_node_id=$8
		   AND target_node_revision_id=$9
		FROM learning_activities WHERE id=$1 FOR SHARE`, activity.ID, activity.Revision,
		activity.SessionID, activity.GoalRevisionID, activity.RouteRevisionID, activity.RouteStepID,
		activity.KnowledgeRevisionID, activity.TargetNodeID, activity.TargetNodeRevisionID).Scan(&identical)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check materialized offline activity: %w", err)
	}
	if !identical {
		return false, &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: learning.OfflineReasonActivityInvalid}
	}
	return true, nil
}

func materializeOfflineActivity(ctx context.Context, tx pgx.Tx, activity learning.Activity) error {
	rubric, err := json.Marshal(activity.Rubric)
	if err != nil {
		return err
	}
	allowed := make([]string, len(activity.AllowedHelp))
	for index := range activity.AllowedHelp {
		allowed[index] = string(activity.AllowedHelp[index])
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_activities(
			id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,
			knowledge_revision_id,target_node_id,target_node_revision_id,prompt,activity_type,
			rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,
			assessment_policy_version,review_policy_version,is_review,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		activity.ID, activity.Revision, activity.SessionID, activity.GoalRevisionID,
		activity.RouteRevisionID, activity.RouteStepID, activity.KnowledgeRevisionID,
		activity.TargetNodeID, activity.TargetNodeRevisionID, activity.Prompt, activity.Type,
		activity.Rubric.Revision, rubric, activity.Difficulty, allowed,
		activity.ActivityPolicyVersion, activity.AssessmentPolicyVersion,
		activity.ReviewPolicyVersion, activity.Review, activity.CreatedAt); err != nil {
		return fmt.Errorf("materialize offline learning activity: %w", err)
	}
	for ordinal, reference := range activity.References {
		hash, err := decodeHash(reference.SliceSHA256)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO learning_activity_references(
				activity_id,ordinal,knowledge_revision_id,node_id,node_revision_id,
				document_revision_id,source_start,source_end,slice_text,slice_hash)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, activity.ID, ordinal,
			reference.KnowledgeRevisionID, reference.NodeID, reference.NodeRevisionID,
			reference.DocumentRevisionID, reference.Range.Start, reference.Range.End,
			reference.Slice, hash); err != nil {
			return fmt.Errorf("materialize offline activity reference: %w", err)
		}
	}
	return nil
}

func (s *Store) planOfflineAttempt(ctx context.Context, tx pgx.Tx, operation learning.OfflineOperation, activity storedOfflineActivity, receivedAt time.Time, existingWinner string, knowledgeHead *string, materialize bool) (offlinePlan, error) {
	payload := operation.Attempt
	attempt := &learning.Attempt{
		ID: operation.SubmissionID, SessionID: activity.Activity.SessionID,
		ActivityID: activity.Activity.ID, ActivityRevision: activity.Activity.Revision,
		AnswerPayloadID: offlineID("attempt-payload", operation.SubmissionID),
		Answer:          payload.Answer, AnswerSHA256: payload.AnswerSHA256, Help: payload.Help,
		ActorDeviceID: operation.DeviceID, OccurredAt: operation.OccurredAt, ReceivedAt: receivedAt,
		ArchiveDisposition: "offline_succeeded", OfflineSubmissionID: operation.SubmissionID,
	}

	otherwiseEligible := true
	reason := ""
	switch {
	case receivedAt.After(activity.EligibleUntil):
		otherwiseEligible, reason = false, learning.OfflineReasonExpiredActivity
	case knowledgeHead == nil || *knowledgeHead != activity.Activity.KnowledgeRevisionID:
		otherwiseEligible, reason = false, learning.OfflineReasonStaleKnowledge
	case activity.Activity.ActivityPolicyVersion != learning.ActivityPolicyVersion || activity.Activity.AssessmentPolicyVersion != learning.AssessmentPolicyVersion || activity.Activity.ReviewPolicyVersion != learning.ReviewPolicyVersion:
		otherwiseEligible, reason = false, learning.OfflineReasonStalePolicy
	case payload.Help == learning.HelpAnswerRevealed:
		otherwiseEligible, reason = false, learning.OfflineReasonAnswerRevealed
	}
	if otherwiseEligible {
		sourceSession, err := s.tutoring.LoadSessionLockedWith(ctx, tx, activity.Activity.SessionID)
		if err != nil {
			return offlinePlan{}, fmt.Errorf("load offline source session context: %w", err)
		}
		if sourceSession.Context.GoalRevisionID != activity.Activity.GoalRevisionID || sourceSession.Context.RouteRevisionID != activity.Activity.RouteRevisionID {
			otherwiseEligible, reason = false, learning.OfflineReasonStaleContext
		}
	}
	winner, err := learning.DecideOfflineWinner(existingWinner, attempt.ID, otherwiseEligible, reason)
	if err != nil {
		return offlinePlan{}, err
	}
	attempt.EvidenceEligibility = winner.EvidenceEligibility
	attempt.EvidenceIneligibleReason = winner.Reason

	plan := offlinePlan{
		activity: activity, materialize: materialize, attempt: attempt,
		assessmentStatus: learning.OfflineAssessmentNotRequested,
		evidenceStatus:   learning.OfflineEvidenceProvisional,
	}
	if winner.Winner {
		plan.evidenceClaim = existingWinner == ""
		plan.evidenceClaimSource = "offline"
	}
	if winner.Reason != "" {
		plan.reasons = []string{winner.Reason}
		if winner.Reason == learning.OfflineReasonAnswerRevealed {
			plan.evidenceStatus = learning.OfflineEvidenceNotEligible
		}
		return plan, nil
	}

	switch activity.Activity.Type {
	case learning.ActivityObjective:
		assessmentID := offlineID("assessment", operation.SubmissionID)
		artifact := &learning.AssessmentArtifact{
			ID: assessmentID, SessionID: activity.Activity.SessionID, AttemptID: attempt.ID,
			ActivityID: activity.Activity.ID, ActivityRevision: activity.Activity.Revision,
			RubricComplete: true, Confidence: 1000, ModelID: "deterministic-objective",
			ModelParameters: map[string]any{}, PromptRevision: "objective-rule-v1",
			ProposalInputHash: learning.SHA256([]byte(operation.SubmissionID + "\nobjective")),
			Attempts:          1, AttemptCategories: []string{"deterministic"}, CreatedAt: receivedAt,
			EvidenceEligibility: true,
		}
		acceptance, err := learning.EvaluateAssessment(activity.Activity, *attempt, *artifact)
		if err != nil {
			return offlinePlan{}, err
		}
		decision := &learning.AssessmentDecision{
			ID: offlineID("decision", operation.SubmissionID), AssessmentID: assessmentID,
			Version: 1, Disposition: acceptance.Disposition, Items: artifact.Items,
			ActorDeviceID: operation.DeviceID, CreatedAt: receivedAt,
		}
		evidence := &learning.AcceptedEvidence{
			ID: offlineID("evidence", operation.SubmissionID), DispositionDecisionID: decision.ID,
			AssessmentID: assessmentID, AttemptID: attempt.ID, ActivityID: activity.Activity.ID,
			ActivityRevision: activity.Activity.Revision, GoalRevisionID: activity.Activity.GoalRevisionID,
			RouteRevisionID:     activity.Activity.RouteRevisionID,
			KnowledgeRevisionID: activity.Activity.KnowledgeRevisionID,
			NodeRevisionID:      activity.Activity.TargetNodeRevisionID,
			RubricRevision:      activity.Activity.Rubric.Revision, Kind: learning.EvidencePracticeRecall,
			ActivityType: activity.Activity.Type, Outcome: acceptance.Outcome, Help: attempt.Help,
			ReceivedAt: receivedAt, AcceptancePolicyVersion: learning.AssessmentPolicyVersion,
			ReducerPolicyVersion: learning.MasteryReducerVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion,
		}
		if activity.Activity.Review {
			evidence.Kind = learning.EvidenceReviewRecall
		}
		decision.ProducedEvidenceID = &evidence.ID
		plan.assessment, plan.decision, plan.evidence = artifact, decision, evidence
		plan.assessmentStatus = learning.OfflineAssessmentCompleted
		plan.evidenceStatus = learning.OfflineEvidenceAccepted
	case learning.ActivityOpen:
		plan.jobID = offlineID("evaluation-job", operation.SubmissionID)
		plan.futureAssessmentID = offlineID("assessment", operation.SubmissionID)
		message, err := outbox.NewMessage(outbox.NewMessageInput{
			BusinessType: "learning.offline-evaluation", AggregateID: operation.SubmissionID,
			IdempotencyKey: "learning.offline-evaluation:" + operation.SubmissionID,
			Revision:       1, Generation: operation.LearnerGeneration,
			Payload: mustMarshalStore(learning.OfflineEvaluationTask{
				JobID: plan.jobID, SubmissionID: operation.SubmissionID,
				FutureAssessmentID: plan.futureAssessmentID, LearnerGeneration: operation.LearnerGeneration,
			}),
			AuditMetadata: json.RawMessage(`{"source":"offline"}`), MaxAttempts: 100,
		}, receivedAt)
		if err != nil {
			return offlinePlan{}, err
		}
		plan.outboxMessage = &message
		plan.assessmentStatus = learning.OfflineAssessmentQueued
		plan.evidenceStatus = learning.OfflineEvidencePendingEvaluation
	default:
		return offlinePlan{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: learning.OfflineReasonActivityInvalid}
	}
	return plan, nil
}

func (s *Store) commitOfflineRejection(ctx context.Context, tx pgx.Tx, operation learning.OfflineOperation, authorization storedOfflineAuthorization, requestHash []byte, receivedAt time.Time, reason string) (learning.OfflineIngestResult, error) {
	activity, err := loadOfflineActivityLocked(ctx, tx, authorization.ActivityID, authorization.ActivityRevision)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	materialized, err := offlineActivityMaterialized(ctx, tx, activity.Activity)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if !materialized {
		if err := materializeOfflineActivity(ctx, tx, activity.Activity); err != nil {
			return learning.OfflineIngestResult{}, err
		}
	}
	draft := offlineDraft(learning.EventOfflineOperationRejected, operation, activity.Activity, learning.OfflineEvidenceUnchanged, map[string]any{
		"operation_id": operation.OperationID, "submission_id": operation.SubmissionID,
		"device_seq": fmt.Sprintf("%d", operation.DeviceSequence), "reason": reason,
		"received_at": receivedAt,
	})
	draft.ArchiveDisposition = "rejected"
	return s.persistOfflineTerminal(ctx, tx, operation, authorization, requestHash, receivedAt,
		learning.OfflineArchivedRejected, learning.OfflineAssessmentNotRequested,
		learning.OfflineEvidenceUnchanged, []string{reason}, []learning.EventDraft{draft}, offlinePlan{})
}

func (s *Store) commitOfflineSuccess(ctx context.Context, tx pgx.Tx, operation learning.OfflineOperation, authorization storedOfflineAuthorization, requestHash []byte, receivedAt time.Time, plan offlinePlan) (learning.OfflineIngestResult, error) {
	events := make([]learning.EventDraft, 0, 6)
	if plan.materialize {
		events = append(events, offlineDraft(learning.EventOfflineActivityMaterialized, operation, plan.activity.Activity, plan.evidenceStatus, plan.activity.Activity))
	}
	if operation.Type == learning.OfflineActivitySkipped {
		events = append(events, offlineDraft(learning.EventOfflineActivitySkipped, operation, plan.activity.Activity, plan.evidenceStatus, map[string]any{
			"submission_id": operation.SubmissionID, "offline_activity_id": operation.OfflineActivityID,
			"reason": operation.Skip.Reason, "received_at": receivedAt,
		}))
	} else {
		events = append(events, offlineDraft(learning.EventOfflineAttemptSubmitted, operation, plan.activity.Activity, plan.evidenceStatus, plan.attempt))
		if plan.assessment != nil {
			events = append(events, offlineDraft(learning.EventAssessmentRecorded, operation, plan.activity.Activity, plan.evidenceStatus, plan.assessment))
		}
		if plan.decision != nil {
			events = append(events, offlineDraft(learning.EventAssessmentAccepted, operation, plan.activity.Activity, plan.evidenceStatus, learning.AssessmentProjectionEvent{
				AssessmentID: plan.assessment.ID, NodeRevisionID: plan.activity.Activity.TargetNodeRevisionID, Decision: *plan.decision,
			}))
		}
		if plan.evidence != nil {
			events = append(events, offlineDraft(learning.EventEvidenceAccepted, operation, plan.activity.Activity, plan.evidenceStatus, plan.evidence))
		}
		if plan.jobID != "" {
			events = append(events, offlineDraft(learning.EventOfflineAssessmentQueued, operation, plan.activity.Activity, plan.evidenceStatus, map[string]any{
				"assessment_id": plan.futureAssessmentID, "job_id": plan.jobID,
				"node_revision_id": plan.activity.Activity.TargetNodeRevisionID, "reasons": []string{},
			}))
		}
	}
	return s.persistOfflineTerminal(ctx, tx, operation, authorization, requestHash, receivedAt,
		learning.OfflineArchivedSucceeded, plan.assessmentStatus, plan.evidenceStatus,
		plan.reasons, events, plan)
}

func (s *Store) persistOfflineTerminal(ctx context.Context, tx pgx.Tx, operation learning.OfflineOperation, authorization storedOfflineAuthorization, requestHash []byte, receivedAt time.Time, archiveStatus learning.OfflineArchiveStatus, assessmentStatus learning.OfflineAssessmentStatus, evidenceStatus learning.OfflineEvidenceStatus, reasons []string, drafts []learning.EventDraft, plan offlinePlan) (learning.OfflineIngestResult, error) {
	reasons = normalizeTextArray(reasons)
	if err := learning.ValidateOfflineResultCombination(archiveStatus, assessmentStatus, evidenceStatus); err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if len(drafts) == 0 {
		return learning.OfflineIngestResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "empty_offline_event_batch"}
	}
	var clock int64
	if err := tx.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&clock); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline event clock: %w", err)
	}
	firstSequence := clock + 1
	for ordinal := range drafts {
		if drafts[ordinal].Type == learning.EventEvidenceAccepted && plan.evidence != nil {
			plan.evidence.AcceptedEventSequence = firstSequence + int64(ordinal)
			drafts[ordinal].Payload = mustMarshalStore(plan.evidence)
		}
	}
	lastSequence := firstSequence + int64(len(drafts)) - 1
	aggregateVersion := int64(len(drafts))

	if plan.attempt != nil {
		if err := insertOfflineAttemptRecords(ctx, tx, operation, plan, firstSequence, drafts); err != nil {
			return learning.OfflineIngestResult{}, err
		}
	}
	if plan.outboxMessage != nil {
		if _, err := outboxpostgres.EnqueueWith(ctx, tx, *plan.outboxMessage); err != nil {
			return learning.OfflineIngestResult{}, fmt.Errorf("enqueue offline evaluation outbox: %w", err)
		}
		frozenRaw := mustMarshalStore(offlineEvaluationFrozenSnapshot{
			Activity: plan.activity.Activity, Attempt: plan.attempt,
			FutureAssessmentID: plan.futureAssessmentID,
		})
		frozen, err := canonicalJSON(frozenRaw)
		if err != nil {
			return learning.OfflineIngestResult{}, fmt.Errorf("canonicalize offline evaluation request: %w", err)
		}
		frozenHash, _ := hex.DecodeString(learning.SHA256(frozen))
		deadline := receivedAt.Add(7 * 24 * time.Hour)
		if authorization.ArchiveUntil.Before(deadline) {
			deadline = authorization.ArchiveUntil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO offline_evaluation_jobs(
				id,submission_id,attempt_id,learner_generation,status,frozen_request,frozen_request_hash,outbox_id,
				available_at,retry_deadline,created_at,updated_at)
			VALUES($1,$2,$3,$4,'queued',$5,$6,$7,$8,$9,$8,$8)`, plan.jobID,
			operation.SubmissionID, plan.attempt.ID, operation.LearnerGeneration, frozen, frozenHash,
			plan.outboxMessage.ID, receivedAt, deadline); err != nil {
			return learning.OfflineIngestResult{}, fmt.Errorf("insert offline evaluation job: %w", err)
		}
	}

	for ordinal, draft := range drafts {
		sequence := firstSequence + int64(ordinal)
		payload, err := canonicalJSON(draft.Payload)
		if err != nil {
			return learning.OfflineIngestResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_event_payload", Cause: err}
		}
		payloadHash := learning.SHA256(payload)
		payloadHashBytes, _ := hex.DecodeString(payloadHash)
		eventID := offlineID(fmt.Sprintf("event-%d", ordinal), operation.OperationID)
		payloadID := offlineID("event-payload", eventID)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,$4)`, payloadID, payload, payloadHashBytes, receivedAt); err != nil {
			return learning.OfflineIngestResult{}, fmt.Errorf("insert offline event payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO learning_events(
				event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,
				aggregate_version,device_id,operation_id,operation_ordinal,received_at,occurred_at,
				payload_id,payload_hash,parent_session_id,event_source,archive_disposition,
				evidence_disposition,goal_revision_id,route_revision_id,knowledge_revision_id,
				activity_id,activity_revision)
			VALUES($1,$2,$3,$4,'offline_attempt',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'offline',$15,$16,$17,$18,$19,$20,$21)`,
			sequence, eventID, draft.Type, learning.EventSchemaVersion, operation.SubmissionID,
			int64(ordinal)+1, operation.DeviceID, operation.OperationID, ordinal, receivedAt,
			operation.OccurredAt, payloadID, payloadHashBytes, draft.ParentSessionID,
			draft.ArchiveDisposition, draft.EvidenceDisposition, draft.GoalRevisionID,
			draft.RouteRevisionID, draft.KnowledgeRevisionID, draft.ActivityID,
			draft.ActivityRevision); err != nil {
			return learning.OfflineIngestResult{}, fmt.Errorf("insert offline event: %w", err)
		}
	}

	terminalStatus := "succeeded"
	headState := learning.OfflineSubmissionClaimedSucceeded
	if archiveStatus == learning.OfflineArchivedRejected {
		terminalStatus = "rejected"
		headState = learning.OfflineSubmissionClaimedRejected
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_device_sequence_claims(
			device_id,device_seq,operation_id,submission_id,operation_hash,terminal_status,claimed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, operation.DeviceID, int64(operation.DeviceSequence),
		operation.OperationID, operation.SubmissionID, requestHash, terminalStatus, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("claim offline device sequence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE offline_attempt_heads
		SET state=$2,terminal_operation_id=$3,aggregate_version=$4,
		    first_event_seq=$5,last_event_seq=$6,updated_at=$7
		WHERE submission_id=$1 AND state='reserved'`, operation.SubmissionID, headState,
		operation.OperationID, aggregateVersion, firstSequence, lastSequence, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("finalize offline attempt head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE learning_aggregate_heads
		SET aggregate_version=$2,last_event_seq=$3,updated_at=$4
		WHERE aggregate_type='offline_attempt' AND aggregate_id=$1`, operation.SubmissionID,
		aggregateVersion, lastSequence, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("advance offline aggregate head: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=$2 WHERE singleton_id=1`, lastSequence, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("advance offline event clock: %w", err)
	}

	allEvents, err := loadEvents(ctx, tx, 0, lastSequence)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	var projectionGeneration string
	if err := tx.QueryRow(ctx, `SELECT active_generation_id::text FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&projectionGeneration); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("lock offline projection head: %w", err)
	}
	projection, err := learning.Replay(allEvents, s.registry, projectionGeneration)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if err := replaceProjection(ctx, tx, projectionGeneration, projection, lastSequence, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, err
	}

	receiptID := offlineID("ingest-receipt", operation.OperationID)
	ticketID := offlineID("status-ticket", operation.OperationID)
	statusRevisionID := offlineID("status-revision-1", operation.OperationID)
	aggregateVersionText, _ := learning.FormatUint63Decimal(uint64(aggregateVersion))
	firstSequenceText, _ := learning.FormatUint63Decimal(uint64(firstSequence))
	lastSequenceText, _ := learning.FormatUint63Decimal(uint64(lastSequence))
	deviceSequenceText, _ := learning.FormatUint63Decimal(operation.DeviceSequence)
	receipt := &learning.OfflineIngestReceipt{
		ReceiptID: receiptID, ArchivedAt: receivedAt, AggregateVersion: aggregateVersionText,
		FirstEventSequence: firstSequenceText, LastEventSequence: lastSequenceText,
		ProjectionAsOf: lastSequenceText, ArchiveStatus: archiveStatus,
	}
	var ticket *learning.OfflineStatusTicket
	if archiveStatus == learning.OfflineArchivedSucceeded {
		ticket = &learning.OfflineStatusTicket{TicketID: ticketID, OperationID: operation.OperationID, Revision: "1", UpdatedAt: receivedAt}
	}
	result := learning.OfflineIngestResult{
		ResultKind: learning.OfflineResultArchived, OperationID: operation.OperationID,
		DeviceSequence: deviceSequenceText, SubmissionID: operation.SubmissionID,
		ArchiveStatus: archiveStatus, AssessmentStatus: assessmentStatus,
		EvidenceStatus: evidenceStatus, ReasonCodes: append([]string{}, reasons...),
		Receipt: receipt, StatusTicket: ticket,
	}
	if plan.assessment != nil {
		result.AssessmentID = plan.assessment.ID
	}
	if plan.evidence != nil {
		result.EvidenceID = plan.evidence.ID
	}
	if err := result.Validate(); err != nil {
		return learning.OfflineIngestResult{}, err
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return learning.OfflineIngestResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_inbox(
			device_id,operation_id,request_hash,aggregate_type,aggregate_id,terminal_status,
			result,first_event_seq,last_event_seq,completed_at)
		VALUES($1,$2,$3,'offline_attempt',$4,$5,$6,$7,$8,$9)`, operation.DeviceID,
		operation.OperationID, requestHash, operation.SubmissionID, terminalStatus,
		resultJSON, firstSequence, lastSequence, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("insert offline inbox result: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_operation_statuses(
			ticket_id,device_id,operation_id,submission_id,ingest_receipt_id,created_at)
		VALUES($1,$2,$3,$4,$5,$6)`, ticketID, operation.DeviceID, operation.OperationID,
		operation.SubmissionID, receiptID, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("insert offline operation status: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_operation_status_revisions(
			id,ticket_id,revision,archive_status,assessment_status,evidence_status,reason_codes,
			aggregate_version,first_event_seq,last_event_seq,projection_as_of_event_seq,
			assessment_id,evidence_id,updated_at)
		VALUES($1,$2,1,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, statusRevisionID,
		ticketID, archiveStatus, assessmentStatus, evidenceStatus, reasons, aggregateVersion,
		firstSequence, lastSequence, lastSequence, nullableUUID(result.AssessmentID),
		nullableUUID(result.EvidenceID), receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("insert offline operation status revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_operation_status_heads(ticket_id,current_revision_id,current_revision,updated_at)
		VALUES($1,$2,1,$3)`, ticketID, statusRevisionID, receivedAt); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("insert offline operation status head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.OfflineIngestResult{}, fmt.Errorf("commit offline ingest: %w", err)
	}
	return result, nil
}

func insertOfflineAttemptRecords(ctx context.Context, tx pgx.Tx, operation learning.OfflineOperation, plan offlinePlan, firstSequence int64, drafts []learning.EventDraft) error {
	attempt := plan.attempt
	answerHash, err := decodeHash(attempt.AnswerSHA256)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_attempt_payloads(id,answer_text,payload_hash,created_at) VALUES($1,$2,$3,$4)`, attempt.AnswerPayloadID, attempt.Answer, answerHash, attempt.ReceivedAt); err != nil {
		return fmt.Errorf("insert offline attempt payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_attempts(
			id,session_id,activity_id,activity_revision,answer_payload_id,help_level,
			actor_device_id,occurred_at,received_at,payload_hash,evidence_eligibility,
			evidence_ineligible_reason,archive_disposition,offline_submission_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),'offline_succeeded',$13)`,
		attempt.ID, attempt.SessionID, attempt.ActivityID, attempt.ActivityRevision,
		attempt.AnswerPayloadID, attempt.Help, attempt.ActorDeviceID, attempt.OccurredAt,
		attempt.ReceivedAt, answerHash, attempt.EvidenceEligibility,
		attempt.EvidenceIneligibleReason, operation.SubmissionID); err != nil {
		return fmt.Errorf("insert offline attempt: %w", err)
	}
	if plan.evidenceClaim {
		claimSequence := int64(0)
		for ordinal, draft := range drafts {
			if draft.Type == learning.EventOfflineAttemptSubmitted {
				claimSequence = firstSequence + int64(ordinal)
				break
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO learning_activity_evidence_claims(
				activity_id,activity_revision,winning_attempt_id,claim_source,claimed_event_seq,claimed_at)
			VALUES($1,$2,$3,'offline',$4,$5)`, attempt.ActivityID, attempt.ActivityRevision,
			attempt.ID, nullableInt64(claimSequence), attempt.ReceivedAt); err != nil {
			return fmt.Errorf("insert offline activity evidence claim: %w", err)
		}
	}
	if plan.assessment == nil {
		return nil
	}
	assessment := plan.assessment
	modelParameters, _ := json.Marshal(assessment.ModelParameters)
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_assessments(
			id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,
			risk_flags,trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,
			model_attempts,attempt_categories,created_at,evidence_eligibility,evidence_ineligible_reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,TRUE,NULL)`,
		assessment.ID, assessment.SessionID, assessment.AttemptID, assessment.ActivityID,
		assessment.ActivityRevision, assessment.RubricComplete, assessment.Confidence,
		[]string{}, assessment.ModelID, modelParameters, assessment.PromptRevision,
		mustDecodeHash(assessment.ProposalInputHash), assessment.Attempts,
		assessment.AttemptCategories, assessment.CreatedAt); err != nil {
		return fmt.Errorf("insert offline objective assessment: %w", err)
	}
	conclusions, _ := json.Marshal(plan.decision.Items)
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_assessment_decisions(
			id,assessment_id,version,disposition,conclusions,actor_device_id,created_at)
		VALUES($1,$2,1,$3,$4,$5,$6)`, plan.decision.ID, plan.decision.AssessmentID,
		plan.decision.Disposition, conclusions, plan.decision.ActorDeviceID,
		plan.decision.CreatedAt); err != nil {
		return fmt.Errorf("insert offline objective decision: %w", err)
	}
	if plan.evidence == nil {
		return nil
	}
	target, ok := targetReference(plan.activity.Activity)
	if !ok {
		return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: learning.OfflineReasonActivityInvalid}
	}
	misconceptions, _ := json.Marshal(plan.evidence.Misconceptions)
	outcomes, _ := json.Marshal(plan.evidence.RubricOutcomes)
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_evidence(
			id,decision_id,assessment_id,session_id,attempt_id,activity_id,activity_revision,
			goal_revision_id,route_revision_id,knowledge_revision_id,node_revision_id,node_id,
			document_revision_id,rubric_revision,evidence_kind,activity_type,outcome,help_level,
			received_at,accepted_event_seq,acceptance_policy_version,reducer_policy_version,
			review_policy_version,misconception_candidates,rubric_outcomes)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
		plan.evidence.ID, plan.evidence.DispositionDecisionID, plan.evidence.AssessmentID,
		plan.activity.Activity.SessionID, plan.evidence.AttemptID, plan.evidence.ActivityID,
		plan.evidence.ActivityRevision, plan.evidence.GoalRevisionID,
		plan.evidence.RouteRevisionID, plan.evidence.KnowledgeRevisionID,
		plan.evidence.NodeRevisionID, target.NodeID, target.DocumentRevisionID,
		plan.evidence.RubricRevision, plan.evidence.Kind, plan.evidence.ActivityType,
		plan.evidence.Outcome, plan.evidence.Help, plan.evidence.ReceivedAt,
		plan.evidence.AcceptedEventSequence, plan.evidence.AcceptancePolicyVersion,
		plan.evidence.ReducerPolicyVersion, plan.evidence.ReviewPolicyVersion,
		misconceptions, outcomes); err != nil {
		return fmt.Errorf("insert offline objective evidence: %w", err)
	}
	return nil
}

func offlineDraft(kind learning.EventType, operation learning.OfflineOperation, activity learning.Activity, evidence learning.OfflineEvidenceStatus, payload any) learning.EventDraft {
	return learning.EventDraft{
		Type: kind, AggregateType: "offline_attempt", AggregateID: operation.SubmissionID,
		Payload: mustMarshalStore(payload), ParentSessionID: activity.SessionID, Source: "offline",
		ArchiveDisposition: "succeeded", EvidenceDisposition: string(evidence),
		GoalRevisionID: activity.GoalRevisionID, RouteRevisionID: activity.RouteRevisionID,
		KnowledgeRevisionID: activity.KnowledgeRevisionID, ActivityID: activity.ID,
		ActivityRevision: activity.Revision,
	}
}

func targetReference(activity learning.Activity) (learning.KnowledgeReference, bool) {
	for _, reference := range activity.References {
		if reference.KnowledgeRevisionID == activity.KnowledgeRevisionID &&
			reference.NodeID == activity.TargetNodeID &&
			reference.NodeRevisionID == activity.TargetNodeRevisionID &&
			reference.DocumentRevisionID != "" {
			return reference, true
		}
	}
	return learning.KnowledgeReference{}, false
}

func offlineID(kind, value string) string {
	return uuid.NewSHA1(offlineNamespace, []byte(kind+"\n"+value)).String()
}

func mustDecodeHash(value string) []byte {
	decoded, err := decodeHash(value)
	if err != nil {
		panic(err)
	}
	return decoded
}

func nullableUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func equalHash(stored []byte, encoded string) bool {
	return hex.EncodeToString(stored) == encoded
}

func blockedOfflineResult(operation learning.OfflineOperation, reason string) learning.OfflineIngestResult {
	deviceSequence, _ := learning.FormatUint63Decimal(operation.DeviceSequence)
	return learning.OfflineIngestResult{
		ResultKind: learning.OfflineResultBlocked, OperationID: operation.OperationID,
		DeviceSequence: deviceSequence, SubmissionID: operation.SubmissionID,
		ArchiveStatus: learning.OfflineNotArchivedBlocked, ReasonCodes: []string{reason},
	}
}

func conflictOfflineResult(operation learning.OfflineOperation, status learning.OfflineArchiveStatus, reason string) learning.OfflineIngestResult {
	deviceSequence, _ := learning.FormatUint63Decimal(operation.DeviceSequence)
	return learning.OfflineIngestResult{
		ResultKind: learning.OfflineResultConflict, OperationID: operation.OperationID,
		DeviceSequence: deviceSequence, SubmissionID: operation.SubmissionID,
		ArchiveStatus: status, ReasonCodes: []string{reason},
	}
}
