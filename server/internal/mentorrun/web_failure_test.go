package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/research"
)

func TestWebReadFailureCategoriesAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		status                               int
		media, cache, body, category, reason string
	}{
		{"missing PDF", 404, "text/plain", "", "missing", "http_404", "http_404"},
		{"empty HTML", 200, "text/html", "", "<script>app()</script>", "parse_empty", "parse_empty"},
		{"storage restricted", 200, "text/plain", "no-store", "private", "storage_restricted", "storage_restricted"},
		{"too large", 200, "text/plain", "", strings.Repeat("x", research.MaxWire+1), "too_large", "body_limit"},
		{"unsupported media", 200, "image/png", "", "image", "unsupported_type", "unsupported_format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newWebHost(t)
			var gets atomic.Int32
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("Cache-Control", tc.cache)
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer page.Close()
			f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}, func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
			})
			ctx := t.Context()
			resource, err := h.registerWebResource(ctx, "http://example.org/page", "可核对的失败页面", "", "user")
			if err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]string{"resource_id": resource.ID})
			call := modelclient.ToolCall{ID: "read", Function: modelclient.ToolFunction{Name: "read_web", Arguments: string(args)}}
			for range 2 {
				result, err := h.executeWebTool(ctx, call)
				raw, _ := json.Marshal(result)
				var decoded map[string]any
				json.Unmarshal(raw, &decoded)
				if err != nil || decoded["category"] != tc.category || decoded["reason"] != tc.reason || decoded["hint"] == "" {
					t.Fatalf("tool lost failure classification: %s %v", raw, err)
				}
			}
			if gets.Load() != 1 || h.body.Web.Progress.ReadAttempted != 1 || h.body.Web.Progress.ReadFailed != 1 {
				t.Fatalf("cached failure fetched/counts again: gets=%d progress=%+v", gets.Load(), h.body.Web.Progress)
			}
			if err := h.checkpoint(ctx, "web_tool_completed"); err != nil {
				t.Fatal(err)
			}
			snapshot := f.snapshot(t, h.owned.RunID)
			raw, _ := json.Marshal(snapshot.Web)
			var progress struct {
				Failures []struct {
					ResourceID              string `json:"resource_id"`
					Title, Category, Reason string
				}
			}
			json.Unmarshal(raw, &progress)
			if len(progress.Failures) != 1 || progress.Failures[0].ResourceID != resource.ID || progress.Failures[0].Category != tc.category || progress.Failures[0].Reason != tc.reason || progress.Failures[0].Title != "可核对的失败页面" {
				t.Fatalf("public progress lost failure: %s", raw)
			}
		})
	}
}

func TestWebPDFAttachmentUsesParserByteLimit(t *testing.T) {
	f, h := newWebHost(t)
	if err := pdfsource.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	original := pdffixture.BuildWithOptions(false, "/Padding ("+strings.Repeat("x", 3<<20)+")", "readable original PDF")
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(original)
	}))
	defer page.Close()
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	resource, err := h.registerWebResource(t.Context(), "http://example.org/book.pdf", "PDF", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"resource_id": resource.ID})
	result, err := h.executeWebTool(t.Context(), modelclient.ToolCall{ID: "read", Function: modelclient.ToolFunction{Name: "read_web", Arguments: string(args)}})
	d, ok := result.(research.WebDocument)
	if err != nil || !ok || d.PDF == nil || len(d.Fragments) == 0 {
		t.Fatalf("valid PDF could not be read and stored: %T %v", result, err)
	}
	var encrypted, restored []byte
	if err = f.pool.QueryRow(t.Context(), `SELECT attachment FROM learning_web_document_payloads WHERE id=$1`, d.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if err = f.service.openWeb("attachment", d.ID, h.owned.Generation, encrypted, &restored); err != nil || !bytes.Equal(original, restored) {
		t.Fatalf("PDF original lost: %v", err)
	}
	if bytes.Contains(encrypted, []byte("%PDF-")) {
		t.Fatal("unencrypted PDF")
	}
}

func TestWebAnswerIncludesReadFailures(t *testing.T) {
	// Exercise the final request against a local HTTP stub; no real model is used.
	var supplied *WebProgress
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		for _, message := range request.Messages {
			if message.Role != "user" {
				continue
			}
			var material struct {
				Progress *WebProgress `json:"network_progress"`
			}
			if json.Unmarshal([]byte(message.Content), &material) == nil && material.Progress != nil {
				supplied = material.Progress
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": `{"answer":"一个页面读取成功，另一个网址返回 HTTP 404。","citations":[]}`}, "finish_reason": "stop"}}})
	})
	f.create = historyRequest(t, f, newHistory(t, f, true, f.goal), "核实公开网页")
	f.accept(t)
	owned, err := f.service.claim(t.Context())
	if err != nil || owned == nil {
		t.Fatal(err)
	}
	h := &executionHost{service: f.service, owned: *owned, body: owned.body}
	_, h.limits, _, _ = f.settings.MentorClient()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.pdf" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<main>可核对正文</main>"))
	}))
	defer page.Close()
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	for _, path := range []string{"/missing.pdf", "/valid"} {
		resource, err := h.registerWebResource(t.Context(), "http://example.org"+path, path, "", "user")
		if err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"resource_id": resource.ID})
		if _, err = h.executeWebTool(t.Context(), modelclient.ToolCall{ID: path, Function: modelclient.ToolFunction{Name: "read_web", Arguments: string(args)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.generateWebAnswer(t.Context()); err != nil {
		t.Fatal(err)
	}
	if supplied == nil || supplied.ReadAttempted != 2 || supplied.ReadSucceeded != 1 || supplied.ReadFailed != 1 || len(supplied.Failures) != 1 || supplied.Failures[0].Category != "http_404" {
		t.Fatalf("actual final request lost read outcome: %+v", supplied)
	}
}
