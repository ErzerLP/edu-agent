package learning

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestOfflineClosedStateTransitions(t *testing.T) {
	submissionStates := []OfflineSubmissionState{
		OfflineSubmissionReserved, OfflineSubmissionClaimedSucceeded, OfflineSubmissionClaimedRejected,
		OfflineSubmissionExpired, OfflineSubmissionRevoked,
	}
	for _, from := range submissionStates {
		for _, to := range submissionStates {
			want := from == OfflineSubmissionReserved && to != OfflineSubmissionReserved
			if got := CanTransitionOfflineSubmission(from, to); got != want {
				t.Fatalf("submission transition %s -> %s=%v want=%v", from, to, got, want)
			}
		}
	}

	if !CanTransitionOfflineLocal(OfflineLocalUploading, OfflineLocalArchivedPendingEvidence, false) ||
		CanTransitionOfflineLocal(OfflineLocalTerminal, OfflineLocalQueued, false) ||
		!CanTransitionOfflineLocal(OfflineLocalConflict, OfflineLocalDiscarded, false) ||
		!CanTransitionOfflineLocal(OfflineLocalQueued, OfflineLocalDiscarded, true) {
		t.Fatal("offline local transition matrix is not closed")
	}
	if !CanTransitionOfflineWorker(OfflineWorkerQueued, OfflineWorkerProcessing) ||
		!CanTransitionOfflineWorker(OfflineWorkerPendingRetry, OfflineWorkerCompleted) ||
		CanTransitionOfflineWorker(OfflineWorkerCompleted, OfflineWorkerProcessing) {
		t.Fatal("offline worker transition matrix is not closed")
	}
}

