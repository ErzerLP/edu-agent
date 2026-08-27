package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const notesyncHTTPDeviceID = "91000000-0000-4000-8000-000000000001"
const notesyncHTTPReviewID = "92000000-0000-4000-8000-000000000001"

// fakeNotesyncReviewHTTP deliberately implements only the transport service
// boundary. It has no remote client or database access.
type fakeNotesyncReviewHTTP struct {
	status        notesync.ReviewStatus
	previewResult notesync.PreviewResult
	previewErr    error
	previewCmd    notesync.PreviewCommand
	listResult    notesync.ReviewPage
	listErr       error
	listCmd       notesync.ReviewListCommand
	reviewResult  notesync.Review
	reviewErr     error
	reviewID      string
	reviewFn      func(context.Context, string) (notesync.Review, error)
	resolveResult notesync.ResolutionResult
	resolveErr    error
	resolveCmd    notesync.ResolutionCommand
	calls         map[string]int
}

func (f *fakeNotesyncReviewHTTP) called(name string) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[name]++
}

func (f *fakeNotesyncReviewHTTP) Status(context.Context) notesync.ReviewStatus {
	f.called("status")
	return f.status
}

func (f *fakeNotesyncReviewHTTP) Preview(_ context.Context, command notesync.PreviewCommand) (notesync.PreviewResult, error) {
	f.called("preview")
	f.previewCmd = command
	return f.previewResult, f.previewErr
}

func (f *fakeNotesyncReviewHTTP) ListReviews(_ context.Context, command notesync.ReviewListCommand) (notesync.ReviewPage, error) {
	f.called("list")
	f.listCmd = command
	return f.listResult, f.listErr
}

func (f *fakeNotesyncReviewHTTP) Review(ctx context.Context, reviewID string) (notesync.Review, error) {
	f.called("review")
	f.reviewID = reviewID
	if f.reviewFn != nil {
		return f.reviewFn(ctx, reviewID)
	}
	return f.reviewResult, f.reviewErr
}

func (f *fakeNotesyncReviewHTTP) Resolve(_ context.Context, command notesync.ResolutionCommand) (notesync.ResolutionResult, error) {
	f.called("resolve")
	f.resolveCmd = command
	return f.resolveResult, f.resolveErr
}

func newNotesyncTestAPI(t *testing.T, scopes []string, service NotesyncReviewService, permits *privacy.ReadPermitManager, logs *bytes.Buffer) http.Handler {
	return newNotesyncTestAPIWithBodyLimit(t, scopes, service, permits, logs, 0)
}

func newNotesyncTestAPIWithBodyLimit(t *testing.T, scopes []string, service NotesyncReviewService, permits *privacy.ReadPermitManager, logs *bytes.Buffer, bodyLimit int64) http.Handler {
	t.Helper()
	if permits == nil {
		permits = privacy.NewReadPermitManager()
	}
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	id := &fakeIdentity{auth: identity.Credential{
		Device: identity.Device{ID: notesyncHTTPDeviceID}, Scopes: scopes,
	}}
	handler, err := New(Options{
		Identity: id, Notesync: service, ReadPermits: permits,
		Readiness:   fakeReadiness{report: health.Report{Status: health.StatusHealthy}},
		Logger:      slog.New(slog.NewJSONHandler(logs, nil)),
		PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute),
		DeviceLimiter: NewFixedWindowLimiter(100, time.Minute), MaxKnowledgeRequestBody: bodyLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func notesyncHTTPResponse(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func notesyncHTTPAuthenticatedResponse(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	return notesyncHTTPResponse(handler, method, path, body, "valid-notesync-token")
}

func notesyncHTTPErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, response.Body.String())
	}
	return payload.Error.Code
}

