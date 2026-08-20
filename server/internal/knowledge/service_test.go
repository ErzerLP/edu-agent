package knowledge

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type memoryCatalogStore struct {
	mu         sync.Mutex
	head       *string
	revisions  map[string]KnowledgeRevision
	operations map[string]ImportOperationRecord
	documents  map[string]struct{}
	nodes      map[string]string
	artifacts  []NodeArtifact
}

func newMemoryCatalogStore() *memoryCatalogStore {
	return &memoryCatalogStore{
		revisions: map[string]KnowledgeRevision{}, operations: map[string]ImportOperationRecord{},
		documents: map[string]struct{}{}, nodes: map[string]string{},
	}
}

func (m *memoryCatalogStore) Head(context.Context) (*KnowledgeRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.head == nil {
		return nil, nil
	}
	revision := m.revisions[*m.head]
	return &revision, nil
}

func (m *memoryCatalogStore) Revision(_ context.Context, id string) (KnowledgeRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	revision, exists := m.revisions[id]
	if !exists {
		return KnowledgeRevision{}, &Error{Code: CodeNotFound}
	}
	return revision, nil
}

func (m *memoryCatalogStore) DocumentIdentityExists(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.documents[id]
	return exists, nil
}

func (m *memoryCatalogStore) NodeIdentityOwner(_ context.Context, id string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, exists := m.nodes[id]
	return owner, exists, nil
}

func (m *memoryCatalogStore) ReadyNodeArtifacts(_ context.Context, _ string) ([]NodeArtifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]NodeArtifact(nil), m.artifacts...), nil
}

func (m *memoryCatalogStore) LookupImportOperation(_ context.Context, id string) (ImportOperationRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	operation, exists := m.operations[id]
	return operation, exists, nil
}

func (m *memoryCatalogStore) CommitImport(_ context.Context, prepared PreparedCommit) (ImportResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if operation, exists := m.operations[prepared.OperationID]; exists {
		if operation.RequestHash != prepared.RequestHash {
			return ImportResult{}, &Error{Code: CodeIdempotencyConflict}
		}
		result := operation.Result
		result.Replayed = true
		return result, nil
	}
	if !sameOptionalID(m.head, prepared.ExpectedParentRevisionID) {
		return ImportResult{}, &Error{Code: CodeRevisionConflict, CurrentRevisionID: cloneString(m.head), CurrentRevisionKnown: true}
	}
	if !prepared.Unchanged {
		m.revisions[prepared.Revision.ID] = prepared.Revision
		id := prepared.Revision.ID
		m.head = &id
		for _, snapshot := range prepared.Revision.Documents {
			m.documents[snapshot.Revision.DocumentID] = struct{}{}
			for _, node := range snapshot.Revision.Nodes {
				m.nodes[node.NodeID] = snapshot.Revision.DocumentID
			}
		}
	}
	result := ImportResult{Revision: prepared.Revision, Unchanged: prepared.Unchanged}
	m.operations[prepared.OperationID] = ImportOperationRecord{RequestHash: prepared.RequestHash, Result: result}
	return result, nil
}

type deterministicUUIDs struct {
	mu      sync.Mutex
	counter int
}

func (d *deterministicUUIDs) next() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counter++
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("knowledge-test-%d", d.counter))).String()
}

