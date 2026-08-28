package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	notesyncReviewID    = "41000000-0000-4000-8000-000000000001"
	notesyncOperationID = "42000000-0000-4000-8000-000000000001"
	notesyncDocumentID  = "43000000-0000-4000-8000-000000000001"
)

func TestNotesyncClientEndpointsAuthenticateAndBindRequests(t *testing.T) {
	t.Parallel()
	basis := strings.Repeat("a", 64)
	content := notesyncMarkdown(notesyncDocumentID)
	var resolveBodies [][]byte
	var resolveCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer device-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/notesync/status":
			writeNotesyncJSON(t, w, http.StatusOK, NotesyncStatus{Configured: true, Compatible: true, Version: "3.6.1", Vault: "Vault", PathPrefix: "edu-agent", ExternalCleanupRequired: true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/notesync/previews":
			var request NotesyncPreviewRequest
			decodeNotesyncRequest(t, r, &request)
			if request.Path != "edu-agent/note.md" || request.Page != 1 || request.PageSize != 1 {
				t.Fatalf("preview request = %+v", request)
			}
			writeNotesyncJSON(t, w, http.StatusOK, NotesyncPreviewResult{Items: []NotesyncPreviewItem{notesyncPreviewItem(basis)}, Page: 1, PageSize: 1, TotalRows: 1})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/notesync/reviews":
			if r.URL.Query().Get("status") != "open" || r.URL.Query().Get("cursor") != "cursor-1" || r.URL.Query().Get("limit") != "1" {
				t.Fatalf("review query = %s", r.URL.RawQuery)
			}
			writeNotesyncJSON(t, w, http.StatusOK, NotesyncReviewPage{Items: []NotesyncReviewSummary{notesyncReviewSummary(basis)}, NextCursor: "cursor-2"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/notesync/reviews/"+notesyncReviewID:
			writeNotesyncJSON(t, w, http.StatusOK, notesyncReview(basis, content))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/notesync/reviews/"+notesyncReviewID+"/resolutions":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			resolveBodies = append(resolveBodies, append([]byte(nil), data...))
			if resolveCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusGatewayTimeout)
				_, _ = io.WriteString(w, "proxy")
				return
			}
			var request NotesyncResolutionRequest
			if err := json.Unmarshal(data, &request); err != nil {
				t.Fatal(err)
			}
			if request.OperationID != notesyncOperationID || request.BasisHash != basis || request.Kind != NotesyncResolutionMerged || request.MergedMarkdown == nil || *request.MergedMarkdown != content {
				t.Fatalf("resolution request = %+v", request)
			}
			writeNotesyncJSON(t, w, http.StatusCreated, NotesyncResolutionResult{
				ReviewID: notesyncReviewID, ResolutionKind: NotesyncResolutionMerged,
				KnowledgeRevisionID: "44000000-0000-4000-8000-000000000001", DocumentID: notesyncDocumentID,
				DocumentRevisionID: "45000000-0000-4000-8000-000000000001", Unchanged: false,
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "device-token", time.Second, nil)
	if _, err := client.NotesyncStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NotesyncPreview(t.Context(), NotesyncPreviewRequest{Path: "edu-agent/note.md", Page: 1, PageSize: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NotesyncReviews(t.Context(), "open", "cursor-1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NotesyncReview(t.Context(), notesyncReviewID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResolveNotesyncReview(t.Context(), notesyncReviewID, NotesyncResolutionRequest{
		BasisHash: basis, OperationID: notesyncOperationID, Kind: NotesyncResolutionMerged, MergedMarkdown: &content,
	}); err != nil {
		t.Fatal(err)
	}
	if len(resolveBodies) != 2 || !bytes.Equal(resolveBodies[0], resolveBodies[1]) {
		t.Fatalf("resolve retry bodies differ: %q %q", resolveBodies[0], resolveBodies[1])
	}
}

func TestNotesyncClientRejectsInvalidRequestsAndResponses(t *testing.T) {
	t.Parallel()
	basis := strings.Repeat("a", 64)
	client := NewClient("http://127.0.0.1:1", "token", time.Second, nil)
	if _, err := client.NotesyncReviews(t.Context(), "pending", "", 1); protocolCategory(err) != "invalid_notesync_review_query" {
		t.Fatalf("reviews error = %v", err)
	}
	if _, err := client.ResolveNotesyncReview(t.Context(), notesyncReviewID, NotesyncResolutionRequest{BasisHash: basis, OperationID: notesyncOperationID, Kind: NotesyncResolutionMerged}); protocolCategory(err) != "invalid_notesync_resolution_request" {
		t.Fatalf("resolve error = %v", err)
	}

	responses := []string{
		`{"configured":true,"compatible":true,"reason":"","version":"3.6.1","vault":"Vault","path_prefix":"edu-agent","external_cleanup_required":true,"token":"secret"}`,
		`{"configured":true,"compatible":true,"reason":"unknown","version":"3.6.1","vault":"Vault","path_prefix":"edu-agent","external_cleanup_required":true}`,
		`{"configured":true,"compatible":false,"reason":"not_configured","vault":"Vault","path_prefix":"edu-agent","external_cleanup_required":true}`,
	}
	for _, body := range responses {
		body := body
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		_, err := NewClient(server.URL, "token", time.Second, nil).NotesyncStatus(t.Context())
		server.Close()
		if protocolCategory(err) == "" {
			t.Fatalf("status accepted %s", body)
		}
	}
}

func TestValidateNotesyncReviewStateAndPreviewReasons(t *testing.T) {
	basis := strings.Repeat("a", 64)
	markdown := notesyncMarkdown(notesyncDocumentID)
	resolvedAt := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)

	resolved := notesyncReview(basis, markdown)
	resolved.Status = "resolved"
	resolved.ResolutionKind = NotesyncResolutionAcceptRemote
	resolved.ResolutionOperationID = notesyncOperationID
	resolved.ResolvedByDeviceID = "48000000-0000-4000-8000-000000000001"
	resolved.ResolvedKnowledgeRevisionID = "49000000-0000-4000-8000-000000000001"
	resolved.ResolvedDocumentID = notesyncDocumentID
	resolved.ResolvedDocumentRevisionID = "4a000000-0000-4000-8000-000000000001"
	resolved.ResolvedAt = &resolvedAt
	if err := ValidateNotesyncReview(resolved); err != nil {
		t.Fatalf("valid resolved review rejected: %v", err)
	}

	closed := notesyncReview(basis, markdown)
	closed.Status = "closed"
	closed.ResolutionKind = NotesyncResolutionPrivacy
	closed.ResolvedAt = &resolvedAt
	if err := ValidateNotesyncReview(closed); err != nil {
		t.Fatalf("valid privacy-closed review rejected: %v", err)
	}
	closed.ResolutionKind = NotesyncResolutionSuperseded
	if err := ValidateNotesyncReview(closed); err != nil {
		t.Fatalf("valid superseded review rejected: %v", err)
	}
	closed.ResolutionOperationID = notesyncOperationID
	if err := ValidateNotesyncReview(closed); err == nil {
		t.Fatal("closed review accepted an operation identity")
	}

	preview := notesyncPreviewItem(basis)
	preview.Category = "in_sync"
	preview.ReasonCode = "in_sync"
	preview.ReviewID = ""
	if err := validateNotesyncPreviewItem(preview); err != nil {
		t.Fatalf("in-sync preview rejected: %v", err)
	}
	for _, reason := range []string{"", "remote_unchanged"} {
		preview.ReasonCode = reason
		if err := validateNotesyncPreviewItem(preview); err == nil {
			t.Fatalf("preview accepted invalid reason %q", reason)
		}
	}
}

func TestNotesyncClientPropagatesStableAPIError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, errorJSON("stale_notesync_review", "request-notesync"))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "token", time.Second, nil).NotesyncReview(t.Context(), notesyncReviewID)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "stale_notesync_review" || apiErr.RequestID != "request-notesync" {
		t.Fatalf("error = %v", err)
	}
}

