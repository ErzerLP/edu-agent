package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
)

type authorityDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadSessionFrom(ctx context.Context, db authorityDB, id string) (tutoring.Session, error) {
	var value tutoring.Session
	var state string
	var goal, route, step, knowledgeRevision, node *string
	var activity, attempt *string
	err := db.QueryRow(ctx, `SELECT id,aggregate_version,state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,attached_quiz FROM tutoring_sessions WHERE id=$1`, id).Scan(&value.ID, &value.AggregateVer, &state, &goal, &route, &step, &knowledgeRevision, &node, &activity, &attempt, &value.AttachedQuiz)
	if errors.Is(err, pgx.ErrNoRows) {
		return tutoring.Session{}, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return tutoring.Session{}, fmt.Errorf("load tutoring session: %w", err)
	}
	value.State = tutoring.State(state)
	value.Context = tutoring.FocusContext{GoalRevisionID: deref(goal), RouteRevisionID: deref(route), RouteStepID: deref(step), KnowledgeRevisionID: deref(knowledgeRevision), FocusNodeRevisionID: deref(node), ActivityID: activity, AttemptID: attempt}
	var frame tutoring.FocusFrame
	var saved string
	var fg, fr, fs, fk, fn *string
	var fa, ft *string
	err = db.QueryRow(ctx, `SELECT id,session_id,saved_state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,saved_aggregate_version,created_event_seq FROM tutoring_focus_frames WHERE session_id=$1 AND invalidated_at IS NULL AND resumed_at IS NULL`, id).Scan(&frame.ID, &frame.SessionID, &saved, &fg, &fr, &fs, &fk, &fn, &fa, &ft, &frame.SavedAggregateVersion, &frame.CreatedEventSequence)
	if err == nil {
		frame.SavedState = tutoring.State(saved)
		frame.Context = tutoring.FocusContext{GoalRevisionID: deref(fg), RouteRevisionID: deref(fr), RouteStepID: deref(fs), KnowledgeRevisionID: deref(fk), FocusNodeRevisionID: deref(fn), ActivityID: fa, AttemptID: ft}
		value.ActiveFrame = &frame
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return tutoring.Session{}, fmt.Errorf("load active focus frame: %w", err)
	}
	return value, nil
}

func (s *Store) loadSession(ctx context.Context, id string) (tutoring.Session, error) {
	return loadSessionFrom(ctx, s.pool, id)
}

