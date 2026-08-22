package learning

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

type EventType string

const (
	EventGoalRevisionCreated            EventType = "GoalRevisionCreated"
	EventRouteRevisionCreated           EventType = "RouteRevisionCreated"
	EventLearningSessionStarted         EventType = "LearningSessionStarted"
	EventTutoringStateChanged           EventType = "TutoringStateChanged"
	EventActivityIssued                 EventType = "ActivityIssued"
	EventActivityPresented              EventType = "ActivityPresented"
	EventAttemptSubmitted               EventType = "AttemptSubmitted"
	EventAssessmentRecorded             EventType = "AssessmentRecorded"
	EventAssessmentMarkedProvisional    EventType = "AssessmentMarkedProvisional"
	EventAssessmentAccepted             EventType = "AssessmentAccepted"
	EventAssessmentOverridden           EventType = "AssessmentOverridden"
	EventAssessmentVoided               EventType = "AssessmentVoided"
	EventEvidenceAccepted               EventType = "EvidenceAccepted"
	EventEvidenceInvalidated            EventType = "EvidenceInvalidated"
	EventExposureRecorded               EventType = "ExposureRecorded"
	EventReviewPresented                EventType = "ReviewPresented"
	EventFocusSuspended                 EventType = "FocusSuspended"
	EventFreeQuestionAsked              EventType = "FreeQuestionAsked"
	EventFreeAnswerRecorded             EventType = "FreeAnswerRecorded"
	EventFocusResumed                   EventType = "FocusResumed"
	EventRouteAdvanced                  EventType = "RouteAdvanced"
	EventLearningCompleted              EventType = "LearningCompleted"
	EventMisconceptionHypothesisRevised EventType = "MisconceptionHypothesisRevised"
	EventRedacted                       EventType = "EventRedacted"
)

type LearningEvent struct {
	EventSequence    int64           `json:"event_seq"`
	ID               string          `json:"event_id"`
	Type             EventType       `json:"event_type"`
	SchemaVersion    int             `json:"event_schema_version"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      string          `json:"aggregate_id"`
	AggregateVersion int64           `json:"aggregate_version"`
	DeviceID         string          `json:"device_id"`
	OperationID      string          `json:"operation_id"`
	OperationOrdinal int             `json:"operation_ordinal"`
	ReceivedAt       time.Time       `json:"received_at"`
	OccurredAt       *time.Time      `json:"occurred_at,omitempty"`
	PayloadID        string          `json:"payload_id"`
	PayloadHash      string          `json:"payload_hash"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	Redacted         bool            `json:"-"`
}

type Decoder func(json.RawMessage) (json.RawMessage, error)

type EventRegistry struct{ decoders map[int]Decoder }

func NewEventRegistry() *EventRegistry {
	registry := &EventRegistry{decoders: map[int]Decoder{}}
	registry.Register(1, func(payload json.RawMessage) (json.RawMessage, error) {
		if !json.Valid(payload) {
			return nil, fmt.Errorf("invalid event payload")
		}
		return append(json.RawMessage(nil), payload...), nil
	})
	return registry
}
func (r *EventRegistry) Register(version int, decoder Decoder) {
	if version > 0 && decoder != nil {
		r.decoders[version] = decoder
	}
}
func (r *EventRegistry) Decode(event LearningEvent) (LearningEvent, error) {
	decoder := r.decoders[event.SchemaVersion]
	if decoder == nil {
		return LearningEvent{}, &Error{Code: CodeUnsupportedEventSchema, Reason: fmt.Sprintf("version_%d", event.SchemaVersion)}
	}
	payload, err := decoder(event.Payload)
	if err != nil {
		return LearningEvent{}, &Error{Code: CodeProjectionUnavailable, Cause: err}
	}
	event.Payload = payload
	return event, nil
}

