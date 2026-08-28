package postgresstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) insertTypedRecords(ctx context.Context, tx pgx.Tx, request learning.CommitRequest) error {
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
			owner, ok := batch.Authority.RouteSteps[step.ID]
			if !ok || owner.KnowledgeRevisionID != value.KnowledgeRevisionID || owner.NodeID != step.NodeID || owner.NodeRevisionID != step.NodeRevisionID || owner.DocumentRevisionID == "" {
				return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: "route_step_owner_missing"}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO learning_route_steps(id,route_revision_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,teaching_intent,completion_condition) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, step.ID, value.ID, step.Ordinal, owner.KnowledgeRevisionID, owner.NodeID, owner.NodeRevisionID, owner.DocumentRevisionID, step.TeachingIntent, step.CompletionCondition); err != nil {
				return fmt.Errorf("insert route step: %w", err)
			}
		}
	}
	if err := s.tutoring.Persist(ctx, tx, tutoringpostgres.WriteSet{
		Session: batch.Session, FocusFrame: batch.FocusFrame,
		InvalidateFrame: batch.InvalidateFrame, ResumeFrame: batch.ResumeFrame,
		FreeQuestion: batch.FreeQuestion, FreeAnswer: batch.FreeAnswer, ReceivedAt: request.ReceivedAt,
	}); err != nil {
		return err
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
		if _, err := tx.Exec(ctx, `INSERT INTO learning_attempts(id,session_id,activity_id,activity_revision,answer_payload_id,help_level,actor_device_id,occurred_at,received_at,payload_hash,evidence_eligibility,evidence_ineligible_reason,archive_disposition,offline_submission_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,NULLIF($14,'')::uuid)`, value.ID, value.SessionID, value.ActivityID, value.ActivityRevision, value.AnswerPayloadID, value.Help, value.ActorDeviceID, value.OccurredAt, value.ReceivedAt, hash, value.EvidenceEligibility, value.EvidenceIneligibleReason, defaultArchiveDisposition(value.ArchiveDisposition), value.OfflineSubmissionID); err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}
		if batch.EvidenceClaimSource != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO learning_activity_evidence_claims(activity_id,activity_revision,winning_attempt_id,claim_source,claimed_event_seq,claimed_at) VALUES($1,$2,$3,$4,$5,$6)`, value.ActivityID, value.ActivityRevision, value.ID, batch.EvidenceClaimSource, nullableInt64(batch.EvidenceClaimEventSeq), value.ReceivedAt); err != nil {
				return fmt.Errorf("insert activity evidence claim: %w", err)
			}
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
		if _, err := tx.Exec(ctx, `INSERT INTO learning_assessments(id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,risk_flags,trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,model_attempts,attempt_categories,created_at,evidence_eligibility,evidence_ineligible_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,NULLIF($17,''))`, value.ID, value.SessionID, value.AttemptID, value.ActivityID, value.ActivityRevision, value.RubricComplete, value.Confidence, risks, value.ModelID, parameters, value.PromptRevision, inputHash, value.Attempts, value.AttemptCategories, value.CreatedAt, value.EvidenceEligibility, value.EvidenceIneligibleReason); err != nil {
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
				if index >= len(batch.Authority.AssessmentItems) {
					return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: "assessment_item_owner_missing"}
				}
				owner := batch.Authority.AssessmentItems[index]
				if owner.KnowledgeRevisionID == "" || owner.NodeID == "" || owner.NodeRevisionID != item.KnowledgeReferenceID || owner.DocumentRevisionID == "" {
					return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: "assessment_item_owner_invalid"}
				}
				knowledgeRevision, node, nodeID, documentRevision = owner.KnowledgeRevisionID, owner.NodeRevisionID, owner.NodeID, owner.DocumentRevisionID
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
		owner, ok := batch.Authority.Evidence[value.ID]
		if !ok || owner.SessionID == "" || owner.KnowledgeRevisionID != value.KnowledgeRevisionID || owner.NodeRevisionID != value.NodeRevisionID || owner.NodeID == "" || owner.DocumentRevisionID == "" {
			return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: "evidence_owner_missing"}
		}
		if value.AcceptedEventSequence < 1 {
			return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "evidence_accepted_event_seq_missing"}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_evidence(id,decision_id,assessment_id,session_id,attempt_id,activity_id,activity_revision,goal_revision_id,route_revision_id,knowledge_revision_id,node_revision_id,node_id,document_revision_id,rubric_revision,evidence_kind,activity_type,outcome,help_level,received_at,accepted_event_seq,acceptance_policy_version,reducer_policy_version,review_policy_version,misconception_candidates,rubric_outcomes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`, value.ID, value.DispositionDecisionID, value.AssessmentID, owner.SessionID, value.AttemptID, value.ActivityID, value.ActivityRevision, value.GoalRevisionID, value.RouteRevisionID, owner.KnowledgeRevisionID, owner.NodeRevisionID, owner.NodeID, owner.DocumentRevisionID, value.RubricRevision, value.Kind, value.ActivityType, value.Outcome, value.Help, value.ReceivedAt, value.AcceptedEventSequence, value.AcceptancePolicyVersion, value.ReducerPolicyVersion, value.ReviewPolicyVersion, candidates, outcomes); err != nil {
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

func defaultArchiveDisposition(value string) string {
	if value == "" {
		return "online"
	}
	return value
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
