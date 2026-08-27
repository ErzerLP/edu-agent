package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	EventSchemaVersion         = 1
	EventRedactedSchemaVersion = 2
	EventEnvelopeVersion       = "learning-event-v1"
	ProjectionVersion          = "learning-projection-v2"
	MasteryReducerVersion      = "mastery-reducer-v1"
	AssessmentPolicyVersion    = "assessment-acceptance-v1"
	ReviewPolicyVersion        = "fixed-interval-v1"
	ActiveTimePolicyVersion    = "estimated-active-time-v1"
	RoutePolicyVersion         = "route-policy-v1"
	ActivityPolicyVersion      = "activity-policy-v1"
	ProposalSchemaVersion      = 1
	MaxGoalRunes               = 4000
	MaxGoalSourceBytes         = 200
	MaxQuestionRunes           = 8000
	MaxAnswerBytes             = 256 << 10
	MaxRubricItems             = 64
	MaxProposalTextRunes       = 32000
)

const (
	CodeInvalidRequest                = "invalid_request"
	CodeNotFound                      = "not_found"
	CodeIdempotencyConflict           = "idempotency_conflict"
	CodeVersionConflict               = "version_conflict"
	CodeInvalidTransition             = "invalid_transition"
	CodeActivityStateConflict         = "activity_state_conflict"
	CodeKnowledgeReferenceInvalid     = "knowledge_reference_invalid"
	CodeStaleProposal                 = "stale_proposal"
	CodeProposalRejected              = "proposal_rejected"
	CodeAssessmentDispositionConflict = "assessment_disposition_conflict"
	CodeFocusFrameInvalidated         = "focus_frame_invalidated"
	CodeUnsupportedEventSchema        = "unsupported_event_schema"
	CodeProjectionUnavailable         = "projection_unavailable"
	CodeContentRedacted               = "content_redacted"
	CodeStaleCursor                   = "stale_cursor"
	CodeModelUnavailable              = "model_unavailable"
	CodeOfflinePrepareUnavailable     = "offline_prepare_unavailable"
	CodeOfflineSignerUnavailable      = "offline_signer_unavailable"
	CodeOfflineOperationNotFound      = "operation_not_found"
)

type Error struct {
	Code               string `json:"code"`
	AggregateType      string `json:"aggregate_type,omitempty"`
	AggregateID        string `json:"aggregate_id,omitempty"`
	ExpectedVersion    int64  `json:"expected_version,omitempty"`
	CurrentVersion     int64  `json:"current_version,omitempty"`
	AsOfEventSequence  int64  `json:"as_of_event_seq,omitempty"`
	CurrentDisposition string `json:"current_disposition,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Cause              error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("learning operation failed: code=%s: %v", e.Code, e.Cause)
	}
	return "learning operation failed: code=" + e.Code
}
func (e *Error) Unwrap() error { return e.Cause }
func ErrorCode(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

type SourceRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type KnowledgeReference struct {
	KnowledgeRevisionID string      `json:"knowledge_revision_id"`
	NodeID              string      `json:"node_id"`
	NodeRevisionID      string      `json:"node_revision_id"`
	DocumentRevisionID  string      `json:"document_revision_id,omitempty"`
	Range               SourceRange `json:"range"`
	Slice               string      `json:"slice,omitempty"`
	SliceSHA256         string      `json:"slice_sha256"`
}

type KnowledgeReferenceResolver interface {
	Resolve(context.Context, string, string) (KnowledgeReference, error)
}

type GoalRevision struct {
	ID                 string    `json:"goal_revision_id"`
	GoalID             string    `json:"goal_id"`
	Revision           int64     `json:"revision"`
	Text               string    `json:"text"`
	Source             string    `json:"source"`
	ActorDeviceID      string    `json:"actor_device_id"`
	CreatedAt          time.Time `json:"created_at"`
	PreviousRevisionID *string   `json:"previous_revision_id,omitempty"`
}

type RouteStep struct {
	ID                  string `json:"route_step_id"`
	Ordinal             int    `json:"ordinal"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	TeachingIntent      string `json:"teaching_intent"`
	CompletionCondition string `json:"completion_condition"`
}

