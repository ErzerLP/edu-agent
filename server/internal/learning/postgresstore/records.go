package postgresstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func insertTypedRecords(ctx context.Context, tx pgx.Tx, request learning.CommitRequest) error {
	batch := request.Batch
	if value := batch.GoalRevision; value != nil {
		var previousGoalID any
		var previousRevision any
		if value.PreviousRevisionID != nil {
			previousGoalID = value.GoalID
			previousRevision = value.Revision - 1
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at,previous_revision_id,previous_goal_id,previous_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, value.ID, value.GoalID, value.Revision, value.Text, value.Source, value.ActorDeviceID, value.CreatedAt, value.PreviousRevisionID, previousGoalID, previousRevision); err != nil {
			return fmt.Errorf("insert goal revision: %w", err)
		}
	}
	if value := batch.RouteRevision; value != nil {
		var proposal any
		if value.SourceProposalID != "" {
			proposal = value.SourceProposalID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_route_revisions(id,route_id,revision,goal_revision_id,knowledge_revision_id,route_policy_version,source_proposal_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, value.ID, value.RouteID, value.Revision, value.GoalRevisionID, value.KnowledgeRevisionID, value.PolicyVersion, proposal, value.CreatedAt); err != nil {
			return fmt.Errorf("insert route revision: %w", err)
		}
		for _, step := range value.Steps {
			var nodeID, documentRevisionID string
			if err := tx.QueryRow(ctx, `SELECT node_id,document_revision_id FROM knowledge_node_revisions nr JOIN knowledge_snapshot_documents sd USING(document_revision_id) WHERE nr.id=$1 AND sd.knowledge_revision_id=$2`, step.NodeRevisionID, value.KnowledgeRevisionID).Scan(&nodeID, &documentRevisionID); err != nil {
				return fmt.Errorf("resolve route step knowledge owner: %w", err)
			}
			if step.NodeID == "" || step.NodeID != nodeID {
				return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: "route_step_node_identity"}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO learning_route_steps(id,route_revision_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,teaching_intent,completion_condition) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, step.ID, value.ID, step.Ordinal, value.KnowledgeRevisionID, step.NodeID, step.NodeRevisionID, documentRevisionID, step.TeachingIntent, step.CompletionCondition); err != nil {
				return fmt.Errorf("insert route step: %w", err)
			}
		}
	}
	if value := batch.Session; value != nil {
		completedAt := any(nil)
		if value.State == tutoring.StateCompleted {
			completedAt = request.ReceivedAt
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tutoring_sessions(id,aggregate_version,state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,attached_quiz,started_at,updated_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12,$13) ON CONFLICT(id) DO UPDATE SET aggregate_version=EXCLUDED.aggregate_version,state=EXCLUDED.state,goal_revision_id=EXCLUDED.goal_revision_id,route_revision_id=EXCLUDED.route_revision_id,route_step_id=EXCLUDED.route_step_id,knowledge_revision_id=EXCLUDED.knowledge_revision_id,focus_node_revision_id=EXCLUDED.focus_node_revision_id,activity_id=EXCLUDED.activity_id,attempt_id=EXCLUDED.attempt_id,attached_quiz=EXCLUDED.attached_quiz,updated_at=EXCLUDED.updated_at,completed_at=EXCLUDED.completed_at`, value.ID, value.AggregateVer, value.State, nullable(value.Context.GoalRevisionID), nullable(value.Context.RouteRevisionID), nullable(value.Context.RouteStepID), nullable(value.Context.KnowledgeRevisionID), nullable(value.Context.FocusNodeRevisionID), value.Context.ActivityID, value.Context.AttemptID, value.AttachedQuiz, request.ReceivedAt, completedAt); err != nil {
			return fmt.Errorf("persist tutoring session: %w", err)
		}
	}
	if value := batch.FocusFrame; value != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO tutoring_focus_frames(id,session_id,saved_state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,activity_id,attempt_id,saved_aggregate_version,created_event_seq,invalidated_at,invalidation_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT(id) DO NOTHING`, value.ID, value.SessionID, value.SavedState, nullable(value.Context.GoalRevisionID), nullable(value.Context.RouteRevisionID), nullable(value.Context.RouteStepID), nullable(value.Context.KnowledgeRevisionID), nullable(value.Context.FocusNodeRevisionID), value.Context.ActivityID, value.Context.AttemptID, value.SavedAggregateVersion, value.CreatedEventSequence, timeIf(value.Invalidated, request.ReceivedAt), nullable(value.InvalidationReason)); err != nil {
			return fmt.Errorf("insert tutoring focus frame: %w", err)
		}
	}
	if batch.InvalidateFrame && batch.Session != nil {
		if _, err := tx.Exec(ctx, `UPDATE tutoring_focus_frames SET invalidated_at=$2,invalidation_reason=COALESCE(NULLIF(invalidation_reason,''),'command') WHERE session_id=$1 AND invalidated_at IS NULL AND resumed_at IS NULL`, batch.Session.ID, request.ReceivedAt); err != nil {
			return fmt.Errorf("invalidate tutoring focus frame: %w", err)
		}
	}
	if batch.ResumeFrame && batch.Session != nil {
		if _, err := tx.Exec(ctx, `UPDATE tutoring_focus_frames SET resumed_at=$2 WHERE session_id=$1 AND invalidated_at IS NULL AND resumed_at IS NULL`, batch.Session.ID, request.ReceivedAt); err != nil {
			return fmt.Errorf("resume tutoring focus frame: %w", err)
		}
	}
	if value := batch.FreeQuestion; value != nil {
		references, _ := json.Marshal(value.References)
		if _, err := tx.Exec(ctx, `INSERT INTO tutoring_free_questions(id,session_id,focus_frame_id,question_text,knowledge_revision_id,references_snapshot,actor_device_id,occurred_at,received_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, value.ID, value.SessionID, value.FocusFrameID, value.Text, value.KnowledgeRevisionID, references, value.ActorDeviceID, value.OccurredAt, value.ReceivedAt); err != nil {
			return fmt.Errorf("insert free question: %w", err)
		}
	}
	if value := batch.FreeAnswer; value != nil {
		references, _ := json.Marshal(value.References)
		if _, err := tx.Exec(ctx, `INSERT INTO tutoring_free_answers(id,session_id,focus_frame_id,free_question_id,answer_text,knowledge_revision_id,references_snapshot,source_proposal_id,received_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, value.ID, value.SessionID, value.FocusFrameID, value.FreeQuestionID, value.Text, value.KnowledgeRevisionID, references, nullable(value.SourceProposalID), value.ReceivedAt); err != nil {
			return fmt.Errorf("insert free answer: %w", err)
		}
	}
	if value := batch.Activity; value != nil {
		rubric, _ := json.Marshal(value.Rubric)
		allowed := make([]string, len(value.AllowedHelp))
		for index := range value.AllowedHelp {
			allowed[index] = string(value.AllowedHelp[index])
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_activities(id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,target_node_id,target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,assessment_policy_version,review_policy_version,source_proposal_id,attached_free_question_id,attached_free_answer_id,is_review,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`, value.ID, value.Revision, value.SessionID, value.GoalRevisionID, value.RouteRevisionID, value.RouteStepID, value.KnowledgeRevisionID, value.TargetNodeID, value.TargetNodeRevisionID, value.Prompt, value.Type, value.Rubric.Revision, rubric, value.Difficulty, allowed, value.ActivityPolicyVersion, value.AssessmentPolicyVersion, value.ReviewPolicyVersion, nullable(value.SourceProposalID), nullable(value.AttachedFreeQuestionID), nullable(value.AttachedFreeAnswerID), value.Review, value.CreatedAt); err != nil {
			return fmt.Errorf("insert learning activity: %w", err)
		}
		for index, ref := range value.References {
			hash, err := decodeHash(ref.SliceSHA256)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO learning_activity_references(activity_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,source_start,source_end,slice_text,slice_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, value.ID, index, ref.KnowledgeRevisionID, ref.NodeID, ref.NodeRevisionID, ref.DocumentRevisionID, ref.Range.Start, ref.Range.End, ref.Slice, hash); err != nil {
				return fmt.Errorf("insert learning activity reference: %w", err)
			}
		}
	}
	if value := batch.Attempt; value != nil {
		hash, err := decodeHash(value.AnswerSHA256)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_attempt_payloads(id,answer_text,payload_hash,created_at) VALUES($1,$2,$3,$4)`, value.AnswerPayloadID, value.Answer, hash, value.ReceivedAt); err != nil {
			return fmt.Errorf("insert attempt payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_attempts(id,session_id,activity_id,activity_revision,answer_payload_id,help_level,actor_device_id,occurred_at,received_at,payload_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, value.ID, value.SessionID, value.ActivityID, value.ActivityRevision, value.AnswerPayloadID, value.Help, value.ActorDeviceID, value.OccurredAt, value.ReceivedAt, hash); err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}
	}
	if value := batch.Assessment; value != nil {
		inputHash, err := decodeHash(value.ProposalInputHash)
		if err != nil {
			return err
		}
		parameters, _ := json.Marshal(value.ModelParameters)
		risks := make([]string, len(value.RiskFlags))
		for index := range value.RiskFlags {
			risks[index] = string(value.RiskFlags[index])
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_assessments(id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,risk_flags,trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,model_attempts,attempt_categories,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, value.ID, value.SessionID, value.AttemptID, value.ActivityID, value.ActivityRevision, value.RubricComplete, value.Confidence, risks, value.ModelID, parameters, value.PromptRevision, inputHash, value.Attempts, value.AttemptCategories, value.CreatedAt); err != nil {
			return fmt.Errorf("insert assessment: %w", err)
		}
		for index, item := range value.Items {
			answerQuoteHash := item.AnswerQuoteSHA256
			if answerQuoteHash == "" {
				answerQuoteHash = learning.SHA256(nil)
			}
			answerHash, err := decodeHash(answerQuoteHash)
			if err != nil {
				return err
			}
			var knowledgeHash any
			if item.KnowledgeQuoteSHA256 != "" {
				decoded, err := decodeHash(item.KnowledgeQuoteSHA256)
				if err != nil {
					return err
				}
				knowledgeHash = decoded
			}
			var knowledgeRevision, node, nodeID, documentRevision, start, end, quote any
			if item.KnowledgeReferenceID != "" {
				var resolvedKnowledgeRevision, resolvedNodeID, resolvedDocumentRevision string
				if err := tx.QueryRow(ctx, `SELECT a.knowledge_revision_id,nr.node_id,nr.document_revision_id FROM learning_activities a JOIN knowledge_node_revisions nr ON nr.id=$2 JOIN knowledge_snapshot_documents sd ON sd.knowledge_revision_id=a.knowledge_revision_id AND sd.document_revision_id=nr.document_revision_id WHERE a.id=$1 AND a.revision=$3`, value.ActivityID, item.KnowledgeReferenceID, value.ActivityRevision).Scan(&resolvedKnowledgeRevision, &resolvedNodeID, &resolvedDocumentRevision); err != nil {
					return fmt.Errorf("resolve assessment item knowledge owner: %w", err)
				}
				knowledgeRevision, node, nodeID, documentRevision = resolvedKnowledgeRevision, item.KnowledgeReferenceID, resolvedNodeID, resolvedDocumentRevision
				start, end, quote = item.KnowledgeRange.Start, item.KnowledgeRange.End, item.KnowledgeQuote
			}
			if _, err := tx.Exec(ctx, `INSERT INTO learning_assessment_items(assessment_id,ordinal,rubric_item_id,conclusion,answer_start,answer_end,answer_quote,answer_quote_hash,knowledge_revision_id,knowledge_node_revision_id,knowledge_node_id,knowledge_document_revision_id,knowledge_start,knowledge_end,knowledge_quote,knowledge_quote_hash,misconception_candidate) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, value.ID, index, item.RubricItemID, item.Conclusion, item.AnswerRange.Start, item.AnswerRange.End, item.AnswerQuote, answerHash, knowledgeRevision, node, nodeID, documentRevision, start, end, quote, knowledgeHash, nullable(item.MisconceptionCandidate)); err != nil {
				return fmt.Errorf("insert assessment item: %w", err)
			}
		}
	}
	for _, value := range batch.Decisions {
		items, _ := json.Marshal(value.Items)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_assessment_decisions(id,assessment_id,version,disposition,conclusions,reason,actor_device_id,replaces_decision_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, value.ID, value.AssessmentID, value.Version, value.Disposition, items, nullable(value.Reason), value.ActorDeviceID, value.ReplacesDecisionID, value.CreatedAt); err != nil {
			return fmt.Errorf("insert assessment decision: %w", err)
		}
	}
	for _, value := range batch.Evidence {
		candidates, _ := json.Marshal(value.Misconceptions)
		outcomes, _ := json.Marshal(value.RubricOutcomes)
		var sessionID, nodeID, documentRevisionID string
		if err := tx.QueryRow(ctx, `SELECT a.session_id,nr.node_id,nr.document_revision_id FROM learning_activities a JOIN knowledge_node_revisions nr ON nr.id=$2 JOIN knowledge_snapshot_documents sd ON sd.knowledge_revision_id=$3 AND sd.document_revision_id=nr.document_revision_id WHERE a.id=$1 AND a.revision=$4 AND a.knowledge_revision_id=$3`, value.ActivityID, value.NodeRevisionID, value.KnowledgeRevisionID, value.ActivityRevision).Scan(&sessionID, &nodeID, &documentRevisionID); err != nil {
			return fmt.Errorf("resolve evidence ownership: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_evidence(id,decision_id,assessment_id,session_id,attempt_id,activity_id,activity_revision,goal_revision_id,route_revision_id,knowledge_revision_id,node_revision_id,node_id,document_revision_id,rubric_revision,evidence_kind,activity_type,outcome,help_level,received_at,acceptance_policy_version,reducer_policy_version,review_policy_version,misconception_candidates,rubric_outcomes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, value.ID, value.DispositionDecisionID, value.AssessmentID, sessionID, value.AttemptID, value.ActivityID, value.ActivityRevision, value.GoalRevisionID, value.RouteRevisionID, value.KnowledgeRevisionID, value.NodeRevisionID, nodeID, documentRevisionID, value.RubricRevision, value.Kind, value.ActivityType, value.Outcome, value.Help, value.ReceivedAt, value.AcceptancePolicyVersion, value.ReducerPolicyVersion, value.ReviewPolicyVersion, candidates, outcomes); err != nil {
			return fmt.Errorf("insert accepted evidence: %w", err)
		}
	}
	for _, value := range batch.Invalidations {
		if _, err := tx.Exec(ctx, `INSERT INTO learning_evidence_invalidations(id,evidence_id,decision_id,reason,event_seq,created_at) VALUES($1,$2,$3,$4,$5,$6)`, value.ID, value.EvidenceID, value.DecisionID, value.Reason, value.EventSeq, value.CreatedAt); err != nil {
			return fmt.Errorf("insert evidence invalidation: %w", err)
		}
	}
	for _, value := range batch.Exposures {
		references, _ := json.Marshal(value.References)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_exposures(id,session_id,exposure_kind,content,references_snapshot,source_proposal_id,received_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, value.ID, value.SessionID, value.Kind, value.Text, references, nullable(value.SourceProposalID), value.ReceivedAt); err != nil {
			return fmt.Errorf("insert exposure: %w", err)
		}
	}
	for _, value := range batch.Misconceptions {
		recordID := uuid.NewSHA1(eventNamespace, []byte(fmt.Sprintf("misconception\n%s\n%d", value.ID, value.Revision))).String()
		hash, err := decodeHash(value.CandidateHash)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_misconception_revisions(id,misconception_id,revision,node_revision_id,rubric_item_id,candidate_hash,candidate_text,status,source_evidence_ids,counter_evidence_ids,caused_by_evidence_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, recordID, value.ID, value.Revision, value.NodeRevisionID, value.RubricItemID, hash, value.Candidate, value.Status, value.SourceEvidenceIDs, value.CounterEvidenceIDs, value.CausedByEvidenceID, request.ReceivedAt); err != nil {
			return fmt.Errorf("insert misconception revision: %w", err)
		}
	}
	return nil
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
