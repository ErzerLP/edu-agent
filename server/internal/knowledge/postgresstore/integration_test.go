package postgresstore_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const integrationActorID = "90000000-0000-4000-8000-000000000001"

type barrierStore struct {
	*postgresstore.Store
	reached *sync.WaitGroup
	release <-chan struct{}
}

func (s *barrierStore) CommitImport(ctx context.Context, prepared knowledge.PreparedCommit) (knowledge.ImportResult, error) {
	s.reached.Done()
	select {
	case <-s.release:
	case <-ctx.Done():
		return knowledge.ImportResult{}, ctx.Err()
	}
	return s.Store.CommitImport(ctx, prepared)
}

func TestPostgreSQLKnowledgeCoreAtomicSnapshots(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL knowledge integration test not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("knowledge_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Check(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := postgresstore.New(pool)
	service, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}

	firstCommand := knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "postgres-integration", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{
			{Path: "a.md", Markdown: "# Alpha\nalpha-only material\n"},
			{Path: "b.md", Markdown: "# Beta\nbeta-only material\n"},
		},
	}
	first, err := service.Import(ctx, firstCommand)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision.RevisionNo != 1 || len(first.Revision.Documents) != 2 {
		t.Fatalf("unexpected first snapshot: %+v", first)
	}
	replay, err := service.Import(ctx, firstCommand)
	if err != nil || !replay.Replayed || replay.Revision.ID != first.Revision.ID {
		t.Fatalf("operation replay failed: result=%+v err=%v", replay, err)
	}
	changed := firstCommand
	changed.Source = "different-request"
	if _, err := service.Import(ctx, changed); knowledge.ErrorCode(err) != knowledge.CodeIdempotencyConflict {
		t.Fatalf("idempotency conflict = %v", err)
	}

	exportedFirst, err := service.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	second, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000002", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "incremental-move", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "moved/a.md", Markdown: exportedFirst.Documents[0].Markdown}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision.RevisionNo != 2 || len(second.Revision.Documents) != 2 {
		t.Fatalf("incremental snapshot did not carry omitted documents: %+v", second.Revision)
	}
	oldExport, err := service.Export(ctx, first.Revision.ID)
	if err != nil || oldExport.Documents[0].Path != "a.md" || oldExport.Documents[1].Path != "b.md" {
		t.Fatalf("old revision export changed: result=%+v err=%v", oldExport, err)
	}
	canonicalizer := knowledge.NewCanonicalizer()
	for _, snapshot := range second.Revision.Documents {
		rebuilt, err := canonicalizer.Index(snapshot.Revision.CanonicalMarkdown)
		if err != nil || rebuilt.ID != snapshot.Revision.ID {
			t.Fatalf("full rebuild differs for %s: rebuilt=%s stored=%s err=%v", snapshot.Path, rebuilt.ID, snapshot.Revision.ID, err)
		}
	}

	newIdentityMarkdown := "---\nedu-agent-format: 1\nedu-agent-document-id: 70000000-0000-4000-8000-000000000001\nedu-agent-root-node-id: 70000000-0000-4000-8000-000000000002\n---\n<!-- edu-agent-node:v1 {\"id\":\"70000000-0000-4000-8000-000000000003\"} -->\n# Replacement\nnew identity\n"
	secondParent := second.Revision.ID
	if _, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000003", ExpectedParentRevisionID: &secondParent,
		ExpectedParentProvided: true, Source: "occupied", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "b.md", Markdown: newIdentityMarkdown}},
	}); knowledge.ErrorCode(err) != knowledge.CodePathOccupied {
		t.Fatalf("occupied path = %v", err)
	}

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION reject_failed_knowledge_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.canonical_path = 'fail.md' THEN RAISE EXCEPTION 'injected knowledge failure'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_failed_knowledge_snapshot
		BEFORE INSERT ON knowledge_snapshot_documents
		FOR EACH ROW EXECUTE FUNCTION reject_failed_knowledge_snapshot()`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000004", ExpectedParentRevisionID: &secondParent,
		ExpectedParentProvided: true, Source: "fault", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "fail.md", Markdown: "# Failure\nfault-injection-unique\n"}},
	}); err == nil {
		t.Fatal("fault injection import unexpectedly succeeded")
	}
	var headID string
	if err := pool.QueryRow(ctx, `SELECT head_revision_id FROM knowledge_catalog WHERE singleton_id=1`).Scan(&headID); err != nil || headID != second.Revision.ID {
		t.Fatalf("failed import changed head: head=%s err=%v", headID, err)
	}
	var failedOperations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_import_operations WHERE operation_id='10000000-0000-4000-8000-000000000004'`).Scan(&failedOperations); err != nil || failedOperations != 0 {
		t.Fatalf("failed import operation persisted: count=%d err=%v", failedOperations, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_failed_knowledge_snapshot ON knowledge_snapshot_documents; DROP FUNCTION reject_failed_knowledge_snapshot()`); err != nil {
		t.Fatal(err)
	}

	var reached sync.WaitGroup
	reached.Add(2)
	release := make(chan struct{})
	concurrentStore := &barrierStore{Store: store, reached: &reached, release: release}
	concurrentService, err := knowledge.NewService(concurrentStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result knowledge.ImportResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for index, document := range []knowledge.ImportDocument{
		{Path: "concurrent-one.md", Markdown: "# Xenolith\nxenolith-exclusive\n"},
		{Path: "concurrent-two.md", Markdown: "# Ytterbium\nytterbium-exclusive\n"},
	} {
		go func(index int, document knowledge.ImportDocument) {
			result, err := concurrentService.Import(ctx, knowledge.ImportCommand{
				OperationID:              fmt.Sprintf("10000000-0000-4000-8000-%012d", 10+index),
				ExpectedParentRevisionID: &secondParent, ExpectedParentProvided: true,
				Source: "concurrent", ActorDeviceID: integrationActorID, Documents: []knowledge.ImportDocument{document},
			})
			outcomes <- outcome{result: result, err: err}
		}(index, document)
	}
	waited := make(chan struct{})
	go func() { reached.Wait(); close(waited) }()
	select {
	case <-waited:
		close(release)
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent imports did not both reach commit")
	}
	successes, conflicts := 0, 0
	for range 2 {
		outcome := <-outcomes
		switch {
		case outcome.err == nil:
			successes++
		case knowledge.ErrorCode(outcome.err) == knowledge.CodeRevisionConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent import outcome: %+v", outcome)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent parent results: successes=%d conflicts=%d", successes, conflicts)
	}
}
