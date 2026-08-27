package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ListOfflineAssessments(ctx context.Context, deviceID string, query learning.OfflineAssessmentQuery) (learning.OfflineAssessmentPage, error) {
	if uuid.Validate(deviceID) != nil || query.Status != learning.OfflineAssessmentFilterProvisional || query.Page.Limit < 1 || query.Page.Limit > 200 {
		return learning.OfflineAssessmentPage{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_assessment_query"}
	}
	return withOfflineAssessmentRead(ctx, s, func(tx pgx.Tx, generation int64, metadata learning.ProjectionMetadata) (learning.OfflineAssessmentPage, error) {
		keys, err := decodeCursor(query.Page.Cursor, "offline-assessments-provisional", metadata.GenerationID, metadata.AsOfEventSequence, 2)
		if err != nil {
			return learning.OfflineAssessmentPage{}, err
		}
		afterTime := time.Time{}
		afterID := uuid.Nil.String()
		if len(keys) > 0 {
			afterTime, err = time.Parse(time.RFC3339Nano, keys[0])
			if err != nil || uuid.Validate(keys[1]) != nil {
				return learning.OfflineAssessmentPage{}, &learning.Error{Code: learning.CodeStaleCursor}
			}
			afterID = keys[1]
		}
		rows, err := tx.Query(ctx, `
			SELECT assessment.id::text,assessment.created_at
			FROM learning_assessments assessment
			JOIN learning_attempts attempt ON attempt.id=assessment.attempt_id
			JOIN offline_attempt_heads offline_head ON offline_head.submission_id=attempt.offline_submission_id
			JOIN offline_submission_authorizations auth ON auth.submission_id=offline_head.submission_id
			JOIN offline_evaluation_jobs job ON job.submission_id=offline_head.submission_id
			  AND job.result_assessment_id=assessment.id AND job.status='completed'
			JOIN LATERAL (
			  SELECT disposition FROM learning_assessment_decisions
			  WHERE assessment_id=assessment.id ORDER BY version DESC,id DESC LIMIT 1
			) current_decision ON TRUE
			WHERE auth.device_id=$1 AND attempt.actor_device_id=$1
			  AND auth.learner_generation=$2
			  AND assessment.evidence_eligibility
			  AND current_decision.disposition='provisional'
			  AND (assessment.created_at,assessment.id)>($3,$4::uuid)
			ORDER BY assessment.created_at,assessment.id
			LIMIT $5`, deviceID, generation, afterTime, afterID, query.Page.Limit+1)
		if err != nil {
			return learning.OfflineAssessmentPage{}, fmt.Errorf("list offline assessments: %w", err)
		}
		defer rows.Close()
		type position struct {
			id string
			at time.Time
		}
		positions := make([]position, 0, query.Page.Limit+1)
		for rows.Next() {
			var item position
			if err := rows.Scan(&item.id, &item.at); err != nil {
				return learning.OfflineAssessmentPage{}, err
			}
			positions = append(positions, item)
		}
		if err := rows.Err(); err != nil {
			return learning.OfflineAssessmentPage{}, err
		}
		result := learning.OfflineAssessmentPage{Metadata: metadata, Items: []learning.OfflineAssessmentSummary{}}
		visible := positions
		if len(visible) > query.Page.Limit {
			visible = visible[:query.Page.Limit]
		}
		for _, item := range visible {
			view, err := loadOfflineAssessmentViewWith(ctx, tx, deviceID, generation, metadata, item.id)
			if err != nil {
				return learning.OfflineAssessmentPage{}, err
			}
			result.Items = append(result.Items, offlineAssessmentSummary(view))
		}
		if len(positions) > query.Page.Limit {
			last := positions[query.Page.Limit-1]
			result.NextCursor = encodeCursor("offline-assessments-provisional", metadata.GenerationID, metadata.AsOfEventSequence, last.at.UTC().Format(time.RFC3339Nano), last.id)
		}
		return result, nil
	})
}

func (s *Store) OfflineAssessment(ctx context.Context, deviceID, assessmentID string) (learning.OfflineAssessmentView, error) {
	if uuid.Validate(deviceID) != nil || uuid.Validate(assessmentID) != nil {
		return learning.OfflineAssessmentView{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_assessment_id"}
	}
	return withOfflineAssessmentRead(ctx, s, func(tx pgx.Tx, generation int64, metadata learning.ProjectionMetadata) (learning.OfflineAssessmentView, error) {
		return loadOfflineAssessmentViewWith(ctx, tx, deviceID, generation, metadata, assessmentID)
	})
}

func withOfflineAssessmentRead[T any](ctx context.Context, s *Store, read func(pgx.Tx, int64, learning.ProjectionMetadata) (T, error)) (T, error) {
	var zero T
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return zero, fmt.Errorf("begin offline assessment read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	learningGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return zero, err
	}
	tutoringGeneration, err := s.tutoring.LockReadWith(ctx, tx)
	if err != nil {
		return zero, err
	}
	if s.knowledge == nil {
		return zero, errors.New("offline assessment knowledge owner is required")
	}
	knowledgeGeneration, err := s.knowledge.LockReadWith(ctx, tx)
	if err != nil {
		return zero, err
	}
	if learningGeneration != tutoringGeneration || learningGeneration != knowledgeGeneration {
		return zero, &learning.Error{Code: learning.CodeContentRedacted, Reason: "owner_generation_mismatch"}
	}
	metadata, _, _, _, err := metadataFrom(ctx, tx)
	if err != nil {
		return zero, err
	}
	result, err := read(tx, learningGeneration, metadata)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, fmt.Errorf("commit offline assessment read: %w", err)
	}
	return result, nil
}

func loadOfflineAssessmentViewWith(ctx context.Context, tx pgx.Tx, deviceID string, generation int64, metadata learning.ProjectionMetadata, assessmentID string) (learning.OfflineAssessmentView, error) {
	var submissionID string
	var activityID string
	var aggregateVersion int64
	var redacted bool
	err := tx.QueryRow(ctx, `
		SELECT offline_head.submission_id::text,assessment.activity_id::text,aggregate_head.aggregate_version,
		       activity.prompt='[redacted]'
		       OR assessment.trusted_model_id='[redacted]'
		       OR payload.answer_text=''
		       OR current_decision.conclusions='{"redacted":true}'::jsonb
		FROM learning_assessments assessment
		JOIN learning_attempts attempt ON attempt.id=assessment.attempt_id
		JOIN learning_attempt_payloads payload ON payload.id=attempt.answer_payload_id
		JOIN learning_activities activity ON activity.id=assessment.activity_id
		  AND activity.revision=assessment.activity_revision
		JOIN offline_attempt_heads offline_head ON offline_head.submission_id=attempt.offline_submission_id
		JOIN offline_submission_authorizations auth ON auth.submission_id=offline_head.submission_id
		JOIN offline_evaluation_jobs job ON job.submission_id=offline_head.submission_id
		  AND job.result_assessment_id=assessment.id AND job.status='completed'
		JOIN learning_aggregate_heads aggregate_head ON aggregate_head.aggregate_type='offline_attempt'
		  AND aggregate_head.aggregate_id=offline_head.submission_id
		JOIN LATERAL (
		  SELECT conclusions FROM learning_assessment_decisions
		  WHERE assessment_id=assessment.id ORDER BY version DESC,id DESC LIMIT 1
		) current_decision ON TRUE
		WHERE assessment.id=$1 AND auth.device_id=$2 AND attempt.actor_device_id=$2
		  AND auth.learner_generation=$3 AND offline_head.state='claimed_succeeded'`, assessmentID, deviceID, generation).Scan(&submissionID, &activityID, &aggregateVersion, &redacted)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.OfflineAssessmentView{}, &learning.Error{Code: learning.CodeNotFound, Reason: "offline_assessment_not_found"}
	}
	if err != nil {
		return learning.OfflineAssessmentView{}, fmt.Errorf("authorize offline assessment read: %w", err)
	}
	if redacted {
		return learning.OfflineAssessmentView{}, &learning.Error{Code: learning.CodeContentRedacted, Reason: "offline_assessment_body_redacted"}
	}
	activity, err := loadActivityForView(ctx, tx, activityID)
	if err != nil {
		return learning.OfflineAssessmentView{}, err
	}
	assessment, err := loadAssessmentWith(ctx, tx, assessmentID, false)
	if err != nil {
		return learning.OfflineAssessmentView{}, err
	}
	attempt, err := loadAttemptForView(ctx, tx, assessment.artifact.AttemptID)
	if err != nil {
		return learning.OfflineAssessmentView{}, err
	}
	if activity.ID != assessment.artifact.ActivityID || activity.Revision != assessment.artifact.ActivityRevision ||
		attempt.ID != assessment.artifact.AttemptID || attempt.ActivityID != activity.ID ||
		attempt.ActivityRevision != activity.Revision || attempt.OfflineSubmissionID != submissionID ||
		activity.Type != learning.ActivityOpen || assessment.decision.AssessmentID != assessment.artifact.ID {
		return learning.OfflineAssessmentView{}, &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "offline_assessment_ownership_mismatch"}
	}
	version, err := learning.FormatUint63Decimal(uint64(aggregateVersion))
	if err != nil {
		return learning.OfflineAssessmentView{}, err
	}
	confirmable := assessment.decision.Disposition == learning.DispositionProvisional && assessment.artifact.EvidenceEligibility &&
		attempt.EvidenceEligibility && learning.ConfirmableAssessment(activity, attempt, assessment.artifact)
	allowed := []string{}
	if assessment.decision.Disposition == learning.DispositionProvisional && assessment.artifact.EvidenceEligibility && attempt.EvidenceEligibility {
		if confirmable {
			allowed = append(allowed, "confirm")
		}
		allowed = append(allowed, "override", "void")
	}
	return learning.OfflineAssessmentView{
		Metadata: metadata, SubmissionID: submissionID, AggregateVersion: version,
		Activity: activity, Attempt: attempt, Assessment: assessment.artifact, Decision: assessment.decision,
		Confirmable: confirmable, AllowedDecisions: allowed,
	}, nil
}

func offlineAssessmentSummary(view learning.OfflineAssessmentView) learning.OfflineAssessmentSummary {
	activityRevision, _ := learning.FormatUint63Decimal(uint64(view.Activity.Revision))
	dispositionVersion, _ := learning.FormatUint63Decimal(uint64(view.Decision.Version))
	return learning.OfflineAssessmentSummary{
		AssessmentID: view.Assessment.ID, AttemptID: view.Attempt.ID, ActivityID: view.Activity.ID,
		ActivityRevision: activityRevision, SubmissionID: view.SubmissionID,
		AggregateVersion: view.AggregateVersion, DispositionVersion: dispositionVersion,
		Disposition: view.Decision.Disposition, Confidence: view.Assessment.Confidence,
		Confirmable: view.Confirmable, AllowedDecisions: append([]string(nil), view.AllowedDecisions...),
		AttemptReceivedAt: view.Attempt.ReceivedAt, AssessmentCreatedAt: view.Assessment.CreatedAt,
	}
}