func (s *Store) LoadAggregateVersion(ctx context.Context, aggregateType, aggregateID string) (int64, error) {
	var version int64
	err := s.pool.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type=$1 AND aggregate_id=$2`, aggregateType, aggregateID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return 0, fmt.Errorf("load aggregate version: %w", err)
	}
	return version, nil
}

func (s *Store) LoadGoalRevision(ctx context.Context, id string) (learning.GoalRevision, error) {
	var value learning.GoalRevision
	err := s.pool.QueryRow(ctx, `SELECT id,goal_id,revision,goal_text,source,actor_device_id,created_at,previous_revision_id FROM learning_goal_revisions WHERE id=$1`, id).Scan(&value.ID, &value.GoalID, &value.Revision, &value.Text, &value.Source, &value.ActorDeviceID, &value.CreatedAt, &value.PreviousRevisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("load goal revision: %w", err)
	}
	return value, nil
}

func (s *Store) LoadRouteRevision(ctx context.Context, id string) (learning.RouteRevision, error) {
	var value learning.RouteRevision
	var proposal *string
	err := s.pool.QueryRow(ctx, `SELECT id,route_id,revision,goal_revision_id,knowledge_revision_id,route_policy_version,source_proposal_id,created_at FROM learning_route_revisions WHERE id=$1`, id).Scan(&value.ID, &value.RouteID, &value.Revision, &value.GoalRevisionID, &value.KnowledgeRevisionID, &value.PolicyVersion, &proposal, &value.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("load route revision: %w", err)
	}
	value.SourceProposalID = deref(proposal)
	rows, err := s.pool.Query(ctx, `SELECT id,ordinal,node_id,node_revision_id,teaching_intent,completion_condition FROM learning_route_steps WHERE route_revision_id=$1 ORDER BY ordinal`, id)
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
}

func (s *Store) LoadActivity(ctx context.Context, id string) (learning.Activity, error) {
	var value learning.Activity
	var rubricRaw []byte
	var activityType string
	var allowed []string
	var proposal, freeQuestion, freeAnswer *string
	err := s.pool.QueryRow(ctx, `SELECT id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,target_node_id,target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,assessment_policy_version,review_policy_version,source_proposal_id,attached_free_question_id,attached_free_answer_id,is_review,created_at FROM learning_activities WHERE id=$1`, id).Scan(&value.ID, &value.Revision, &value.SessionID, &value.GoalRevisionID, &value.RouteRevisionID, &value.RouteStepID, &value.KnowledgeRevisionID, &value.TargetNodeID, &value.TargetNodeRevisionID, &value.Prompt, &activityType, &value.Rubric.Revision, &rubricRaw, &value.Difficulty, &allowed, &value.ActivityPolicyVersion, &value.AssessmentPolicyVersion, &value.ReviewPolicyVersion, &proposal, &freeQuestion, &freeAnswer, &value.Review, &value.CreatedAt)
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
	rows, err := s.pool.Query(ctx, `SELECT knowledge_revision_id,node_id,node_revision_id,document_revision_id,source_start,source_end,slice_text,slice_hash FROM learning_activity_references WHERE activity_id=$1 ORDER BY ordinal`, id)
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
}
func (s *Store) LoadAttempt(ctx context.Context, id string) (learning.Attempt, error) {
	var value learning.Attempt
	var hash []byte
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.session_id,a.activity_id,a.activity_revision,a.answer_payload_id,p.answer_text,a.help_level,a.actor_device_id,a.occurred_at,a.received_at,a.payload_hash FROM learning_attempts a JOIN learning_attempt_payloads p ON p.id=a.answer_payload_id WHERE a.id=$1`, id).Scan(&value.ID, &value.SessionID, &value.ActivityID, &value.ActivityRevision, &value.AnswerPayloadID, &value.Answer, &value.Help, &value.ActorDeviceID, &value.OccurredAt, &value.ReceivedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.Attempt{}, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return learning.Attempt{}, fmt.Errorf("load attempt: %w", err)
	}
	value.AnswerSHA256 = hex.EncodeToString(hash)
	return value, nil
}
func (s *Store) LoadAssessment(ctx context.Context, id string) (learning.AssessmentArtifact, learning.AssessmentDecision, error) {
	var value learning.AssessmentArtifact
	var params []byte
	var inputHash []byte
	var risks []string
	err := s.pool.QueryRow(ctx, `SELECT id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,risk_flags,trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,model_attempts,attempt_categories,created_at FROM learning_assessments WHERE id=$1`, id).Scan(&value.ID, &value.SessionID, &value.AttemptID, &value.ActivityID, &value.ActivityRevision, &value.RubricComplete, &value.Confidence, &risks, &value.ModelID, &params, &value.PromptRevision, &inputHash, &value.Attempts, &value.AttemptCategories, &value.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, learning.AssessmentDecision{}, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return value, learning.AssessmentDecision{}, fmt.Errorf("load assessment: %w", err)
	}
	_ = json.Unmarshal(params, &value.ModelParameters)
	value.ProposalInputHash = hex.EncodeToString(inputHash)
	for _, risk := range risks {
		value.RiskFlags = append(value.RiskFlags, learning.RiskFlag(risk))
	}
	rows, err := s.pool.Query(ctx, `SELECT rubric_item_id,conclusion,answer_start,answer_end,answer_quote,answer_quote_hash,knowledge_node_revision_id,knowledge_start,knowledge_end,knowledge_quote,knowledge_quote_hash,misconception_candidate FROM learning_assessment_items WHERE assessment_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return value, learning.AssessmentDecision{}, err
	}
	for rows.Next() {
		var item learning.AssessmentItem
		var answerHash []byte
		var node, quote, misconception *string
		var start, end *int
		var knowledgeHash []byte
		if err := rows.Scan(&item.RubricItemID, &item.Conclusion, &item.AnswerRange.Start, &item.AnswerRange.End, &item.AnswerQuote, &answerHash, &node, &start, &end, &quote, &knowledgeHash, &misconception); err != nil {
			rows.Close()
			return value, learning.AssessmentDecision{}, err
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
		value.Items = append(value.Items, item)
	}
	rows.Close()
	var decision learning.AssessmentDecision
	var conclusions []byte
	var reason *string
	err = s.pool.QueryRow(ctx, `SELECT d.id,d.assessment_id,d.version,d.disposition,d.conclusions,d.reason,d.actor_device_id,d.created_at,d.replaces_decision_id,e.id FROM learning_assessment_decisions d LEFT JOIN learning_evidence e ON e.decision_id=d.id WHERE d.assessment_id=$1 ORDER BY d.version DESC LIMIT 1`, id).Scan(&decision.ID, &decision.AssessmentID, &decision.Version, &decision.Disposition, &conclusions, &reason, &decision.ActorDeviceID, &decision.CreatedAt, &decision.ReplacesDecisionID, &decision.ProducedEvidenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, decision, &learning.Error{Code: learning.CodeNotFound, Reason: "assessment_decision_missing"}
	}
	if err != nil {
		return value, decision, err
	}
	decision.Reason = deref(reason)
	if err := json.Unmarshal(conclusions, &decision.Items); err != nil {
		return value, decision, err
	}
	return value, decision, nil
}
func (s *Store) LoadProposal(ctx context.Context, id string) (learning.ProposalArtifact, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT artifact FROM tutoring_proposal_artifacts WHERE id=$1`, id).Scan(&raw)
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
}

