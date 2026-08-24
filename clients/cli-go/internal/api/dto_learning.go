package api

import "time"

const ProposalContextSchemaVersion = "go-cli-context-v1"

type KnowledgeRetrievalRequest struct {
	Query                     string                `json:"query"`
	KnowledgeRevisionID       string                `json:"knowledge_revision_id,omitempty"`
	QueryContextSchemaVersion string                `json:"query_context_schema_version,omitempty"`
	Context                   map[string]any        `json:"context,omitempty"`
	Limits                    *KnowledgeQueryLimits `json:"limits,omitempty"`
}

type KnowledgeQueryLimits struct {
	MaxDepth           int `json:"max_depth,omitempty"`
	CandidatesPerLayer int `json:"candidates_per_layer,omitempty"`
	MaxHits            int `json:"max_hits,omitempty"`
	TotalCandidates    int `json:"total_candidates,omitempty"`
}

type KnowledgeRetrievalResult struct {
	KnowledgeRevisionID       string           `json:"knowledge_revision_id"`
	RetrieverVersion          string           `json:"retriever_version"`
	SelectorVersion           string           `json:"selector_version"`
	QueryContextSchemaVersion string           `json:"query_context_schema_version"`
	SummarySnapshot           []string         `json:"summary_snapshot"`
	DocumentShortlist         []string         `json:"document_shortlist"`
	Trace                     []RetrievalTrace `json:"trace"`
	Hits                      []RetrievalHit   `json:"hits"`
	Degraded                  bool             `json:"degraded"`
	Truncated                 bool             `json:"truncated"`
}

type RetrievalTrace struct {
	Index                int                  `json:"index"`
	Depth                int                  `json:"depth"`
	ParentNodeRevisionID string               `json:"parent_node_revision_id"`
	Candidates           []RetrievalCandidate `json:"candidates"`
	Decisions            []RetrievalDecision  `json:"decisions"`
	CandidateSetHash     string               `json:"candidate_set_hash"`
	ReasonCode           string               `json:"reason_code,omitempty"`
	Degraded             bool                 `json:"degraded"`
	Truncated            bool                 `json:"truncated"`
}

type RetrievalCandidate struct {
	Ordinal           int    `json:"ordinal"`
	NodeRevisionID    string `json:"node_revision_id"`
	Score             int    `json:"score"`
	Title             string `json:"title"`
	TitleSHA256       string `json:"title_sha256"`
	SummaryArtifactID string `json:"summary_artifact_id,omitempty"`
	HasChildren       bool   `json:"has_children"`
	LocalBodyScore    int    `json:"local_body_score"`
}

type RetrievalDecision struct {
	NodeRevisionID string `json:"node_revision_id"`
	Action         string `json:"action"`
}

type RetrievalHit struct {
	DocumentID         string      `json:"document_id"`
	DocumentRevisionID string      `json:"document_revision_id"`
	NodeID             string      `json:"node_id"`
	NodeRevisionID     string      `json:"node_revision_id"`
	Path               string      `json:"path"`
	HeadingRange       SourceRange `json:"heading_range"`
	LocalBodyRange     SourceRange `json:"local_body_range"`
	SectionRange       SourceRange `json:"section_range"`
	CanonicalSlice     string      `json:"canonical_slice"`
	SliceSHA256        string      `json:"slice_sha256"`
	TraceIndex         int         `json:"trace_index"`
	Depth              int         `json:"depth"`
	Provenance         string      `json:"provenance"`
}

type LearningGoalRequest struct {
	OperationID          string `json:"operation_id"`
	PayloadSchemaVersion int    `json:"payload_schema_version"`
	AggregateType        string `json:"aggregate_type"`
	AggregateID          string `json:"aggregate_id"`
	ExpectedVersion      int64  `json:"expected_version"`
	GoalID               string `json:"goal_id,omitempty"`
	Text                 string `json:"text"`
	Source               string `json:"source"`
	PreviousRevisionID   string `json:"previous_revision_id,omitempty"`
}

