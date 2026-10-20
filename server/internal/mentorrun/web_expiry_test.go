package mentorrun

import (
	"bytes"
	"context"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestWebExpiryRemovesSnapshotAndQuotationCopies(t *testing.T) {
	f, h := newWebHost(t)
	ctx := context.Background()
	resource, err := h.registerWebResource(ctx, "https://example.org/expires", "到期网页", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	quote := "ORIGINAL_PRIVATE_WEB_QUOTE"
	source := research.Source{ID: resource.ID, RevisionID: uuid.NewString(), Locator: resource.URL, FinalURL: resource.URL, Title: resource.Title, Status: "parsed", Kind: "text/plain", Coverage: "complete_text", Text: quote, StorageAllowed: true, FetchedAt: &now, Fragments: []research.Fragment{{ID: uuid.NewString(), Start: 0, End: len(quote), Text: quote}}}
	document, err := h.storeWebDocument(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	citation, err := document.Cite(source.Fragments[0].ID, quote, []string{source.Fragments[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Hour)
	h.body.WebExpiresAt = &past
	h.body.Output = "已给出资料回答。[1]"
	h.body.Citations = []SourceCitation{{ID: "1", Origin: "web", Web: &citation}}
	h.body.Messages = append(h.body.Messages, modelclient.Message{Role: "assistant", Content: quote})
	if err = f.service.mutate(ctx, &h.owned, "completed", func(item *row) error { item.body = h.body; item.Status = "succeeded"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=$2,state=jsonb_set(state,'{expires_at}',to_jsonb($2::timestamptz)) WHERE id=$1`, h.owned.RunID, past); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadWebDocument(ctx, f.actor, learningspace.DefaultID, document.ID, "", func(research.WebDocument) error { t.Error("expired body emitted"); return nil }); err == nil {
		t.Fatal("expired document still available")
	}
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_web_resources)+(SELECT count(*) FROM learning_web_document_payloads)`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("expired content remains: %d %v", remaining, err)
	}
	var raw []byte
	if err = f.pool.QueryRow(ctx, `SELECT ciphertext FROM learning_tutor_turns WHERE run_id=$1`, h.owned.RunID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	a := f.service.aead
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(historyAAD(h.owned.ConversationID, h.owned.Generation, h.owned.RunID)))
	if err != nil || bytes.Contains(plain, []byte(quote)) {
		t.Fatalf("history retained expired quotation: %v", err)
	}
}
