package learning

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEvidenceCarryoverCandidatesAreDeterministic(t *testing.T) {
	left := EvidenceCarryoverCandidate{
		KnowledgeRevisionID: "30000000-0000-4000-8000-000000000001",
		NodeID:              "40000000-0000-4000-8000-000000000002",
		NodeRevisionID:      "50000000-0000-4000-8000-000000000002",
		DocumentRevisionID:  "60000000-0000-4000-8000-000000000002",
	}
	right := EvidenceCarryoverCandidate{
		KnowledgeRevisionID: "30000000-0000-4000-8000-000000000001",
		NodeID:              "40000000-0000-4000-8000-000000000001",
		NodeRevisionID:      "50000000-0000-4000-8000-000000000001",
		DocumentRevisionID:  "60000000-0000-4000-8000-000000000001",
	}
	first, firstFingerprint, err := NormalizeEvidenceCarryoverCandidates([]EvidenceCarryoverCandidate{left, right})
	if err != nil {
		t.Fatal(err)
	}
	second, secondFingerprint, err := NormalizeEvidenceCarryoverCandidates([]EvidenceCarryoverCandidate{right, left})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || firstFingerprint != secondFingerprint || first[0].NodeRevisionID != right.NodeRevisionID {
		t.Fatalf("carryover candidates are not deterministic: first=%+v second=%+v", first, second)
	}
	if _, _, err := NormalizeEvidenceCarryoverCandidates([]EvidenceCarryoverCandidate{right, right}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("duplicate candidate error=%v", err)
	}
}

func TestEvidenceCarryoverReplayIsProvisionalAndReducerNeutral(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	evidence := AcceptedEvidence{
		ID: "10000000-0000-4000-8000-000000000001", DispositionDecisionID: "10000000-0000-4000-8000-000000000002",
		AssessmentID: "10000000-0000-4000-8000-000000000003", AttemptID: "10000000-0000-4000-8000-000000000004",
		ActivityID: "10000000-0000-4000-8000-000000000005", ActivityRevision: 1,
		GoalRevisionID: "10000000-0000-4000-8000-000000000006", RouteRevisionID: "10000000-0000-4000-8000-000000000007",
		KnowledgeRevisionID: "20000000-0000-4000-8000-000000000001", NodeRevisionID: "30000000-0000-4000-8000-000000000001",
		RubricRevision: "rubric-v1", Kind: EvidencePracticeRecall, ActivityType: ActivityObjective,
		Outcome: OutcomePass, Help: HelpNone, ReceivedAt: now, AcceptedEventSequence: 1,
		AcceptancePolicyVersion: AssessmentPolicyVersion, ReducerPolicyVersion: MasteryReducerVersion,
		ReviewPolicyVersion: ReviewPolicyVersion,
	}
	acceptedPayload, _ := json.Marshal(evidence)
	accepted := LearningEvent{
		EventSequence: 1, ID: "70000000-0000-4000-8000-000000000001", Type: EventEvidenceAccepted,
		SchemaVersion: EventSchemaVersion, AggregateType: "session", AggregateID: "80000000-0000-4000-8000-000000000001",
		AggregateVersion: 1, DeviceID: "90000000-0000-4000-8000-000000000001", OperationID: "a0000000-0000-4000-8000-000000000001",
		ReceivedAt: now, Payload: acceptedPayload,
	}
	link := EvidenceCarryoverLink{
		ID: "b0000000-0000-4000-8000-000000000001", ProposalID: "c0000000-0000-4000-8000-000000000001",
		SourceEvidenceID: evidence.ID, TargetKnowledgeRevisionID: "20000000-0000-4000-8000-000000000002",
		TargetNodeID: "d0000000-0000-4000-8000-000000000001", TargetNodeRevisionID: "30000000-0000-4000-8000-000000000002",
		TargetDocumentRevisionID: "e0000000-0000-4000-8000-000000000001",
		DecisionID:               "f0000000-0000-4000-8000-000000000001", EventID: "70000000-0000-4000-8000-000000000002",
		EventSequence: 2, CreatedAt: now.Add(time.Second),
	}
	provisional := ProvisionalEvidenceCarryover{
		ProposalID: link.ProposalID, KnowledgeProposalID: "c0000000-0000-4000-8000-000000000002",
		SourceEvidenceID: evidence.ID, SourceKnowledgeRevisionID: evidence.KnowledgeRevisionID,
		SourceNodeRevisionID: evidence.NodeRevisionID, TargetKnowledgeRevisionID: link.TargetKnowledgeRevisionID,
		Links: []EvidenceCarryoverLink{link}, BasisFingerprint: SHA256([]byte("carryover-basis")),
		PolicyVersion: EvidenceCarryoverPolicyVersion, ApprovedEventSequence: 2,
	}
	approvedPayload, _ := json.Marshal(EvidenceCarryoverEvent{
		ProposalID: provisional.ProposalID, DecisionID: link.DecisionID, RequestedDecision: "approve",
		Outcome: string(EvidenceCarryoverApproved), Reason: "explicit review", Provisional: &provisional,
	})
	approved := LearningEvent{
		EventSequence: 2, ID: link.EventID, Type: EventEvidenceCarryoverApproved,
		SchemaVersion: EventSchemaVersion, AggregateType: "evidence_carryover", AggregateID: provisional.ProposalID,
		AggregateVersion: 1, DeviceID: accepted.DeviceID, OperationID: "a0000000-0000-4000-8000-000000000002",
		ReceivedAt: now.Add(time.Second), Payload: approvedPayload,
	}
	registry := NewEventRegistry()
	incremental, err := Replay([]LearningEvent{accepted}, registry, "generation")
	if err != nil {
		t.Fatal(err)
	}
	beforeEvidence := incremental.Evidence[evidence.ID]
	beforeNode := incremental.Nodes[evidence.NodeRevisionID]
	if err := ApplyEvent(&incremental, registry, approved); err != nil {
		t.Fatal(err)
	}
	full, err := Replay([]LearningEvent{approved, accepted}, registry, "generation")
	if err != nil {
		t.Fatal(err)
	}
	incrementalFingerprint, err := ProjectionFingerprint(incremental)
	if err != nil {
		t.Fatal(err)
	}
	fullFingerprint, err := ProjectionFingerprint(full)
	if err != nil {
		t.Fatal(err)
	}
	if incrementalFingerprint != fullFingerprint || !reflect.DeepEqual(incremental.Carryovers, full.Carryovers) {
		t.Fatalf("incremental/full carryover replay diverged: incremental=%s full=%s", incrementalFingerprint, fullFingerprint)
	}
	if !reflect.DeepEqual(incremental.Evidence[evidence.ID], beforeEvidence) || !reflect.DeepEqual(incremental.Nodes[evidence.NodeRevisionID], beforeNode) {
		t.Fatalf("carryover changed authoritative evidence or node reduction: evidence=%+v node=%+v", incremental.Evidence[evidence.ID], incremental.Nodes[evidence.NodeRevisionID])
	}
	if len(incremental.Carryovers) != 1 || len(incremental.Carryovers[provisional.ProposalID].Links) != 1 {
		t.Fatalf("approved carryover projection=%+v", incremental.Carryovers)
	}
}