func testKnowledgeService(t *testing.T) (*Service, *memoryCatalogStore) {
	t.Helper()
	store := newMemoryCatalogStore()
	ids := &deterministicUUIDs{}
	service, err := NewService(store, NewCanonicalizer(), ServiceOptions{
		NewUUID: ids.next,
		Now:     func() time.Time { return time.Date(2026, 8, 19, 5, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func TestImportIsIdempotentAndMovePreservesRevisions(t *testing.T) {
	service, _ := testKnowledgeService(t)
	ctx := context.Background()
	firstCommand := ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "test", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{{Path: "notes/topic.md", Markdown: "# Topic\nStable body with enough useful words for a semantic section.\n"}},
	}
	first, err := service.Import(ctx, firstCommand)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision.RevisionNo != 1 || first.Unchanged || len(first.Revision.Documents) != 1 {
		t.Fatalf("unexpected first import: %+v", first)
	}
	replayed, err := service.Import(ctx, firstCommand)
	if err != nil || !replayed.Replayed || replayed.Revision.ID != first.Revision.ID {
		t.Fatalf("idempotent replay failed: result=%+v err=%v", replayed, err)
	}
	changedRequest := firstCommand
	changedRequest.Source = "different"
	if _, err := service.Import(ctx, changedRequest); ErrorCode(err) != CodeIdempotencyConflict {
		t.Fatalf("same operation with changed request = %v", err)
	}

	exported, err := service.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	move, err := service.Import(ctx, ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000002", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "move", ActorDeviceID: firstCommand.ActorDeviceID,
		Documents: []ImportDocument{{Path: "moved/topic.md", Markdown: exported.Documents[0].Markdown}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if move.Revision.RevisionNo != 2 || move.Revision.Documents[0].Path != "moved/topic.md" {
		t.Fatalf("move did not create expected snapshot: %+v", move)
	}
	before, after := first.Revision.Documents[0].Revision, move.Revision.Documents[0].Revision
	if before.DocumentID != after.DocumentID || before.ID != after.ID || before.Nodes[1].ID != after.Nodes[1].ID {
		t.Fatalf("pure move changed canonical identities: before=%+v after=%+v", before, after)
	}

	moveParent := move.Revision.ID
	unchanged, err := service.Import(ctx, ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000003", ExpectedParentRevisionID: &moveParent,
		ExpectedParentProvided: true, Source: "repeat", ActorDeviceID: firstCommand.ActorDeviceID,
		Documents: []ImportDocument{{Path: "moved/topic.md", Markdown: exported.Documents[0].Markdown}},
	})
	if err != nil || !unchanged.Unchanged || unchanged.Revision.ID != move.Revision.ID {
		t.Fatalf("unchanged import created a revision: result=%+v err=%v", unchanged, err)
	}
}

func TestMarkerlessAmbiguousEditRequiresReviewWithoutChangingHead(t *testing.T) {
	service, store := testKnowledgeService(t)
	ctx := context.Background()
	first, err := service.Import(ctx, ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000011", ExpectedParentProvided: true,
		Source: "test", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{{Path: "topic.md", Markdown: "# Topic\noriginal words one two three four five six seven eight\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	_, err = service.Import(ctx, ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000012", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "edit", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{{Path: "topic.md", Markdown: "# Topic renamed\ncompletely changed replacement text with several unrelated tokens here now\n"}},
	})
	var domainErr *Error
	if !asKnowledgeError(err, &domainErr) || domainErr.Code != CodeIdentityReviewRequired || domainErr.Review == nil || len(domainErr.Review.Documents) != 1 {
		t.Fatalf("expected document review, got %v", err)
	}
	head, err := store.Head(ctx)
	if err != nil || head.ID != first.Revision.ID {
		t.Fatalf("review changed the committed head: head=%+v err=%v", head, err)
	}
	if _, exists, _ := store.LookupImportOperation(ctx, "10000000-0000-4000-8000-000000000012"); exists {
		t.Fatal("review response persisted an import operation")
	}
}

func TestExplicitIdentityOrdinaryEditAndApprovedRewrite(t *testing.T) {
	service, store := testKnowledgeService(t)
	ctx := context.Background()
	actor := "90000000-0000-4000-8000-000000000001"
	first, err := service.Import(ctx, ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000031", ExpectedParentProvided: true,
		Source: "identity-fixture", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "identity.md", Markdown: "# Topic\none two three four five six seven eight nine ten.\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := service.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryMarkdown := strings.Replace(exported.Documents[0].Markdown, "# Topic\none two three four five six seven eight nine ten.", "# Renamed\none two three four five six seven eight nine ten eleven.", 1)
	parent := first.Revision.ID
	ordinary, err := service.Import(ctx, ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000032", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "ordinary-edit", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "identity.md", Markdown: ordinaryMarkdown}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, after := first.Revision.Documents[0].Revision, ordinary.Revision.Documents[0].Revision
	if before.DocumentID != after.DocumentID || before.Nodes[1].NodeID != after.Nodes[1].NodeID || before.ID == after.ID || before.Nodes[1].ID == after.Nodes[1].ID {
		t.Fatalf("ordinary edit identity/revision behavior is wrong: before=%+v after=%+v", before, after)
	}

	ordinaryExport, err := service.Export(ctx, ordinary.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	rewriteMarkdown := strings.Replace(ordinaryExport.Documents[0].Markdown, "one two three four five six seven eight nine ten eleven.", "unrelated replacement", 1)
	ordinaryParent := ordinary.Revision.ID
	reviewCommand := ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000033", ExpectedParentRevisionID: &ordinaryParent,
		ExpectedParentProvided: true, Source: "major-rewrite", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "identity.md", Markdown: rewriteMarkdown}},
	}
	_, err = service.Import(ctx, reviewCommand)
	var reviewErr *Error
	if !asKnowledgeError(err, &reviewErr) || reviewErr.Code != CodeIdentityReviewRequired || reviewErr.Review == nil || len(reviewErr.Review.Nodes) != 1 || len(reviewErr.Review.Nodes[0].Candidates) == 0 {
		t.Fatalf("major marked rewrite did not request review: %v", err)
	}
	review := reviewErr.Review.Nodes[0]
	reviewCommand.OperationID = "10000000-0000-4000-8000-000000000034"
	reviewCommand.IdentityReviewBasisHash = reviewErr.Review.BasisHash
	reviewCommand.IdentityReviewOperationID = reviewErr.Review.OperationID
	reviewCommand.IdentityReviewReceipt = reviewErr.Review.Receipt
	reviewCommand.NodeResolutions = []NodeResolution{{
		Locator: review.Locator, Action: "rewrite",
		SourceNodeRevisionIDs: []string{review.Candidates[0].RevisionID}, Reason: "section semantics replaced",
	}}
	rewritten, err := service.Import(ctx, reviewCommand)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewritten.Revision.Lineages) != 1 || rewritten.Revision.Lineages[0].Action != "rewrite" || rewritten.Revision.Documents[0].Revision.Nodes[1].NodeID == after.Nodes[1].NodeID {
		t.Fatalf("approved rewrite did not create a new identity and lineage: %+v", rewritten.Revision)
	}
	stored, err := store.Revision(ctx, rewritten.Revision.ID)
	if err != nil || len(stored.Lineages) != 1 {
		t.Fatalf("lineage is not queryable from revision tree: revision=%+v err=%v", stored, err)
	}
}

func TestIdentityReviewReceiptRequiresFreshOperation(t *testing.T) {
	service, store := testKnowledgeService(t)
	actor := "90000000-0000-4000-8000-000000000001"
	first, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000191", ExpectedParentProvided: true,
		Source: "receipt baseline", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "receipt.md", Markdown: "# Topic\none two three four five six seven eight nine ten\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := service.Export(t.Context(), first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	reviewCommand := ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000192", ExpectedParentRevisionID: &parent, ExpectedParentProvided: true,
		Source: "receipt review", ActorDeviceID: actor,
		Documents: []ImportDocument{{Path: "receipt.md", Markdown: strings.Replace(exported.Documents[0].Markdown, "one two three four five six seven eight nine ten", "quartz nickel tungsten cobalt radon xenon boron neon", 1)}},
	}
	_, err = service.Import(t.Context(), reviewCommand)
	var reviewErr *Error
	if !asKnowledgeError(err, &reviewErr) || reviewErr.Code != CodeIdentityReviewRequired || reviewErr.Review == nil || len(reviewErr.Review.Nodes) != 1 {
		t.Fatalf("expected identity review, got %v", err)
	}
	review := reviewErr.Review
	if review.OperationID != reviewCommand.OperationID || review.Receipt != identityReviewReceipt(review.BasisHash, reviewCommand.OperationID) {
		t.Fatalf("review receipt does not bind its operation: %+v", review)
	}
	if _, exists, err := store.LookupImportOperation(t.Context(), reviewCommand.OperationID); err != nil || exists {
		t.Fatalf("review persisted an operation: exists=%v err=%v", exists, err)
	}
	resolved := reviewCommand
	resolved.OperationID = "10000000-0000-4000-8000-000000000193"
	resolved.IdentityReviewBasisHash = review.BasisHash
	resolved.IdentityReviewOperationID = review.OperationID
	resolved.IdentityReviewReceipt = review.Receipt
	resolved.NodeResolutions = []NodeResolution{{
		Locator: review.Nodes[0].Locator, Action: "rewrite", SourceNodeRevisionIDs: []string{review.Nodes[0].Candidates[0].RevisionID}, Reason: "semantic replacement",
	}}
	for _, test := range []struct {
		name   string
		mutate func(*ImportCommand)
	}{
		{name: "missing receipt", mutate: func(command *ImportCommand) { command.IdentityReviewReceipt = "" }},
		{name: "wrong receipt", mutate: func(command *ImportCommand) { command.IdentityReviewReceipt = strings.Repeat("0", 64) }},
		{name: "wrong basis", mutate: func(command *ImportCommand) { command.IdentityReviewBasisHash = strings.Repeat("0", 64) }},
		{name: "reused review operation", mutate: func(command *ImportCommand) { command.OperationID = review.OperationID }},
		{name: "invented review operation", mutate: func(command *ImportCommand) {
			command.IdentityReviewOperationID = "20000000-0000-4000-8000-000000000194"
			command.IdentityReviewReceipt = identityReviewReceipt(command.IdentityReviewBasisHash, command.IdentityReviewOperationID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := resolved
			test.mutate(&invalid)
			if _, err := service.Import(t.Context(), invalid); ErrorCode(err) != CodeStaleIdentityReview {
				t.Fatalf("invalid review retry error = %v", err)
			}
			head, err := service.Head(t.Context())
			if err != nil || head == nil || head.ID != first.Revision.ID {
				t.Fatalf("invalid review retry changed head: head=%+v err=%v", head, err)
			}
		})
	}
	committed, err := service.Import(t.Context(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	modifiedReceipt := resolved
	modifiedReceipt.IdentityReviewReceipt = strings.Repeat("f", 64)
	if _, err := service.Import(t.Context(), modifiedReceipt); ErrorCode(err) != CodeIdempotencyConflict {
		t.Fatalf("receipt is absent from idempotency request hash: %v", err)
	}
	if committed.Revision.ID == first.Revision.ID {
		t.Fatal("approved review did not create a new revision")
	}
}

func TestIdentityReorderDuplicateTemplateAndMerge(t *testing.T) {
	actor := "90000000-0000-4000-8000-000000000001"
	t.Run("reorder preserves stable IDs with new revisions", func(t *testing.T) {
		service, _ := testKnowledgeService(t)
		first, err := service.Import(t.Context(), ImportCommand{
			OperationID: "10000000-0000-4000-8000-000000000201", ExpectedParentProvided: true, Source: "reorder base", ActorDeviceID: actor,
			Documents: []ImportDocument{{Path: "reorder.md", Markdown: "# Alpha\nalpha beta gamma delta epsilon zeta eta theta\n\n# Beta\niota kappa lambda mu nu xi omicron pi\n"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		exported, err := service.Export(t.Context(), first.Revision.ID)
		if err != nil {
			t.Fatal(err)
		}
		prefix, body := splitExportedMarkdown(t, exported.Documents[0].Markdown)
		betaMarker := fmt.Sprintf("<!-- edu-agent-node:v1 {\"id\":\"%s\"} -->", first.Revision.Documents[0].Revision.Nodes[2].NodeID)
		betaStart := strings.Index(body, betaMarker)
		if betaStart < 0 {
			t.Fatal("missing Beta marker")
		}
		parent := first.Revision.ID
		second, err := service.Import(t.Context(), ImportCommand{
			OperationID: "10000000-0000-4000-8000-000000000202", ExpectedParentRevisionID: &parent, ExpectedParentProvided: true, Source: "reorder", ActorDeviceID: actor,
			Documents: []ImportDocument{{Path: "reorder.md", Markdown: prefix + body[betaStart:] + body[:betaStart]}},
		})
		if err != nil {
			t.Fatal(err)
		}
		before, after := first.Revision.Documents[0].Revision, second.Revision.Documents[0].Revision
		if before.ID == after.ID || before.Nodes[2].NodeID != after.Nodes[1].NodeID || before.Nodes[1].NodeID != after.Nodes[2].NodeID || before.Nodes[2].ID == after.Nodes[1].ID || before.Nodes[1].ID == after.Nodes[2].ID {
			t.Fatalf("reorder identity/revisions are wrong: before=%+v after=%+v", before.Nodes, after.Nodes)
		}
	})
	t.Run("duplicate template requires review and leaves head", func(t *testing.T) {
		service, _ := testKnowledgeService(t)
		first, err := service.Import(t.Context(), ImportCommand{
			OperationID: "10000000-0000-4000-8000-000000000203", ExpectedParentProvided: true, Source: "template base", ActorDeviceID: actor,
			Documents: []ImportDocument{{Path: "template.md", Markdown: "# Original\nalpha beta gamma delta epsilon zeta eta theta\n"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		exported, err := service.Export(t.Context(), first.Revision.ID)
		if err != nil {
			t.Fatal(err)
		}
		prefix, _ := splitExportedMarkdown(t, exported.Documents[0].Markdown)
		parent := first.Revision.ID
		_, err = service.Import(t.Context(), ImportCommand{
			OperationID: "10000000-0000-4000-8000-000000000204", ExpectedParentRevisionID: &parent, ExpectedParentProvided: true, Source: "template duplicate", ActorDeviceID: actor,
			Documents: []ImportDocument{{Path: "template.md", Markdown: prefix + "# Copy\nalpha beta gamma delta epsilon zeta eta theta\n\n# Copy\nalpha beta gamma delta epsilon zeta eta theta\n"}},
		})
		var reviewErr *Error
		if !asKnowledgeError(err, &reviewErr) || reviewErr.Code != CodeIdentityReviewRequired || reviewErr.Review == nil || len(reviewErr.Review.Nodes) != 2 {
			t.Fatalf("duplicate template did not request review: %v", err)
		}
		head, err := service.Head(t.Context())
		if err != nil || head == nil || head.ID != first.Revision.ID {
			t.Fatalf("duplicate template changed head: head=%+v err=%v", head, err)
		}
	})
	t.Run("merge creates one approved lineage group", func(t *testing.T) {
		service, _ := testKnowledgeService(t)
		first, err := service.Import(t.Context(), ImportCommand{
			OperationID: "10000000-0000-4000-8000-000000000205", ExpectedParentProvided: true, Source: "merge base", ActorDeviceID: actor,
			Documents: []ImportDocument{{Path: "merge.md", Markdown: "# One\nalpha beta gamma delta epsilon zeta eta theta\n\n# Two\niota kappa lambda mu nu xi omicron pi\n"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		exported, err := service.Export(t.Context(), first.Revision.ID)
		if err != nil {
			t.Fatal(err)
		}
		prefix, _ := splitExportedMarkdown(t, exported.Documents[0].Markdown)
		parent := first.Revision.ID
		command := ImportCommand{
			OperationID: "10000000-0000-4000-8000-000000000206", ExpectedParentRevisionID: &parent, ExpectedParentProvided: true, Source: "merge review", ActorDeviceID: actor,
			Documents: []ImportDocument{{Path: "merge.md", Markdown: prefix + "# Combined\nalpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi\n"}},
		}
		_, err = service.Import(t.Context(), command)
		var reviewErr *Error
		if !asKnowledgeError(err, &reviewErr) || reviewErr.Review == nil || len(reviewErr.Review.Nodes) != 1 || len(reviewErr.Review.Nodes[0].Candidates) != 2 {
			t.Fatalf("merge did not produce one two-source review: %v", err)
		}
		head, err := service.Head(t.Context())
		if err != nil || head == nil || head.ID != first.Revision.ID {
			t.Fatalf("merge review changed head: head=%+v err=%v", head, err)
		}
		command.OperationID = "10000000-0000-4000-8000-000000000207"
		command.IdentityReviewBasisHash = reviewErr.Review.BasisHash
		command.IdentityReviewOperationID = reviewErr.Review.OperationID
		command.IdentityReviewReceipt = reviewErr.Review.Receipt
		command.NodeResolutions = []NodeResolution{{
			Locator: reviewErr.Review.Nodes[0].Locator, Action: "merge", Reason: "combined two sections",
			SourceNodeRevisionIDs: []string{reviewErr.Review.Nodes[0].Candidates[0].RevisionID, reviewErr.Review.Nodes[0].Candidates[1].RevisionID},
		}}
		merged, err := service.Import(t.Context(), command)
		if err != nil {
			t.Fatal(err)
		}
		if len(merged.Revision.Lineages) != 1 || len(merged.Revision.Lineages[0].Members) != 3 || merged.Revision.Lineages[0].Action != "merge" || merged.Revision.Documents[0].Revision.Nodes[1].NodeID == first.Revision.Documents[0].Revision.Nodes[1].NodeID || merged.Revision.Documents[0].Revision.Nodes[1].NodeID == first.Revision.Documents[0].Revision.Nodes[2].NodeID {
			t.Fatalf("merge lineage/target identity is wrong: %+v", merged.Revision)
		}
	})
}

func splitExportedMarkdown(t *testing.T, markdown string) (string, string) {
	t.Helper()
	if !strings.HasPrefix(markdown, "---\n") {
		t.Fatalf("export is missing frontmatter: %q", markdown)
	}
	closing := strings.Index(markdown[len("---\n"):], "---\n")
	if closing < 0 {
		t.Fatalf("export is missing closing frontmatter: %q", markdown)
	}
	bodyStart := len("---\n") + closing + len("---\n")
	return markdown[:bodyStart], markdown[bodyStart:]
}

func TestRetrievalUsesGlobalFIFOAcrossDocumentRoots(t *testing.T) {
	service, _ := testKnowledgeService(t)
	result, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000211", ExpectedParentProvided: true, Source: "global bfs", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{
			{Path: "a.md", Markdown: "# A\n\n## A Child\nneedle\n\n### A Grandchild\nneedle\n"},
			{Path: "b.md", Markdown: "# B\n\n## B Child\nneedle\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	retrieved, err := service.Retrieve(t.Context(), RetrievalCommand{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(retrieved.DocumentShortlist) != 2 || retrieved.DocumentShortlist[0] != "a.md" || retrieved.DocumentShortlist[1] != "b.md" {
		t.Fatalf("unexpected document shortlist: %+v", retrieved.DocumentShortlist)
	}
	var titles []string
	for _, trace := range retrieved.Trace {
		for _, document := range result.Revision.Documents {
			for _, node := range document.Revision.Nodes {
				if node.ID == trace.ParentNodeRevisionID {
					titles = append(titles, node.Title)
				}
			}
		}
	}
	if len(titles) < 4 || !reflect.DeepEqual(titles[:4], []string{"", "", "A", "B"}) {
		t.Fatalf("retrieval did not use global FIFO depth order: %v trace=%+v", titles, retrieved.Trace)
	}
}

func TestRetrievalTraversesZeroScoreShortlistWithoutIrrelevantHits(t *testing.T) {
	service, _ := testKnowledgeService(t)
	imported, err := service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000212", ExpectedParentProvided: true, Source: "zero shortlist", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{
			{Path: "match.md", Markdown: "# Match\n\n## Needle\nneedle body\n"},
			{Path: "unrelated.md", Markdown: "# Unrelated\n\n## Other\nother body\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Retrieve(t.Context(), RetrievalCommand{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Trace) < 2 {
		t.Fatalf("zero-score shortlist document was not traversed: %+v", result.Trace)
	}
	unrelatedRoot := imported.Revision.Documents[1].Revision.Nodes[0].ID
	traversed := false
	for _, trace := range result.Trace {
		if trace.ParentNodeRevisionID == unrelatedRoot {
			traversed = true
			break
		}
	}
	if !traversed {
		t.Fatalf("unrelated document root was not present in global BFS trace: %+v", result.Trace)
	}
	for _, hit := range result.Hits {
		if hit.Path == "unrelated.md" {
			t.Fatalf("zero-score document produced irrelevant hit: %+v", hit)
		}
	}
}

func TestFixedCorpusLexicalRetrieval(t *testing.T) {
	service, _ := testKnowledgeService(t)
	result, err := service.Import(context.Background(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000021", ExpectedParentProvided: true,
		Source: "fixture", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{
			{Path: "go/concurrency.md", Markdown: "# Concurrency\n\n## Goroutine\nRuns work.\n\n## Channel\nPasses channel values.\n"},
			{Path: "db/index.md", Markdown: "# Database Index\nLookup structures.\n"},
			{Path: "systems.md", Markdown: "# Systems\n\n## Queue Overview\nA queue overview.\n\n## Messaging\nTransport.\n\n### Queue Types\nQueue types.\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := service.Retrieve(context.Background(), RetrievalCommand{Query: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	if len(channel.DocumentShortlist) == 0 || channel.DocumentShortlist[0] != "go/concurrency.md" || !channel.Degraded {
		t.Fatalf("unexpected channel shortlist/degraded state: %+v", channel)
	}
	if hitTitle(result.Revision, channel.Hits[0].NodeRevisionID) != "Channel" {
		t.Fatalf("first channel hit was not Channel: %+v", channel.Hits)
	}
	foundLayer := false
	for _, trace := range channel.Trace {
		if len(trace.Candidates) >= 2 && trace.Candidates[0].Title == "Channel" && trace.Candidates[1].Title == "Goroutine" {
			foundLayer = trace.Decisions[0].Action == "select" && trace.ReasonCode == "selector_not_configured"
		}
	}
	if !foundLayer {
		t.Fatalf("Channel/Goroutine layer did not use deterministic fallback: %+v", channel.Trace)
	}
	queue, err := service.Retrieve(context.Background(), RetrievalCommand{Query: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Hits) < 2 || hitTitle(result.Revision, queue.Hits[0].NodeRevisionID) != "Queue Overview" || hitTitle(result.Revision, queue.Hits[1].NodeRevisionID) != "Queue Types" {
		t.Fatalf("unexpected queue hit order: %+v", queue.Hits)
	}
}

func hitTitle(revision KnowledgeRevision, nodeRevisionID string) string {
	for _, document := range revision.Documents {
		for _, node := range document.Revision.Nodes {
			if node.ID == nodeRevisionID {
				return node.Title
			}
		}
	}
	return ""
}

func asKnowledgeError(err error, target **Error) bool {
	for err != nil {
		if typed, ok := err.(*Error); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		unwrapped, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
