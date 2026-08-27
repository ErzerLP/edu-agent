package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type maintenanceMemoryStore struct {
	mu         sync.Mutex
	catalog    *memoryCatalogStore
	proposals  map[string]Proposal
	operations map[string]MaintenanceOperationRecord
	commits    map[string]PreparedCommit
}

func newMaintenanceMemoryStore(catalog *memoryCatalogStore) *maintenanceMemoryStore {
	return &maintenanceMemoryStore{
		catalog: catalog, proposals: map[string]Proposal{}, operations: map[string]MaintenanceOperationRecord{},
		commits: map[string]PreparedCommit{},
	}
}

func (s *maintenanceMemoryStore) MaintenanceBase(_ context.Context, revisionID string) (MaintenanceBaseSnapshot, error) {
	s.catalog.mu.Lock()
	defer s.catalog.mu.Unlock()
	revision, exists := s.catalog.revisions[revisionID]
	if !exists {
		return MaintenanceBaseSnapshot{}, &Error{Code: CodeNotFound}
	}
	return MaintenanceBaseSnapshot{
		Revision: revision, HeadRevisionID: optionalTestID(s.catalog.head), KnowledgeGeneration: 1,
	}, nil
}

func optionalTestID(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *maintenanceMemoryStore) LookupMaintenanceOperation(_ context.Context, id string) (MaintenanceOperationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.operations[id]
	return value, ok, nil
}

func (s *maintenanceMemoryStore) SaveProposal(_ context.Context, prepared PreparedProposal) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operation, ok := s.operations[prepared.Proposal.RequestID]; ok {
		if operation.RequestHash != prepared.Proposal.RequestHash {
			return Proposal{}, &Error{Code: CodeIdempotencyConflict}
		}
		proposal := s.proposals[operation.ProposalID]
		proposal.Replayed = true
		return proposal, nil
	}
	if prepared.Proposal.KnowledgeGeneration != 1 {
		return Proposal{}, &Error{Code: CodeProposalStale}
	}
	s.catalog.mu.Lock()
	defer s.catalog.mu.Unlock()
	if s.catalog.head == nil || *s.catalog.head != prepared.Proposal.BaseRevisionID {
		return Proposal{}, &Error{Code: CodeRevisionConflict, CurrentRevisionID: cloneString(s.catalog.head), CurrentRevisionKnown: true}
	}
	proposal := prepared.Proposal
	proposal.BasisHash = ComputeProposalBasis(proposal)
	s.commits[proposal.ID] = prepared.Commit
	if proposal.Risk.AutoApply {
		s.applyLocked(prepared.Commit)
		proposal.Status = ProposalApplied
		proposal.AppliedRevisionID = prepared.Commit.Revision.ID
		proposal.CurrentRevisionID = prepared.Commit.Revision.ID
		proposal.Origin = maintenanceMemoryOrigin(proposal)
		proposal.UpdatedAt = proposal.Decision.CreatedAt
	}
	s.proposals[proposal.ID] = proposal
	s.operations[proposal.RequestID] = MaintenanceOperationRecord{RequestHash: proposal.RequestHash, ProposalID: proposal.ID}
	return proposal, nil
}

func (s *maintenanceMemoryStore) ListProposals(_ context.Context, command ProposalListCommand) (ProposalPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Proposal, 0, len(s.proposals))
	for _, proposal := range s.proposals {
		if command.Status == "all" || string(proposal.Status) == command.Status {
			proposal.CandidateSnapshot = nil
			items = append(items, proposal)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	if len(items) > command.Limit {
		items = items[:command.Limit]
	}
	return ProposalPage{Items: items}, nil
}

func (s *maintenanceMemoryStore) Proposal(_ context.Context, id string) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	proposal, ok := s.proposals[id]
	if !ok {
		return Proposal{}, &Error{Code: CodeNotFound}
	}
	return proposal, nil
}