type ProjectionMetadata struct {
	AsOfEventSequence     int64    `json:"as_of_event_seq"`
	ProjectionVersion     string   `json:"projection_version"`
	MasteryReducerVersion string   `json:"mastery_reducer_version"`
	AssessmentPolicy      string   `json:"assessment_policy_version"`
	ReviewPolicy          string   `json:"review_policy_version"`
	KnowledgeRevisionID   string   `json:"knowledge_revision_id,omitempty"`
	GenerationID          string   `json:"generation"`
	Rebuilding            bool     `json:"rebuilding"`
	Degraded              bool     `json:"degraded"`
	Incomplete            bool     `json:"incomplete"`
	ReasonCodes           []string `json:"reason_codes"`
}

type TimelineItem struct {
	EventSequence     int64      `json:"event_seq"`
	EventID           string     `json:"event_id"`
	Type              EventType  `json:"event_type"`
	AggregateID       string     `json:"aggregate_id"`
	ReceivedAt        time.Time  `json:"received_at"`
	OccurredAt        *time.Time `json:"occurred_at,omitempty"`
	OccurredAtTrusted bool       `json:"occurred_at_trusted"`
}
type RouteProjection struct {
	Route         RouteRevision `json:"route"`
	EventSequence int64         `json:"event_seq"`
	Current       bool          `json:"current"`
}
type SessionProjection struct {
	Session              tutoring.Session `json:"session"`
	UpdatedEventSequence int64            `json:"updated_event_seq"`
}

type AssessmentProjectionEvent struct {
	AssessmentID   string             `json:"assessment_id"`
	NodeRevisionID string             `json:"node_revision_id"`
	Reasons        []string           `json:"reasons,omitempty"`
	Decision       AssessmentDecision `json:"decision"`
}

type PendingAssessment struct {
	AssessmentID   string   `json:"assessment_id"`
	NodeRevisionID string   `json:"node_revision_id"`
	Reasons        []string `json:"reasons"`
}

type Projection struct {
	Metadata       ProjectionMetadata            `json:"metadata"`
	Timeline       []TimelineItem                `json:"timeline"`
	Routes         []RouteProjection             `json:"routes"`
	Sessions       map[string]SessionProjection  `json:"sessions"`
	Stats          map[string]ActiveTimeEstimate `json:"stats"`
	Evidence       map[string]AcceptedEvidence   `json:"evidence"`
	Invalidated    map[string]bool               `json:"invalidated_evidence"`
	Pending        map[string]PendingAssessment  `json:"pending_assessments"`
	Nodes          map[string]NodeReduction      `json:"nodes"`
	RedactedEvents map[string]bool               `json:"redacted_events"`
	Exposures      []Exposure                    `json:"exposures"`
}

func EmptyProjection(generationID string) Projection {
	return Projection{Metadata: ProjectionMetadata{ProjectionVersion: ProjectionVersion, MasteryReducerVersion: MasteryReducerVersion, AssessmentPolicy: AssessmentPolicyVersion, ReviewPolicy: ReviewPolicyVersion, GenerationID: generationID}, Sessions: map[string]SessionProjection{}, Stats: map[string]ActiveTimeEstimate{}, Evidence: map[string]AcceptedEvidence{}, Invalidated: map[string]bool{}, Pending: map[string]PendingAssessment{}, Nodes: map[string]NodeReduction{}, RedactedEvents: map[string]bool{}}
}