type RouteRevision struct {
	ID                  string      `json:"route_revision_id"`
	RouteID             string      `json:"route_id"`
	Revision            int64       `json:"revision"`
	GoalRevisionID      string      `json:"goal_revision_id"`
	KnowledgeRevisionID string      `json:"knowledge_revision_id"`
	PolicyVersion       string      `json:"route_policy_version"`
	SourceProposalID    string      `json:"source_proposal_id"`
	Steps               []RouteStep `json:"steps"`
	CreatedAt           time.Time   `json:"created_at"`
}

type ActivityType string

const (
	ActivityObjective   ActivityType = "objective"
	ActivityOpen        ActivityType = "open"
	ActivityExplanation ActivityType = "explanation"
)

type HelpLevel string

const (
	HelpNone           HelpLevel = "none"
	HelpHint           HelpLevel = "hint"
	HelpScaffold       HelpLevel = "scaffold"
	HelpAnswerRevealed HelpLevel = "answer_revealed"
)

type RubricItem struct {
	ID                   string   `json:"rubric_item_id"`
	Criterion            string   `json:"criterion"`
	RequiredReferenceIDs []string `json:"required_reference_ids,omitempty"`
}

type ObjectiveRule struct {
	AcceptedAnswers []string `json:"accepted_answers"`
	CaseSensitive   bool     `json:"case_sensitive"`
	TrimSpace       bool     `json:"trim_space"`
}

type Rubric struct {
	Revision      string         `json:"rubric_revision"`
	Items         []RubricItem   `json:"items"`
	ObjectiveRule *ObjectiveRule `json:"objective_rule,omitempty"`
}

type Activity struct {
	ID                      string               `json:"activity_id"`
	Revision                int64                `json:"revision"`
	SessionID               string               `json:"session_id"`
	GoalRevisionID          string               `json:"goal_revision_id"`
	RouteRevisionID         string               `json:"route_revision_id"`
	RouteStepID             string               `json:"route_step_id"`
	KnowledgeRevisionID     string               `json:"knowledge_revision_id"`
	TargetNodeID            string               `json:"target_node_id"`
	TargetNodeRevisionID    string               `json:"target_node_revision_id"`
	References              []KnowledgeReference `json:"knowledge_references"`
	Prompt                  string               `json:"prompt"`
	Type                    ActivityType         `json:"type"`
	Rubric                  Rubric               `json:"rubric"`
	Difficulty              int                  `json:"difficulty"`
	AllowedHelp             []HelpLevel          `json:"allowed_help"`
	ActivityPolicyVersion   string               `json:"activity_policy_version"`
	AssessmentPolicyVersion string               `json:"assessment_policy_version"`
	ReviewPolicyVersion     string               `json:"review_policy_version"`
	SourceProposalID        string               `json:"source_proposal_id,omitempty"`
	AttachedFreeQuestionID  string               `json:"attached_free_question_id,omitempty"`
	AttachedFreeAnswerID    string               `json:"attached_free_answer_id,omitempty"`
	Review                  bool                 `json:"review"`
	CreatedAt               time.Time            `json:"created_at"`
}

type Attempt struct {
	ID                       string     `json:"attempt_id"`
	SessionID                string     `json:"session_id"`
	ActivityID               string     `json:"activity_id"`
	ActivityRevision         int64      `json:"activity_revision"`
	AnswerPayloadID          string     `json:"answer_payload_id"`
	Answer                   string     `json:"answer"`
	AnswerSHA256             string     `json:"answer_sha256"`
	Help                     HelpLevel  `json:"help"`
	ActorDeviceID            string     `json:"actor_device_id"`
	OccurredAt               *time.Time `json:"occurred_at,omitempty"`
	ReceivedAt               time.Time  `json:"received_at"`
	EvidenceEligibility      bool       `json:"evidence_eligibility"`
	EvidenceIneligibleReason string     `json:"evidence_ineligible_reason,omitempty"`
	ArchiveDisposition       string     `json:"archive_disposition,omitempty"`
	OfflineSubmissionID      string     `json:"offline_submission_id,omitempty"`
}

type Conclusion string

const (
	ConclusionPass       Conclusion = "pass"
	ConclusionPartial    Conclusion = "partial"
	ConclusionFail       Conclusion = "fail"
	ConclusionUnassessed Conclusion = "unassessed"
)

