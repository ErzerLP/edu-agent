package fixture

import (
	"fmt"
	"strings"
	"sync"
)

type RequestKind string

const (
	KindCapabilityProbe RequestKind = "capability_probe"
	KindRoute           RequestKind = "route"
	KindActivity        RequestKind = "activity"
	KindAssessment      RequestKind = "assessment"
	KindFreeAnswer      RequestKind = "free_answer"
	KindExplanation     RequestKind = "explanation"
)

func AllRequestKinds() []RequestKind {
	return []RequestKind{KindCapabilityProbe, KindRoute, KindActivity, KindAssessment, KindFreeAnswer, KindExplanation}
}

func ParseRequestKind(value string) (RequestKind, error) {
	kind := RequestKind(value)
	for _, candidate := range AllRequestKinds() {
		if kind == candidate {
			return kind, nil
		}
	}
	return "", fmt.Errorf("unknown request kind %q", value)
}

type ScenarioKind string

const (
	ScenarioAccepted          ScenarioKind = "accepted"
	ScenarioProvisional       ScenarioKind = "provisional"
	ScenarioRisk              ScenarioKind = "risk"
	ScenarioMalformed         ScenarioKind = "malformed"
	ScenarioMalformedEnvelope ScenarioKind = "malformed_envelope"
	ScenarioSchemaMismatch    ScenarioKind = "schema_mismatch"
	ScenarioRateLimited       ScenarioKind = "rate_limited"
	ScenarioHTTPError         ScenarioKind = "http_error"
	ScenarioTimeout           ScenarioKind = "timeout"
	ScenarioUnauthorized      ScenarioKind = "unauthorized"
	ScenarioNoNativeSchema    ScenarioKind = "no_native_schema"
)

const (
	RiskIncompleteRubric             = "incomplete_rubric"
	RiskInsufficientAnswerEvidence   = "insufficient_answer_evidence"
	RiskInsufficientKnowledgeSupport = "insufficient_knowledge_support"
	RiskConflictingEvidence          = "conflicting_evidence"
	RiskAmbiguousRubric              = "ambiguous_rubric"
	RiskUnsafeContent                = "unsafe_content"
	RiskSchemaRepaired               = "schema_repaired"
	RiskStaleContext                 = "stale_context"
	RiskRetryExhausted               = "retry_exhausted"
)

var knownRiskFlags = map[string]bool{
	RiskIncompleteRubric: true, RiskInsufficientAnswerEvidence: true,
	RiskInsufficientKnowledgeSupport: true, RiskConflictingEvidence: true,
	RiskAmbiguousRubric: true, RiskUnsafeContent: true, RiskSchemaRepaired: true,
	RiskStaleContext: true, RiskRetryExhausted: true,
}

func RiskFlags() []string {
	return []string{
		RiskIncompleteRubric,
		RiskInsufficientAnswerEvidence,
		RiskInsufficientKnowledgeSupport,
		RiskConflictingEvidence,
		RiskAmbiguousRubric,
		RiskUnsafeContent,
		RiskSchemaRepaired,
		RiskStaleContext,
		RiskRetryExhausted,
	}
}

type Scenario struct {
	Kind                 ScenarioKind `json:"kind"`
	RiskFlag             string       `json:"risk_flag,omitempty"`
	StatusCode           int          `json:"status_code,omitempty"`
	DelayMillis          int64        `json:"delay_ms,omitempty"`
	RetryAfter           string       `json:"retry_after,omitempty"`
	ActivityType         string       `json:"activity_type,omitempty"`
	AllowedHelp          []string     `json:"allowed_help,omitempty"`
	AssessmentConclusion string       `json:"assessment_conclusion,omitempty"`
}

func DefaultScenario() Scenario { return Scenario{Kind: ScenarioAccepted} }