func ApplyEvent(projection *Projection, registry *EventRegistry, event LearningEvent) error {
	if event.Redacted {
		if event.EventSequence <= projection.Metadata.AsOfEventSequence {
			return fmt.Errorf("event sequence is not strictly increasing")
		}
		projection.Metadata.AsOfEventSequence = event.EventSequence
		projection.RedactedEvents[event.ID] = true
		return nil
	}
	decoded, err := registry.Decode(event)
	if err != nil {
		projection.Metadata.Incomplete = true
		projection.Metadata.ReasonCodes = appendUnique(projection.Metadata.ReasonCodes, ErrorCode(err))
		return err
	}
	if decoded.EventSequence <= projection.Metadata.AsOfEventSequence {
		return fmt.Errorf("event sequence is not strictly increasing")
	}
	projection.Metadata.AsOfEventSequence = decoded.EventSequence
	if decoded.Type == EventRedacted && decoded.AggregateType == "privacy" {
		var payload struct {
			ErasureID       string `json:"erasure_id"`
			Generation      int64  `json:"generation"`
			RedactedThrough int64  `json:"redacted_through"`
			PolicyVersion   string `json:"policy_version"`
			ReasonCode      string `json:"reason_code"`
		}
		if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
			return err
		}
		if payload.ErasureID == "" || payload.Generation < 2 || payload.RedactedThrough < 0 || payload.RedactedThrough >= decoded.EventSequence || payload.PolicyVersion == "" || payload.ReasonCode == "" || decoded.AggregateID != payload.ErasureID {
			return fmt.Errorf("privacy redaction payload is incomplete")
		}
		generationID := projection.Metadata.GenerationID
		*projection = EmptyProjection(generationID)
		projection.Metadata.AsOfEventSequence = decoded.EventSequence
		projection.RedactedEvents[decoded.ID] = true
		projection.Timeline = append(projection.Timeline, TimelineItem{EventSequence: decoded.EventSequence, EventID: decoded.ID, Type: decoded.Type, AggregateID: decoded.AggregateID, ReceivedAt: decoded.ReceivedAt, OccurredAt: decoded.OccurredAt, OccurredAtTrusted: false})
		return nil
	}
	projection.Timeline = append(projection.Timeline, TimelineItem{EventSequence: decoded.EventSequence, EventID: decoded.ID, Type: decoded.Type, AggregateID: decoded.AggregateID, ReceivedAt: decoded.ReceivedAt, OccurredAt: decoded.OccurredAt, OccurredAtTrusted: false})
	switch decoded.Type {
	case EventRouteRevisionCreated:
		var route RouteRevision
		if err := json.Unmarshal(decoded.Payload, &route); err != nil {
			return err
		}
		if route.ID == "" || route.RouteID == "" || route.GoalRevisionID == "" || route.KnowledgeRevisionID == "" || !StableRouteSteps(route.Steps) {
			return fmt.Errorf("route revision payload is incomplete")
		}
		for i := range projection.Routes {
			if projection.Routes[i].Route.RouteID == route.RouteID {
				projection.Routes[i].Current = false
			}
		}
		projection.Routes = append(projection.Routes, RouteProjection{Route: route, EventSequence: decoded.EventSequence, Current: true})
		projection.Metadata.KnowledgeRevisionID = route.KnowledgeRevisionID
	case EventTutoringStateChanged, EventLearningSessionStarted, EventFocusSuspended, EventFocusResumed, EventRouteAdvanced, EventLearningCompleted:
		var session SessionProjection
		if err := json.Unmarshal(decoded.Payload, &session); err != nil {
			return err
		}
		if session.Session.ID == "" || session.Session.State == "" {
			return fmt.Errorf("session projection payload is incomplete")
		}
		session.UpdatedEventSequence = decoded.EventSequence
		projection.Sessions[session.Session.ID] = session
		if _, ok := projection.Stats[session.Session.ID]; !ok {
			projection.Stats[session.Session.ID] = EstimateActiveTime(session.Session.ID, nil)
		}
	case EventEvidenceAccepted:
		var evidence AcceptedEvidence
		if err := json.Unmarshal(decoded.Payload, &evidence); err != nil {
			return err
		}
		projection.Evidence[evidence.ID] = evidence
		recomputeNode(projection, evidence.NodeRevisionID)
	case EventEvidenceInvalidated:
		var payload struct {
			EvidenceID string `json:"evidence_id"`
		}
		if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
			return err
		}
		projection.Invalidated[payload.EvidenceID] = true
		if evidence, ok := projection.Evidence[payload.EvidenceID]; ok {
			recomputeNode(projection, evidence.NodeRevisionID)
		}
	case EventAssessmentMarkedProvisional:
		var payload AssessmentProjectionEvent
		if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
			return err
		}
		if payload.AssessmentID == "" || payload.NodeRevisionID == "" {
			return fmt.Errorf("provisional assessment payload is incomplete")
		}
		projection.Pending[payload.AssessmentID] = PendingAssessment{
			AssessmentID: payload.AssessmentID, NodeRevisionID: payload.NodeRevisionID,
			Reasons: append([]string(nil), payload.Reasons...),
		}
		recomputeNode(projection, payload.NodeRevisionID)
	case EventAssessmentAccepted, EventAssessmentOverridden, EventAssessmentVoided:
		var payload AssessmentProjectionEvent
		if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
			return err
		}
		if payload.AssessmentID == "" || payload.NodeRevisionID == "" {
			return fmt.Errorf("assessment disposition payload is incomplete")
		}
		if pending, ok := projection.Pending[payload.AssessmentID]; ok {
			delete(projection.Pending, payload.AssessmentID)
			recomputeNode(projection, pending.NodeRevisionID)
		}
		recomputeNode(projection, payload.NodeRevisionID)
	case EventExposureRecorded:
		var exposure Exposure
		if err := json.Unmarshal(decoded.Payload, &exposure); err != nil {
			return err
		}
		projection.Exposures = append(projection.Exposures, exposure)
	case EventRedacted:
		var payload struct {
			EventID    string `json:"event_id"`
			EvidenceID string `json:"evidence_id"`
		}
		if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
			return err
		}
		projection.RedactedEvents[payload.EventID] = true
		if payload.EvidenceID != "" {
			projection.Invalidated[payload.EvidenceID] = true
			if evidence, ok := projection.Evidence[payload.EvidenceID]; ok {
				recomputeNode(projection, evidence.NodeRevisionID)
			}
		}
	}
	if isActiveTimeEvent(decoded.Type) && decoded.AggregateType == "session" {
		projection.Stats[decoded.AggregateID] = estimateActiveTimeFromTimeline(decoded.AggregateID, projection.Timeline)
	}
	if decoded.AggregateType == "session" {
		if session, ok := projection.Sessions[decoded.AggregateID]; ok {
			session.Session.AggregateVer = decoded.AggregateVersion
			session.UpdatedEventSequence = decoded.EventSequence
			projection.Sessions[decoded.AggregateID] = session
		}
	}
	return nil
}