func notesyncHTTPReviewFixture() notesync.Review {
	return notesync.Review{
		ReviewID: notesyncHTTPReviewID, Category: notesync.PreviewCategoryBothChanged,
		ReasonCode: notesync.ReviewReasonBothSidesChanged, Status: notesync.ReviewStatusOpen,
		BasisHash: strings.Repeat("a", 64), Generation: 3,
		HeadRevisionID: "93000000-0000-4000-8000-000000000001", HeadRevisionNo: 7,
		DocumentID: "94000000-0000-4000-8000-000000000001", RemoteDocumentID: "94000000-0000-4000-8000-000000000001",
		CanonicalPath: "topic.md", RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
		Base:      notesync.ReviewSnapshot{Path: "topic.md", Markdown: "base markdown", SHA256: strings.Repeat("b", 64)},
		Local:     notesync.ReviewSnapshot{Path: "topic.md", Markdown: "local markdown", SHA256: strings.Repeat("c", 64)},
		Remote:    notesync.ReviewSnapshot{Path: "edu-agent/topic.md", Markdown: "remote markdown", SHA256: strings.Repeat("d", 64)},
		Diff:      notesync.ThreeWayDiff{BaseToLocal: "local diff", BaseToRemote: "remote diff"},
		CreatedAt: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 28, 0, 1, 0, 0, time.UTC),
	}
}

func TestNotesyncHTTPDisabledStatusAndRoutes(t *testing.T) {
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:read", "knowledge:write"}, nil, nil, &logs)

	status := notesyncHTTPAuthenticatedResponse(handler, http.MethodGet, "/v1/knowledge/notesync/status", "")
	if status.Code != http.StatusOK {
		t.Fatalf("disabled status=%d body=%s", status.Code, status.Body.String())
	}
	var statusPayload notesync.ReviewStatus
	if err := json.Unmarshal(status.Body.Bytes(), &statusPayload); err != nil {
		t.Fatal(err)
	}
	if statusPayload.Configured || statusPayload.Compatible || statusPayload.Reason != "not_configured" || !statusPayload.ExternalCleanupRequired {
		t.Fatalf("disabled status=%+v", statusPayload)
	}

	for _, route := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/knowledge/notesync/previews", `{}`},
		{http.MethodGet, "/v1/knowledge/notesync/reviews", ""},
		{http.MethodGet, "/v1/knowledge/notesync/reviews/" + notesyncHTTPReviewID, ""},
		{http.MethodPost, "/v1/knowledge/notesync/reviews/" + notesyncHTTPReviewID + "/resolutions", `{"basis_hash":"basis","operation_id":"operation","kind":"keep_canonical"}`},
	} {
		response := notesyncHTTPAuthenticatedResponse(handler, route.method, route.path, route.body)
		if response.Code != http.StatusServiceUnavailable || notesyncHTTPErrorCode(t, response) != notesyncNotConfigured {
			t.Fatalf("disabled %s %s=%d code=%s body=%s", route.method, route.path, response.Code, notesyncHTTPErrorCode(t, response), response.Body.String())
		}
	}
}