type AssessmentItem struct {
	RubricItemID           string      `json:"rubric_item_id"`
	Conclusion             Conclusion  `json:"conclusion"`
	AnswerQuote            string      `json:"answer_quote"`
	AnswerRange            SourceRange `json:"answer_range"`
	AnswerQuoteSHA256      string      `json:"answer_quote_sha256"`
	KnowledgeReferenceID   string      `json:"knowledge_reference_id"`
	KnowledgeQuote         string      `json:"knowledge_quote"`
	KnowledgeRange         SourceRange `json:"knowledge_range"`
	KnowledgeQuoteSHA256   string      `json:"knowledge_quote_sha256"`
	MisconceptionCandidate string      `json:"misconception_candidate,omitempty"`
}

type RiskFlag string

const (
	RiskIncompleteRubric             RiskFlag = "incomplete_rubric"
	RiskInsufficientAnswerEvidence   RiskFlag = "insufficient_answer_evidence"
	RiskInsufficientKnowledgeSupport RiskFlag = "insufficient_knowledge_support"
	RiskConflictingEvidence          RiskFlag = "conflicting_evidence"
	RiskAmbiguousRubric              RiskFlag = "ambiguous_rubric"
	RiskUnsafeContent                RiskFlag = "unsafe_content"
	RiskSchemaRepaired               RiskFlag = "schema_repaired"
	RiskStaleContext                 RiskFlag = "stale_context"
	RiskRetryExhausted               RiskFlag = "retry_exhausted"
)

type AssessmentArtifact struct {
	ID                       string           `json:"assessment_id"`
	SessionID                string           `json:"session_id"`
	AttemptID                string           `json:"attempt_id"`
	ActivityID               string           `json:"activity_id"`
	ActivityRevision         int64            `json:"activity_revision"`
	Items                    []AssessmentItem `json:"items"`
	RubricComplete           bool             `json:"rubric_complete"`
	Confidence               int              `json:"confidence"`
	RiskFlags                []RiskFlag       `json:"risk_flags"`
	ModelID                  string           `json:"model_id"`
	ModelParameters          map[string]any   `json:"model_parameters"`
	PromptRevision           string           `json:"prompt_revision"`
	ProposalInputHash        string           `json:"proposal_input_hash"`
	Attempts                 int              `json:"attempts"`
	AttemptCategories        []string         `json:"attempt_categories"`
	CreatedAt                time.Time        `json:"created_at"`
	EvidenceEligibility      bool             `json:"evidence_eligibility"`
	EvidenceIneligibleReason string           `json:"evidence_ineligible_reason,omitempty"`
}

type Disposition string

const (
	DispositionProvisional Disposition = "provisional"
	DispositionAccepted    Disposition = "accepted"
	DispositionOverridden  Disposition = "overridden"
	DispositionVoided      Disposition = "voided"
)

type AssessmentDecision struct {
	ID                 string           `json:"decision_id"`
	AssessmentID       string           `json:"assessment_id"`
	Version            int64            `json:"version"`
	Disposition        Disposition      `json:"disposition"`
	Items              []AssessmentItem `json:"items"`
	Reason             string           `json:"reason,omitempty"`
	ActorDeviceID      string           `json:"actor_device_id"`
	CreatedAt          time.Time        `json:"created_at"`
	ReplacesDecisionID *string          `json:"replaces_decision_id,omitempty"`
	ProducedEvidenceID *string          `json:"produced_evidence_id,omitempty"`
}

type EvidenceKind string

const (
	EvidencePracticeRecall EvidenceKind = "practice_recall"
	EvidenceReviewRecall   EvidenceKind = "review_recall"
)

type Outcome string

const (
	OutcomePass    Outcome = "pass"
	OutcomePartial Outcome = "partial"
	OutcomeFail    Outcome = "fail"
)

type MisconceptionCandidate struct {
	RubricItemID string `json:"rubric_item_id"`
	Text         string `json:"text"`
}

type RubricOutcome struct {
	RubricItemID string     `json:"rubric_item_id"`
	Conclusion   Conclusion `json:"conclusion"`
}

