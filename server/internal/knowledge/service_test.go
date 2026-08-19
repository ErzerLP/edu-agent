package knowledge

import (
	"context"
	"fmt"
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