func Replay(events []LearningEvent, registry *EventRegistry, generationID string) (Projection, error) {
	projection := EmptyProjection(generationID)
	sorted := append([]LearningEvent(nil), events...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].EventSequence < sorted[j].EventSequence })
	for _, event := range sorted {
		if err := ApplyEvent(&projection, registry, event); err != nil {
			return projection, err
		}
	}
	return projection, nil
}

func ProjectionFingerprint(projection Projection) (string, error) {
	copy := projection
	copy.Metadata.GenerationID = ""
	copy.Metadata.Rebuilding = false
	copy.Metadata.Degraded = false
	copy.Metadata.Incomplete = false
	copy.Metadata.ReasonCodes = nil
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	return SHA256(encoded), nil
}

func recomputeNode(projection *Projection, node string) {
	if node == "" {
		return
	}
	values := make([]AcceptedEvidence, 0)
	for _, evidence := range projection.Evidence {
		if evidence.NodeRevisionID == node {
			values = append(values, evidence)
		}
	}
	assessmentIDs := make([]string, 0)
	for assessmentID, pending := range projection.Pending {
		if pending.NodeRevisionID == node {
			assessmentIDs = append(assessmentIDs, assessmentID)
		}
	}
	sort.Strings(assessmentIDs)
	pending := make([]PendingAssessment, 0, len(assessmentIDs))
	for _, assessmentID := range assessmentIDs {
		pending = append(pending, projection.Pending[assessmentID])
	}
	projection.Nodes[node] = ReduceNode(node, values, projection.Invalidated, pending)
}
func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}
