package knowledge

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNormalizePathEnforcesIndexSafeLimits(t *testing.T) {
	for _, value := range []string{
		strings.Repeat("a", MaxPathRunes+1),
		strings.Repeat("界", MaxPathBytes/3+1),
	} {
		if _, err := NormalizePath(value); ErrorCode(err) != CodeInvalidPath {
			t.Fatalf("oversized path was accepted: bytes=%d runes=%d err=%v", len(value), utf8.RuneCountInString(value), err)
		}
	}
}

func TestImportRequiresExplicitExpectedParent(t *testing.T) {
	service, _ := testKnowledgeService(t)
	_, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000100",
		Source:      "missing expected parent", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{{Path: "missing-parent.md", Markdown: "body"}},
	})
	if ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("missing expected_parent_revision_id error = %v", err)
	}
}

func TestRevisionTimeUsesPostgreSQLPrecision(t *testing.T) {
	store := newMemoryCatalogStore()
	service, err := NewService(store, NewCanonicalizer(), ServiceOptions{
		Now: func() time.Time { return time.Date(2026, 8, 19, 5, 0, 0, 123456789, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000103", ExpectedParentProvided: true,
		Source: "time precision", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{{Path: "time.md", Markdown: "body"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Revision.CreatedAt.Nanosecond(); got != 123456000 {
		t.Fatalf("revision nanoseconds = %d, want PostgreSQL microsecond precision", got)
	}
}

func TestImportSourceUsesUnicodeRuneLimit(t *testing.T) {
	service, _ := testKnowledgeService(t)
	valid := strings.Repeat("知", MaxSourceRunes)
	if _, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000101", ExpectedParentProvided: true,
		Source: valid, ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{{Path: "source.md", Markdown: "body"}},
	}); err != nil {
		t.Fatalf("500-rune source rejected: %v", err)
	}
	parent, err := service.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000102", ExpectedParentRevisionID: &parent.ID,
		ExpectedParentProvided: true, Source: strings.Repeat("知", MaxSourceRunes+1),
		ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents:     []ImportDocument{{Path: "other.md", Markdown: "body"}},
	}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("501-rune source error = %v", err)
	}
}

func TestSnapshotPathSwapAndFoldedCollision(t *testing.T) {
	service, _ := testKnowledgeService(t)
	actor := "90000000-0000-4000-8000-000000000001"
	first, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000111", ExpectedParentProvided: true,
		Source: "paths", ActorDeviceID: actor,
		Documents: []ImportDocument{
			{Path: "a.md", Markdown: "# Alpha\nalpha body\n"},
			{Path: "b.md", Markdown: "# Beta\nbeta body\n"},
			{Path: "Résumé.md", Markdown: "resume body"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := service.Export(t.Context(), first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]string{}
	for _, document := range exported.Documents {
		byPath[document.Path] = document.Markdown
	}
	parent := first.Revision.ID
	swapped, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000112", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "swap", ActorDeviceID: actor,
		Documents: []ImportDocument{
			{Path: "a.md", Markdown: byPath["b.md"]},
			{Path: "b.md", Markdown: byPath["a.md"]},
		},
	})
	if err != nil {
		t.Fatalf("path swap failed: %v", err)
	}
	firstIDs := documentIDsByPath(first.Revision)
	swappedIDs := documentIDsByPath(swapped.Revision)
	if swappedIDs["a.md"] != firstIDs["b.md"] || swappedIDs["b.md"] != firstIDs["a.md"] {
		t.Fatalf("path swap did not exchange identities: before=%v after=%v", firstIDs, swappedIDs)
	}

	swapParent := swapped.Revision.ID
	newIdentity := "---\nedu-agent-format: 1\nedu-agent-document-id: 70000000-0000-4000-8000-000000000111\nedu-agent-root-node-id: 70000000-0000-4000-8000-000000000112\n---\nnew resume"
	_, err = service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000113", ExpectedParentRevisionID: &swapParent,
		ExpectedParentProvided: true, Source: "folded", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "re\u0301sume\u0301.MD", Markdown: newIdentity}},
	})
	if ErrorCode(err) != CodePathOccupied {
		t.Fatalf("cross-batch folded path conflict = %v", err)
	}
}