type AcceptedEvidence struct {
	ID                      string                   `json:"evidence_id"`
	DispositionDecisionID   string                   `json:"disposition_decision_id"`
	AssessmentID            string                   `json:"assessment_id"`
	AttemptID               string                   `json:"attempt_id"`
	ActivityID              string                   `json:"activity_id"`
	ActivityRevision        int64                    `json:"activity_revision"`
	GoalRevisionID          string                   `json:"goal_revision_id"`
	RouteRevisionID         string                   `json:"route_revision_id"`
	KnowledgeRevisionID     string                   `json:"knowledge_revision_id"`
	NodeRevisionID          string                   `json:"node_revision_id"`
	RubricRevision          string                   `json:"rubric_revision"`
	Kind                    EvidenceKind             `json:"kind"`
	ActivityType            ActivityType             `json:"activity_type"`
	Outcome                 Outcome                  `json:"outcome"`
	Help                    HelpLevel                `json:"help"`
	ReceivedAt              time.Time                `json:"received_at"`
	AcceptedEventSequence   int64                    `json:"accepted_event_seq"`
	AcceptancePolicyVersion string                   `json:"acceptance_policy_version"`
	ReducerPolicyVersion    string                   `json:"reducer_policy_version"`
	ReviewPolicyVersion     string                   `json:"review_policy_version"`
	Misconceptions          []MisconceptionCandidate `json:"misconceptions,omitempty"`
	RubricOutcomes          []RubricOutcome          `json:"rubric_outcomes,omitempty"`
}

type Exposure struct {
	ID               string               `json:"exposure_id"`
	SessionID        string               `json:"session_id"`
	Kind             string               `json:"kind"`
	Text             string               `json:"text"`
	References       []KnowledgeReference `json:"knowledge_references"`
	SourceProposalID string               `json:"source_proposal_id,omitempty"`
	ReceivedAt       time.Time            `json:"received_at"`
}

type OperationEnvelope struct {
	OperationID          string          `json:"operation_id"`
	PayloadSchemaVersion int             `json:"payload_schema_version"`
	AggregateType        string          `json:"aggregate_type"`
	AggregateID          string          `json:"aggregate_id"`
	ExpectedVersion      int64           `json:"expected_version"`
	OccurredAt           *time.Time      `json:"occurred_at,omitempty"`
	Payload              json.RawMessage `json:"payload"`
}

type OperationResult struct {
	Status              string          `json:"status"`
	Replayed            bool            `json:"replayed"`
	Archived            bool            `json:"archived"`
	AggregateType       string          `json:"aggregate_type"`
	AggregateID         string          `json:"aggregate_id"`
	AggregateVersion    int64           `json:"aggregate_version"`
	FirstEventSequence  int64           `json:"first_event_seq"`
	LastEventSequence   int64           `json:"last_event_seq"`
	ProjectionAsOf      int64           `json:"projection_as_of_event_seq"`
	TutoringState       string          `json:"tutoring_state,omitempty"`
	EvidenceDisposition Disposition     `json:"evidence_disposition,omitempty"`
	Result              json.RawMessage `json:"result"`
}

type ProposalType string

const (
	ProposalRoute       ProposalType = "route"
	ProposalActivity    ProposalType = "activity"
	ProposalAssessment  ProposalType = "assessment"
	ProposalFreeAnswer  ProposalType = "free_answer"
	ProposalExplanation ProposalType = "explanation"
)