type TutoringSessionRequest struct {
	OperationID          string `json:"operation_id"`
	PayloadSchemaVersion int    `json:"payload_schema_version"`
	AggregateType        string `json:"aggregate_type"`
	AggregateID          string `json:"aggregate_id"`
	ExpectedVersion      int64  `json:"expected_version"`
	GoalRevisionID       string `json:"goal_revision_id"`
}

type TutoringProposalRequest struct {
	RequestID           string         `json:"request_id"`
	ProposalType        string         `json:"proposal_type"`
	AggregateType       string         `json:"aggregate_type"`
	AggregateID         string         `json:"aggregate_id"`
	AggregateVersion    int64          `json:"aggregate_version"`
	GoalRevisionID      string         `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string         `json:"route_revision_id,omitempty"`
	RouteStepID         string         `json:"route_step_id,omitempty"`
	FocusNodeRevisionID string         `json:"focus_node_revision_id,omitempty"`
	ActivityID          string         `json:"activity_id,omitempty"`
	AttemptID           string         `json:"attempt_id,omitempty"`
	FreeQuestionID      string         `json:"free_question_id,omitempty"`
	FreeAnswerID        string         `json:"free_answer_id,omitempty"`
	FocusFrameID        string         `json:"focus_frame_id,omitempty"`
	TutoringState       string         `json:"tutoring_state,omitempty"`
	KnowledgeRevisionID string         `json:"knowledge_revision_id"`
	NodeRevisionIDs     []string       `json:"node_revision_ids"`
	Input               map[string]any `json:"input"`
}

type ProposalContext struct {
	SchemaVersion string                   `json:"schema_version"`
	WorkItem      SessionWorkItem          `json:"work_item"`
	Retrieval     ProposalContextRetrieval `json:"retrieval"`
}

type ProposalContextRetrieval struct {
	KnowledgeRevisionID string                     `json:"knowledge_revision_id"`
	Hits                []ProposalContextReference `json:"hits"`
}

type ProposalContextReference struct {
	KnowledgeRevisionID string              `json:"knowledge_revision_id"`
	DocumentRevisionID  string              `json:"document_revision_id"`
	NodeID              string              `json:"node_id"`
	NodeRevisionID      string              `json:"node_revision_id"`
	Range               LearningSourceRange `json:"range"`
	Slice               string              `json:"slice"`
	SliceSHA256         string              `json:"slice_sha256"`
}

type TutoringAction interface{ tutoringAction() }

type SessionOperation struct {
	OperationID          string `json:"operation_id"`
	PayloadSchemaVersion int    `json:"payload_schema_version"`
	AggregateType        string `json:"aggregate_type"`
	AggregateID          string `json:"aggregate_id"`
	ExpectedVersion      int64  `json:"expected_version"`
}

type ActionNoFieldsRequest struct {
	SessionOperation
	Action string `json:"action"`
}

func (ActionNoFieldsRequest) tutoringAction() {}

type ActionProposalRequest struct {
	SessionOperation
	Action     string `json:"action"`
	ProposalID string `json:"proposal_id"`
}

func (ActionProposalRequest) tutoringAction() {}

type ActionAssessmentRequest struct {
	SessionOperation
	Action     string `json:"action"`
	ProposalID string `json:"proposal_id,omitempty"`
}

func (ActionAssessmentRequest) tutoringAction() {}

type ActionAttemptRequest struct {
	SessionOperation
	Action string `json:"action"`
	Answer string `json:"answer"`
	Help   string `json:"help"`
}

func (ActionAttemptRequest) tutoringAction() {}

type ActionQuestionRequest struct {
	SessionOperation
	Action   string `json:"action"`
	Question string `json:"question"`
}

func (ActionQuestionRequest) tutoringAction() {}

type KnowledgeReferenceInput struct {
	KnowledgeRevisionID string               `json:"knowledge_revision_id,omitempty"`
	NodeID              string               `json:"node_id,omitempty"`
	NodeRevisionID      string               `json:"node_revision_id"`
	DocumentRevisionID  string               `json:"document_revision_id,omitempty"`
	Range               *LearningSourceRange `json:"range,omitempty"`
	Slice               string               `json:"slice,omitempty"`
	SliceSHA256         string               `json:"slice_sha256,omitempty"`
}

type ActionDirectExposureRequest struct {
	SessionOperation
	Action              string                    `json:"action"`
	ExposureKind        string                    `json:"exposure_kind"`
	ExposureText        string                    `json:"exposure_text"`
	KnowledgeReferences []KnowledgeReferenceInput `json:"knowledge_references,omitempty"`
}

func (ActionDirectExposureRequest) tutoringAction() {}

type ActionProposalExposureRequest struct {
	SessionOperation
	Action       string `json:"action"`
	ProposalID   string `json:"proposal_id"`
	ExposureKind string `json:"exposure_kind,omitempty"`
}

func (ActionProposalExposureRequest) tutoringAction() {}

type ActionAttachedQuizRequest struct {
	SessionOperation
	Action     string `json:"action"`
	ProposalID string `json:"proposal_id"`
	Question   string `json:"question"`
	Answer     string `json:"answer"`
}

func (ActionAttachedQuizRequest) tutoringAction() {}

type ActionSwitchGoalRequest struct {
	SessionOperation
	Action         string `json:"action"`
	GoalRevisionID string `json:"goal_revision_id"`
}

func (ActionSwitchGoalRequest) tutoringAction() {}

type AssessmentDecisionRequest interface{ assessmentDecision() }

type AssessmentConfirmRequest struct {
	SessionOperation
	Kind                       string `json:"kind"`
	ExpectedDispositionVersion int64  `json:"expected_disposition_version"`
}

func (AssessmentConfirmRequest) assessmentDecision() {}

type AssessmentOverrideRequest struct {
	SessionOperation
	Kind                       string           `json:"kind"`
	ExpectedDispositionVersion int64            `json:"expected_disposition_version"`
	Reason                     string           `json:"reason"`
	Items                      []AssessmentItem `json:"items"`
}

func (AssessmentOverrideRequest) assessmentDecision() {}

type AssessmentVoidRequest struct {
	SessionOperation
	Kind                       string `json:"kind"`
	ExpectedDispositionVersion int64  `json:"expected_disposition_version"`
	Reason                     string `json:"reason"`
}

func (AssessmentVoidRequest) assessmentDecision() {}

type GoalOperationResult struct {
	Status                 string       `json:"status"`
	Replayed               bool         `json:"replayed"`
	Archived               bool         `json:"archived"`
	AggregateType          string       `json:"aggregate_type"`
	AggregateID            string       `json:"aggregate_id"`
	AggregateVersion       int64        `json:"aggregate_version"`
	FirstEventSeq          int64        `json:"first_event_seq"`
	LastEventSeq           int64        `json:"last_event_seq"`
	ProjectionAsOfEventSeq int64        `json:"projection_as_of_event_seq"`
	TutoringState          string       `json:"tutoring_state,omitempty"`
	EvidenceDisposition    string       `json:"evidence_disposition,omitempty"`
	Result                 GoalRevision `json:"result"`
}

type SessionOperationResult struct {
	Status                 string          `json:"status"`
	Replayed               bool            `json:"replayed"`
	Archived               bool            `json:"archived"`
	AggregateType          string          `json:"aggregate_type"`
	AggregateID            string          `json:"aggregate_id"`
	AggregateVersion       int64           `json:"aggregate_version"`
	FirstEventSeq          int64           `json:"first_event_seq"`
	LastEventSeq           int64           `json:"last_event_seq"`
	ProjectionAsOfEventSeq int64           `json:"projection_as_of_event_seq"`
	TutoringState          string          `json:"tutoring_state,omitempty"`
	EvidenceDisposition    string          `json:"evidence_disposition,omitempty"`
	Result                 TutoringSession `json:"result"`
}

type AssessmentDecisionOperationResult struct {
	Status                 string             `json:"status"`
	Replayed               bool               `json:"replayed"`
	Archived               bool               `json:"archived"`
	AggregateType          string             `json:"aggregate_type"`
	AggregateID            string             `json:"aggregate_id"`
	AggregateVersion       int64              `json:"aggregate_version"`
	FirstEventSeq          int64              `json:"first_event_seq"`
	LastEventSeq           int64              `json:"last_event_seq"`
	ProjectionAsOfEventSeq int64              `json:"projection_as_of_event_seq"`
	TutoringState          string             `json:"tutoring_state,omitempty"`
	EvidenceDisposition    string             `json:"evidence_disposition,omitempty"`
	Result                 AssessmentDecision `json:"result"`
}

type TutoringProposal struct {
	ProposalID          string                  `json:"proposal_id"`
	SchemaVersion       int                     `json:"schema_version"`
	InputHash           string                  `json:"input_hash"`
	ProposalType        string                  `json:"proposal_type"`
	AggregateType       string                  `json:"aggregate_type"`
	AggregateID         string                  `json:"aggregate_id"`
	AggregateVersion    int64                   `json:"aggregate_version"`
	GoalRevisionID      string                  `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string                  `json:"route_revision_id,omitempty"`
	ActivityID          string                  `json:"activity_id,omitempty"`
	AttemptID           string                  `json:"attempt_id,omitempty"`
	KnowledgeRevisionID string                  `json:"knowledge_revision_id"`
	FrozenRequest       TutoringProposalRequest `json:"frozen_request"`
	Route               []RouteProposalStep     `json:"route,omitempty"`
	Activity            *ActivityProposal       `json:"activity,omitempty"`
	Assessment          *AssessmentArtifact     `json:"assessment,omitempty"`
	Text                *TextProposal           `json:"text,omitempty"`
	ModelID             string                  `json:"model_id"`
	ModelParameters     map[string]any          `json:"model_parameters"`
	PromptRevision      string                  `json:"prompt_revision"`
	AttemptCategories   []string                `json:"attempt_categories"`
	CreatedAt           time.Time               `json:"created_at"`
}

