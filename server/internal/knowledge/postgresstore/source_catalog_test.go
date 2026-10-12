package postgresstore_test

import (
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestPostgreSQLSourceCatalogPermissionsPaginationAndReceipts(t *testing.T) {
	ctx, pool, _, s := newReviewerPostgresHarness(t)
	spaceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO learning_spaces(id,name,status,version) VALUES($1,'来源区','active',1)`, spaceID); err != nil {
		t.Fatal(err)
	}
	ctx, _ = space.WithScope(ctx, spaceID)
	var items []knowledge.SourceItem
	for _, name := range []string{"甲集合", "乙集合"} {
		id := uuid.NewString()
		if _, err := s.ChangeCollection(ctx, knowledge.CollectionCommand{ID: id, Action: "create", Name: name, Source: "test"}); err != nil {
			t.Fatal(err)
		}
		selected, _ := knowledge.WithCollection(ctx, id)
		body, receipt, err := s.PrepareFetchedSource(ctx, research.Source{Locator: "https://example.org/start", FinalURL: "https://example.org/final", Kind: "text/html", Status: "parsed", Parser: "html-v1", Coverage: "complete_text", Fingerprint: strings.Repeat("a", 64), Text: name + "的原文片段", StorageAllowed: true})
		if err != nil {
			t.Fatal(err)
		}
		c := knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: integrationActorID, Source: "test", Documents: []knowledge.ImportDocument{{Path: "同名.md", Markdown: body, SourceReceipt: receipt}}}
		p, err := s.PreviewImport(selected, c)
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.ConfirmImport(selected, knowledge.ConfirmImportCommand{Request: c, Receipt: p.Receipt})
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.ConfirmImport(selected, knowledge.ConfirmImportCommand{Request: c, Receipt: p.Receipt})
		if err != nil || !again.Replayed || again.Revision.ID != r.Revision.ID {
			t.Fatal("replay", again, err)
		}
		changed := c
		changed.Source = "different payload"
		if _, err := s.Import(selected, changed); knowledge.ErrorCode(err) != knowledge.CodeIdempotencyConflict {
			t.Fatal("conflict", err)
		}
		page, err := s.Sources(selected, knowledge.SourceQuery{})
		if err != nil || len(page.Items) != 1 {
			t.Fatal(page, err)
		}
		item := page.Items[0]
		items = append(items, item)
		if item.Metadata == nil || item.Metadata.FinalURL != "https://example.org/final" {
			t.Fatal(item)
		}
		detail, err := s.Source(selected, item.RevisionID, item.DocumentRevisionID)
		if err != nil || len(detail.Fragments) == 0 {
			t.Fatal(detail, err)
		}
		if !strings.Contains(detail.Fragments[len(detail.Fragments)-1].Text, name+"的原文片段") {
			t.Fatal(detail)
		}
	}
	first, err := s.Sources(ctx, knowledge.SourceQuery{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	second, err := s.Sources(ctx, knowledge.SourceQuery{Limit: 1, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].CollectionID == first.Items[0].CollectionID || second.NextCursor != "" {
		t.Fatal(second, err)
	}
	if _, err := s.Sources(ctx, knowledge.SourceQuery{Cursor: "invalid"}); knowledge.ErrorCode(err) != knowledge.CodeInvalidRequest {
		t.Fatal(err)
	}
	other, _ := space.WithScope(ctx, space.DefaultID)
	if p, err := s.Sources(other, knowledge.SourceQuery{}); err != nil || len(p.Items) != 0 {
		t.Fatal("cross-space list", p, err)
	}
	other, _ = knowledge.WithCollection(other, items[0].CollectionID)
	if _, err := s.Source(other, items[0].RevisionID, items[0].DocumentRevisionID); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatal("cross-space detail", err)
	}
	wrong, _ := knowledge.WithCollection(ctx, items[1].CollectionID)
	if _, err := s.Source(wrong, items[0].RevisionID, items[0].DocumentRevisionID); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatal("same-name collection confusion", err)
	}
	if _, err := s.ChangeCollection(ctx, knowledge.CollectionCommand{ID: items[0].CollectionID, Action: "share", ExpectedVersion: 1, Shared: true}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Sources(other, knowledge.SourceQuery{}); knowledge.ErrorCode(err) != knowledge.CodeNotFound || len(p.Items) != 0 {
		t.Fatal("unlinked shared collection became readable", p, err)
	}
	if _, err := s.ChangeCollection(other, knowledge.CollectionCommand{ID: items[0].CollectionID, Action: "link"}); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Source(other, items[0].RevisionID, items[0].DocumentRevisionID); err != nil || d.DocumentRevisionID != items[0].DocumentRevisionID {
		t.Fatal("linked shared source unavailable", d, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE knowledge_source_metadata SET metadata='{}' WHERE document_revision_id=$1`, items[0].DocumentRevisionID); err == nil {
		t.Fatal("immutable provenance overwritten")
	}
	selected, _ := knowledge.WithCollection(ctx, items[0].CollectionID)
	if _, err := pool.Exec(ctx, `DELETE FROM knowledge_collection_links WHERE space_id=$1 AND collection_id=$2`, spaceID, items[0].CollectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Source(selected, items[0].RevisionID, items[0].DocumentRevisionID); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatal("unlinked detail", err)
	}
	if _, err := s.Source(other, items[0].RevisionID, items[0].DocumentRevisionID); err != nil {
		t.Fatal("unlink leaked across learning spaces", err)
	}
}