func TestNotesyncHTTPAuthenticationAndScopeBoundaries(t *testing.T) {
	service := &fakeNotesyncReviewHTTP{
		status:        notesync.ReviewStatus{Configured: true, Compatible: true, Reason: ""},
		previewResult: notesync.PreviewResult{Items: []notesync.PreviewItem{}},
		listResult:    notesync.ReviewPage{Items: []notesync.ReviewSummary{}},
		reviewResult:  notesyncHTTPReviewFixture(),
		resolveResult: notesync.ResolutionResult{KnowledgeRevisionID: "95000000-0000-4000-8000-000000000001"},
	}
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:read", "knowledge:write"}, service, nil, &logs)

	missingAuth := notesyncHTTPResponse(handler, http.MethodGet, "/v1/knowledge/notesync/status", "", "")
	if missingAuth.Code != http.StatusUnauthorized || service.calls["status"] != 0 {
		t.Fatalf("missing authentication=%d calls=%d body=%s", missingAuth.Code, service.calls["status"], missingAuth.Body.String())
	}

	cases := []struct {
		name, method, path, body, allowedScope string
	}{
		{"status", http.MethodGet, "/v1/knowledge/notesync/status", "", "knowledge:read"},
		{"preview", http.MethodPost, "/v1/knowledge/notesync/previews", `{}`, "knowledge:read"},
		{"list", http.MethodGet, "/v1/knowledge/notesync/reviews", "", "knowledge:read"},
		{"show", http.MethodGet, "/v1/knowledge/notesync/reviews/" + notesyncHTTPReviewID, "", "knowledge:read"},
		{"resolution", http.MethodPost, "/v1/knowledge/notesync/reviews/" + notesyncHTTPReviewID + "/resolutions", `{"basis_hash":"basis","operation_id":"operation","kind":"keep_canonical"}`, "knowledge:write"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service.calls = make(map[string]int)
			allowed := newNotesyncTestAPI(t, []string{test.allowedScope}, service, nil, &logs)
			response := notesyncHTTPAuthenticatedResponse(allowed, test.method, test.path, test.body)
			if response.Code != http.StatusOK && response.Code != http.StatusCreated {
				t.Fatalf("allowed scope=%d body=%s", response.Code, response.Body.String())
			}
			if len(service.calls) != 1 {
				t.Fatalf("allowed scope calls=%v", service.calls)
			}

			wrongScope := "knowledge:write"
			if test.allowedScope == wrongScope {
				wrongScope = "knowledge:read"
			}
			service.calls = make(map[string]int)
			forbidden := newNotesyncTestAPI(t, []string{wrongScope}, service, nil, &logs)
			response = notesyncHTTPAuthenticatedResponse(forbidden, test.method, test.path, test.body)
			if response.Code != http.StatusForbidden || service.calls[test.name] != 0 {
				t.Fatalf("wrong scope=%d calls=%v body=%s", response.Code, service.calls, response.Body.String())
			}
		})
	}
}

func TestNotesyncHTTPPreviewClosedQueryBodyLimitAndEmptyItems(t *testing.T) {
	service := &fakeNotesyncReviewHTTP{previewResult: notesync.PreviewResult{Page: 2, PageSize: 4}}
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:read"}, service, nil, &logs)

	response := notesyncHTTPAuthenticatedResponse(handler, http.MethodPost, "/v1/knowledge/notesync/previews", `{"path":"topic.md","page":2,"page_size":4,"unknown":true}`)
	if response.Code != http.StatusBadRequest || notesyncHTTPErrorCode(t, response) != notesync.CodeReviewInvalidRequest || service.calls["preview"] != 0 {
		t.Fatalf("unknown preview field=%d code=%s calls=%v body=%s", response.Code, notesyncHTTPErrorCode(t, response), service.calls, response.Body.String())
	}

	response = notesyncHTTPAuthenticatedResponse(handler, http.MethodPost, "/v1/knowledge/notesync/previews?unexpected=1", `{}`)
	if response.Code != http.StatusBadRequest || notesyncHTTPErrorCode(t, response) != notesync.CodeReviewInvalidRequest || service.calls["preview"] != 0 {
		t.Fatalf("preview query=%d code=%s calls=%v body=%s", response.Code, notesyncHTTPErrorCode(t, response), service.calls, response.Body.String())
	}

	response = notesyncHTTPAuthenticatedResponse(handler, http.MethodPost, "/v1/knowledge/notesync/previews", `{"path":"topic.md","page":2,"page_size":4}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[]`) || service.previewCmd.Path != "topic.md" || service.previewCmd.Page != 2 || service.previewCmd.PageSize != 4 {
		t.Fatalf("preview=%d command=%+v body=%s", response.Code, service.previewCmd, response.Body.String())
	}

	limited := newNotesyncTestAPIWithBodyLimit(t, []string{"knowledge:read"}, &fakeNotesyncReviewHTTP{}, nil, &logs, 32)
	largeBody := `{"path":"` + strings.Repeat("x", 64) + `"}`
	response = notesyncHTTPAuthenticatedResponse(limited, http.MethodPost, "/v1/knowledge/notesync/previews", largeBody)
	if response.Code != http.StatusRequestEntityTooLarge || notesyncHTTPErrorCode(t, response) != knowledge.CodePayloadTooLarge {
		t.Fatalf("oversized preview=%d code=%s body=%s", response.Code, notesyncHTTPErrorCode(t, response), response.Body.String())
	}
}