func TestMarkerlessEmptyBodyDoesNotInheritByExactHash(t *testing.T) {
	service, _ := testKnowledgeService(t)
	actor := "90000000-0000-4000-8000-000000000001"
	first, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000121", ExpectedParentProvided: true,
		Source: "empty", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "empty.md", Markdown: "# Old Title\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := service.Export(t.Context(), first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(exported.Documents[0].Markdown, "\n")
	var clean []string
	for _, line := range lines {
		if !strings.Contains(line, "edu-agent-node:v1") {
			clean = append(clean, line)
		}
	}
	markerless := strings.Replace(strings.Join(clean, "\n"), "# Old Title", "# Completely Different", 1)
	parent := first.Revision.ID
	command := ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000122", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "empty edit", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "empty.md", Markdown: markerless}},
	}
	_, err = service.Import(t.Context(), command)
	var reviewErr *Error
	if !asKnowledgeError(err, &reviewErr) || reviewErr.Code != CodeIdentityReviewRequired || reviewErr.Review == nil || len(reviewErr.Review.Nodes) != 1 {
		t.Fatalf("markerless empty-body heading did not enter review: %v", err)
	}
	command.OperationID = "10000000-0000-4000-8000-000000000123"
	command.IdentityReviewBasisHash = reviewErr.Review.BasisHash
	command.IdentityReviewOperationID = reviewErr.Review.OperationID
	command.IdentityReviewReceipt = reviewErr.Review.Receipt
	command.NodeResolutions = []NodeResolution{{Locator: reviewErr.Review.Nodes[0].Locator, Action: "new", Reason: "title semantics changed"}}
	second, err := service.Import(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision.Documents[0].Revision.Nodes[1].NodeID == second.Revision.Documents[0].Revision.Nodes[1].NodeID {
		t.Fatal("markerless empty-body heading inherited the old NodeID")
	}
}

func TestSplitLineageAggregatesTargetsAndReplays(t *testing.T) {
	service, _ := testKnowledgeService(t)
	actor := "90000000-0000-4000-8000-000000000001"
	first, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000131", ExpectedParentProvided: true,
		Source: "split", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "split.md", Markdown: "# Original\nshared one two three four five six seven eight\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := service.Export(t.Context(), first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker := strings.Index(exported.Documents[0].Markdown, "<!-- edu-agent-node:v1")
	if marker < 0 {
		t.Fatal("export is missing node marker")
	}
	target := exported.Documents[0].Markdown[:marker] + "# Part One\nshared one two three four five six seven eight\n\n# Part Two\nshared one two three four five six seven eight\n"
	parent := first.Revision.ID
	command := ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000132", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "split", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "split.md", Markdown: target}},
	}
	_, err = service.Import(t.Context(), command)
	var reviewErr *Error
	if !asKnowledgeError(err, &reviewErr) || reviewErr.Review == nil || len(reviewErr.Review.Nodes) != 2 {
		t.Fatalf("split did not produce two node reviews: %v", err)
	}
	command.OperationID = "10000000-0000-4000-8000-000000000133"
	command.IdentityReviewBasisHash = reviewErr.Review.BasisHash
	command.IdentityReviewOperationID = reviewErr.Review.OperationID
	command.IdentityReviewReceipt = reviewErr.Review.Receipt
	for _, review := range reviewErr.Review.Nodes {
		if len(review.Candidates) != 1 {
			t.Fatalf("split review candidates = %+v", review.Candidates)
		}
		command.NodeResolutions = append(command.NodeResolutions, NodeResolution{
			Locator: review.Locator, Action: "split", SourceNodeRevisionIDs: []string{review.Candidates[0].RevisionID},
			Reason: "one section became two",
		})
	}
	result, err := service.Import(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Revision.Lineages) != 1 {
		t.Fatalf("split lineage groups = %+v", result.Revision.Lineages)
	}
	sources, targets := 0, 0
	for _, member := range result.Revision.Lineages[0].Members {
		if member.Role == "source" {
			sources++
		} else if member.Role == "target" {
			targets++
		}
	}
	if sources != 1 || targets != 2 || result.Revision.Lineages[0].KnowledgeRevisionID != result.Revision.ID {
		t.Fatalf("split lineage cardinality = %+v", result.Revision.Lineages[0])
	}
	replay, err := service.Import(t.Context(), command)
	if err != nil || !reflect.DeepEqual(result.Revision.Lineages, replay.Revision.Lineages) {
		t.Fatalf("lineage replay differs: first=%+v replay=%+v err=%v", result.Revision.Lineages, replay.Revision.Lineages, err)
	}
}