func (s Scenario) validate() error {
	switch s.Kind {
	case ScenarioAccepted, ScenarioProvisional, ScenarioMalformed, ScenarioMalformedEnvelope,
		ScenarioSchemaMismatch, ScenarioRateLimited, ScenarioTimeout, ScenarioUnauthorized,
		ScenarioNoNativeSchema:
	case ScenarioRisk:
		if !knownRiskFlags[s.RiskFlag] {
			return fmt.Errorf("unknown assessment risk %q", s.RiskFlag)
		}
	case ScenarioHTTPError:
		if s.StatusCode < 500 || s.StatusCode > 599 {
			return fmt.Errorf("HTTP error status must be 5xx")
		}
	default:
		return fmt.Errorf("unknown scenario kind %q", s.Kind)
	}
	if s.DelayMillis < 0 || s.DelayMillis > 300_000 {
		return fmt.Errorf("delay_ms must be between 0 and 300000")
	}
	if s.ActivityType != "" && s.ActivityType != "open" && s.ActivityType != "objective" {
		return fmt.Errorf("activity_type must be open or objective")
	}
	seenHelp := map[string]bool{}
	for _, help := range s.AllowedHelp {
		if help != "none" && help != "hint" && help != "scaffold" && help != "answer_revealed" {
			return fmt.Errorf("unknown help level %q", help)
		}
		if seenHelp[help] {
			return fmt.Errorf("duplicate help level %q", help)
		}
		seenHelp[help] = true
	}
	if s.AssessmentConclusion != "" && s.AssessmentConclusion != "pass" && s.AssessmentConclusion != "partial" && s.AssessmentConclusion != "fail" && s.AssessmentConclusion != "unassessed" {
		return fmt.Errorf("unknown assessment conclusion %q", s.AssessmentConclusion)
	}
	if strings.ContainsAny(s.RetryAfter, "\r\n") {
		return fmt.Errorf("retry_after contains a line break")
	}
	return nil
}

type AuditEntry struct {
	Sequence       uint64      `json:"sequence"`
	Method         string      `json:"method"`
	Path           string      `json:"path"`
	Model          string      `json:"model,omitempty"`
	RequestKind    RequestKind `json:"request_kind,omitempty"`
	RequestID      string      `json:"request_id,omitempty"`
	ResponseFormat string      `json:"response_format,omitempty"`
	Scenario       Scenario    `json:"scenario"`
	Status         int         `json:"status"`
	RequestBytes   int         `json:"request_bytes"`
	RequestSHA256  string      `json:"request_sha256,omitempty"`
}

type program struct {
	sequence []Scenario
	next     int
}

type Controller struct {
	mu         sync.Mutex
	programs   map[RequestKind]*program
	audit      []AuditEntry
	sequence   uint64
	generation uint64
}

func NewController() *Controller {
	return &Controller{programs: map[RequestKind]*program{}, generation: 1}
}

func (c *Controller) Configure(kind RequestKind, sequence ...Scenario) error {
	if _, err := ParseRequestKind(string(kind)); err != nil {
		return err
	}
	if len(sequence) == 0 {
		return fmt.Errorf("scenario sequence must not be empty")
	}
	cloned := make([]Scenario, len(sequence))
	for index, scenario := range sequence {
		if err := scenario.validate(); err != nil {
			return fmt.Errorf("scenario %d: %w", index, err)
		}
		cloned[index] = cloneScenario(scenario)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.programs[kind] = &program{sequence: cloned}
	return nil
}

func (c *Controller) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.programs = map[RequestKind]*program{}
	c.audit = nil
	c.sequence = 0
}

func (c *Controller) Programs() map[RequestKind][]Scenario {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[RequestKind][]Scenario, len(c.programs))
	for kind, configured := range c.programs {
		result[kind] = cloneScenarios(configured.sequence)
	}
	return result
}

func (c *Controller) Audit() []AuditEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]AuditEntry, len(c.audit))
	copy(result, c.audit)
	for index := range result {
		result[index].Scenario = cloneScenario(result[index].Scenario)
	}
	return result
}

func (c *Controller) beginRequest() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *Controller) nextScenario(generation uint64, kind RequestKind) Scenario {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return DefaultScenario()
	}
	configured := c.programs[kind]
	if configured == nil || len(configured.sequence) == 0 {
		return DefaultScenario()
	}
	index := configured.next
	if index >= len(configured.sequence) {
		index = len(configured.sequence) - 1
	}
	if configured.next < len(configured.sequence)-1 {
		configured.next++
	}
	return cloneScenario(configured.sequence[index])
}

func (c *Controller) record(generation uint64, entry AuditEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	c.sequence++
	entry.Sequence = c.sequence
	entry.Scenario = cloneScenario(entry.Scenario)
	c.audit = append(c.audit, entry)
}

func cloneScenarios(input []Scenario) []Scenario {
	result := make([]Scenario, len(input))
	for index := range input {
		result[index] = cloneScenario(input[index])
	}
	return result
}

func cloneScenario(input Scenario) Scenario {
	input.AllowedHelp = append([]string(nil), input.AllowedHelp...)
	return input
}
