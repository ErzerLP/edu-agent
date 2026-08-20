package postgresstore_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type concurrentImportOutcome struct {
	result knowledge.ImportResult
	err    error
}

func newReviewerPostgresHarness(t *testing.T) (context.Context, *pgxpool.Pool, *postgresstore.Store, *knowledge.Service) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL reviewer regression test not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("knowledge_reviewer_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
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
	return ctx, pool, store, service
}

func reviewerIdentityMarkdown(documentID, rootNodeID, headingNodeID, title, body string) string {
	return fmt.Sprintf("---\nedu-agent-format: 1\nedu-agent-document-id: %s\nedu-agent-root-node-id: %s\n---\n<!-- edu-agent-node:v1 {\"id\":\"%s\"} -->\n# %s\n%s\n", documentID, rootNodeID, headingNodeID, title, body)
}

func runConcurrentImports(t *testing.T, ctx context.Context, store *postgresstore.Store, commands []knowledge.ImportCommand) []concurrentImportOutcome {
	t.Helper()
	if len(commands) != 2 {
		t.Fatalf("concurrent import fixture requires two commands, got %d", len(commands))
	}
	var reached sync.WaitGroup
	reached.Add(len(commands))
	release := make(chan struct{})
	concurrentStore := &barrierStore{Store: store, reached: &reached, release: release}
	service, err := knowledge.NewService(concurrentStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := make(chan concurrentImportOutcome, len(commands))
	for _, command := range commands {
		command := command
		go func() {
			result, err := service.Import(ctx, command)
			outcomes <- concurrentImportOutcome{result: result, err: err}
		}()
	}
	waited := make(chan struct{})
	go func() { reached.Wait(); close(waited) }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-waited:
		close(release)
	case outcome := <-outcomes:
		close(release)
		t.Fatalf("concurrent import returned before CommitImport: result=%+v err=%v", outcome.result, outcome.err)
	case <-timer.C:
		close(release)
		t.Fatal("concurrent imports did not both reach CommitImport")
	}
	results := make([]concurrentImportOutcome, 0, len(commands))
	for range commands {
		results = append(results, <-outcomes)
	}
	return results
}