func TestNotesyncHTTPListClosedQueryAndShow(t *testing.T) {
	review := notesyncHTTPReviewFixture()
	service := &fakeNotesyncReviewHTTP{
		listResult:   notesync.ReviewPage{Items: []notesync.ReviewSummary{notesync.SummarizeReview(review)}, NextCursor: "next"},
		reviewResult: review,
	}
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:read"}, service, nil, &logs)

	response := notesyncHTTPAuthenticatedResponse(handler, http.MethodGet, "/v1/knowledge/notesync/reviews?status=resolved&cursor=cursor-token&limit=7", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"next_cursor":"next"`) ||
		!strings.Contains(response.Body.String(), `"review_id":"`+notesyncHTTPReviewID+`"`) ||
		strings.Contains(response.Body.String(), "local markdown") || strings.Contains(response.Body.String(), "remote markdown") ||
		strings.Contains(response.Body.String(), "local diff") || strings.Contains(response.Body.String(), "remote diff") ||
		service.listCmd.Status != "resolved" || service.listCmd.Cursor != "cursor-token" || service.listCmd.Limit != 7 {
		t.Fatalf("list=%d command=%+v body=%s", response.Code, service.listCmd, response.Body.String())
	}

	for _, query := range []string{"?unexpected=secret", "?limit=1&limit=2", "?limit=not-a-number"} {
		response = notesyncHTTPAuthenticatedResponse(handler, http.MethodGet, "/v1/knowledge/notesync/reviews"+query, "")
		if response.Code != http.StatusBadRequest || notesyncHTTPErrorCode(t, response) != notesync.CodeReviewInvalidRequest {
			t.Fatalf("invalid list query %s=%d code=%s body=%s", query, response.Code, notesyncHTTPErrorCode(t, response), response.Body.String())
		}
	}

	response = notesyncHTTPAuthenticatedResponse(handler, http.MethodGet, "/v1/knowledge/notesync/reviews/"+notesyncHTTPReviewID, "")
	if response.Code != http.StatusOK || service.reviewID != notesyncHTTPReviewID || !strings.Contains(response.Body.String(), `"review_id":"`+notesyncHTTPReviewID+`"`) {
		t.Fatalf("show=%d review_id=%q body=%s", response.Code, service.reviewID, response.Body.String())
	}
}

func TestNotesyncHTTPResolutionDeviceAndStatusCodes(t *testing.T) {
	service := &fakeNotesyncReviewHTTP{}
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:write"}, service, nil, &logs)
	path := "/v1/knowledge/notesync/reviews/" + notesyncHTTPReviewID + "/resolutions"
	body := `{"basis_hash":"` + strings.Repeat("a", 64) + `","operation_id":"96000000-0000-4000-8000-000000000001","kind":"merged","merged_markdown":"merged markdown","identity_review_basis_hash":"` + strings.Repeat("b", 64) + `","identity_review_operation_id":"96000000-0000-4000-8000-000000000002","identity_review_receipt":"` + strings.Repeat("c", 64) + `","node_resolutions":[{"locator":"node-1","action":"new","reason":"remote rewrite"}]}`

	cases := []struct {
		name   string
		result notesync.ResolutionResult
		status int
	}{
		{"created", notesync.ResolutionResult{ReviewID: notesyncHTTPReviewID, ResolutionKind: notesync.ResolutionAcceptRemote, KnowledgeRevisionID: "97000000-0000-4000-8000-000000000001"}, http.StatusCreated},
		{"keep", notesync.ResolutionResult{ReviewID: notesyncHTTPReviewID, ResolutionKind: notesync.ResolutionKeepCanonical, Unchanged: true}, http.StatusOK},
		{"unchanged", notesync.ResolutionResult{ReviewID: notesyncHTTPReviewID, ResolutionKind: notesync.ResolutionAcceptRemote, KnowledgeRevisionID: "97000000-0000-4000-8000-000000000001", Unchanged: true}, http.StatusOK},
		{"replayed", notesync.ResolutionResult{ReviewID: notesyncHTTPReviewID, ResolutionKind: notesync.ResolutionAcceptRemote, KnowledgeRevisionID: "97000000-0000-4000-8000-000000000001", Replayed: true}, http.StatusOK},
	}
	for _, test := range cases {
		service.resolveResult = test.result
		response := notesyncHTTPAuthenticatedResponse(handler, http.MethodPost, path, body)
		if response.Code != test.status || service.resolveCmd.DeviceID != notesyncHTTPDeviceID ||
			service.resolveCmd.IdentityReviewBasisHash != strings.Repeat("b", 64) ||
			service.resolveCmd.IdentityReviewOperationID != "96000000-0000-4000-8000-000000000002" ||
			service.resolveCmd.IdentityReviewReceipt != strings.Repeat("c", 64) || len(service.resolveCmd.NodeResolutions) != 1 {
			t.Fatalf("%s status=%d want=%d command=%+v body=%s", test.name, response.Code, test.status, service.resolveCmd, response.Body.String())
		}
	}

	service.resolveResult = notesync.ResolutionResult{}
	before := service.calls["resolve"]
	response := notesyncHTTPAuthenticatedResponse(handler, http.MethodPost, path, `{"basis_hash":"`+strings.Repeat("a", 64)+`","operation_id":"96000000-0000-4000-8000-000000000001","kind":"keep_canonical","device_id":"98000000-0000-4000-8000-000000000001"}`)
	if response.Code != http.StatusBadRequest || notesyncHTTPErrorCode(t, response) != notesync.CodeReviewInvalidRequest || service.calls["resolve"] != before {
		t.Fatalf("client device id=%d code=%s calls=%d body=%s", response.Code, notesyncHTTPErrorCode(t, response), service.calls["resolve"], response.Body.String())
	}

	limited := newNotesyncTestAPIWithBodyLimit(t, []string{"knowledge:write"}, &fakeNotesyncReviewHTTP{}, nil, &logs, 32)
	largeBody := `{"basis_hash":"` + strings.Repeat("a", 64) + `","operation_id":"96000000-0000-4000-8000-000000000001","kind":"merged","merged_markdown":"` + strings.Repeat("x", 64) + `"}`
	response = notesyncHTTPAuthenticatedResponse(limited, http.MethodPost, path, largeBody)
	if response.Code != http.StatusRequestEntityTooLarge || notesyncHTTPErrorCode(t, response) != knowledge.CodePayloadTooLarge {
		t.Fatalf("oversized resolution=%d code=%s body=%s", response.Code, notesyncHTTPErrorCode(t, response), response.Body.String())
	}
}

func TestNotesyncHTTPErrorMappingsUseExactDomainCodes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid", &notesync.ReviewError{Code: notesync.CodeReviewInvalidRequest}, http.StatusBadRequest, notesync.CodeReviewInvalidRequest},
		{"notfound", &notesync.ReviewError{Code: notesync.CodeReviewNotFound}, http.StatusNotFound, notesync.CodeReviewNotFound},
		{"stale", &notesync.ReviewError{Code: notesync.CodeReviewStale}, http.StatusConflict, notesync.CodeReviewStale},
		{"idempotency", &notesync.ReviewError{Code: notesync.CodeReviewIdempotencyConflict}, http.StatusConflict, notesync.CodeReviewIdempotencyConflict},
		{"unavailable", &notesync.ReviewError{Code: notesync.CodeReviewUnavailable}, http.StatusServiceUnavailable, notesync.CodeReviewUnavailable},
		{"content redacted", &notesync.ReviewError{Code: notesync.CodeReviewContentRedacted}, http.StatusServiceUnavailable, notesync.CodeReviewContentRedacted},
		{"privacy content redacted", &privacy.Error{Code: privacy.CodeContentRedacted}, http.StatusServiceUnavailable, memory.CodeContentRedacted},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeNotesyncReviewHTTP{reviewErr: test.err}
			var logs bytes.Buffer
			handler := newNotesyncTestAPI(t, []string{"knowledge:read"}, service, nil, &logs)
			response := notesyncHTTPAuthenticatedResponse(handler, http.MethodGet, "/v1/knowledge/notesync/reviews/"+notesyncHTTPReviewID, "")
			if response.Code != test.status || notesyncHTTPErrorCode(t, response) != test.code {
				t.Fatalf("status=%d code=%s body=%s", response.Code, notesyncHTTPErrorCode(t, response), response.Body.String())
			}
		})
	}
}

func TestNotesyncHTTPIdentityReviewDetailsArePreserved(t *testing.T) {
	review := &knowledge.IdentityReview{
		BasisHash: strings.Repeat("a", 64), OperationID: "99000000-0000-4000-8000-000000000001", Receipt: strings.Repeat("b", 64),
		Documents: []knowledge.DocumentIdentityReview{{Path: "topic.md", Locator: "document-locator", ReasonCode: "ambiguous"}},
		Nodes:     []knowledge.NodeIdentityReview{},
	}
	service := &fakeNotesyncReviewHTTP{reviewErr: &knowledge.Error{Code: knowledge.CodeIdentityReviewRequired, Review: review}}
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:read"}, service, nil, &logs)
	response := notesyncHTTPAuthenticatedResponse(handler, http.MethodGet, "/v1/knowledge/notesync/reviews/"+notesyncHTTPReviewID, "")
	if response.Code != http.StatusConflict || notesyncHTTPErrorCode(t, response) != knowledge.CodeIdentityReviewRequired {
		t.Fatalf("identity review status=%d code=%s body=%s", response.Code, notesyncHTTPErrorCode(t, response), response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	identityReview, ok := payload["identity_review"].(map[string]any)
	if !ok || identityReview["identity_review_operation_id"] != review.OperationID || identityReview["identity_review_receipt"] != review.Receipt || identityReview["identity_review_basis_hash"] != review.BasisHash {
		t.Fatalf("identity review details=%v body=%s", payload["identity_review"], response.Body.String())
	}
}

func TestNotesyncHTTPResponsePermitDropsMarkdownWhenBarrierCancels(t *testing.T) {
	secret := "remote markdown must be discarded before response flush"
	started := make(chan struct{})
	service := &fakeNotesyncReviewHTTP{reviewFn: func(ctx context.Context, _ string) (notesync.Review, error) {
		close(started)
		<-ctx.Done()
		review := notesyncHTTPReviewFixture()
		review.Remote.Markdown = secret
		return review, nil
	}}
	manager := privacy.NewReadPermitManager()
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:read"}, service, manager, &logs)
	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- notesyncHTTPAuthenticatedResponse(handler, http.MethodGet, "/v1/knowledge/notesync/reviews/"+notesyncHTTPReviewID, "")
	}()
	<-started
	drainCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		drainCh <- manager.CloseAndDrain(ctx, 2, privacy.OwnerKnowledge)
	}()
	response := <-responseCh
	if err := <-drainCh; err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), memory.CodeContentRedacted) || strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("barrier response=%d body=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}
}

func TestNotesyncHTTPInternalFailuresDoNotLeakSensitiveValues(t *testing.T) {
	remoteMarkdown := "remote markdown secret"
	mergedMarkdown := "merged content secret"
	underlying := "database password and remote token secret"
	service := &fakeNotesyncReviewHTTP{resolveErr: errors.New(underlying)}
	var logs bytes.Buffer
	handler := newNotesyncTestAPI(t, []string{"knowledge:write"}, service, nil, &logs)
	body := `{"basis_hash":"` + strings.Repeat("a", 64) + `","operation_id":"9a000000-0000-4000-8000-000000000001","kind":"merged","merged_markdown":"` + mergedMarkdown + `"}`
	response := notesyncHTTPResponse(handler, http.MethodPost, "/v1/knowledge/notesync/reviews/"+notesyncHTTPReviewID+"/resolutions", body, "bearer-token-secret")
	for _, secret := range []string{remoteMarkdown, mergedMarkdown, "bearer-token-secret", underlying} {
		if strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("sensitive value leaked: %q response=%s logs=%s", secret, response.Body.String(), logs.String())
		}
	}
	if response.Code != http.StatusInternalServerError || notesyncHTTPErrorCode(t, response) != "internal_error" {
		t.Fatalf("internal failure=%d code=%s body=%s", response.Code, notesyncHTTPErrorCode(t, response), response.Body.String())
	}
}