func notesyncPreviewItem(basis string) NotesyncPreviewItem {
	return NotesyncPreviewItem{
		Category: "remote_changed", ReasonCode: "remote_content_changed", ReviewID: notesyncReviewID, BasisHash: basis,
		DocumentID: notesyncDocumentID, RemotePath: "edu-agent/note.md",
		Base: NotesyncReviewSnapshotSummary{Missing: true}, Local: NotesyncReviewSnapshotSummary{Missing: true},
		Remote: NotesyncReviewSnapshotSummary{Path: "edu-agent/note.md", SHA256: strings.Repeat("b", 64)},
		Diff:   NotesyncThreeWayDiff{},
	}
}

func notesyncReviewSummary(basis string) NotesyncReviewSummary {
	now := time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC)
	return NotesyncReviewSummary{
		ReviewID: notesyncReviewID, Category: "remote_changed", ReasonCode: "remote_content_changed", Status: "open",
		BasisHash: basis, Generation: 1, DocumentID: notesyncDocumentID, CanonicalPath: "note.md", RemoteVault: "Vault", RemotePath: "edu-agent/note.md",
		Base: NotesyncReviewSnapshotSummary{Missing: true}, Local: NotesyncReviewSnapshotSummary{Missing: true},
		Remote: NotesyncReviewSnapshotSummary{Path: "edu-agent/note.md", SHA256: strings.Repeat("b", 64)}, CreatedAt: now, UpdatedAt: now,
	}
}

func notesyncReview(basis, markdown string) NotesyncReview {
	summary := notesyncReviewSummary(basis)
	return NotesyncReview{
		ReviewID: summary.ReviewID, Category: summary.Category, ReasonCode: summary.ReasonCode, Status: summary.Status,
		BasisHash: summary.BasisHash, Generation: summary.Generation, DocumentID: summary.DocumentID, CanonicalPath: summary.CanonicalPath,
		RemoteVault: summary.RemoteVault, RemotePath: summary.RemotePath,
		Base: NotesyncReviewSnapshot{Missing: true}, Local: NotesyncReviewSnapshot{Missing: true},
		Remote: NotesyncReviewSnapshot{Path: summary.RemotePath, Markdown: markdown, SHA256: strings.Repeat("b", 64)},
		Diff:   NotesyncThreeWayDiff{}, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt,
	}
}

func notesyncMarkdown(documentID string) string {
	return "---\nedu-agent-document-id: " + documentID + "\nedu-agent-root-node-id: 46000000-0000-4000-8000-000000000001\nedu-agent-source-revision-id: 47000000-0000-4000-8000-000000000001\n---\n# Note\n"
}

func writeNotesyncJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func decodeNotesyncRequest(t *testing.T, r *http.Request, target any) {
	t.Helper()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}

func protocolCategory(err error) string {
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) {
		return protocolErr.Category
	}
	return ""
}
