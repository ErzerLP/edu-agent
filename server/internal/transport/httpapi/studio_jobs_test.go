package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

// Missing or ambiguous revisions must fail at the HTTP boundary, before a
// planning command can bypass the job's optimistic concurrency checks.
func TestStudioJobsHTTPRequiresUnambiguousVersions(t *testing.T) {
	store, _ := learningcontent.New(nil, nil, nil)
	actor := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: testAggregateID}, Scopes: []string{"learning:read", "learning:write", "knowledge:read"}}}
	h, err := New(Options{Identity: actor, LearningSpaces: contentTestSpaces{}, LearningContent: store, ReadPermits: privacy.NewReadPermitManager(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Readiness: fakeReadiness{}, PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	const base = "/v1/learning/studio/jobs"
	const job = base + "/00000000-0000-4000-8000-000000000001"
	const operation = `"operation_id":"00000000-0000-4000-8000-000000000002"`
	const selection = `"source_selection":{"mode":"items","items":[{"collection_id":"00000000-0000-4000-8000-000000000003","revision_id":"00000000-0000-4000-8000-000000000004","document_id":"00000000-0000-4000-8000-000000000005","document_revision_id":"00000000-0000-4000-8000-000000000006"}]}`
	for _, test := range []struct {
		name, method, path, body string
	}{
		{"create missing direction", "POST", base, `{` + operation + `,"privacy_generation":1,"kind":"guide",` + selection + `}`},
		{"create null direction", "POST", base, `{` + operation + `,"privacy_generation":1,"kind":"guide","direction_version":null,` + selection + `}`},
		{"message missing version", "POST", job + "/plan-messages", `{` + operation + `,"plan_revision":1,"text":"先讲基础"}`},
		{"message null revision", "POST", job + "/plan-messages", `{` + operation + `,"expected_version":1,"plan_revision":null,"text":"先讲基础"}`},
		{"message duplicate revision", "POST", job + "/plan-messages", `{` + operation + `,"expected_version":1,"plan_revision":1,"plan_revision":2,"text":"先讲基础"}`},
		{"message unknown input", "POST", job + "/plan-messages", `{` + operation + `,"expected_version":1,"plan_revision":1,"text":"先讲基础","conversation_id":"private"}`},
		{"patch missing revision", "PATCH", job + "/plan", `{` + operation + `,"expected_version":1,"delete_ids":["section-a"]}`},
		{"patch null order", "PATCH", job + "/plan", `{` + operation + `,"expected_version":1,"plan_revision":1,"ordered_ids":null}`},
		{"confirm missing revision", "POST", job + "/confirm", `{` + operation + `,"expected_version":1}`},
		{"confirm unrelated mutation", "POST", job + "/confirm", `{` + operation + `,"expected_version":1,"plan_revision":1,"text":"change"}`},
		{"command missing version", "POST", job + "/commands", `{` + operation + `,"plan_revision":0,"kind":"retry"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			r.Header.Set("Authorization", "Bearer studio-test")
			r.Header.Set(learningspace.Header, learningspace.DefaultID)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatalf("ambiguous planning input accepted: %d %s", w.Code, w.Body.String())
			}
		})
	}
	// A new space without a direction is valid. This reaches the unavailable
	// service instead of confusing a present zero revision with a missing one.
	r := httptest.NewRequest("POST", base, strings.NewReader(`{`+operation+`,"privacy_generation":1,"kind":"guide","direction_version":0,`+selection+`}`))
	r.Header.Set("Authorization", "Bearer studio-test")
	r.Header.Set(learningspace.Header, learningspace.DefaultID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("valid initial direction rejected: %d %s", w.Code, w.Body.String())
	}
}