type ProjectionMetadata struct {
	AsOfEventSeq            int64    `json:"as_of_event_seq"`
	ProjectionVersion       string   `json:"projection_version"`
	MasteryReducerVersion   string   `json:"mastery_reducer_version"`
	AssessmentPolicyVersion string   `json:"assessment_policy_version"`
	ReviewPolicyVersion     string   `json:"review_policy_version"`
	KnowledgeRevisionID     string   `json:"knowledge_revision_id,omitempty"`
	Generation              string   `json:"generation"`
	Rebuilding              bool     `json:"rebuilding"`
	Degraded                bool     `json:"degraded"`
	Incomplete              bool     `json:"incomplete"`
	ReasonCodes             []string `json:"reason_codes"`
}

type LearningSourceRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type KnowledgeReference struct {
	KnowledgeRevisionID string              `json:"knowledge_revision_id"`
	NodeID              string              `json:"node_id"`
	NodeRevisionID      string              `json:"node_revision_id"`
	DocumentRevisionID  string              `json:"document_revision_id,omitempty"`
	Range               LearningSourceRange `json:"range"`
	Slice               string              `json:"slice,omitempty"`
	SliceSHA256         string              `json:"slice_sha256"`
}

type GoalRevision struct {
	GoalRevisionID     string    `json:"goal_revision_id"`
	GoalID             string    `json:"goal_id"`
	Revision           int64     `json:"revision"`
	Text               string    `json:"text"`
	Source             string    `json:"source"`
	ActorDeviceID      string    `json:"actor_device_id"`
	CreatedAt          time.Time `json:"created_at"`
	PreviousRevisionID string    `json:"previous_revision_id,omitempty"`
}

