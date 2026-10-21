package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
)

// The real model added an explanatory fragment_note to a valid web citation.
// A schema-aware stub repeats that shape unless repair supplies the exact fields.
func TestWebAnswerRepairsCitationAnnotationWithExplicitSchema(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	f, snapshot := webCitationAnswer(t, func(request modelclient.Request, attempt int, d research.WebDocument) map[string]any {
		citation := map[string]any{"origin": "web", "document_id": d.ID, "revision_id": d.RevisionID, "fragment_id": d.Fragments[0].ID, "quote": "Go 1.27.3", "fragment_note": "private-annotation-sentinel"}
		if attempt > 1 {
			var feedback struct {
				OutputSchema struct {
					Properties struct {
						Citations struct {
							Items struct {
								AnyOf []struct {
									Properties map[string]json.RawMessage `json:"properties"`
								} `json:"anyOf"`
							} `json:"items"`
						} `json:"citations"`
					} `json:"properties"`
				} `json:"output_schema"`
			}
			_ = json.Unmarshal([]byte(request.Messages[len(request.Messages)-1].Content), &feedback)
			if choices := feedback.OutputSchema.Properties.Citations.Items.AnyOf; len(choices) > 0 {
				for field := range citation {
					if _, allowed := choices[0].Properties[field]; !allowed {
						delete(citation, field)
					}
				}
			}
		}
		return map[string]any{"answer": "最新稳定版为 Go 1.27.3。[1]", "citations": []any{citation}}
	})
	if snapshot.Status != "succeeded" || f.calls.Load() != 2 || len(snapshot.Citations) != 1 || snapshot.Citations[0].Web == nil {
		t.Fatalf("citation format repair failed: status=%s reason=%s calls=%d", snapshot.Status, snapshot.Reason, f.calls.Load())
	}
	if !strings.Contains(logs.String(), `"rejection_reason":"citation_format"`) || !strings.Contains(logs.String(), `"rejection_field":"citations"`) || strings.Contains(logs.String(), "private-annotation-sentinel") {
		t.Fatal("citation annotation diagnostics missing or leaked model content")
	}
}

func TestWebAnswerCitationRejectionsKeepEvidenceBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		answer string
		want   string
		field  string
	}{
		{"document", func(c map[string]any) { c["document_id"] = uuid.NewString() }, "事实。[1]", "web_citation_unknown_document", "citations.document_id"},
		{"revision", func(c map[string]any) { c["revision_id"] = uuid.NewString() }, "事实。[1]", "web_citation_revision_mismatch", "citations.revision_id"},
		{"unread_fragment", func(c map[string]any) { c["fragment_id"] = uuid.NewString() }, "事实。[1]", "web_citation_unsupplied_fragment", "citations.fragment_id"},
		{"quote", func(c map[string]any) { c["quote"] = "private-quote-sentinel" }, "事实。[1]", "web_quote_mismatch", "citations.quote"},
		{"whitespace_is_not_verbatim", func(c map[string]any) { c["quote"] = "Go  1.27.3" }, "事实。[1]", "web_quote_mismatch", "citations.quote"},
		{"number", nil, "事实。[2]", "citation_number_out_of_range", "answer"},
		{"unreferenced", nil, "没有使用引用编号。", "citation_not_referenced", "answer"},
		{"source", func(c map[string]any) { clear(c); c["origin"], c["source"], c["fragment"] = "learning_source", 100, 1 }, "事实。[1]", "source_citation_out_of_range", "citations.source"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			f, snapshot := webCitationAnswer(t, func(_ modelclient.Request, _ int, d research.WebDocument) map[string]any {
				citation := map[string]any{"origin": "web", "document_id": d.ID, "revision_id": d.RevisionID, "fragment_id": d.Fragments[0].ID, "quote": "Go 1.27.3"}
				if test.mutate != nil {
					test.mutate(citation)
				}
				return map[string]any{"answer": test.answer, "citations": []any{citation}}
			})
			if snapshot.Status != "failed" || snapshot.Output != "" || len(snapshot.Citations) != 0 || f.calls.Load() != 2 {
				t.Fatalf("invalid evidence published: status=%s calls=%d", snapshot.Status, f.calls.Load())
			}
			if !strings.Contains(logs.String(), `"rejection_reason":"`+test.want+`"`) || !strings.Contains(logs.String(), `"rejection_field":"`+test.field+`"`) || strings.Contains(logs.String(), "private-quote-sentinel") {
				t.Fatalf("missing safe diagnostics: want reason=%s field=%s", test.want, test.field)
			}
		})
	}
}

// Keep the real database, source selection, snapshot storage, validation, and
// publication path. Only the model boundary is deterministic.
func webCitationAnswer(t *testing.T, reply func(modelclient.Request, int, research.WebDocument) map[string]any) (*runtimeFixture, Snapshot) {
	t.Helper()
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		var request modelclient.Request
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid model request")
			return
		}
		var document research.WebDocument
		for _, message := range request.Messages {
			var material struct {
				Documents []research.WebDocument `json:"web_documents"`
			}
			if json.Unmarshal([]byte(message.Content), &material) == nil && len(material.Documents) > 0 {
				document = material.Documents[0]
			}
		}
		if len(document.Fragments) == 0 {
			t.Error("no supplied web fragment")
			return
		}
		raw, _ := json.Marshal(reply(request, attempt, document))
		studioReply(w, string(raw), "stop")
	})
	f.create = historyRequest(t, f, newHistory(t, f, true, f.goal), "Go 最新版本和变化？")
	f.create.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{}}
	for i := 0; i < 4; i++ {
		f.create.SourceSelection.Items = append(f.create.SourceSelection.Items, selectedDocument(t, f, "学习区资料，不含最新版本。"))
	}
	receipt := f.accept(t)
	ctx := context.Background()
	owned, err := f.service.claim(ctx)
	if err != nil || owned == nil {
		t.Fatal("could not claim web answer")
	}
	h := &executionHost{service: f.service, owned: *owned, body: owned.body, limits: f.settings.View().Limits}
	resource, err := h.registerWebResource(ctx, "https://example.org/release", "Release notes", "summary is not evidence", "user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	const text = "Go 1.27.3 is stable. Generic methods are supported."
	d, err := h.storeWebDocument(ctx, research.Source{ID: resource.ID, RevisionID: uuid.NewString(), Locator: resource.URL, FinalURL: resource.URL, Title: resource.Title, Text: text, Status: "parsed", StorageAllowed: true, FetchedAt: &now, Fragments: []research.Fragment{{ID: uuid.NewString(), Start: 0, End: len(text), Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	h.body.Web.Documents = []string{d.ID}
	h.body.Web.Supplied[d.ID] = []string{d.Fragments[0].ID}
	h.body.Web.Done = true
	if err = h.generateWebAnswer(ctx); err != nil {
		logRunFailure(ctx, h.owned, err)
		if err = f.service.finish(ctx, *owned, err); err != nil {
			t.Fatal(err)
		}
	}
	return f, f.snapshot(t, receipt.RunID)
}