func TestLineageRejectsSourceReuseAcrossGroups(t *testing.T) {
	service, _ := testKnowledgeService(t)
	built := []DocumentRevision{{Nodes: []NodeRevision{
		{ID: "20000000-0000-4000-8000-000000000001"},
		{ID: "20000000-0000-4000-8000-000000000002"},
		{ID: "20000000-0000-4000-8000-000000000003"},
	}}}
	revision := KnowledgeRevision{ID: "30000000-0000-4000-8000-000000000001", CreatedByDeviceID: "90000000-0000-4000-8000-000000000001"}
	source := "10000000-0000-4000-8000-000000000001"
	drafts := []pendingLineage{
		{action: "rewrite", reason: "first rewrite", sourceRevisionIDs: []string{source}, documentIndex: 0, targetPreorder: 0},
		{action: "rewrite", reason: "second rewrite", sourceRevisionIDs: []string{source}, documentIndex: 0, targetPreorder: 1},
	}
	if _, err := service.materializeLineages(drafts, built, revision); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("source reused across lineage groups = %v", err)
	}
}

func TestLineageCardinalityRejectsSingleTargetSplitAndSingleSourceMerge(t *testing.T) {
	service, _ := testKnowledgeService(t)
	built := []DocumentRevision{{Nodes: []NodeRevision{{ID: "20000000-0000-4000-8000-000000000001"}, {ID: "20000000-0000-4000-8000-000000000002"}}}}
	revision := KnowledgeRevision{ID: "30000000-0000-4000-8000-000000000001", CreatedByDeviceID: "90000000-0000-4000-8000-000000000001"}
	for _, draft := range []pendingLineage{
		{action: "split", reason: "bad split", sourceRevisionIDs: []string{"10000000-0000-4000-8000-000000000001"}, documentIndex: 0, targetPreorder: 0},
		{action: "merge", reason: "bad merge", sourceRevisionIDs: []string{"10000000-0000-4000-8000-000000000001"}, documentIndex: 0, targetPreorder: 0},
	} {
		if _, err := service.materializeLineages([]pendingLineage{draft}, built, revision); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("invalid %s cardinality = %v", draft.action, err)
		}
	}
	validMerge := pendingLineage{action: "merge", reason: "combine", sourceRevisionIDs: []string{
		"10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002",
	}, documentIndex: 0, targetPreorder: 0}
	lineages, err := service.materializeLineages([]pendingLineage{validMerge}, built, revision)
	if err != nil || len(lineages) != 1 || len(lineages[0].Members) != 3 {
		t.Fatalf("valid merge lineage = %+v err=%v", lineages, err)
	}
}

func documentIDsByPath(revision KnowledgeRevision) map[string]string {
	result := make(map[string]string, len(revision.Documents))
	for _, document := range revision.Documents {
		result[document.Path] = document.Revision.DocumentID
	}
	return result
}