func TestRejectedEvidenceCarryoverHasNoProvisionalEffect(t *testing.T) {
	payload, _ := json.Marshal(EvidenceCarryoverEvent{
		ProposalID: "10000000-0000-4000-8000-000000000001",
		DecisionID: "20000000-0000-4000-8000-000000000001", RequestedDecision: "reject",
		Outcome: string(EvidenceCarryoverRejected), Reason: "not equivalent",
	})
	projection, err := Replay([]LearningEvent{{
		EventSequence: 1, ID: "30000000-0000-4000-8000-000000000001", Type: EventEvidenceCarryoverRejected,
		SchemaVersion: EventSchemaVersion, AggregateType: "evidence_carryover",
		AggregateID: "10000000-0000-4000-8000-000000000001", AggregateVersion: 1,
		DeviceID: "40000000-0000-4000-8000-000000000001", OperationID: "50000000-0000-4000-8000-000000000001",
		ReceivedAt: time.Now().UTC(), Payload: payload,
	}}, NewEventRegistry(), "generation")
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Carryovers) != 0 || len(projection.Evidence) != 0 || len(projection.Nodes) != 0 {
		t.Fatalf("rejected carryover changed projection authority: %+v", projection)
	}
}

func TestEvidenceCarryoverDecisionRejectsCallerActor(t *testing.T) {
	var command EvidenceCarryoverDecisionCommand
	if err := json.Unmarshal([]byte(`{"operation_id":"10000000-0000-4000-8000-000000000001","proposal_id":"20000000-0000-4000-8000-000000000001","decision":"approve","reason":"reviewed","actor_device_id":"30000000-0000-4000-8000-000000000001"}`), &command); err == nil {
		t.Fatal("carryover decision accepted caller-supplied actor identity")
	}
}