func TestUint63DecimalAndJCSBoundaries(t *testing.T) {
	for _, value := range []uint64{0, 1, 1<<53 - 1, 1 << 53, MaxUint63} {
		encoded, err := FormatUint63Decimal(value)
		if err != nil {
			t.Fatalf("format %d: %v", value, err)
		}
		decoded, err := ParseUint63Decimal(encoded)
		if err != nil || decoded != value {
			t.Fatalf("round trip %d encoded=%q decoded=%d err=%v", value, encoded, decoded, err)
		}
	}
	for _, value := range []string{"", "00", "01", "-0", "-1", "9223372036854775808"} {
		if _, err := ParseUint63Decimal(value); err == nil {
			t.Fatalf("invalid uint63 value %q accepted", value)
		}
	}
	if _, err := FormatUint63Decimal(MaxUint63 + 1); err == nil {
		t.Fatal("uint63 overflow accepted")
	}

	canonical, err := CanonicalizeJCS([]byte(`{"z":0,"a":["中",true],"n":9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"a":["中",true],"n":9007199254740992,"z":0}`
	if string(canonical) != expected {
		t.Fatalf("canonical JCS=%s want=%s", canonical, expected)
	}
	for _, invalid := range []string{
		`{"a":1,"a":2}`, `{"n":1.0}`, `{"n":1e3}`,
		`{"text":"\uD800"}`, `{"text":"\uDC00"}`, `{"text":"\uD800x"}`,
	} {
		if _, err := CanonicalizeJCS([]byte(invalid)); err == nil {
			t.Fatalf("invalid JCS profile input accepted: %s", invalid)
		}
	}
	paired, err := CanonicalizeJCS([]byte(`{"text":"\uD83D\uDE00"}`))
	if err != nil || string(paired) != `{"text":"😀"}` {
		t.Fatalf("valid surrogate pair canonical=%q err=%v", paired, err)
	}
}

func TestOfflineWinnerAndIneligibleRules(t *testing.T) {
	const contender = "10000000-0000-4000-8000-000000000001"
	const other = "10000000-0000-4000-8000-000000000002"
	winner, err := DecideOfflineWinner("", contender, true, "")
	if err != nil || !winner.Winner || !winner.EvidenceEligibility || winner.Reason != "" {
		t.Fatalf("first winner=%+v err=%v", winner, err)
	}
	duplicate, err := DecideOfflineWinner(other, contender, true, "")
	if err != nil || duplicate.Winner || duplicate.EvidenceEligibility || duplicate.Reason != OfflineReasonDuplicateActivity {
		t.Fatalf("duplicate contender=%+v err=%v", duplicate, err)
	}
	ineligible, err := DecideOfflineWinner("", contender, false, OfflineReasonExpiredActivity)
	if err != nil || ineligible.Winner || ineligible.EvidenceEligibility || ineligible.Reason != OfflineReasonExpiredActivity {
		t.Fatalf("ineligible contender=%+v err=%v", ineligible, err)
	}
}

func TestOfflineReplayUsesAcceptedEventSequenceAndDoesNotAdvanceSession(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	session := tutoring.Session{ID: "10000000-0000-4000-8000-000000000010", State: tutoring.StateAwaitingResponse, AggregateVer: 3}
	sessionPayload, _ := json.Marshal(SessionProjection{Session: session})
	offlineAttempt := Attempt{ID: "10000000-0000-4000-8000-000000000020", SessionID: session.ID, ActivityID: "10000000-0000-4000-8000-000000000030", ActivityRevision: 1, EvidenceEligibility: true}
	offlinePayload, _ := json.Marshal(offlineAttempt)
	evidence := AcceptedEvidence{
		ID: "10000000-0000-4000-8000-000000000040", AttemptID: offlineAttempt.ID,
		ActivityID: offlineAttempt.ActivityID, ActivityRevision: 1,
		NodeRevisionID: "10000000-0000-4000-8000-000000000050",
		Kind:           EvidencePracticeRecall, ActivityType: ActivityObjective, Outcome: OutcomePass,
		Help: HelpNone, ReceivedAt: now.Add(time.Minute), AcceptedEventSequence: 3,
	}
	evidencePayload, _ := json.Marshal(evidence)
	events := []LearningEvent{
		{EventSequence: 1, ID: "event-online", Type: EventLearningSessionStarted, SchemaVersion: 1, AggregateType: "session", AggregateID: session.ID, AggregateVersion: 3, Source: "online", ReceivedAt: now, Payload: sessionPayload},
		{EventSequence: 2, ID: "event-offline-attempt", Type: EventOfflineAttemptSubmitted, SchemaVersion: 1, AggregateType: "offline_attempt", AggregateID: offlineAttempt.ID, AggregateVersion: 1, ParentSessionID: session.ID, Source: "offline", ArchiveDisposition: "succeeded", EvidenceDisposition: string(OfflineEvidenceAccepted), DeviceID: "device", ReceivedAt: now.Add(time.Minute), Payload: offlinePayload},
		{EventSequence: 3, ID: "event-offline-evidence", Type: EventEvidenceAccepted, SchemaVersion: 1, AggregateType: "offline_attempt", AggregateID: offlineAttempt.ID, AggregateVersion: 2, ParentSessionID: session.ID, Source: "offline", ArchiveDisposition: "succeeded", EvidenceDisposition: string(OfflineEvidenceAccepted), DeviceID: "device", ReceivedAt: now.Add(time.Minute), Payload: evidencePayload},
	}
	projection, err := Replay(events, NewEventRegistry(), "generation")
	if err != nil {
		t.Fatal(err)
	}
	projected := projection.Sessions[session.ID]
	if projected.Session.State != tutoring.StateAwaitingResponse || projected.Session.AggregateVer != 3 || projected.UpdatedEventSequence != 1 {
		t.Fatalf("offline replay advanced session projection: %+v", projected)
	}
	if len(projection.Timeline) != 3 || projection.Timeline[1].ParentSessionID != session.ID || projection.Timeline[1].Source != "offline" {
		t.Fatalf("offline timeline=%+v", projection.Timeline)
	}
	if got := projection.Nodes[evidence.NodeRevisionID].Mastery.ValidEvidenceCount; got != 1 {
		t.Fatalf("offline evidence was not reduced: %+v", projection.Nodes[evidence.NodeRevisionID])
	}

	olderReceivedLaterSeq := evidence
	olderReceivedLaterSeq.ID = "b"
	olderReceivedLaterSeq.AcceptedEventSequence = 20
	olderReceivedLaterSeq.ReceivedAt = now
	newerReceivedEarlierSeq := evidence
	newerReceivedEarlierSeq.ID = "a"
	newerReceivedEarlierSeq.AcceptedEventSequence = 10
	newerReceivedEarlierSeq.ReceivedAt = now.Add(24 * time.Hour)
	values := []AcceptedEvidence{olderReceivedLaterSeq, newerReceivedEarlierSeq}
	SortEvidence(values)
	if values[0].AcceptedEventSequence != 10 || values[1].AcceptedEventSequence != 20 {
		t.Fatalf("evidence order ignored accepted_event_seq: %+v", values)
	}
}
