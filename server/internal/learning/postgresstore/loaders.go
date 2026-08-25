package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/jackc/pgx/v5"
)

type learningLoaderDB interface {
	privacy.DBTX
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func withLearningReadGate[T any](ctx context.Context, db learningLoaderDB, read func(learningLoaderDB) (T, error)) (T, error) {
	var zero T
	if _, err := privacy.LockOwnerRead(ctx, db, privacy.OwnerLearning); err != nil {
		return zero, err
	}
	return read(db)
}

func withLearningLoaderRead[T any](ctx context.Context, store *Store, read func(learningLoaderDB) (T, error)) (T, error) {
	var zero T
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return zero, fmt.Errorf("begin learning loader read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	value, err := withLearningReadGate(ctx, tx, read)
	if err != nil {
		return value, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, fmt.Errorf("commit learning loader read: %w", err)
	}
	return value, nil
}

func mapTutoringLoadError(err error) error {
	if errors.Is(err, tutoringpostgres.ErrNotFound) {
		return &learning.Error{Code: learning.CodeNotFound}
	}
	return err
}

func (s *Store) LoadAggregateVersion(ctx context.Context, aggregateType, aggregateID string) (int64, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (int64, error) {
		var version int64
		err := db.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type=$1 AND aggregate_id=$2`, aggregateType, aggregateID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return 0, fmt.Errorf("load aggregate version: %w", err)
		}
		return version, nil
	})
}

func (s *Store) LoadGoalRevision(ctx context.Context, id string) (learning.GoalRevision, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.GoalRevision, error) {
		var value learning.GoalRevision
		err := db.QueryRow(ctx, `SELECT id,goal_id,revision,goal_text,source,actor_device_id,created_at,previous_revision_id FROM learning_goal_revisions WHERE id=$1`, id).Scan(&value.ID, &value.GoalID, &value.Revision, &value.Text, &value.Source, &value.ActorDeviceID, &value.CreatedAt, &value.PreviousRevisionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return value, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return value, fmt.Errorf("load goal revision: %w", err)
		}
		return value, nil
	})
}

func (s *Store) LoadRouteRevision(ctx context.Context, id string) (learning.RouteRevision, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.RouteRevision, error) {
		var value learning.RouteRevision
		var proposal *string
		err := db.QueryRow(ctx, `SELECT id,route_id,revision,goal_revision_id,knowledge_revision_id,route_policy_version,source_proposal_id,created_at FROM learning_route_revisions WHERE id=$1`, id).Scan(&value.ID, &value.RouteID, &value.Revision, &value.GoalRevisionID, &value.KnowledgeRevisionID, &value.PolicyVersion, &proposal, &value.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return value, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return value, fmt.Errorf("load route revision: %w", err)
		}
		value.SourceProposalID = deref(proposal)
		rows, err := db.Query(ctx, `SELECT id,ordinal,node_id,node_revision_id,teaching_intent,completion_condition FROM learning_route_steps WHERE route_revision_id=$1 ORDER BY ordinal`, id)
		if err != nil {
			return value, err
		}
		defer rows.Close()
		for rows.Next() {
			var step learning.RouteStep
			if err := rows.Scan(&step.ID, &step.Ordinal, &step.NodeID, &step.NodeRevisionID, &step.TeachingIntent, &step.CompletionCondition); err != nil {
				return value, err
			}
			value.Steps = append(value.Steps, step)
		}
		return value, rows.Err()
	})
}

func (s *Store) LoadActivity(ctx context.Context, id string) (learning.Activity, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.Activity, error) {
		var value learning.Activity
		var rubricRaw []byte
		var activityType string
		var allowed []string
		var proposal, freeQuestion, freeAnswer *string
		err := db.QueryRow(ctx, `SELECT id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,target_node_id,target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,assessment_policy_version,review_policy_version,source_proposal_id,attached_free_question_id,attached_free_answer_id,is_review,created_at FROM learning_activities WHERE id=$1`, id).Scan(&value.ID, &value.Revision, &value.SessionID, &value.GoalRevisionID, &value.RouteRevisionID, &value.RouteStepID, &value.KnowledgeRevisionID, &value.TargetNodeID, &value.TargetNodeRevisionID, &value.Prompt, &activityType, &value.Rubric.Revision, &rubricRaw, &value.Difficulty, &allowed, &value.ActivityPolicyVersion, &value.AssessmentPolicyVersion, &value.ReviewPolicyVersion, &proposal, &freeQuestion, &freeAnswer, &value.Review, &value.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return learning.Activity{}, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return learning.Activity{}, fmt.Errorf("load activity: %w", err)
		}
		value.Type = learning.ActivityType(activityType)
		if err := json.Unmarshal(rubricRaw, &value.Rubric); err != nil {
			return learning.Activity{}, err
		}
		value.SourceProposalID = deref(proposal)
		value.AttachedFreeQuestionID = deref(freeQuestion)
		value.AttachedFreeAnswerID = deref(freeAnswer)
		for _, item := range allowed {
			value.AllowedHelp = append(value.AllowedHelp, learning.HelpLevel(item))
		}
		rows, err := db.Query(ctx, `SELECT knowledge_revision_id,node_id,node_revision_id,document_revision_id,source_start,source_end,slice_text,slice_hash FROM learning_activity_references WHERE activity_id=$1 ORDER BY ordinal`, id)
		if err != nil {
			return learning.Activity{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var ref learning.KnowledgeReference
			var hash []byte
			if err := rows.Scan(&ref.KnowledgeRevisionID, &ref.NodeID, &ref.NodeRevisionID, &ref.DocumentRevisionID, &ref.Range.Start, &ref.Range.End, &ref.Slice, &hash); err != nil {
				return learning.Activity{}, err
			}
			ref.SliceSHA256 = hex.EncodeToString(hash)
			value.References = append(value.References, ref)
		}
		return value, rows.Err()
	})
}
func (s *Store) LoadAttempt(ctx context.Context, id string) (learning.Attempt, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.Attempt, error) {
		var value learning.Attempt
		var hash []byte
		err := db.QueryRow(ctx, `SELECT a.id,a.session_id,a.activity_id,a.activity_revision,a.answer_payload_id,p.answer_text,a.help_level,a.actor_device_id,a.occurred_at,a.received_at,a.payload_hash FROM learning_attempts a JOIN learning_attempt_payloads p ON p.id=a.answer_payload_id WHERE a.id=$1`, id).Scan(&value.ID, &value.SessionID, &value.ActivityID, &value.ActivityRevision, &value.AnswerPayloadID, &value.Answer, &value.Help, &value.ActorDeviceID, &value.OccurredAt, &value.ReceivedAt, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return learning.Attempt{}, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return learning.Attempt{}, fmt.Errorf("load attempt: %w", err)
		}
		value.AnswerSHA256 = hex.EncodeToString(hash)
		return value, nil
	})
}

type assessmentItemRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanAssessmentItems(rows assessmentItemRows) ([]learning.AssessmentItem, error) {
	var items []learning.AssessmentItem
	for rows.Next() {
		var item learning.AssessmentItem
		var answerHash []byte
		var node, quote, misconception *string
		var start, end *int
		var knowledgeHash []byte
		if err := rows.Scan(&item.RubricItemID, &item.Conclusion, &item.AnswerRange.Start, &item.AnswerRange.End, &item.AnswerQuote, &answerHash, &node, &start, &end, &quote, &knowledgeHash, &misconception); err != nil {
			return nil, err
		}
		item.AnswerQuoteSHA256 = hex.EncodeToString(answerHash)
		item.KnowledgeReferenceID = deref(node)
		if start != nil {
			item.KnowledgeRange = learning.SourceRange{Start: *start, End: *end}
		}
		item.KnowledgeQuote = deref(quote)
		if knowledgeHash != nil {
			item.KnowledgeQuoteSHA256 = hex.EncodeToString(knowledgeHash)
		}
		item.MisconceptionCandidate = deref(misconception)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

type assessmentLoadResult struct {
	artifact learning.AssessmentArtifact
	decision learning.AssessmentDecision
}

func (s *Store) LoadAssessment(ctx context.Context, id string) (learning.AssessmentArtifact, learning.AssessmentDecision, error) {
	loaded, err := withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (assessmentLoadResult, error) {
		return loadAssessmentWith(ctx, db, id, false)
	})
	return loaded.artifact, loaded.decision, err
}

func (s *Store) LoadAssessmentForAttempt(ctx context.Context, attemptID string) (learning.AssessmentArtifact, learning.AssessmentDecision, error) {
	loaded, err := withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (assessmentLoadResult, error) {
		return loadAssessmentWith(ctx, db, attemptID, true)
	})
	return loaded.artifact, loaded.decision, err
}

func loadAssessmentWith(ctx context.Context, db learningLoaderDB, key string, byAttempt bool) (assessmentLoadResult, error) {
	var result assessmentLoadResult
	var params []byte
	var inputHash []byte
	var risks []string
	query := `SELECT id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,risk_flags,trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,model_attempts,attempt_categories,created_at FROM learning_assessments WHERE id=$1`
	if byAttempt {
		query = `SELECT id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,risk_flags,trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,model_attempts,attempt_categories,created_at FROM learning_assessments WHERE attempt_id=$1`
	}
	err := db.QueryRow(ctx, query, key).Scan(&result.artifact.ID, &result.artifact.SessionID, &result.artifact.AttemptID, &result.artifact.ActivityID, &result.artifact.ActivityRevision, &result.artifact.RubricComplete, &result.artifact.Confidence, &risks, &result.artifact.ModelID, &params, &result.artifact.PromptRevision, &inputHash, &result.artifact.Attempts, &result.artifact.AttemptCategories, &result.artifact.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return result, fmt.Errorf("load assessment: %w", err)
	}
	if err := json.Unmarshal(params, &result.artifact.ModelParameters); err != nil {
		return result, fmt.Errorf("decode assessment model parameters: %w", err)
	}
	result.artifact.ProposalInputHash = hex.EncodeToString(inputHash)
	for _, risk := range risks {
		result.artifact.RiskFlags = append(result.artifact.RiskFlags, learning.RiskFlag(risk))
	}
	rows, err := db.Query(ctx, `SELECT rubric_item_id,conclusion,answer_start,answer_end,answer_quote,answer_quote_hash,knowledge_node_revision_id,knowledge_start,knowledge_end,knowledge_quote,knowledge_quote_hash,misconception_candidate FROM learning_assessment_items WHERE assessment_id=$1 ORDER BY ordinal`, result.artifact.ID)
	if err != nil {
		return result, err
	}
	items, err := scanAssessmentItems(rows)
	rows.Close()
	if err != nil {
		return result, err
	}
	result.artifact.Items = items

	decisionRows, err := db.Query(ctx, `SELECT d.id,d.assessment_id,d.version,d.disposition,d.conclusions,d.reason,d.actor_device_id,d.created_at,d.replaces_decision_id,e.id FROM learning_assessment_decisions d LEFT JOIN learning_evidence e ON e.decision_id=d.id WHERE d.assessment_id=$1 ORDER BY d.version,d.id`, result.artifact.ID)
	if err != nil {
		return result, err
	}
	defer decisionRows.Close()
	expectedVersion := int64(1)
	previousID := ""
	for decisionRows.Next() {
		var decision learning.AssessmentDecision
		var conclusions []byte
		var reason *string
		if err := decisionRows.Scan(&decision.ID, &decision.AssessmentID, &decision.Version, &decision.Disposition, &conclusions, &reason, &decision.ActorDeviceID, &decision.CreatedAt, &decision.ReplacesDecisionID, &decision.ProducedEvidenceID); err != nil {
			return result, err
		}
		decision.Reason = deref(reason)
		if err := json.Unmarshal(conclusions, &decision.Items); err != nil {
			return result, fmt.Errorf("decode assessment decision items: %w", err)
		}
		if decision.AssessmentID != result.artifact.ID || decision.Version != expectedVersion || (expectedVersion == 1 && decision.ReplacesDecisionID != nil) || (expectedVersion > 1 && (decision.ReplacesDecisionID == nil || *decision.ReplacesDecisionID != previousID)) {
			return result, &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "assessment_decision_lineage"}
		}
		result.decision = decision
		previousID = decision.ID
		expectedVersion++
	}
	if err := decisionRows.Err(); err != nil {
		return result, err
	}
	if expectedVersion == 1 {
		return result, &learning.Error{Code: learning.CodeNotFound, Reason: "assessment_decision_missing"}
	}
	return result, nil
}
func (s *Store) LoadProposal(ctx context.Context, id string) (learning.ProposalArtifact, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.ProposalArtifact, error) {
		var raw []byte
		err := db.QueryRow(ctx, `SELECT artifact FROM tutoring_proposal_artifacts WHERE id=$1`, id).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return learning.ProposalArtifact{}, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return learning.ProposalArtifact{}, err
		}
		var value learning.ProposalArtifact
		if err := json.Unmarshal(raw, &value); err != nil {
			return value, err
		}
		return value, nil
	})
}

type tutoringLoaderOwner interface {
	LoadFreeQuestionWith(context.Context, tutoringpostgres.DBTX, string) (tutoring.FreeQuestion, error)
	LoadFreeAnswerWith(context.Context, tutoringpostgres.DBTX, string) (tutoring.FreeAnswer, error)
	LatestFreeQuestionWith(context.Context, tutoringpostgres.DBTX, string) (string, error)
	LatestFreeQuestionForFrame(context.Context, string, string) (string, error)
}

func (s *Store) LoadFreeQuestion(ctx context.Context, id string) (tutoring.FreeQuestion, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (tutoring.FreeQuestion, error) {
		owner, ok := s.tutoring.(tutoringLoaderOwner)
		if !ok {
			return tutoring.FreeQuestion{}, fmt.Errorf("load free question: tutoring transactional loader unavailable")
		}
		value, err := owner.LoadFreeQuestionWith(ctx, db, id)
		return value, mapTutoringLoadError(err)
	})
}

func (s *Store) LoadFreeAnswer(ctx context.Context, id string) (tutoring.FreeAnswer, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (tutoring.FreeAnswer, error) {
		owner, ok := s.tutoring.(tutoringLoaderOwner)
		if !ok {
			return tutoring.FreeAnswer{}, fmt.Errorf("load free answer: tutoring transactional loader unavailable")
		}
		value, err := owner.LoadFreeAnswerWith(ctx, db, id)
		return value, mapTutoringLoadError(err)
	})
}

func (s *Store) LoadValidEvidence(ctx context.Context, nodeRevisionID string) ([]learning.AcceptedEvidence, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) ([]learning.AcceptedEvidence, error) {
		rows, err := db.Query(ctx, `SELECT e.id,e.decision_id,e.assessment_id,e.attempt_id,e.activity_id,e.activity_revision,e.goal_revision_id,e.route_revision_id,e.knowledge_revision_id,e.node_revision_id,e.rubric_revision,e.evidence_kind,e.activity_type,e.outcome,e.help_level,e.received_at,e.acceptance_policy_version,e.reducer_policy_version,e.review_policy_version,e.misconception_candidates,e.rubric_outcomes FROM learning_evidence e LEFT JOIN learning_evidence_invalidations i ON i.evidence_id=e.id WHERE e.node_revision_id=$1 AND i.id IS NULL ORDER BY e.received_at,e.id`, nodeRevisionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var result []learning.AcceptedEvidence
		for rows.Next() {
			var value learning.AcceptedEvidence
			var misconceptions, outcomes []byte
			if err := rows.Scan(&value.ID, &value.DispositionDecisionID, &value.AssessmentID, &value.AttemptID, &value.ActivityID, &value.ActivityRevision, &value.GoalRevisionID, &value.RouteRevisionID, &value.KnowledgeRevisionID, &value.NodeRevisionID, &value.RubricRevision, &value.Kind, &value.ActivityType, &value.Outcome, &value.Help, &value.ReceivedAt, &value.AcceptancePolicyVersion, &value.ReducerPolicyVersion, &value.ReviewPolicyVersion, &misconceptions, &outcomes); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(misconceptions, &value.Misconceptions); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(outcomes, &value.RubricOutcomes); err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, rows.Err()
	})
}

func (s *Store) LoadMisconceptions(ctx context.Context, nodeRevisionID string) ([]learning.MisconceptionHypothesis, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) ([]learning.MisconceptionHypothesis, error) {
		rows, err := db.Query(ctx, `SELECT DISTINCT ON (misconception_id) misconception_id,revision,node_revision_id,rubric_item_id,candidate_hash,candidate_text,status,source_evidence_ids,counter_evidence_ids,caused_by_evidence_id FROM learning_misconception_revisions WHERE node_revision_id=$1 ORDER BY misconception_id,revision DESC`, nodeRevisionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var result []learning.MisconceptionHypothesis
		for rows.Next() {
			var value learning.MisconceptionHypothesis
			var hash []byte
			if err := rows.Scan(&value.ID, &value.Revision, &value.NodeRevisionID, &value.RubricItemID, &hash, &value.Candidate, &value.Status, &value.SourceEvidenceIDs, &value.CounterEvidenceIDs, &value.CausedByEvidenceID); err != nil {
				return nil, err
			}
			value.CandidateHash = hex.EncodeToString(hash)
			result = append(result, value)
		}
		return result, rows.Err()
	})
}

func (s *Store) LatestFreeQuestion(ctx context.Context, sessionID string) (string, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (string, error) {
		owner, ok := s.tutoring.(tutoringLoaderOwner)
		if !ok {
			return "", fmt.Errorf("load latest free question: tutoring transactional loader unavailable")
		}
		id, err := owner.LatestFreeQuestionWith(ctx, db, sessionID)
		return id, mapTutoringLoadError(err)
	})
}

func (s *Store) LatestFreeQuestionForFrame(ctx context.Context, sessionID, frameID string) (string, error) {
	owner, ok := s.tutoring.(tutoringLoaderOwner)
	if !ok {
		return "", fmt.Errorf("load latest frame free question: tutoring transactional loader unavailable")
	}
	id, err := owner.LatestFreeQuestionForFrame(ctx, sessionID, frameID)
	return id, mapTutoringLoadError(err)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
