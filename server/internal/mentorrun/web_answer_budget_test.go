package mentorrun

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

// Dropping the answer projection or effort forwarding must fail at the real
// model boundary. The model alone is a stub; storage and publication stay real.
func TestWebAnswerBoundsContextAndReasoning(t *testing.T) {
	for _, test := range []struct{ api, model, effort string }{
		{"chat_completions", "deepseek-v4-flash", "low"},
		{"responses", "deepseek-v4-flash", "low"},
		{"chat_completions", "mentor-fixture", ""},
	} {
		t.Run(test.api+"/"+test.model, func(t *testing.T) {
			f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				var request struct {
					Messages        []modelclient.Message   `json:"messages"`
					Input           []modelclient.Message   `json:"input"`
					ReasoningEffort string                  `json:"reasoning_effort"`
					Reasoning       struct{ Effort string } `json:"reasoning"`
					MaxTokens       int                     `json:"max_tokens"`
					MaxOutputTokens int                     `json:"max_output_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				messages, effort, output := request.Messages, request.ReasoningEffort, request.MaxTokens
				if test.api == "responses" {
					messages, effort, output = request.Input, request.Reasoning.Effort, request.MaxOutputTokens
				}
				if effort != test.effort || output != 64 {
					t.Errorf("answer effort/output limit = %q/%d, want %q/64", effort, output, test.effort)
				}
				var fragments []struct {
					ID     string `json:"id"`
					Quotes []struct {
						Candidate int    `json:"candidate"`
						Text      string `json:"text"`
					} `json:"quotes"`
				}
				for _, message := range messages {
					var material struct {
						Documents []struct {
							Fragments json.RawMessage `json:"fragments"`
						} `json:"web_documents"`
					}
					if json.Unmarshal([]byte(message.Content), &material) == nil && len(material.Documents) == 1 {
						_ = json.Unmarshal(material.Documents[0].Fragments, &fragments)
					}
				}
				textBytes := 0
				relevant := 0
				for _, fragment := range fragments {
					for _, quote := range fragment.Quotes {
						textBytes += len(quote.Text)
						if strings.Contains(quote.Text, "unread-sentinel") {
							t.Error("unread fragment entered answer context")
						}
						if utf8.RuneCountInString(quote.Text) > 240 {
							t.Error("candidate quote exceeds output limit")
						}
						if strings.HasPrefix(quote.Text, "release-marker") {
							relevant = quote.Candidate
						}
					}
				}
				if len(fragments) != 6 || textBytes == 0 || textBytes > 7200 {
					t.Errorf("answer context is unbounded: fragments=%d bytes=%d", len(fragments), textBytes)
				}
				if relevant == 0 {
					t.Error("late relevant fragment was discarded")
					return
				}
				raw, _ := json.Marshal(map[string]any{
					"answer":    "版本事实。[1]",
					"citations": []any{map[string]any{"origin": "web", "candidate": relevant}},
				})
				if test.api == "responses" {
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": string(raw)}}}}})
				} else {
					studioReply(w, string(raw), "stop")
				}
			})
			view := f.settings.View()
			connection := view.Mentor.Connection
			connection.APIStyle, connection.Model = test.api, test.model
			reuse := true
			if _, err := f.settings.Update(settings.Update{ExpectedRevision: view.Revision, Target: settings.Teaching, Connection: &connection, MentorUsesTeaching: &reuse}); err != nil {
				t.Fatal(err)
			}
			f.create = historyRequest(t, f, newHistory(t, f, true, f.goal), "release-marker 最新版本主要变化")
			receipt := f.accept(t)
			owned, err := f.service.claim(t.Context())
			if err != nil || owned == nil {
				t.Fatal("could not claim answer")
			}
			h := &executionHost{service: f.service, owned: *owned, body: owned.body, limits: f.settings.View().Limits}
			resource, err := h.registerWebResource(t.Context(), "https://example.org/releases", "Release notes", "summary is not evidence", "user")
			if err != nil {
				t.Fatal(err)
			}
			text := ""
			fragments := []research.Fragment{}
			for i := 0; i < 9; i++ {
				part := strings.Repeat("archive ", 150)
				if i == 7 {
					part = "release-marker 最新版本主要变化 " + strings.Repeat("evidence ", 128)
				} else if i == 8 {
					part = "unread-sentinel release-marker 最新版本主要变化"
				}
				fragments = append(fragments, research.Fragment{ID: uuid.NewString(), Start: len(text), End: len(text) + len(part), Text: part})
				text += part
			}
			now := time.Now()
			d, err := h.storeWebDocument(t.Context(), research.Source{ID: resource.ID, RevisionID: uuid.NewString(), Locator: resource.URL, FinalURL: resource.URL, Title: resource.Title, Text: text, Status: "parsed", StorageAllowed: true, FetchedAt: &now, Fragments: fragments})
			if err != nil {
				t.Fatal(err)
			}
			h.body.Web.Documents = []string{d.ID}
			for _, fragment := range d.Fragments[:8] {
				h.body.Web.Supplied[d.ID] = append(h.body.Web.Supplied[d.ID], fragment.ID)
			}
			h.body.Web.Done = true
			if err := h.generateWebAnswer(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot := f.snapshot(t, receipt.RunID)
			if snapshot.Status != "succeeded" || len(snapshot.Citations) != 1 || snapshot.Citations[0].Web == nil || snapshot.Citations[0].Web.FragmentID != d.Fragments[7].ID {
				t.Fatalf("bounded answer did not publish verified evidence: status=%s citations=%+v", snapshot.Status, snapshot.Citations)
			}
		})
	}
}

func TestWebAnswerCompactCandidateBoundary(t *testing.T) {
	for _, test := range []struct {
		name     string
		citation map[string]any
		status   string
	}{
		{"valid", map[string]any{"origin": "web", "candidate": 1}, "succeeded"},
		{"zero", map[string]any{"origin": "web", "candidate": 0}, "failed"},
		{"unknown", map[string]any{"origin": "web", "candidate": 2}, "failed"},
		{"quote_override", map[string]any{"origin": "web", "candidate": 1, "quote": "unread override"}, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, snapshot := webCitationAnswer(t, func(_ modelclient.Request, _ int, _ research.WebDocument) map[string]any {
				return map[string]any{"answer": "版本事实。[1]", "citations": []any{test.citation}}
			})
			if snapshot.Status != test.status {
				t.Fatalf("compact citation result=%s, want %s", snapshot.Status, test.status)
			}
			if test.status == "succeeded" {
				if len(snapshot.Citations) != 1 || snapshot.Citations[0].Web == nil || snapshot.Citations[0].Web.Quote != "Go 1.27.3 is stable. Generic methods are supported." || f.calls.Load() != 1 {
					t.Fatal("candidate did not resolve to verified original text")
				}
			} else if snapshot.Output != "" || len(snapshot.Citations) != 0 || f.calls.Load() != 2 {
				t.Fatal("candidate escaped the supplied evidence boundary")
			}
		})
	}
}

func TestWebAnswerRejectsExcessiveOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		count  int
		quote  string
		answer string
	}{
		{"citations", 7, "Go 1.27.3", "事实。[1][2][3][4][5][6][7]"},
		{"quote", 1, strings.Repeat("证", 241), "事实。[1]"},
		{"answer", 1, "Go 1.27.3", strings.Repeat("答", 801) + "[1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, snapshot := webCitationAnswer(t, func(_ modelclient.Request, _ int, d research.WebDocument) map[string]any {
				citations := []any{}
				for i := 0; i < test.count; i++ {
					citations = append(citations, map[string]any{"origin": "web", "document_id": d.ID, "revision_id": d.RevisionID, "fragment_id": d.Fragments[0].ID, "quote": test.quote})
				}
				return map[string]any{"answer": test.answer, "citations": citations}
			}, "Go 1.27.3 "+strings.Repeat("证", 300))
			if snapshot.Status != "failed" || snapshot.Output != "" || len(snapshot.Citations) != 0 || f.calls.Load() != 2 {
				t.Fatalf("excessive %s was published: status=%s calls=%d", test.name, snapshot.Status, f.calls.Load())
			}
		})
	}
}