type FocusContext struct {
	GoalRevisionID      string `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string `json:"route_revision_id,omitempty"`
	RouteStepID         string `json:"route_step_id,omitempty"`
	KnowledgeRevisionID string `json:"knowledge_revision_id,omitempty"`
	FocusNodeRevisionID string `json:"focus_node_revision_id,omitempty"`
	ActivityID          string `json:"activity_id,omitempty"`
	AttemptID           string `json:"attempt_id,omitempty"`
}

type FocusFrame struct {
	FocusFrameID          string       `json:"focus_frame_id"`
	SessionID             string       `json:"session_id"`
	SavedState            string       `json:"saved_state"`
	Context               FocusContext `json:"context"`
	SavedAggregateVersion int64        `json:"saved_aggregate_version"`
	CreatedEventSeq       int64        `json:"created_event_seq"`
	Invalidated           bool         `json:"invalidated"`
	InvalidationReason    string       `json:"invalidation_reason,omitempty"`
}

type TutoringSession struct {
	SessionID        string       `json:"session_id"`
	State            string       `json:"state"`
	AggregateVersion int64        `json:"aggregate_version"`
	Focus            FocusContext `json:"focus"`
	ActiveFocusFrame *FocusFrame  `json:"active_focus_frame,omitempty"`
	AttachedQuiz     bool         `json:"attached_quiz"`
	CompletedRoute   bool         `json:"completed_route"`
}

type ActiveTimeEstimate struct {
	DurationSeconds  int64      `json:"duration_seconds"`
	Estimated        bool       `json:"estimated"`
	AlgorithmVersion string     `json:"algorithm_version"`
	SampleCount      int64      `json:"sample_count"`
	FirstReceivedAt  *time.Time `json:"first_received_at,omitempty"`
	LastReceivedAt   *time.Time `json:"last_received_at,omitempty"`
}

type SessionView struct {
	Metadata            ProjectionMetadata `json:"metadata"`
	Session             TutoringSession    `json:"session"`
	EstimatedActiveTime ActiveTimeEstimate `json:"estimated_active_time"`
	WorkItem            *SessionWorkItem   `json:"work_item"`
}

type SessionWorkItem struct {
	AllowedActions             []string            `json:"allowed_actions"`
	AllowedAssessmentDecisions []string            `json:"allowed_assessment_decisions"`
	GoalRevision               *GoalRevision       `json:"goal_revision,omitempty"`
	RouteRevision              *RouteRevision      `json:"route_revision,omitempty"`
	Activity                   *Activity           `json:"activity,omitempty"`
	Attempt                    *Attempt            `json:"attempt,omitempty"`
	Assessment                 *AssessmentArtifact `json:"assessment,omitempty"`
	AssessmentDecision         *AssessmentDecision `json:"assessment_decision,omitempty"`
	FreeQuestion               *FreeQuestion       `json:"free_question,omitempty"`
	FreeAnswer                 *FreeAnswer         `json:"free_answer,omitempty"`
}

type Attempt struct {
	AttemptID        string     `json:"attempt_id"`
	SessionID        string     `json:"session_id"`
	ActivityID       string     `json:"activity_id"`
	ActivityRevision int64      `json:"activity_revision"`
	AnswerPayloadID  string     `json:"answer_payload_id"`
	Answer           string     `json:"answer"`
	AnswerSHA256     string     `json:"answer_sha256"`
	Help             string     `json:"help"`
	ActorDeviceID    string     `json:"actor_device_id"`
	OccurredAt       *time.Time `json:"occurred_at,omitempty"`
	ReceivedAt       time.Time  `json:"received_at"`
}

type FrozenReference struct {
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	DocumentRevisionID  string `json:"document_revision_id"`
	Start               int    `json:"start"`
	End                 int    `json:"end"`
	Slice               string `json:"slice"`
	SliceSHA256         string `json:"slice_sha256"`
}

type FreeQuestion struct {
	FreeQuestionID          string            `json:"free_question_id"`
	SessionID               string            `json:"session_id"`
	FocusFrameID            string            `json:"focus_frame_id"`
	SessionAggregateVersion int64             `json:"session_aggregate_version"`
	Text                    string            `json:"text"`
	KnowledgeRevisionID     string            `json:"knowledge_revision_id"`
	References              []FrozenReference `json:"references"`
	ActorDeviceID           string            `json:"actor_device_id"`
	OccurredAt              *time.Time        `json:"occurred_at,omitempty"`
	ReceivedAt              time.Time         `json:"received_at"`
}

type FreeAnswer struct {
	FreeAnswerID        string            `json:"free_answer_id"`
	SessionID           string            `json:"session_id"`
	FocusFrameID        string            `json:"focus_frame_id"`
	FreeQuestionID      string            `json:"free_question_id"`
	Text                string            `json:"text"`
	KnowledgeRevisionID string            `json:"knowledge_revision_id"`
	References          []FrozenReference `json:"references"`
	SourceProposalID    string            `json:"source_proposal_id,omitempty"`
	ReceivedAt          time.Time         `json:"received_at"`
}

type TimelineItem struct {
	EventSeq          int64      `json:"event_seq"`
	EventID           string     `json:"event_id"`
	EventType         string     `json:"event_type"`
	AggregateID       string     `json:"aggregate_id"`
	ReceivedAt        time.Time  `json:"received_at"`
	OccurredAt        *time.Time `json:"occurred_at,omitempty"`
	OccurredAtTrusted bool       `json:"occurred_at_trusted"`
}

type TimelinePage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []TimelineItem     `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type RouteStep struct {
	RouteStepID         string `json:"route_step_id"`
	Ordinal             int    `json:"ordinal"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	TeachingIntent      string `json:"teaching_intent"`
	CompletionCondition string `json:"completion_condition"`
}

type RouteRevision struct {
	RouteRevisionID     string      `json:"route_revision_id"`
	RouteID             string      `json:"route_id"`
	Revision            int64       `json:"revision"`
	GoalRevisionID      string      `json:"goal_revision_id"`
	KnowledgeRevisionID string      `json:"knowledge_revision_id"`
	RoutePolicyVersion  string      `json:"route_policy_version"`
	SourceProposalID    string      `json:"source_proposal_id"`
	Steps               []RouteStep `json:"steps"`
	CreatedAt           time.Time   `json:"created_at"`
}

type RouteProjection struct {
	Route    RouteRevision `json:"route"`
	EventSeq int64         `json:"event_seq"`
	Current  bool          `json:"current"`
}

type RoutesPage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []RouteProjection  `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type MisconceptionCandidate struct {
	RubricItemID string `json:"rubric_item_id"`
	Text         string `json:"text"`
}

type RubricOutcome struct {
	RubricItemID string `json:"rubric_item_id"`
	Conclusion   string `json:"conclusion"`
}

type AcceptedEvidence struct {
	EvidenceID              string                   `json:"evidence_id"`
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
	Kind                    string                   `json:"kind"`
	ActivityType            string                   `json:"activity_type"`
	Outcome                 string                   `json:"outcome"`
	Help                    string                   `json:"help"`
	ReceivedAt              time.Time                `json:"received_at"`
	AcceptancePolicyVersion string                   `json:"acceptance_policy_version"`
	ReducerPolicyVersion    string                   `json:"reducer_policy_version"`
	ReviewPolicyVersion     string                   `json:"review_policy_version"`
	Misconceptions          []MisconceptionCandidate `json:"misconceptions,omitempty"`
	RubricOutcomes          []RubricOutcome          `json:"rubric_outcomes,omitempty"`
}

type EvidencePage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []AcceptedEvidence `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type ReviewSchedule struct {
	NodeRevisionID string    `json:"node_revision_id"`
	Step           int       `json:"step"`
	DueAt          time.Time `json:"due_at"`
	Intervals      []int64   `json:"intervals"`
	PolicyVersion  string    `json:"policy_version"`
}

type ReviewsPage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []ReviewSchedule   `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type KindCounts struct {
	PracticeRecall int64 `json:"practice_recall,omitempty"`
	ReviewRecall   int64 `json:"review_recall,omitempty"`
}

type OutcomeCounts struct {
	Pass    int64 `json:"pass,omitempty"`
	Partial int64 `json:"partial,omitempty"`
	Fail    int64 `json:"fail,omitempty"`
}

type HelpCounts struct {
	None           int64 `json:"none,omitempty"`
	Hint           int64 `json:"hint,omitempty"`
	Scaffold       int64 `json:"scaffold,omitempty"`
	AnswerRevealed int64 `json:"answer_revealed,omitempty"`
}

type MasteryProjection struct {
	NodeRevisionID     string        `json:"node_revision_id"`
	State              string        `json:"state"`
	BaselineState      string        `json:"baseline_state"`
	ValidEvidenceCount int64         `json:"valid_evidence_count"`
	Kinds              KindCounts    `json:"kinds"`
	Outcomes           OutcomeCounts `json:"outcomes"`
	Help               HelpCounts    `json:"help"`
	LastEvidenceAt     *time.Time    `json:"last_evidence_at,omitempty"`
	PendingAssessments int64         `json:"pending_assessments"`
	UncertaintyReasons []string      `json:"uncertainty_reasons"`
	ReducerVersion     string        `json:"reducer_version"`
}

type MisconceptionHypothesis struct {
	MisconceptionID    string   `json:"misconception_id"`
	Revision           int64    `json:"revision"`
	NodeRevisionID     string   `json:"node_revision_id"`
	RubricItemID       string   `json:"rubric_item_id"`
	CandidateHash      string   `json:"candidate_hash"`
	Candidate          string   `json:"candidate"`
	Status             string   `json:"status"`
	SourceEvidenceIDs  []string `json:"source_evidence_ids"`
	CounterEvidenceIDs []string `json:"counter_evidence_ids"`
	CausedByEvidenceID string   `json:"caused_by_evidence_id"`
}

type NodeReduction struct {
	Mastery        MasteryProjection         `json:"mastery"`
	Review         *ReviewSchedule           `json:"review,omitempty"`
	Misconceptions []MisconceptionHypothesis `json:"misconceptions"`
}

type NodeView struct {
	Metadata ProjectionMetadata `json:"metadata"`
	Node     NodeReduction      `json:"node"`
	Evidence []AcceptedEvidence `json:"evidence"`
}

type AssessmentItem struct {
	RubricItemID           string              `json:"rubric_item_id"`
	Conclusion             string              `json:"conclusion"`
	AnswerQuote            string              `json:"answer_quote"`
	AnswerRange            LearningSourceRange `json:"answer_range"`
	AnswerQuoteSHA256      string              `json:"answer_quote_sha256"`
	KnowledgeReferenceID   string              `json:"knowledge_reference_id"`
	KnowledgeQuote         string              `json:"knowledge_quote"`
	KnowledgeRange         LearningSourceRange `json:"knowledge_range"`
	KnowledgeQuoteSHA256   string              `json:"knowledge_quote_sha256"`
	MisconceptionCandidate string              `json:"misconception_candidate,omitempty"`
}

type AssessmentArtifact struct {
	AssessmentID      string           `json:"assessment_id"`
	SessionID         string           `json:"session_id"`
	AttemptID         string           `json:"attempt_id"`
	ActivityID        string           `json:"activity_id"`
	ActivityRevision  int64            `json:"activity_revision"`
	Items             []AssessmentItem `json:"items"`
	RubricComplete    bool             `json:"rubric_complete"`
	Confidence        int              `json:"confidence"`
	RiskFlags         []string         `json:"risk_flags"`
	ModelID           string           `json:"model_id"`
	ModelParameters   map[string]any   `json:"model_parameters"`
	PromptRevision    string           `json:"prompt_revision"`
	ProposalInputHash string           `json:"proposal_input_hash"`
	Attempts          int              `json:"attempts"`
	AttemptCategories []string         `json:"attempt_categories"`
	CreatedAt         time.Time        `json:"created_at"`
}

type AssessmentDecision struct {
	DecisionID         string           `json:"decision_id"`
	AssessmentID       string           `json:"assessment_id"`
	Version            int64            `json:"version"`
	Disposition        string           `json:"disposition"`
	Items              []AssessmentItem `json:"items"`
	Reason             string           `json:"reason,omitempty"`
	ActorDeviceID      string           `json:"actor_device_id"`
	CreatedAt          time.Time        `json:"created_at"`
	ReplacesDecisionID string           `json:"replaces_decision_id,omitempty"`
	ProducedEvidenceID string           `json:"produced_evidence_id,omitempty"`
}

type RouteProposalStep struct {
	NodeRevisionID      string `json:"node_revision_id"`
	TeachingIntent      string `json:"teaching_intent"`
	CompletionCondition string `json:"completion_condition"`
}

type RubricItem struct {
	RubricItemID         string   `json:"rubric_item_id"`
	Criterion            string   `json:"criterion"`
	RequiredReferenceIDs []string `json:"required_reference_ids,omitempty"`
}

type ObjectiveRule struct {
	AcceptedAnswers []string `json:"accepted_answers"`
	CaseSensitive   bool     `json:"case_sensitive"`
	TrimSpace       bool     `json:"trim_space"`
}

type Rubric struct {
	RubricRevision string         `json:"rubric_revision"`
	Items          []RubricItem   `json:"items"`
	ObjectiveRule  *ObjectiveRule `json:"objective_rule,omitempty"`
}

type Activity struct {
	ActivityID              string               `json:"activity_id"`
	Revision                int64                `json:"revision"`
	SessionID               string               `json:"session_id"`
	GoalRevisionID          string               `json:"goal_revision_id"`
	RouteRevisionID         string               `json:"route_revision_id"`
	RouteStepID             string               `json:"route_step_id"`
	KnowledgeRevisionID     string               `json:"knowledge_revision_id"`
	TargetNodeID            string               `json:"target_node_id"`
	TargetNodeRevisionID    string               `json:"target_node_revision_id"`
	KnowledgeReferences     []KnowledgeReference `json:"knowledge_references"`
	Prompt                  string               `json:"prompt"`
	Type                    string               `json:"type"`
	Rubric                  Rubric               `json:"rubric"`
	Difficulty              int                  `json:"difficulty"`
	AllowedHelp             []string             `json:"allowed_help"`
	ActivityPolicyVersion   string               `json:"activity_policy_version"`
	AssessmentPolicyVersion string               `json:"assessment_policy_version"`
	ReviewPolicyVersion     string               `json:"review_policy_version"`
	SourceProposalID        string               `json:"source_proposal_id,omitempty"`
	AttachedFreeQuestionID  string               `json:"attached_free_question_id,omitempty"`
	AttachedFreeAnswerID    string               `json:"attached_free_answer_id,omitempty"`
	Review                  bool                 `json:"review"`
	CreatedAt               time.Time            `json:"created_at"`
}

type ActivityProposal struct {
	Prompt              string               `json:"prompt"`
	Type                string               `json:"type"`
	Rubric              Rubric               `json:"rubric"`
	Difficulty          int                  `json:"difficulty"`
	AllowedHelp         []string             `json:"allowed_help"`
	KnowledgeReferences []KnowledgeReference `json:"knowledge_references"`
}

type TextProposal struct {
	Text                string               `json:"text"`
	KnowledgeReferences []KnowledgeReference `json:"knowledge_references"`
}

type ProjectionStatus struct {
	Metadata                ProjectionMetadata `json:"metadata"`
	CommittedEventHighWater int64              `json:"committed_event_high_water"`
	Fingerprint             string             `json:"fingerprint"`
	ActiveGenerationID      string             `json:"active_generation_id"`
	RebuildingGenerationID  string             `json:"rebuilding_generation_id,omitempty"`
}