type ProposalRequest struct {
	RequestID           string          `json:"request_id"`
	Type                ProposalType    `json:"proposal_type"`
	AggregateType       string          `json:"aggregate_type"`
	AggregateID         string          `json:"aggregate_id"`
	AggregateVersion    int64           `json:"aggregate_version"`
	GoalRevisionID      string          `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string          `json:"route_revision_id,omitempty"`
	RouteStepID         string          `json:"route_step_id,omitempty"`
	FocusNodeRevisionID string          `json:"focus_node_revision_id,omitempty"`
	ActivityID          string          `json:"activity_id,omitempty"`
	AttemptID           string          `json:"attempt_id,omitempty"`
	FreeQuestionID      string          `json:"free_question_id,omitempty"`
	FreeAnswerID        string          `json:"free_answer_id,omitempty"`
	FocusFrameID        string          `json:"focus_frame_id,omitempty"`
	TutoringState       string          `json:"tutoring_state,omitempty"`
	KnowledgeRevisionID string          `json:"knowledge_revision_id"`
	NodeRevisionIDs     []string        `json:"node_revision_ids"`
	Input               json.RawMessage `json:"input"`
}

type RouteProposalStep struct {
	NodeRevisionID      string `json:"node_revision_id"`
	TeachingIntent      string `json:"teaching_intent"`
	CompletionCondition string `json:"completion_condition"`
}
type ActivityProposal struct {
	Prompt      string               `json:"prompt"`
	Type        ActivityType         `json:"type"`
	Rubric      Rubric               `json:"rubric"`
	Difficulty  int                  `json:"difficulty"`
	AllowedHelp []HelpLevel          `json:"allowed_help"`
	References  []KnowledgeReference `json:"knowledge_references"`
}
type TextProposal struct {
	Text       string               `json:"text"`
	References []KnowledgeReference `json:"knowledge_references"`
}

type ProposalArtifact struct {
	ID                  string              `json:"proposal_id"`
	SchemaVersion       int                 `json:"schema_version"`
	InputHash           string              `json:"input_hash"`
	Type                ProposalType        `json:"proposal_type"`
	AggregateType       string              `json:"aggregate_type"`
	AggregateID         string              `json:"aggregate_id"`
	AggregateVersion    int64               `json:"aggregate_version"`
	GoalRevisionID      string              `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string              `json:"route_revision_id,omitempty"`
	ActivityID          string              `json:"activity_id,omitempty"`
	AttemptID           string              `json:"attempt_id,omitempty"`
	KnowledgeRevisionID string              `json:"knowledge_revision_id"`
	FrozenRequest       ProposalRequest     `json:"frozen_request"`
	Route               []RouteProposalStep `json:"route,omitempty"`
	Activity            *ActivityProposal   `json:"activity,omitempty"`
	Assessment          *AssessmentArtifact `json:"assessment,omitempty"`
	Text                *TextProposal       `json:"text,omitempty"`
	ModelID             string              `json:"model_id"`
	ModelParameters     map[string]any      `json:"model_parameters"`
	PromptRevision      string              `json:"prompt_revision"`
	AttemptCategories   []string            `json:"attempt_categories"`
	CreatedAt           time.Time           `json:"created_at"`
}

func HashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return "", err
	}
	encoded, err = json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func SHA256(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }

func ValidateGoal(value string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > MaxGoalRunes {
		return &Error{Code: CodeInvalidRequest}
	}
	return nil
}

func ValidateGoalSource(value string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || len(value) > MaxGoalSourceBytes {
		return &Error{Code: CodeInvalidRequest}
	}
	return nil
}

func ValidateOperation(operation OperationEnvelope) error {
	if operation.OperationID == "" || operation.PayloadSchemaVersion != 1 || operation.AggregateType == "" || operation.AggregateID == "" || operation.ExpectedVersion < 0 || !json.Valid(operation.Payload) {
		return &Error{Code: CodeInvalidRequest}
	}
	return nil
}

func CloneActivity(value Activity) Activity {
	value.References = append([]KnowledgeReference(nil), value.References...)
	value.Rubric.Items = append([]RubricItem(nil), value.Rubric.Items...)
	for i := range value.Rubric.Items {
		value.Rubric.Items[i].RequiredReferenceIDs = append([]string(nil), value.Rubric.Items[i].RequiredReferenceIDs...)
	}
	if value.Rubric.ObjectiveRule != nil {
		copy := *value.Rubric.ObjectiveRule
		copy.AcceptedAnswers = append([]string(nil), copy.AcceptedAnswers...)
		value.Rubric.ObjectiveRule = &copy
	}
	value.AllowedHelp = append([]HelpLevel(nil), value.AllowedHelp...)
	return value
}

func StableRouteSteps(steps []RouteStep) bool {
	seen := map[string]bool{}
	for i, step := range steps {
		if step.Ordinal != i || step.ID == "" || step.NodeID == "" || step.NodeRevisionID == "" || seen[step.NodeRevisionID] {
			return false
		}
		seen[step.NodeRevisionID] = true
	}
	return len(steps) > 0
}

func SortEvidence(values []AcceptedEvidence) {
	sort.SliceStable(values, func(i, j int) bool {
		left, right := values[i], values[j]
		if left.AcceptedEventSequence != right.AcceptedEventSequence {
			if left.AcceptedEventSequence == 0 {
				return false
			}
			if right.AcceptedEventSequence == 0 {
				return true
			}
			return left.AcceptedEventSequence < right.AcceptedEventSequence
		}
		if left.AcceptedEventSequence == 0 && !left.ReceivedAt.Equal(right.ReceivedAt) {
			return left.ReceivedAt.Before(right.ReceivedAt)
		}
		return left.ID < right.ID
	})
}