func TestPostgreSQLKnowledgeCoreReviewerRegressions(t *testing.T) {
	ctx, pool, store, service := newReviewerPostgresHarness(t)
	actor := integrationActorID
	first, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000201", ExpectedParentProvided: true,
		Source: "reviewer-regressions", ActorDeviceID: actor,
		Documents: []knowledge.ImportDocument{
			{Path: "swap-a.md", Markdown: "# Alpha\nalpha body\n"},
			{Path: "swap-b.md", Markdown: "# Beta\nbeta body\n"},
			{Path: "Résumé.md", Markdown: "# Resume\nresume body\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := service.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	markdownByPath := map[string]string{}
	documentIDByPath := map[string]string{}
	for _, document := range exported.Documents {
		markdownByPath[document.Path] = document.Markdown
	}
	for _, document := range first.Revision.Documents {
		documentIDByPath[document.Path] = document.Revision.DocumentID
	}
	parent := first.Revision.ID
	swapped, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000202", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "path-swap", ActorDeviceID: actor,
		Documents: []knowledge.ImportDocument{
			{Path: "swap-a.md", Markdown: markdownByPath["swap-b.md"]},
			{Path: "swap-b.md", Markdown: markdownByPath["swap-a.md"]},
		},
	})
	if err != nil {
		t.Fatalf("PostgreSQL path swap failed: %v", err)
	}
	swappedIDs := map[string]string{}
	for _, document := range swapped.Revision.Documents {
		swappedIDs[document.Path] = document.Revision.DocumentID
	}
	if swappedIDs["swap-a.md"] != documentIDByPath["swap-b.md"] || swappedIDs["swap-b.md"] != documentIDByPath["swap-a.md"] {
		t.Fatalf("PostgreSQL path swap identities = %v", swappedIDs)
	}
	var persistedFolded string
	if err := pool.QueryRow(ctx, `
		SELECT folded_path FROM knowledge_snapshot_documents
		WHERE knowledge_revision_id=$1 AND canonical_path='Résumé.md'`, swapped.Revision.ID).Scan(&persistedFolded); err != nil {
		t.Fatal(err)
	}
	if persistedFolded != knowledge.FoldPath("Résumé.md") {
		t.Fatalf("persisted folded path = %q, want %q", persistedFolded, knowledge.FoldPath("Résumé.md"))
	}
	foldedParent := swapped.Revision.ID
	foldedConflict := "---\nedu-agent-format: 1\nedu-agent-document-id: 70000000-0000-4000-8000-000000000201\nedu-agent-root-node-id: 70000000-0000-4000-8000-000000000202\n---\nnew resume"
	if _, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000203", ExpectedParentRevisionID: &foldedParent,
		ExpectedParentProvided: true, Source: "folded-conflict", ActorDeviceID: actor,
		Documents: []knowledge.ImportDocument{{Path: "re\u0301sume\u0301.MD", Markdown: foldedConflict}},
	}); knowledge.ErrorCode(err) != knowledge.CodePathOccupied {
		t.Fatalf("PostgreSQL folded path conflict = %v", err)
	}

	sameParent := swapped.Revision.ID
	sameCommand := knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000211", ExpectedParentRevisionID: &sameParent,
		ExpectedParentProvided: true, Source: "same-operation", ActorDeviceID: actor,
		Documents: []knowledge.ImportDocument{{Path: "same-operation.md", Markdown: reviewerIdentityMarkdown(
			"70000000-0000-4000-8000-000000000301", "70000000-0000-4000-8000-000000000302", "70000000-0000-4000-8000-000000000303", "Same", "same operation body")}},
	}
	sameOutcomes := runConcurrentImports(t, ctx, store, []knowledge.ImportCommand{sameCommand, sameCommand})
	replays := 0
	for _, outcome := range sameOutcomes {
		if outcome.err != nil {
			t.Fatalf("same-hash concurrent operation failed: %v", outcome.err)
		}
		if outcome.result.Replayed {
			replays++
		}
	}
	if replays != 1 || sameOutcomes[0].result.Revision.ID != sameOutcomes[1].result.Revision.ID {
		t.Fatalf("same-hash concurrency did not replay one stored result: %+v", sameOutcomes)
	}

	head, err := service.Head(ctx)
	if err != nil || head == nil {
		t.Fatalf("head after same operation: %+v err=%v", head, err)
	}
	differentParent := head.ID
	differentBase := knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000212", ExpectedParentRevisionID: &differentParent,
		ExpectedParentProvided: true, Source: "different-hash", ActorDeviceID: actor,
	}
	left, right := differentBase, differentBase
	left.Documents = []knowledge.ImportDocument{{Path: "different-left.md", Markdown: reviewerIdentityMarkdown(
		"70000000-0000-4000-8000-000000000311", "70000000-0000-4000-8000-000000000312", "70000000-0000-4000-8000-000000000313", "Left", "left body")}}
	right.Documents = []knowledge.ImportDocument{{Path: "different-right.md", Markdown: reviewerIdentityMarkdown(
		"70000000-0000-4000-8000-000000000321", "70000000-0000-4000-8000-000000000322", "70000000-0000-4000-8000-000000000323", "Right", "right body")}}
	differentOutcomes := runConcurrentImports(t, ctx, store, []knowledge.ImportCommand{left, right})
	successes, idempotencyConflicts := 0, 0
	for _, outcome := range differentOutcomes {
		switch {
		case outcome.err == nil:
			successes++
		case knowledge.ErrorCode(outcome.err) == knowledge.CodeIdempotencyConflict:
			idempotencyConflicts++
		default:
			t.Fatalf("different-hash same-operation result = %+v", outcome)
		}
	}
	if successes != 1 || idempotencyConflicts != 1 {
		t.Fatalf("different-hash same-operation outcomes: success=%d conflict=%d", successes, idempotencyConflicts)
	}

	head, err = service.Head(ctx)
	if err != nil || head == nil {
		t.Fatalf("head before unchanged concurrency: %+v err=%v", head, err)
	}
	headExport, err := service.Export(ctx, head.ID)
	if err != nil {
		t.Fatal(err)
	}
	unchangedParent := head.ID
	unchangedCommand := knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000213", ExpectedParentRevisionID: &unchangedParent,
		ExpectedParentProvided: true, Source: "unchanged-concurrent", ActorDeviceID: actor,
		Documents: []knowledge.ImportDocument{{Path: headExport.Documents[0].Path, Markdown: headExport.Documents[0].Markdown}},
	}
	unchangedOutcomes := runConcurrentImports(t, ctx, store, []knowledge.ImportCommand{unchangedCommand, unchangedCommand})
	replays = 0
	for _, outcome := range unchangedOutcomes {
		if outcome.err != nil || !outcome.result.Unchanged || outcome.result.Revision.ID != unchangedParent {
			t.Fatalf("unchanged concurrent import result = %+v", outcome)
		}
		if outcome.result.Replayed {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("unchanged concurrent replay count = %d", replays)
	}

	head, err = service.Head(ctx)
	if err != nil || head == nil {
		t.Fatal(err)
	}
	lineageParent := head.ID
	lineageBase, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000221", ExpectedParentRevisionID: &lineageParent,
		ExpectedParentProvided: true, Source: "lineage-base", ActorDeviceID: actor,
		Documents: []knowledge.ImportDocument{{Path: "lineage.md", Markdown: "# Lineage\nalpha beta gamma delta epsilon zeta eta theta\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lineageExport, err := service.Export(ctx, lineageBase.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lineageMarkdown string
	for _, document := range lineageExport.Documents {
		if document.Path == "lineage.md" {
			lineageMarkdown = document.Markdown
			break
		}
	}
	if lineageMarkdown == "" {
		t.Fatal("lineage export document is missing")
	}
	lineageMarkdown = strings.Replace(lineageMarkdown, "alpha beta gamma delta epsilon zeta eta theta", "quartz nickel tungsten cobalt radon xenon boron neon", 1)
	lineageReviewParent := lineageBase.Revision.ID
	lineageCommand := knowledge.ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000222", ExpectedParentRevisionID: &lineageReviewParent,
		ExpectedParentProvided: true, Source: "lineage-rewrite", ActorDeviceID: actor,
		Documents: []knowledge.ImportDocument{{Path: "lineage.md", Markdown: lineageMarkdown}},
	}
	_, err = service.Import(ctx, lineageCommand)
	var reviewErr *knowledge.Error
	if !asPostgresKnowledgeError(err, &reviewErr) || reviewErr.Review == nil || len(reviewErr.Review.Nodes) != 1 || len(reviewErr.Review.Nodes[0].Candidates) != 1 {
		t.Fatalf("PostgreSQL rewrite review = %v", err)
	}
	lineageCommand.OperationID = "10000000-0000-4000-8000-000000000223"
	lineageCommand.IdentityReviewBasisHash = reviewErr.Review.BasisHash
	lineageCommand.IdentityReviewOperationID = reviewErr.Review.OperationID
	lineageCommand.IdentityReviewReceipt = reviewErr.Review.Receipt
	lineageCommand.NodeResolutions = []knowledge.NodeResolution{{
		Locator: reviewErr.Review.Nodes[0].Locator, Action: "rewrite",
		SourceNodeRevisionIDs: []string{reviewErr.Review.Nodes[0].Candidates[0].RevisionID}, Reason: "semantic rewrite",
	}}
	lineageFirst, err := service.Import(ctx, lineageCommand)
	if err != nil {
		t.Fatal(err)
	}
	lineageReplay, err := service.Import(ctx, lineageCommand)
	if err != nil || !lineageReplay.Replayed || !reflect.DeepEqual(lineageFirst.Revision.Lineages, lineageReplay.Revision.Lineages) {
		t.Fatalf("PostgreSQL lineage replay differs: first=%+v replay=%+v err=%v", lineageFirst.Revision.Lineages, lineageReplay.Revision.Lineages, err)
	}
	if len(lineageFirst.Revision.Lineages) != 1 || lineageFirst.Revision.Lineages[0].KnowledgeRevisionID != lineageFirst.Revision.ID {
		t.Fatalf("lineage is not bound to its creating revision: %+v", lineageFirst.Revision.Lineages)
	}

	var artifactNodeRevisionID string
	if err := pool.QueryRow(ctx, `
		SELECT nr.id
		FROM knowledge_snapshot_documents sd
		JOIN knowledge_node_revisions nr ON nr.document_revision_id=sd.document_revision_id
		WHERE sd.knowledge_revision_id=$1 AND sd.canonical_path='lineage.md' AND nr.parent_node_revision_id IS NOT NULL
		ORDER BY nr.sibling_index LIMIT 1`, lineageFirst.Revision.ID).Scan(&artifactNodeRevisionID); err != nil {
		t.Fatal(err)
	}
	artifactID := "40000000-0000-4000-8000-000000000201"
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_node_artifacts(
			id, node_revision_id, kind, producer_version, prompt_version, model_version,
			input_hash, content, status, created_at
		) VALUES ($1,$2,'summary','producer-v1','prompt-v1','model-v1',decode(repeat('ab',32),'hex'),'database pinned summary','ready',now())`,
		artifactID, artifactNodeRevisionID); err != nil {
		t.Fatal(err)
	}
	artifactRevision := lineageFirst.Revision.ID
	retrieved, err := service.Retrieve(ctx, knowledge.RetrievalCommand{KnowledgeRevisionID: &artifactRevision, Query: "lineage quartz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(retrieved.SummarySnapshot) != 1 || retrieved.SummarySnapshot[0] != artifactID {
		t.Fatalf("PostgreSQL ready artifact snapshot = %+v", retrieved.SummarySnapshot)
	}
	artifactBound := false
	for _, trace := range retrieved.Trace {
		for _, candidate := range trace.Candidates {
			if candidate.NodeRevisionID == artifactNodeRevisionID && candidate.SummaryArtifactID == artifactID {
				artifactBound = true
			}
		}
	}
	if !artifactBound {
		t.Fatalf("PostgreSQL artifact was not bound to a candidate: %+v", retrieved.Trace)
	}

	assertPostgreSQLOwnershipConstraints(t, ctx, pool, lineageFirst.Revision.ID)
}

func asPostgresKnowledgeError(err error, target **knowledge.Error) bool {
	if err == nil {
		return false
	}
	value, ok := err.(*knowledge.Error)
	if ok {
		*target = value
	}
	return ok
}

type postgresDocumentOwner struct {
	documentID       string
	documentRevision string
	rootRevision     string
}

func assertPostgreSQLOwnershipConstraints(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revisionID string) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT sd.document_id, sd.document_revision_id, root.id
		FROM knowledge_snapshot_documents sd
		JOIN knowledge_node_revisions root
		  ON root.document_revision_id=sd.document_revision_id AND root.parent_node_revision_id IS NULL
		WHERE sd.knowledge_revision_id=$1
		ORDER BY sd.canonical_path LIMIT 2`, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	var owners []postgresDocumentOwner
	for rows.Next() {
		var owner postgresDocumentOwner
		if err := rows.Scan(&owner.documentID, &owner.documentRevision, &owner.rootRevision); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(owners) != 2 {
		t.Fatalf("ownership fixture documents = %d", len(owners))
	}

	expectPostgreSQLConstraintFailure(t, ctx, pool, "document revision exact root", func(tx pgx.Tx) error {
		documentID := "60000000-0000-4000-8000-000000000208"
		rootNodeID := "60000000-0000-4000-8000-000000000209"
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_documents(id,created_at) VALUES ($1,now())`, documentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_nodes(id,document_id,created_at) VALUES ($1,$2,now())`, rootNodeID, documentID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO knowledge_document_revisions(
				id,document_id,canonical_hash,semantic_hash,root_node_id,parser_version,created_at
			) VALUES ('60000000-0000-4000-8000-000000000210',$1,decode(repeat('cd',32),'hex'),decode(repeat('ef',32),'hex'),$2,'knowledge-parser-v1',now())`,
			documentID, rootNodeID)
		return err
	})
	expectPostgreSQLConstraintFailure(t, ctx, pool, "snapshot document/revision ownership", func(tx pgx.Tx) error {
		newDocument := "60000000-0000-4000-8000-000000000201"
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_documents(id, created_at) VALUES ($1,now())`, newDocument); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO knowledge_snapshot_documents(
				knowledge_revision_id, canonical_path, folded_path, document_id, document_revision_id
			) VALUES ($1,'invalid-owner.md','invalid-owner.md',$2,$3)`, revisionID, newDocument, owners[0].documentRevision)
		return err
	})
	expectPostgreSQLConstraintFailure(t, ctx, pool, "node/document revision ownership", func(tx pgx.Tx) error {
		nodeID := "60000000-0000-4000-8000-000000000202"
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_nodes(id,document_id,created_at) VALUES ($1,$2,now())`, nodeID, owners[1].documentID); err != nil {
			return err
		}
		return insertConstraintNodeRevision(ctx, tx, "60000000-0000-4000-8000-000000000203", nodeID, owners[1].documentID, owners[0].documentRevision, owners[0].rootRevision)
	})
	expectPostgreSQLConstraintFailure(t, ctx, pool, "parent revision ownership", func(tx pgx.Tx) error {
		nodeID := "60000000-0000-4000-8000-000000000204"
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_nodes(id,document_id,created_at) VALUES ($1,$2,now())`, nodeID, owners[0].documentID); err != nil {
			return err
		}
		return insertConstraintNodeRevision(ctx, tx, "60000000-0000-4000-8000-000000000205", nodeID, owners[0].documentID, owners[0].documentRevision, owners[1].rootRevision)
	})
	expectPostgreSQLConstraintFailure(t, ctx, pool, "single root per document revision", func(tx pgx.Tx) error {
		nodeID := "60000000-0000-4000-8000-000000000206"
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_nodes(id,document_id,created_at) VALUES ($1,$2,now())`, nodeID, owners[0].documentID); err != nil {
			return err
		}
		return insertConstraintNodeRevision(ctx, tx, "60000000-0000-4000-8000-000000000207", nodeID, owners[0].documentID, owners[0].documentRevision, nil)
	})
}

func insertConstraintNodeRevision(ctx context.Context, tx pgx.Tx, revisionID, nodeID, documentID, documentRevisionID string, parent any) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO knowledge_node_revisions(
			id,node_id,document_id,document_revision_id,parent_node_revision_id,sibling_index,heading_level,
			title,ancestor_titles,heading_start,heading_end,heading_start_line,heading_end_line,
			local_body_start,local_body_end,local_body_start_line,local_body_end_line,
			section_start,section_end,section_start_line,section_end_line,semantic_local_body_hash,indexer_version
		) VALUES ($1,$2,$3,$4,$5,999,1,'invalid','[]'::jsonb,0,0,1,1,0,0,1,1,0,0,1,1,decode(repeat('00',32),'hex'),'knowledge-index-v1')`,
		revisionID, nodeID, documentID, documentRevisionID, parent)
	return err
}

func expectPostgreSQLConstraintFailure(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string, action func(pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = action(tx)
	if err == nil {
		err = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
	if err == nil {
		t.Fatalf("%s constraint accepted invalid ownership", name)
	}
}