type recordingSelector struct {
	requests []SelectorRequest
}

func (s *recordingSelector) Select(_ context.Context, request SelectorRequest) (SelectorResponse, error) {
	s.requests = append(s.requests, request)
	return SelectorResponse{KnowledgeRevisionID: request.KnowledgeRevisionID, CandidateSetHash: request.CandidateSetHash, Decisions: []Decision{}}, nil
}

func TestRetrievalPinsArtifactsAndRejectsStaleOrCrossRevision(t *testing.T) {
	for _, test := range []struct {
		name       string
		artifact   func(nodeID string) NodeArtifact
		reason     string
		wantCalled bool
	}{
		{name: "ready", artifact: func(nodeID string) NodeArtifact {
			return NodeArtifact{
				ID: "40000000-0000-4000-8000-000000000001", NodeRevisionID: nodeID, Kind: "summary",
				ProducerVersion: "producer-v1", PromptVersion: "prompt-v1", ModelVersion: "model-v1",
				InputHash: strings.Repeat("a", 64), Content: "pinned summary", Status: "ready", CreatedAt: time.Unix(1, 0),
			}
		}, wantCalled: true},
		{name: "stale", artifact: func(nodeID string) NodeArtifact {
			return NodeArtifact{
				ID: "40000000-0000-4000-8000-000000000002", NodeRevisionID: nodeID, Kind: "summary",
				InputHash: strings.Repeat("b", 64), Status: "stale",
			}
		}, reason: "selector_stale_artifact"},
		{name: "cross revision", artifact: func(string) NodeArtifact {
			return NodeArtifact{
				ID: "40000000-0000-4000-8000-000000000003", NodeRevisionID: "50000000-0000-4000-8000-000000000099",
				Kind: "summary", InputHash: strings.Repeat("c", 64), Status: "ready",
			}
		}, reason: "selector_cross_revision_artifact"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store := testKnowledgeService(t)
			imported, err := service.Import(t.Context(), ImportCommand{
				OperationID: "10000000-0000-4000-8000-000000000141", ExpectedParentProvided: true,
				Source: "artifact", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
				Documents: []ImportDocument{{Path: "artifact.md", Markdown: "# Topic\nsearchable body\n"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			nodeID := imported.Revision.Documents[0].Revision.Nodes[1].ID
			store.artifacts = []NodeArtifact{test.artifact(nodeID)}
			selector := &recordingSelector{}
			service.selector = selector
			result, err := service.Retrieve(t.Context(), RetrievalCommand{Query: "topic"})
			if err != nil {
				t.Fatal(err)
			}
			if test.wantCalled {
				if len(selector.requests) == 0 || len(result.SummarySnapshot) != 1 || len(selector.requests[0].SummarySnapshot) != 1 || selector.requests[0].SummarySnapshot[0].Content != "pinned summary" || selector.requests[0].Candidates[0].SummaryArtifactID == "" {
					t.Fatalf("ready artifact was not pinned: result=%+v requests=%+v", result, selector.requests)
				}
			} else {
				if len(selector.requests) != 0 || !result.Degraded || len(result.Trace) == 0 || result.Trace[0].ReasonCode != test.reason {
					t.Fatalf("invalid artifact did not atomically fallback: result=%+v calls=%d", result, len(selector.requests))
				}
			}
		})
	}
}

func TestRetrievalSortsSameTraceHitsBeforeTruncation(t *testing.T) {
	service, _ := testKnowledgeService(t)
	_, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000181", ExpectedParentProvided: true,
		Source: "hit sorting", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{{Path: "hits.md", Markdown: "# First\nneedle repeated needle\n\n# Second\nneedle\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Retrieve(t.Context(), RetrievalCommand{Query: "needle", Limits: RetrievalLimits{MaxHits: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Trace) == 0 || len(result.Hits) != 1 {
		t.Fatalf("unexpected retrieval result: %+v", result)
	}
	var selected []string
	for _, decision := range result.Trace[0].Decisions {
		if decision.Action == "select" || decision.Action == "select_expand" {
			selected = append(selected, decision.NodeRevisionID)
		}
	}
	if len(selected) < 2 {
		t.Fatalf("fixture did not select two same-trace hits: %+v", result.Trace[0])
	}
	sort.Strings(selected)
	if result.Hits[0].NodeRevisionID != selected[0] {
		t.Fatalf("hit truncation ignored NodeRevisionID ordering: got=%s want=%s", result.Hits[0].NodeRevisionID, selected[0])
	}
}

func TestHistoricalManifestCanBecomeANewRevision(t *testing.T) {
	service, _ := testKnowledgeService(t)
	actor := "90000000-0000-4000-8000-000000000001"
	first, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000161", ExpectedParentProvided: true,
		Source: "first manifest", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "manifest.md", Markdown: "original body\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstExport, err := service.Export(t.Context(), first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	second, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000162", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "second manifest", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "manifest.md", Markdown: strings.Replace(firstExport.Documents[0].Markdown, "original body", "changed body with enough distinct content", 1)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondParent := second.Revision.ID
	third, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000163", ExpectedParentRevisionID: &secondParent,
		ExpectedParentProvided: true, Source: "reverse revision", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "manifest.md", Markdown: firstExport.Documents[0].Markdown}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Unchanged || third.Revision.ID == first.Revision.ID || third.Revision.ManifestHash != first.Revision.ManifestHash {
		t.Fatalf("historical manifest was not recorded as a new revision: first=%+v third=%+v", first.Revision, third.Revision)
	}
}

func TestRetrievalTraceMarksDeferredWorkAsTruncated(t *testing.T) {
	for _, test := range []struct {
		name   string
		limits RetrievalLimits
	}{
		{name: "exact candidate budget", limits: RetrievalLimits{TotalCandidates: 1}},
		{name: "max depth", limits: RetrievalLimits{MaxDepth: 1}},
		{name: "max hits", limits: RetrievalLimits{MaxHits: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _ := testKnowledgeService(t)
			markdown := "# Parent\nparent match\n\n## Child\nchild match\n"
			if test.name == "max hits" {
				markdown = "# One\nmatch\n\n# Two\nmatch\n"
			}
			_, err := service.Import(t.Context(), ImportCommand{
				OperationID: "10000000-0000-4000-8000-000000000171", ExpectedParentProvided: true,
				Source: "deferred work", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
				Documents: []ImportDocument{{Path: "deferred.md", Markdown: markdown}},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Retrieve(t.Context(), RetrievalCommand{Query: "match", Limits: test.limits})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Truncated || len(result.Trace) == 0 {
				t.Fatalf("deferred work was not marked truncated: %+v", result)
			}
			marked := false
			for _, trace := range result.Trace {
				marked = marked || trace.Truncated
			}
			if !marked {
				t.Fatalf("no trace records deferred work truncation: %+v", result.Trace)
			}
		})
	}
}

func TestRetrievalTraceIncludesLayerTruncation(t *testing.T) {
	for _, limits := range []RetrievalLimits{{CandidatesPerLayer: 1}, {TotalCandidates: 1}} {
		service, _ := testKnowledgeService(t)
		_, err := service.Import(t.Context(), ImportCommand{
			OperationID: "10000000-0000-4000-8000-000000000151", ExpectedParentProvided: true,
			Source: "truncate", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
			Documents: []ImportDocument{{Path: "truncate.md", Markdown: "# One\nmatch\n\n# Two\nmatch\n"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.Retrieve(t.Context(), RetrievalCommand{Query: "match", Limits: limits})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Truncated || len(result.Trace) == 0 || !result.Trace[0].Truncated {
			t.Fatalf("layer truncation missing from trace: limits=%+v result=%+v", limits, result)
		}
	}
}