func (s *maintenanceMemoryStore) DecideProposal(_ context.Context, command PreparedProposalDecision) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operation, ok := s.operations[command.OperationID]; ok {
		if operation.RequestHash != command.RequestHash {
			return Proposal{}, &Error{Code: CodeIdempotencyConflict}
		}
		proposal := s.proposals[operation.ProposalID]
		proposal.Replayed = true
		return proposal, nil
	}
	proposal, ok := s.proposals[command.ProposalID]
	if !ok {
		return Proposal{}, &Error{Code: CodeNotFound}
	}
	if proposal.Status != ProposalOpen {
		return Proposal{}, &Error{Code: CodeProposalClosed}
	}
	s.catalog.mu.Lock()
	defer s.catalog.mu.Unlock()
	decision := ProposalDecision{
		ID: command.DecisionID, OperationID: command.OperationID, RequestedDecision: command.RequestedDecision,
		Reason: command.Reason, ActorDeviceID: command.ActorDeviceID, CreatedAt: command.DecidedAt,
	}
	stale := s.catalog.head == nil || *s.catalog.head != proposal.BaseRevisionID ||
		proposal.KnowledgeGeneration != 1 || proposal.EvidenceImpact.Generation != command.EvidenceGeneration ||
		proposal.EvidenceImpact.Fingerprint != command.EvidenceFingerprint || proposal.BasisHash != ComputeProposalBasis(proposal) ||
		proposal.CanonicalizerVersion != command.CanonicalizerVersion || proposal.IdentityPolicyVersion != command.IdentityPolicyVersion ||
		proposal.DiffVersion != command.DiffVersion || proposal.RiskVersion != command.RiskVersion ||
		proposal.AutoPolicyVersion != command.AutoPolicyVersion
	if stale {
		decision.Outcome = string(ProposalStale)
		proposal.Status = ProposalStale
		if s.catalog.head != nil {
			proposal.CurrentRevisionID = *s.catalog.head
		}
	} else if command.RequestedDecision == "reject" {
		decision.Outcome = string(ProposalRejected)
		proposal.Status = ProposalRejected
	} else {
		decision.Outcome = string(ProposalApplied)
		commit := s.commits[proposal.ID]
		s.applyLocked(commit)
		proposal.Status = ProposalApplied
		proposal.AppliedRevisionID = commit.Revision.ID
		proposal.CurrentRevisionID = commit.Revision.ID
		proposal.Origin = maintenanceMemoryOrigin(proposal)
	}
	proposal.Decision = &decision
	proposal.UpdatedAt = command.DecidedAt
	s.proposals[proposal.ID] = proposal
	s.operations[command.OperationID] = MaintenanceOperationRecord{RequestHash: command.RequestHash, ProposalID: proposal.ID}
	return proposal, nil
}

func (s *maintenanceMemoryStore) applyLocked(commit PreparedCommit) {
	s.catalog.revisions[commit.Revision.ID] = commit.Revision
	id := commit.Revision.ID
	s.catalog.head = &id
	for _, snapshot := range commit.Revision.Documents {
		s.catalog.documents[snapshot.Revision.DocumentID] = struct{}{}
		for _, node := range snapshot.Revision.Nodes {
			s.catalog.nodes[node.NodeID] = snapshot.Revision.DocumentID
		}
	}
}

func maintenanceMemoryOrigin(proposal Proposal) *RevisionOrigin {
	origin := &RevisionOrigin{Version: MaintenanceOriginVersion, Kind: string(proposal.Kind), ProposalID: proposal.ID, BaseRevisionID: proposal.BaseRevisionID, BasisHash: proposal.BasisHash}
	if proposal.RollbackTargetRevisionID != "" {
		target := proposal.RollbackTargetRevisionID
		origin.RollbackTargetRevisionID = &target
	}
	return origin
}

type maintenanceEvidenceReader struct {
	mu         sync.Mutex
	generation int64
	byNode     map[string][]AcceptedEvidenceReference
}

func (r *maintenanceEvidenceReader) AcceptedEvidenceImpact(_ context.Context, nodeRevisionIDs []string) (AcceptedEvidenceImpact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := AcceptedEvidenceImpact{Generation: r.generation, References: []AcceptedEvidenceReference{}}
	for _, id := range nodeRevisionIDs {
		result.References = append(result.References, r.byNode[id]...)
	}
	return result, nil
}

