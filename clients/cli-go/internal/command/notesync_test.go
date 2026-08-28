package command

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
)

const (
	commandNotesyncReviewID    = "51000000-0000-4000-8000-000000000001"
	commandNotesyncOperationID = "52000000-0000-4000-8000-000000000001"
	commandNotesyncDocumentID  = "53000000-0000-4000-8000-000000000001"
)

func TestKnowledgeNotesyncCommandsDispatchAndAuthenticate(t *testing.T) {
	t.Parallel()
	basis := strings.Repeat("a", 64)
	markdown := commandNotesyncMarkdown()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer notesync-device-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/notesync/status":
			writeJSONTest(w, http.StatusOK, api.NotesyncStatus{Configured: true, Compatible: true, Version: "3.6.1", Vault: "Vault", PathPrefix: "edu-agent", ExternalCleanupRequired: true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/notesync/previews":
			var request api.NotesyncPreviewRequest
			decodeCommandJSON(t, r, &request)
			if request.Path != "edu-agent/note.md" || request.Page != 1 || request.PageSize != 1 {
				t.Fatalf("preview request = %+v", request)
			}
			writeJSONTest(w, http.StatusOK, api.NotesyncPreviewResult{Items: []api.NotesyncPreviewItem{commandNotesyncPreviewItem(basis)}, Page: 1, PageSize: 1, TotalRows: 1})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/notesync/reviews":
			if r.URL.Query().Get("status") != "open" || r.URL.Query().Get("cursor") != "cursor-one" || r.URL.Query().Get("limit") != "1" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			writeJSONTest(w, http.StatusOK, api.NotesyncReviewPage{Items: []api.NotesyncReviewSummary{commandNotesyncSummary(basis)}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/notesync/reviews/"+commandNotesyncReviewID:
			writeJSONTest(w, http.StatusOK, commandNotesyncReview(basis, markdown))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/notesync/reviews/"+commandNotesyncReviewID+"/resolutions":
			var request api.NotesyncResolutionRequest
			decodeCommandJSON(t, r, &request)
			if request.Kind != api.NotesyncResolutionMerged || request.OperationID != commandNotesyncOperationID || request.BasisHash != basis || request.MergedMarkdown == nil || *request.MergedMarkdown != markdown {
				t.Fatalf("resolution request = %+v", request)
			}
			writeJSONTest(w, http.StatusCreated, api.NotesyncResolutionResult{
				ReviewID: commandNotesyncReviewID, ResolutionKind: api.NotesyncResolutionMerged,
				KnowledgeRevisionID: "54000000-0000-4000-8000-000000000001", DocumentID: commandNotesyncDocumentID,
				DocumentRevisionID: "55000000-0000-4000-8000-000000000001", Unchanged: false,
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	contentFile := filepath.Join(t.TempDir(), "merged.md")
	if err := os.WriteFile(contentFile, []byte(markdown), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"knowledge", "notesync", "status"},
		{"knowledge", "notesync", "preview", "--path", "edu-agent/note.md", "--page", "1", "--page-size", "1"},
		{"knowledge", "notesync", "reviews", "--status", "open", "--cursor", "cursor-one", "--limit", "1"},
		{"knowledge", "notesync", "review", commandNotesyncReviewID},
		{"knowledge", "notesync", "resolve", commandNotesyncReviewID, "--kind", "merge", "--basis-hash", basis, "--operation-id", commandNotesyncOperationID, "--content-file", contentFile},
	}
	for _, args := range commands {
		configStore, credentialStore := pairedStores(server.URL, "notesync-device-token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), args); exit != ExitOK {
			t.Fatalf("args=%v exit=%d out=%q err=%q", args, exit, out.String(), errOut.String())
		}
		if strings.Contains(out.String()+errOut.String(), "notesync-device-token") {
			t.Fatalf("token leaked for %v", args)
		}
	}
	if calls.Load() != int32(len(commands)) {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestKnowledgeNotesyncRejectsInvalidParametersBeforeNetwork(t *testing.T) {
	t.Parallel()
	basis := strings.Repeat("a", 64)
	tests := [][]string{
		{"knowledge", "notesync"},
		{"knowledge", "notesync", "unknown"},
		{"knowledge", "notesync", "status", "extra"},
		{"knowledge", "notesync", "preview", "--page-size", "26"},
		{"knowledge", "notesync", "reviews", "--status", "pending"},
		{"knowledge", "notesync", "review", "not-a-uuid"},
		{"knowledge", "notesync", "resolve", commandNotesyncReviewID, "--kind", "merge", "--basis-hash", basis, "--operation-id", commandNotesyncOperationID},
		{"knowledge", "notesync", "resolve", commandNotesyncReviewID, "--kind", "accept-remote", "--basis-hash", basis, "--operation-id", commandNotesyncOperationID, "--content-file", "note.md"},
		{"knowledge", "notesync", "resolve", commandNotesyncReviewID, "--kind", "invalid", "--basis-hash", basis, "--operation-id", commandNotesyncOperationID},
	}
	for _, args := range tests {
		configStore, credentialStore := pairedStores(config.DefaultServerURL, "token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		app.NewClient = func(string, string, time.Duration) APIClient { panic("invalid input must not create a client") }
		if exit := app.Run(t.Context(), args); exit != ExitInput {
			t.Fatalf("args=%v exit=%d out=%q err=%q", args, exit, out.String(), errOut.String())
		}
	}
}

func TestKnowledgeNotesyncResolveMapsAcceptLocalAndPropagatesAPIErrors(t *testing.T) {
	t.Parallel()
	basis := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/knowledge/notesync/reviews/"+commandNotesyncReviewID+"/resolutions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var request api.NotesyncResolutionRequest
		decodeCommandJSON(t, r, &request)
		if request.Kind != api.NotesyncResolutionKeepCanonical || request.MergedMarkdown != nil {
			t.Fatalf("request = %+v", request)
		}
		writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_notesync_review", Message: "stale", RequestID: "request-stale"}})
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	exit := app.Run(t.Context(), []string{"knowledge", "notesync", "resolve", commandNotesyncReviewID, "--kind", "accept-local", "--basis-hash", basis, "--operation-id", commandNotesyncOperationID})
	if exit != ExitConflict || !strings.Contains(errOut.String(), "stale_notesync_review") || !strings.Contains(errOut.String(), "request-stale") || out.Len() != 0 {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
}

func commandNotesyncPreviewItem(basis string) api.NotesyncPreviewItem {
	return api.NotesyncPreviewItem{
		Category: "remote_changed", ReasonCode: "remote_content_changed", ReviewID: commandNotesyncReviewID,
		BasisHash: basis, DocumentID: commandNotesyncDocumentID, RemotePath: "edu-agent/note.md",
		Base: api.NotesyncReviewSnapshotSummary{Missing: true}, Local: api.NotesyncReviewSnapshotSummary{Missing: true},
		Remote: api.NotesyncReviewSnapshotSummary{Path: "edu-agent/note.md", SHA256: strings.Repeat("b", 64)}, Diff: api.NotesyncThreeWayDiff{},
	}
}

func commandNotesyncSummary(basis string) api.NotesyncReviewSummary {
	now := time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC)
	return api.NotesyncReviewSummary{
		ReviewID: commandNotesyncReviewID, Category: "remote_changed", ReasonCode: "remote_content_changed", Status: "open",
		BasisHash: basis, Generation: 1, DocumentID: commandNotesyncDocumentID, CanonicalPath: "note.md", RemoteVault: "Vault", RemotePath: "edu-agent/note.md",
		Base: api.NotesyncReviewSnapshotSummary{Missing: true}, Local: api.NotesyncReviewSnapshotSummary{Missing: true},
		Remote: api.NotesyncReviewSnapshotSummary{Path: "edu-agent/note.md", SHA256: strings.Repeat("b", 64)}, CreatedAt: now, UpdatedAt: now,
	}
}

func commandNotesyncReview(basis, markdown string) api.NotesyncReview {
	summary := commandNotesyncSummary(basis)
	return api.NotesyncReview{
		ReviewID: summary.ReviewID, Category: summary.Category, ReasonCode: summary.ReasonCode, Status: summary.Status,
		BasisHash: summary.BasisHash, Generation: summary.Generation, DocumentID: summary.DocumentID, CanonicalPath: summary.CanonicalPath,
		RemoteVault: summary.RemoteVault, RemotePath: summary.RemotePath,
		Base: api.NotesyncReviewSnapshot{Missing: true}, Local: api.NotesyncReviewSnapshot{Missing: true},
		Remote: api.NotesyncReviewSnapshot{Path: summary.RemotePath, Markdown: markdown, SHA256: strings.Repeat("b", 64)},
		Diff:   api.NotesyncThreeWayDiff{}, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt,
	}
}

func commandNotesyncMarkdown() string {
	return "---\nedu-agent-document-id: " + commandNotesyncDocumentID + "\nedu-agent-root-node-id: 56000000-0000-4000-8000-000000000001\nedu-agent-source-revision-id: 57000000-0000-4000-8000-000000000001\n---\n# Note\n"
}

func decodeCommandJSON(t *testing.T, r *http.Request, target any) {
	t.Helper()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		t.Fatalf("trailing request JSON: %v", err)
	}
}
