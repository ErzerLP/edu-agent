package learning

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func eventFixture(sequence int64, eventType EventType, payload any) LearningEvent {
	encoded, _ := json.Marshal(payload)
	return LearningEvent{
		EventSequence: sequence, ID: "event-" + string(rune('a'+sequence)), Type: eventType,
		SchemaVersion: 1, AggregateType: "session", AggregateID: "session-1",
		AggregateVersion: sequence, DeviceID: "device-1", OperationID: "operation-1",
		OperationOrdinal: int(sequence - 1), ReceivedAt: time.Date(2026, 8, 20, 10, int(sequence), 0, 0, time.UTC),
		PayloadID: "payload-1", PayloadHash: SHA256(encoded), Payload: encoded,
	}
}

func TestReplayMatchesIncrementalProjectionWithCompensation(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	first := evidence("evidence-1", "activity-1", start, OutcomePass, HelpNone)
	second := evidence("evidence-2", "activity-2", start.Add(24*time.Hour), OutcomePass, HelpNone)
	route := RouteRevision{ID: "route-rev-1", RouteID: "route-1", Revision: 1, GoalRevisionID: "goal-rev-1", KnowledgeRevisionID: "knowledge-1", PolicyVersion: RoutePolicyVersion, Steps: []RouteStep{{ID: "step-1", Ordinal: 0, NodeID: "node-1", NodeRevisionID: "node-rev-1", TeachingIntent: "teach", CompletionCondition: "pass"}}}
	events := []LearningEvent{
		eventFixture(1, EventRouteRevisionCreated, route),
		eventFixture(2, EventLearningSessionStarted, SessionProjection{Session: tutoring.Session{ID: "session-1", State: tutoring.StateGoalReady}}),
		eventFixture(3, EventEvidenceAccepted, first),
		eventFixture(4, EventEvidenceAccepted, second),
		eventFixture(5, EventAssessmentMarkedProvisional, map[string]any{"assessment_id": "assessment-1", "node_revision_id": "node-1", "reasons": []string{"low_confidence"}}),
		eventFixture(6, EventAssessmentOverridden, AssessmentProjectionEvent{AssessmentID: "assessment-1", NodeRevisionID: "node-1"}),
		eventFixture(7, EventEvidenceInvalidated, map[string]any{"evidence_id": "evidence-2"}),
		eventFixture(8, EventRedacted, map[string]any{"event_id": "event-d", "evidence_id": "evidence-1"}),
	}
	registry := NewEventRegistry()
	incremental := EmptyProjection("generation-9")
	for _, event := range events {
		if err := ApplyEvent(&incremental, registry, event); err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := Replay([]LearningEvent{events[5], events[0], events[7], events[2], events[1], events[4], events[3], events[6]}, registry, "generation-9")
	if err != nil {
		t.Fatal(err)
	}
	left, err := ProjectionFingerprint(incremental)
	if err != nil {
		t.Fatal(err)
	}
	right, err := ProjectionFingerprint(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if left != right || !reflect.DeepEqual(incremental, replayed) {
		t.Fatalf("incremental and replay differ: %s != %s\nincremental=%+v\nreplayed=%+v", left, right, incremental, replayed)
	}
	if incremental.Nodes["node-1"].Mastery.State != MasteryUnseen || incremental.Nodes["node-1"].Mastery.ValidEvidenceCount != 0 {
		t.Fatalf("compensated evidence survived replay: %+v", incremental.Nodes["node-1"])
	}
	if !incremental.RedactedEvents["event-d"] {
		t.Fatal("EventRedacted was not preserved as an audit fact")
	}
	if incremental.Metadata.KnowledgeRevisionID != route.KnowledgeRevisionID || replayed.Metadata.KnowledgeRevisionID != route.KnowledgeRevisionID {
		t.Fatalf("knowledge revision was not replayed: incremental=%q replayed=%q", incremental.Metadata.KnowledgeRevisionID, replayed.Metadata.KnowledgeRevisionID)
	}
}

func TestRouteRevisionReplayRequiresStableNodeIdentity(t *testing.T) {
	route := RouteRevision{ID: "route-rev", RouteID: "route", Revision: 1, GoalRevisionID: "goal-rev", KnowledgeRevisionID: "knowledge", Steps: []RouteStep{{ID: "step", Ordinal: 0, NodeRevisionID: "node-rev", TeachingIntent: "teach", CompletionCondition: "pass"}}}
	projection := EmptyProjection("generation")
	if err := ApplyEvent(&projection, NewEventRegistry(), eventFixture(1, EventRouteRevisionCreated, route)); err == nil {
		t.Fatal("route event without stable node_id was replayed")
	}
	if len(projection.Routes) != 0 {
		t.Fatalf("invalid route partially projected: %+v", projection.Routes)
	}
}

func TestUnknownEventSchemaStopsProjection(t *testing.T) {
	projection := EmptyProjection("generation-1")
	event := eventFixture(1, EventExposureRecorded, Exposure{ID: "exposure-1"})
	event.SchemaVersion = 99
	err := ApplyEvent(&projection, NewEventRegistry(), event)
	if ErrorCode(err) != CodeUnsupportedEventSchema {
		t.Fatalf("error=%v", err)
	}
	if !projection.Metadata.Incomplete || projection.Metadata.AsOfEventSequence != 0 || len(projection.Timeline) != 0 {
		t.Fatalf("unsupported event partially applied: %+v", projection)
	}
}

func TestExplicitUpcasterRegistry(t *testing.T) {
	registry := NewEventRegistry()
	called := false
	registry.Register(2, func(payload json.RawMessage) (json.RawMessage, error) {
		called = true
		var legacy map[string]any
		if err := json.Unmarshal(payload, &legacy); err != nil {
			return nil, err
		}
		legacy["kind"] = "reading"
		return json.Marshal(legacy)
	})
	projection := EmptyProjection("generation-1")
	event := eventFixture(1, EventExposureRecorded, Exposure{ID: "exposure-1", SessionID: "session-1"})
	event.SchemaVersion = 2
	if err := ApplyEvent(&projection, registry, event); err != nil {
		t.Fatal(err)
	}
	if !called || len(projection.Exposures) != 1 || projection.Exposures[0].Kind != "reading" {
		t.Fatalf("upcaster was not used: called=%v projection=%+v", called, projection)
	}
}

func TestProjectionFingerprintExcludesRuntimeGenerationMetadata(t *testing.T) {
	const migrationEmptyProjectionFingerprint = "2b2fe0642e3c18f6c9a9adb8fc4e8195acf5d426c906a13db6ff1434086fe831"

	left := EmptyProjection("generation-left")
	left.Metadata.Rebuilding = true
	left.Metadata.Degraded = true
	left.Metadata.Incomplete = true
	left.Metadata.ReasonCodes = []string{"runtime"}
	right := EmptyProjection("generation-right")
	leftHash, err := ProjectionFingerprint(left)
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := ProjectionFingerprint(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftHash != rightHash {
		t.Fatalf("runtime metadata changed fingerprint: %s != %s", leftHash, rightHash)
	}
	if rightHash != migrationEmptyProjectionFingerprint {
		t.Fatalf("versioned empty projection fingerprint changed: got %s want migration seed %s", rightHash, migrationEmptyProjectionFingerprint)
	}
}

func TestProjectionStatsUseOnlyCanonicalUserInteractionEvents(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	occurred := start.Add(-24 * time.Hour)
	events := []LearningEvent{
		eventFixture(1, EventLearningSessionStarted, SessionProjection{Session: tutoring.Session{ID: "session-1", State: tutoring.StateGoalReady}}),
		eventFixture(2, EventAttemptSubmitted, map[string]any{}),
		eventFixture(3, EventActivityPresented, map[string]any{}),
		eventFixture(4, EventFreeQuestionAsked, map[string]any{}),
		eventFixture(5, EventReviewPresented, map[string]any{}),
	}
	events[1].ReceivedAt = start.Add(20 * time.Minute)
	events[1].OccurredAt = &occurred
	events[2].ReceivedAt = start.Add(2 * time.Minute)
	events[3].ReceivedAt = start
	events[4].ReceivedAt = start.Add(4 * time.Minute)
	projection, err := Replay(events, NewEventRegistry(), "generation")
	if err != nil {
		t.Fatal(err)
	}
	estimate := projection.Stats["session-1"]
	if !estimate.Estimated || estimate.AlgorithmVersion != ActiveTimePolicyVersion || estimate.SampleCount != 3 || estimate.DurationSeconds != int64((4*time.Minute+5*time.Minute)/time.Second) {
		t.Fatalf("projected estimate=%+v", estimate)
	}
	if estimate.FirstReceivedAt == nil || !estimate.FirstReceivedAt.Equal(start) || estimate.LastReceivedAt == nil || !estimate.LastReceivedAt.Equal(start.Add(20*time.Minute)) {
		t.Fatalf("projection used non-canonical timestamps: %+v", estimate)
	}
}

func TestProjectionStatsInitializeEmptyAndFingerprintTheirContent(t *testing.T) {
	projection := EmptyProjection("generation")
	event := eventFixture(1, EventLearningSessionStarted, SessionProjection{Session: tutoring.Session{ID: "session-1", State: tutoring.StateGoalReady}})
	if err := ApplyEvent(&projection, NewEventRegistry(), event); err != nil {
		t.Fatal(err)
	}
	estimate := projection.Stats["session-1"]
	if !estimate.Estimated || estimate.AlgorithmVersion != ActiveTimePolicyVersion || estimate.SampleCount != 0 {
		t.Fatalf("empty session estimate=%+v", estimate)
	}
	before, err := ProjectionFingerprint(projection)
	if err != nil {
		t.Fatal(err)
	}
	estimate.SampleCount = 1
	projection.Stats["session-1"] = estimate
	after, err := ProjectionFingerprint(projection)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("projection stats did not affect fingerprint")
	}
}

func TestActiveTimeOrderingUsesEventSequenceForReceivedAtTies(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	result := EstimateActiveTime("s1", []InteractionSample{
		{EventSequence: 3, SessionID: "s1", ReceivedAt: start.Add(time.Minute), UserInitiated: true},
		{EventSequence: 2, SessionID: "s1", ReceivedAt: start, UserInitiated: true},
		{EventSequence: 1, SessionID: "s1", ReceivedAt: start, UserInitiated: true},
	})
	if result.SampleCount != 3 || result.DurationSeconds != 60 || result.FirstReceivedAt == nil || !result.FirstReceivedAt.Equal(start) {
		t.Fatalf("tie-ordered estimate=%+v", result)
	}
}

func TestPendingAssessmentsAreClearedByAssessmentID(t *testing.T) {
	projection := EmptyProjection("generation")
	registry := NewEventRegistry()
	events := []LearningEvent{
		eventFixture(1, EventAssessmentMarkedProvisional, AssessmentProjectionEvent{AssessmentID: "assessment-a", NodeRevisionID: "node-1", Reasons: []string{"low_confidence", "risk_flag"}}),
		eventFixture(2, EventAssessmentMarkedProvisional, AssessmentProjectionEvent{AssessmentID: "assessment-b", NodeRevisionID: "node-1", Reasons: []string{"risk_flag"}}),
		eventFixture(3, EventAssessmentAccepted, AssessmentProjectionEvent{AssessmentID: "assessment-a", NodeRevisionID: "node-1"}),
	}
	for index, event := range events {
		if err := ApplyEvent(&projection, registry, event); err != nil {
			t.Fatal(err)
		}
		if index == 1 && projection.Nodes["node-1"].Mastery.PendingAssessments != 2 {
			t.Fatalf("pending assessments counted reasons instead of IDs: %+v", projection.Nodes["node-1"].Mastery)
		}
	}
	if _, exists := projection.Pending["assessment-a"]; exists {
		t.Fatal("accepted assessment remained pending")
	}
	if pending, exists := projection.Pending["assessment-b"]; !exists || len(pending.Reasons) != 1 || pending.Reasons[0] != "risk_flag" {
		t.Fatalf("unrelated pending assessment was cleared: %+v", projection.Pending)
	}
	if got := projection.Nodes["node-1"].Mastery.PendingAssessments; got != 1 {
		t.Fatalf("node pending assessment count=%d", got)
	}
}

func TestCanonicalEventTypeSet(t *testing.T) {
	values := []EventType{
		EventGoalRevisionCreated, EventRouteRevisionCreated, EventLearningSessionStarted,
		EventTutoringStateChanged, EventActivityIssued, EventActivityPresented,
		EventAttemptSubmitted, EventAssessmentRecorded, EventAssessmentMarkedProvisional,
		EventAssessmentAccepted, EventAssessmentOverridden, EventAssessmentVoided,
		EventEvidenceAccepted, EventEvidenceInvalidated, EventExposureRecorded,
		EventReviewPresented, EventFocusSuspended, EventFreeQuestionAsked,
		EventFreeAnswerRecorded, EventFocusResumed, EventRouteAdvanced,
		EventLearningCompleted, EventMisconceptionHypothesisRevised, EventRedacted,
	}
	seen := map[EventType]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			t.Fatalf("invalid canonical event type %q", value)
		}
		seen[value] = true
	}
	if len(seen) != 24 {
		t.Fatalf("canonical event count=%d, want 24", len(seen))
	}
}