func maintenanceServiceForTest(t *testing.T) (*Service, *memoryCatalogStore, *maintenanceMemoryStore, *maintenanceEvidenceReader) {
	t.Helper()
	catalog := newMemoryCatalogStore()
	maintenance := newMaintenanceMemoryStore(catalog)
	evidence := &maintenanceEvidenceReader{generation: 1, byNode: map[string][]AcceptedEvidenceReference{}}
	ids := &deterministicUUIDs{}
	service, err := NewService(catalog, NewCanonicalizer(), ServiceOptions{
		MaintenanceStore: maintenance, EvidenceImpactReader: evidence, NewUUID: ids.next,
		Now: func() time.Time { return time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, catalog, maintenance, evidence
}

func TestMaintenanceCommandsRejectComputedFields(t *testing.T) {
	var command CreateProposalCommand
	if err := json.Unmarshal([]byte(`{"request_id":"10000000-0000-4000-8000-000000000001","base_revision_id":"10000000-0000-4000-8000-000000000002","sources":[],"candidate_snapshot":[],"risk":{"level":"low"}}`), &command); err == nil {
		t.Fatal("create proposal accepted caller-supplied risk")
	}
	var decision ProposalDecisionCommand
	if err := json.Unmarshal([]byte(`{"operation_id":"10000000-0000-4000-8000-000000000001","proposal_id":"10000000-0000-4000-8000-000000000002","decision":"approve","reason":"ok","actor_device_id":"10000000-0000-4000-8000-000000000003"}`), &decision); err == nil {
		t.Fatal("proposal decision accepted caller-supplied actor identity")
	}
}

func TestMaintenanceDeterministicAutoApplyAndIdempotency(t *testing.T) {
	service, _, _, _ := maintenanceServiceForTest(t)
	actor := "90000000-0000-4000-8000-000000000001"
	base, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "maintenance-base", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "base.md", Markdown: "# Base\nstable base body\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := service.Export(t.Context(), base.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	command := CreateProposalCommand{
		RequestID: "20000000-0000-4000-8000-000000000001", BaseRevisionID: base.Revision.ID,
		ActorDeviceID: actor, Sources: []ProposalSource{maintenanceSourceFixture("note", "agent/run-1", "bounded addition")},
		CandidateSnapshot: []ImportDocument{
			{Path: "base.md", Markdown: exported.Documents[0].Markdown},
			{Path: "new.md", Markdown: "# New\nsmall bounded addition with one two three four five six seven eight\n"},
		},
	}
	proposal, err := service.Create(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != ProposalApplied || !proposal.Risk.AutoApply || proposal.AppliedRevisionID == "" || len(proposal.Diff) != 1 || proposal.Diff[0].Kind != "add" {
		t.Fatalf("unexpected auto-applied proposal: %+v", proposal)
	}
	if proposal.Origin == nil || proposal.Origin.ProposalID != proposal.ID || proposal.BasisHash == "" {
		t.Fatalf("auto-applied proposal lacks origin/basis: %+v", proposal)
	}
	replay, err := service.Create(t.Context(), command)
	if err != nil || !replay.Replayed || replay.ID != proposal.ID {
		t.Fatalf("proposal replay=%+v err=%v", replay, err)
	}
	changed := command
	changed.Sources = []ProposalSource{maintenanceSourceFixture("note", "agent/run-1", "changed provenance")}
	if _, err := service.Create(t.Context(), changed); ErrorCode(err) != CodeIdempotencyConflict {
		t.Fatalf("changed proposal replay error=%v", err)
	}
	changedActor := command
	changedActor.ActorDeviceID = "90000000-0000-4000-8000-000000000002"
	if _, err := service.Create(t.Context(), changedActor); ErrorCode(err) != CodeIdempotencyConflict {
		t.Fatalf("cross-actor proposal replay error=%v", err)
	}

	autoExport, err := service.Export(t.Context(), proposal.AppliedRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	localCandidate := make([]ImportDocument, len(autoExport.Documents))
	for index := range autoExport.Documents {
		localCandidate[index] = ImportDocument{Path: autoExport.Documents[index].Path, Markdown: autoExport.Documents[index].Markdown}
		if localCandidate[index].Path == "new.md" {
			localCandidate[index].Markdown = strings.Replace(localCandidate[index].Markdown, "one two three four five six seven eight", "one two three four five six seven eight nine", 1)
		}
	}
	local, err := service.Create(t.Context(), CreateProposalCommand{
		RequestID: "20000000-0000-4000-8000-000000000002", BaseRevisionID: proposal.AppliedRevisionID,
		ActorDeviceID: actor, Sources: []ProposalSource{maintenanceSourceFixture("note", "agent/run-2", "bounded local edit")},
		CandidateSnapshot: localCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if local.Status != ProposalApplied || !local.Risk.AutoApply || len(local.Diff) != 1 || local.Diff[0].Kind != "edit" || !local.Diff[0].LocalBodyOnly {
		t.Fatalf("bounded local edit did not auto apply: %+v", local)
	}
}

func TestMaintenanceProposalDiffIsAggregateBounded(t *testing.T) {
	diffs := []DocumentDiff{
		{Unified: strings.Repeat("a", MaxMaintenanceDiffBytes-8)},
		{Unified: strings.Repeat("b", 100)},
		{Unified: strings.Repeat("c", 100)},
	}
	boundAggregateMaintenanceDiff(diffs)
	total := 0
	for _, item := range diffs {
		total += len(item.Unified)
	}
	if total > MaxMaintenanceDiffBytes || !diffs[1].Truncated || !diffs[2].Truncated {
		t.Fatalf("aggregate diff bound total=%d diffs=%+v", total, diffs)
	}
}

func TestMaintenanceDestructiveAndLineageRisksNeverAutoApply(t *testing.T) {
	localEdit := DocumentDiff{Kind: "edit", LocalBodyOnly: true, EditedNodeIDs: []string{"node"}}
	for _, test := range []struct {
		name     string
		analysis maintenanceAnalysis
		reason   string
	}{
		{name: "move", analysis: maintenanceAnalysis{diff: []DocumentDiff{{Kind: "move"}}, lineage: LineageImpact{Move: true}}, reason: "move"},
		{name: "delete", analysis: maintenanceAnalysis{diff: []DocumentDiff{{Kind: "delete"}}, lineage: LineageImpact{Delete: true}}, reason: "delete"},
		{name: "restore", analysis: maintenanceAnalysis{diff: []DocumentDiff{{Kind: "add"}}, lineage: LineageImpact{Restore: true}}, reason: "restore"},
		{name: "rollback", analysis: maintenanceAnalysis{diff: []DocumentDiff{{Kind: "edit", LocalBodyOnly: true, EditedNodeIDs: []string{"node"}}}, lineage: LineageImpact{Rollback: true}}, reason: "rollback"},
		{name: "lineage", analysis: maintenanceAnalysis{diff: []DocumentDiff{localEdit}, lineage: LineageImpact{Lineages: []Lineage{{Action: "rewrite"}}}}, reason: "lineage_change"},
	} {
		t.Run(test.name, func(t *testing.T) {
			risk := maintenanceRisk(test.analysis, AcceptedEvidenceImpact{})
			if risk.AutoApply || !containsString(risk.Reasons, test.reason) {
				t.Fatalf("risk did not fail closed: %+v", risk)
			}
		})
	}
}

func TestMaintenancePolicyFailsClosedForStructureAndEvidence(t *testing.T) {
	service, _, _, evidence := maintenanceServiceForTest(t)
	actor := "90000000-0000-4000-8000-000000000001"
	base := importMaintenanceBase(t, service, actor, "# Topic\none two three four five six seven eight nine ten\n")
	exported, _ := service.Export(t.Context(), base.ID)
	structural := strings.Replace(exported.Documents[0].Markdown, "# Topic", "# Renamed", 1)
	proposal, err := service.Create(t.Context(), CreateProposalCommand{
		RequestID: "20000000-0000-4000-8000-000000000011", BaseRevisionID: base.ID, ActorDeviceID: actor,
		Sources:           []ProposalSource{maintenanceSourceFixture("note", "agent/structure", "rename")},
		CandidateSnapshot: []ImportDocument{{Path: "topic.md", Markdown: structural}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != ProposalOpen || proposal.Risk.AutoApply || !containsString(proposal.Risk.Reasons, "non_local_edit") {
		t.Fatalf("structural proposal did not fail closed: %+v", proposal)
	}

	// Reject the structural proposal so the next scenario remains on the same head.
	rejectCommand := ProposalDecisionCommand{
		OperationID: "30000000-0000-4000-8000-000000000011", ProposalID: proposal.ID,
		Decision: "reject", Reason: "do not rename", ActorDeviceID: actor,
	}
	rejected, err := service.Decide(t.Context(), rejectCommand)
	if err != nil || rejected.Status != ProposalRejected {
		t.Fatalf("reject decision=%+v err=%v", rejected, err)
	}
	replayedReject, err := service.Decide(t.Context(), rejectCommand)
	if err != nil || !replayedReject.Replayed || replayedReject.ID != proposal.ID {
		t.Fatalf("reject replay=%+v err=%v", replayedReject, err)
	}
	changedReject := rejectCommand
	changedReject.Reason = "different reason"
	if _, err := service.Decide(t.Context(), changedReject); ErrorCode(err) != CodeIdempotencyConflict {
		t.Fatalf("changed reject replay error=%v", err)
	}
	if _, err := service.Decide(t.Context(), ProposalDecisionCommand{
		OperationID: "30000000-0000-4000-8000-000000000099", ProposalID: proposal.ID,
		Decision: "approve", Reason: "cannot reopen", ActorDeviceID: actor,
	}); ErrorCode(err) != CodeProposalClosed {
		t.Fatalf("terminal proposal accepted another decision: %v", err)
	}
	oldNode := base.Documents[0].Revision.Nodes[1]
	evidence.byNode[oldNode.ID] = []AcceptedEvidenceReference{{
		EvidenceID: "40000000-0000-4000-8000-000000000001", NodeRevisionID: oldNode.ID, KnowledgeRevisionID: base.ID,
	}}
	bodyEdit := strings.Replace(exported.Documents[0].Markdown, "one two three four five six seven eight nine ten", "one two three four five six seven eight nine ten eleven", 1)
	withEvidence, err := service.Create(t.Context(), CreateProposalCommand{
		RequestID: "20000000-0000-4000-8000-000000000012", BaseRevisionID: base.ID, ActorDeviceID: actor,
		Sources:           []ProposalSource{maintenanceSourceFixture("note", "agent/evidence", "local edit")},
		CandidateSnapshot: []ImportDocument{{Path: "topic.md", Markdown: bodyEdit}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if withEvidence.Status != ProposalOpen || withEvidence.EvidenceImpact.Count != 1 || withEvidence.Risk.AutoApply || !containsString(withEvidence.Risk.Reasons, "accepted_evidence_affected") {
		t.Fatalf("accepted evidence did not force review: %+v", withEvidence)
	}
	page, err := service.List(t.Context(), ProposalListCommand{Status: "open", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != withEvidence.ID {
		t.Fatalf("open proposal list=%+v err=%v", page, err)
	}
	detail, err := service.Get(t.Context(), withEvidence.ID)
	if err != nil || detail.ID != withEvidence.ID || detail.EvidenceImpact.Count != 1 {
		t.Fatalf("proposal detail=%+v err=%v", detail, err)
	}
}

func TestMaintenanceConcurrentBaseBecomesStaleAndRollbackAppends(t *testing.T) {
	service, catalog, _, _ := maintenanceServiceForTest(t)
	actor := "90000000-0000-4000-8000-000000000001"
	base := importMaintenanceBase(t, service, actor, "# Topic\none two three four five six seven eight nine ten\n")
	exported, _ := service.Export(t.Context(), base.ID)
	makeProposal := func(requestID, title string) Proposal {
		candidate := strings.Replace(exported.Documents[0].Markdown, "# Topic", "# "+title, 1)
		proposal, err := service.Create(t.Context(), CreateProposalCommand{
			RequestID: requestID, BaseRevisionID: base.ID, ActorDeviceID: actor,
			Sources:           []ProposalSource{maintenanceSourceFixture("note", "agent/"+title, title)},
			CandidateSnapshot: []ImportDocument{{Path: "topic.md", Markdown: candidate}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return proposal
	}
	first := makeProposal("20000000-0000-4000-8000-000000000021", "First")
	second := makeProposal("20000000-0000-4000-8000-000000000022", "Second")
	applied, err := service.Decide(t.Context(), ProposalDecisionCommand{
		OperationID: "30000000-0000-4000-8000-000000000021", ProposalID: first.ID,
		Decision: "approve", Reason: "approved first", ActorDeviceID: actor,
	})
	if err != nil || applied.Status != ProposalApplied {
		t.Fatalf("first decision=%+v err=%v", applied, err)
	}
	stale, err := service.Decide(t.Context(), ProposalDecisionCommand{
		OperationID: "30000000-0000-4000-8000-000000000022", ProposalID: second.ID,
		Decision: "approve", Reason: "approved second", ActorDeviceID: actor,
	})
	if err != nil || stale.Status != ProposalStale || stale.CurrentRevisionID != applied.AppliedRevisionID {
		t.Fatalf("second decision=%+v err=%v", stale, err)
	}
	beforeRollback := applied.AppliedRevisionID
	rollback, err := service.CreateRollback(t.Context(), CreateRollbackCommand{
		RequestID: "20000000-0000-4000-8000-000000000023", BaseRevisionID: beforeRollback,
		TargetRevisionID: base.ID, ActorDeviceID: actor,
		Sources: []ProposalSource{maintenanceSourceFixture("note", "human/rollback", "restore base")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Status != ProposalOpen || rollback.Risk.AutoApply || !rollback.LineageImpact.Rollback {
		t.Fatalf("rollback did not require approval: %+v", rollback)
	}
	rolledBack, err := service.Decide(t.Context(), ProposalDecisionCommand{
		OperationID: "30000000-0000-4000-8000-000000000023", ProposalID: rollback.ID,
		Decision: "approve", Reason: "restore known-good ancestor", ActorDeviceID: actor,
	})
	if err != nil || rolledBack.Status != ProposalApplied || rolledBack.AppliedRevisionID == base.ID || rolledBack.AppliedRevisionID == beforeRollback {
		t.Fatalf("rollback decision=%+v err=%v", rolledBack, err)
	}
	head, _ := catalog.Head(t.Context())
	if head == nil || head.ID != rolledBack.AppliedRevisionID || head.ParentRevisionID == nil || *head.ParentRevisionID != beforeRollback || head.ManifestHash != base.ManifestHash {
		t.Fatalf("rollback head is not an appended ancestor snapshot: %+v", head)
	}
	if _, err := catalog.Revision(t.Context(), beforeRollback); err != nil {
		t.Fatalf("intervening revision was not preserved: %v", err)
	}
}

func importMaintenanceBase(t *testing.T, service *Service, actor, markdown string) KnowledgeRevision {
	t.Helper()
	result, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000101", ExpectedParentProvided: true,
		Source: "maintenance-base", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "topic.md", Markdown: markdown}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Revision
}

func maintenanceSourceFixture(kind, locator, excerpt string) ProposalSource {
	hash := sha256.Sum256([]byte(excerpt))
	return ProposalSource{Kind: kind, Locator: locator, Excerpt: excerpt, SHA256: hex.EncodeToString(hash[:])}
}

func containsString(values []string, expected string) bool {
	return slices.Contains(values, expected)
}
