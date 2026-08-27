package notesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestClientUsesPinnedRoutesBearerAndClientIdentity(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-Client") != ClientType ||
			r.Header.Get("X-Client-Name") != ClientName || r.Header.Get("X-Client-Version") != ClientVersion ||
			r.Header.Get("User-Agent") != UserAgent || r.URL.Query().Get("client") != ClientType {
			t.Errorf("incorrect authentication or identity: headers=%v query=%v", r.Header, r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/version":
			writeJSON(t, w, successEnvelope(versionData(SupportedVersion)))
		case "/api/health":
			writeJSON(t, w, successEnvelope(map[string]any{"status": "healthy", "version": SupportedVersion, "uptime": 1.5, "database": "connected"}))
		case "/api/vault":
			writeJSON(t, w, successEnvelope([]map[string]any{vaultData("Knowledge")}))
		case "/api/note":
			response := noteData("edu-agent/a.md", true)
			if r.Method == http.MethodPost {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["createOnly"] != true || body["vault"] != "Knowledge" || body["path"] != "edu-agent/a.md" {
					t.Errorf("unexpected write body: %#v", body)
				}
				response["id"] = int64(1)
				delete(response, "fileLinks")
			}
			writeJSON(t, w, successEnvelope(response))
		case "/api/notes":
			if r.URL.Query().Get("vault") != "Knowledge" || r.URL.Query().Get("page") != "1" || r.URL.Query().Get("pageSize") != "10" {
				t.Errorf("unexpected list query: %v", r.URL.Query())
			}
			listed := noteData("edu-agent/a.md", false)
			listed["id"] = int64(1)
			writeJSON(t, w, successEnvelope(map[string]any{"list": []any{listed}, "pager": map[string]any{"page": 1, "pageSize": 10, "totalRows": 1}}))
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, nil)
	ctx := context.Background()
	if _, err := client.Version(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Vaults(ctx); err != nil {
		t.Fatal(err)
	}
	note, err := client.GetNote(ctx, "Knowledge", "edu-agent/a.md")
	if err != nil || note.Vault != "Knowledge" || note.Content != "body" {
		t.Fatalf("exact note=%+v err=%v", note, err)
	}
	page, err := client.ListNotes(ctx, "Knowledge", 1, 10)
	if err != nil || len(page.Notes) != 1 || page.Notes[0].Vault != "Knowledge" || page.Notes[0].Content != "" {
		t.Fatalf("listed notes=%+v err=%v", page, err)
	}
	if _, err := client.CreateOrUpdateNote(ctx, NoteWrite{Vault: "Knowledge", Path: "edu-agent/a.md", Content: "body", Ctime: 1, Mtime: 1, CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if requests != 6 {
		t.Fatalf("unexpected request count: %d", requests)
	}
}

func TestNoteValidationAcceptsZeroVersionAndLastTime(t *testing.T) {
	empty := ""
	wire := noteWire{
		Path: "edu-agent/zero.md", Content: &empty, Version: 0, Ctime: 1, Mtime: 1, LastTime: 0,
		UpdatedAt: json.RawMessage(`"2026-08-28T00:00:00Z"`), CreatedAt: json.RawMessage(`"2026-08-28T00:00:00Z"`),
	}
	note, err := validateNote("list_notes", "Knowledge", wire.Path, false, wire)
	if err != nil || note.Version != 0 || note.LastTime != 0 {
		t.Fatalf("zero-valued note=%+v err=%v", note, err)
	}
	wire.Version = -1
	if _, err := validateNote("list_notes", "Knowledge", wire.Path, false, wire); err == nil {
		t.Fatal("negative note version was accepted")
	}
}

func TestGetNoteRequiresContentWhileListAcceptsNoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		withoutContent := noteData("edu-agent/a.md", false)
		if r.URL.Path == "/api/notes" {
			writeJSON(t, w, successEnvelope(map[string]any{
				"list":  []any{withoutContent},
				"pager": map[string]any{"page": 1, "pageSize": 10, "totalRows": 1},
			}))
			return
		}
		writeJSON(t, w, successEnvelope(withoutContent))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, nil)
	if _, err := client.GetNote(context.Background(), "Knowledge", "edu-agent/a.md"); err == nil || Category(err) != CategoryContractMismatch {
		t.Fatalf("exact GET accepted missing content: %v", err)
	}
	page, err := client.ListNotes(context.Background(), "Knowledge", 1, 10)
	if err != nil || len(page.Notes) != 1 || page.Notes[0].Content != "" {
		t.Fatalf("content-free list page=%+v err=%v", page, err)
	}
}

func TestProbeCompatibilityMatrix(t *testing.T) {
	for _, test := range []struct {
		name, version, healthStatus, database, reason string
		vaults                                        []string
		configuredCode, sentinelCode                  int
		compatible                                    bool
	}{
		{name: "compatible", version: SupportedVersion, healthStatus: "healthy", database: "connected", vaults: []string{"Knowledge"}, configuredCode: businessInvalidPath, sentinelCode: businessVaultDenied, compatible: true},
		{name: "older", version: "3.6.0", healthStatus: "healthy", database: "connected", vaults: []string{"Knowledge"}, reason: "version_unsupported"},
		{name: "newer", version: "3.7.0", healthStatus: "healthy", database: "connected", vaults: []string{"Knowledge"}, reason: "version_untested"},
		{name: "invalid", version: "latest", healthStatus: "healthy", database: "connected", vaults: []string{"Knowledge"}, reason: "version_unavailable"},
		{name: "unhealthy", version: SupportedVersion, healthStatus: "unhealthy", database: "error", vaults: []string{"Knowledge"}, reason: "capability_unavailable"},
		{name: "wrong vault", version: SupportedVersion, healthStatus: "healthy", database: "connected", vaults: []string{"Other"}, reason: "capability_unavailable"},
		{name: "multiple visible vaults", version: SupportedVersion, healthStatus: "healthy", database: "connected", vaults: []string{"Knowledge", "Other"}, reason: "capability_unavailable"},
		{name: "write scope missing", version: SupportedVersion, healthStatus: "healthy", database: "connected", vaults: []string{"Knowledge"}, configuredCode: businessVaultDenied, sentinelCode: businessVaultDenied, reason: "capability_unavailable"},
		{name: "vault unrestricted", version: SupportedVersion, healthStatus: "healthy", database: "connected", vaults: []string{"Knowledge"}, configuredCode: businessInvalidPath, sentinelCode: businessInvalidPath, reason: "capability_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/version":
					writeJSON(t, w, successEnvelope(versionData(test.version)))
				case "/api/health":
					writeJSON(t, w, successEnvelope(map[string]any{"status": test.healthStatus, "version": test.version, "uptime": 2.0, "database": test.database}))
				case "/api/vault":
					vaults := make([]map[string]any, 0, len(test.vaults))
					for _, name := range test.vaults {
						vaults = append(vaults, vaultData(name))
					}
					writeJSON(t, w, successEnvelope(vaults))
				case "/api/note":
					var input NoteWrite
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					if input.Path != capabilityProbePath || !input.CreateOnly {
						t.Fatalf("unsafe capability probe: %+v", input)
					}
					code := test.sentinelCode
					if input.Vault == "Knowledge" {
						code = test.configuredCode
					}
					writeJSON(t, w, map[string]any{"code": code, "status": false, "message": "expected probe rejection"})
				default:
					t.Fatalf("unexpected probe path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			capability := newTestClient(t, server.URL, nil).Probe(context.Background(), "Knowledge")
			if capability.Compatible != test.compatible || capability.Reason != test.reason || capability.Version != test.version {
				t.Fatalf("unexpected capability: %+v", capability)
			}
		})
	}
}

func TestClientFailsClosedOnBusinessAndEnvelopeErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want ErrorCategory
	}{
		{"http 200 business auth", `{"code":315,"status":false,"message":"denied"}`, CategoryAuth},
		{"http 200 business not found", `{"code":430,"status":false,"message":"missing"}`, CategoryNotFound},
		{"missing API route", `{"code":304,"status":false,"message":"missing route"}`, CategoryContractMismatch},
		{"unknown code", `{"code":999,"status":false,"message":"unknown"}`, CategoryContractMismatch},
		{"false success code", `{"code":1,"status":false,"message":"bad"}`, CategoryContractMismatch},
		{"missing status", `{"code":1,"data":{}}`, CategoryContractMismatch},
		{"app error missing timestamp", `{"code":431,"message":"exists"}`, CategoryContractMismatch},
		{"app error invalid timestamp", `{"code":431,"message":"exists","timestamp":"not-a-time"}`, CategoryContractMismatch},
		{"app error mixed data", `{"code":431,"message":"exists","timestamp":"2026-08-27T10:00:00Z","data":{}}`, CategoryContractMismatch},
		{"app error invalid details", `{"code":431,"message":"exists","timestamp":"2026-08-27T10:00:00Z","details":[""]}`, CategoryContractMismatch},
		{"unknown envelope field", `{"code":1,"status":true,"data":{},"surprise":true}`, CategoryContractMismatch},
		{"multiple values", `{"code":1,"status":true,"data":{}} {}`, CategoryContractMismatch},
		{"malformed", `{"code":1`, CategoryContractMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := newTestClient(t, server.URL, nil).Version(context.Background())
			if err == nil || Category(err) != test.want {
				t.Fatalf("unexpected error: %v category=%s", err, Category(err))
			}
		})
	}
}

func TestClientAcceptsPinnedApplicationErrorEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":431,"message":"Note already exists","details":["createOnly"],"traceId":"trace-1","timestamp":"2026-08-27T10:00:00Z"}`))
	}))
	defer server.Close()
	_, err := newTestClient(t, server.URL, nil).CreateOrUpdateNote(context.Background(), NoteWrite{
		Vault: "Knowledge", Path: "edu-agent/a.md", Content: "body", Ctime: 1, Mtime: 1, CreateOnly: true,
	})
	var classified *Error
	if !errors.As(err, &classified) || classified.Category() != CategoryConflict || classified.BusinessCode() != businessNoteExists {
		t.Fatalf("pinned application error was not classified: %v", err)
	}
}

func TestClientRejectsRedirectOversizeAndMismatchedData(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		redirected := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/target" {
				redirected = true
				t.Fatal("redirect was followed")
			}
			http.Redirect(w, r, "/target", http.StatusFound)
		}))
		defer server.Close()
		_, err := newTestClient(t, server.URL, nil).Version(context.Background())
		if err == nil || Category(err) != CategoryContractMismatch || redirected {
			t.Fatalf("redirect did not fail closed: %v", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Repeat("x", 65)))
		}))
		defer server.Close()
		base, _ := url.Parse(server.URL)
		client, err := New(Options{BaseURL: base, APIToken: testToken, Timeout: time.Second, BodyLimit: 64})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Version(context.Background()); err == nil || Category(err) != CategoryContractMismatch {
			t.Fatalf("oversize body accepted: %v", err)
		}
	})
	t.Run("managed markdown envelope", func(t *testing.T) {
		const maxManagedMarkdownBytes = 4 << 20
		if DefaultBodyLimit < int64(6*maxManagedMarkdownBytes+64<<10) {
			t.Fatalf("default body limit %d cannot hold a worst-case encoded managed note", DefaultBodyLimit)
		}
		content := strings.Repeat("\"", maxManagedMarkdownBytes)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			data := noteData("managed.md", true)
			data["content"] = content
			data["size"] = len(content)
			writeJSON(t, w, successEnvelope(data))
		}))
		defer server.Close()
		note, err := newTestClient(t, server.URL, nil).GetNote(context.Background(), "Knowledge", "managed.md")
		if err != nil || note.Content != content {
			t.Fatalf("maximum managed note readback bytes=%d err=%v cause=%v", len(note.Content), err, errors.Unwrap(err))
		}
	})
	t.Run("note path", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, successEnvelope(noteData("other.md", true)))
		}))
		defer server.Close()
		if _, err := newTestClient(t, server.URL, nil).GetNote(context.Background(), "Knowledge", "wanted.md"); err == nil || Category(err) != CategoryContractMismatch {
			t.Fatalf("mismatched note accepted: %v", err)
		}
	})
}