func (s *Store) LoadFreeQuestion(ctx context.Context, id string) (tutoring.FreeQuestion, error) {
	var value tutoring.FreeQuestion
	var refs []byte
	err := s.pool.QueryRow(ctx, `SELECT id,session_id,focus_frame_id,question_text,knowledge_revision_id,references_snapshot,actor_device_id,occurred_at,received_at FROM tutoring_free_questions WHERE id=$1`, id).Scan(&value.ID, &value.SessionID, &value.FocusFrameID, &value.Text, &value.KnowledgeRevisionID, &refs, &value.ActorDeviceID, &value.OccurredAt, &value.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("load free question: %w", err)
	}
	if err := json.Unmarshal(refs, &value.References); err != nil {
		return value, err
	}
	return value, nil
}

func (s *Store) LoadFreeAnswer(ctx context.Context, id string) (tutoring.FreeAnswer, error) {
	var value tutoring.FreeAnswer
	var refs []byte
	var proposal *string
	err := s.pool.QueryRow(ctx, `SELECT id,session_id,focus_frame_id,free_question_id,answer_text,knowledge_revision_id,references_snapshot,source_proposal_id,received_at FROM tutoring_free_answers WHERE id=$1`, id).Scan(&value.ID, &value.SessionID, &value.FocusFrameID, &value.FreeQuestionID, &value.Text, &value.KnowledgeRevisionID, &refs, &proposal, &value.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("load free answer: %w", err)
	}
	value.SourceProposalID = deref(proposal)
	if err := json.Unmarshal(refs, &value.References); err != nil {
		return value, err
	}
	return value, nil
}

func (s *Store) LoadValidEvidence(ctx context.Context, nodeRevisionID string) ([]learning.AcceptedEvidence, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.decision_id,e.assessment_id,e.attempt_id,e.activity_id,e.activity_revision,e.goal_revision_id,e.route_revision_id,e.knowledge_revision_id,e.node_revision_id,e.rubric_revision,e.evidence_kind,e.activity_type,e.outcome,e.help_level,e.received_at,e.acceptance_policy_version,e.reducer_policy_version,e.review_policy_version,e.misconception_candidates,e.rubric_outcomes FROM learning_evidence e LEFT JOIN learning_evidence_invalidations i ON i.evidence_id=e.id WHERE e.node_revision_id=$1 AND i.id IS NULL ORDER BY e.received_at,e.id`, nodeRevisionID)
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
}

func (s *Store) LoadMisconceptions(ctx context.Context, nodeRevisionID string) ([]learning.MisconceptionHypothesis, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (misconception_id) misconception_id,revision,node_revision_id,rubric_item_id,candidate_hash,candidate_text,status,source_evidence_ids,counter_evidence_ids,caused_by_evidence_id FROM learning_misconception_revisions WHERE node_revision_id=$1 ORDER BY misconception_id,revision DESC`, nodeRevisionID)
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
}

func (s *Store) LatestFreeQuestion(ctx context.Context, sessionID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM tutoring_free_questions WHERE session_id=$1 ORDER BY received_at DESC,id DESC LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return "", fmt.Errorf("load latest free question: %w", err)
	}
	return id, nil
}

func (s *Store) loadSessionInteractionEvents(ctx context.Context, sessionID string) ([]learning.InteractionSample, error) {
	rows, err := s.pool.Query(ctx, `SELECT event_seq,received_at,event_type FROM learning_events WHERE aggregate_type='session' AND aggregate_id=$1 ORDER BY event_seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []learning.InteractionSample
	for rows.Next() {
		var sample learning.InteractionSample
		var eventType learning.EventType
		if err := rows.Scan(&sample.EventSequence, &sample.ReceivedAt, &eventType); err != nil {
			return nil, err
		}
		sample.SessionID = sessionID
		switch eventType {
		case learning.EventAttemptSubmitted, learning.EventFreeQuestionAsked, learning.EventReviewPresented:
			sample.UserInitiated = true
		}
		result = append(result, sample)
	}
	return result, rows.Err()
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
