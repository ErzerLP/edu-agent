package migrations

import (
	"context"
	"testing"
)

func TestSourceMetadataMigrationUpgrade(t *testing.T) {
	pool := migrationPoolThrough(t, 33)
	ctx := context.Background()
	// A pre-2a text version with a forged-looking prefix must survive verbatim and
	// must not acquire provenance during upgrade.
	if _, err := pool.Exec(ctx, `BEGIN;
 INSERT INTO knowledge_documents VALUES('10000000-0000-4000-8000-000000000001',now());
 INSERT INTO knowledge_nodes VALUES('10000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001',now());
 INSERT INTO knowledge_document_revisions(id,document_id,canonical_hash,semantic_hash,root_node_id,parser_version,created_at)
 VALUES('10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001',decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'10000000-0000-4000-8000-000000000002','legacy',now());
 INSERT INTO knowledge_node_revisions(id,node_id,document_id,document_revision_id,sibling_index,heading_level,title,ancestor_titles,
 heading_start,heading_end,heading_start_line,heading_end_line,local_body_start,local_body_end,local_body_start_line,local_body_end_line,section_start,section_end,section_start_line,section_end_line,semantic_local_body_hash,indexer_version)
 VALUES('10000000-0000-4000-8000-000000000004','10000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003',0,0,'旧资料','[]',0,0,1,1,0,0,1,1,0,0,1,1,decode(repeat('33',32),'hex'),'legacy');
 INSERT INTO knowledge_document_payloads(document_revision_id,canonical_markdown) VALUES('10000000-0000-4000-8000-000000000003','来源：https://forged.example');
 COMMIT;`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_source_metadata`).Scan(&count); err != nil || count != 0 {
		t.Fatal("legacy provenance must remain unknown", count, err)
	}
	var text string
	if err := pool.QueryRow(ctx, `SELECT canonical_markdown FROM knowledge_document_payloads WHERE document_revision_id='10000000-0000-4000-8000-000000000003'`).Scan(&text); err != nil || text != "来源：https://forged.example" {
		t.Fatal("legacy body changed", text, err)
	}
	var gates int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid='knowledge_source_metadata'::regclass AND NOT tgisinternal`).Scan(&gates); err != nil || gates != 2 {
		t.Fatal("source metadata must have privacy and immutable gates", gates, err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal("migration replay", err)
	}
}