func TestNewRejectsUnsafeTokenAndEncodedBasePath(t *testing.T) {
	base, err := url.Parse("https://notes.example.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{strings.Repeat("n", 31), strings.Repeat("界", 32), strings.Repeat("n", 31) + "\n"} {
		if _, err := New(Options{BaseURL: base, APIToken: token, Timeout: time.Second}); err == nil {
			t.Fatalf("unsafe token was accepted: %q", token)
		}
	}
	encoded, err := url.Parse("https://notes.example.test/%2e%2e")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{BaseURL: encoded, APIToken: testToken, Timeout: time.Second}); err == nil {
		t.Fatal("percent-encoded base path was accepted")
	}
}

func TestErrorsDoNotLeakTokenOrRawBody(t *testing.T) {
	raw := "raw-secret-response"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	}))
	defer server.Close()
	_, err := newTestClient(t, server.URL, nil).Version(context.Background())
	if err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), raw) || strings.Contains(fmt.Sprintf("%+v", err), testToken) {
		t.Fatalf("error leaked sensitive data: %v", err)
	}
	var classified *Error
	if !errors.As(err, &classified) {
		t.Fatalf("error is not typed: %T", err)
	}
}

func newTestClient(t *testing.T, rawURL string, httpClient *http.Client) *Client {
	t.Helper()
	base, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{BaseURL: base, APIToken: testToken, Timeout: time.Second, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func successEnvelope(data any) map[string]any {
	return map[string]any{"code": 1, "status": true, "message": "success", "data": data}
}

func versionData(version string) map[string]any {
	return map[string]any{
		"version": version, "gitTag": "v" + version, "buildTime": "fixed", "versionIsNew": false,
		"versionNewName": "", "versionNewLink": "", "versionNewChangelog": "", "versionNewChangelogContent": "",
		"versionHistory": []any{}, "pluginVersionNewName": "", "pluginVersionNewLink": "", "pluginVersionNewChangelog": "",
		"pluginVersionNewChangelogContent": "", "pluginVersionHistory": []any{},
	}
}

func vaultData(name string) map[string]any {
	return map[string]any{"id": 1, "vault": name, "noteCount": 0, "noteSize": 0, "fileCount": 0, "fileSize": 0, "size": 0, "createdAt": "now", "updatedAt": "now"}
}

func noteData(path string, withContent bool) map[string]any {
	data := map[string]any{
		"path": path, "pathHash": "ph", "contentHash": "ch", "version": 1, "ctime": 1, "mtime": 1, "size": 4,
		"clientName": ClientName, "clientType": ClientType, "clientVersion": ClientVersion, "lastTime": 1,
		"updatedAt": "2026-01-01T00:00:00Z", "createdAt": "2026-01-01T00:00:00Z",
	}
	if withContent {
		data["content"] = "body"
		data["fileLinks"] = map[string]string{}
	}
	return data
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
