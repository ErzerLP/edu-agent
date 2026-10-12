package httpapi

import (
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStudioHTTPRequiresExplicitSpaceAndScopes(t *testing.T) {
	store, _ := learningcontent.New(nil, nil, nil)
	actor := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: testAggregateID}, Scopes: []string{"learning:read", "learning:write"}}}
	h, err := New(Options{Identity: actor, LearningSpaces: contentTestSpaces{}, LearningContent: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Readiness: fakeReadiness{}, PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		space string
		want  int
	}{{"", 400}, {learningspace.DefaultID, 503}} {
		r := httptest.NewRequest("GET", "/v1/learning/studio/artifacts", nil)
		r.Header.Set("Authorization", "Bearer studio-test")
		if c.space != "" {
			r.Header.Set(learningspace.Header, c.space)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Fatalf("want %d got %d: %s", c.want, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{
		`{"operation_id":"00000000-0000-4000-8000-000000000001","privacy_generation":1,"kind":"note","goal_id":null,"title":"title","markdown":"body"}`,
		`{"operation_id":"00000000-0000-4000-8000-000000000001","expected_version":null,"privacy_generation":1,"kind":"note","goal_id":null,"title":"title","markdown":"body"}`,
	} {
		r := httptest.NewRequest("POST", "/v1/learning/studio/artifacts", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer studio-test")
		r.Header.Set(learningspace.Header, learningspace.DefaultID)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("missing or null fields accepted: %d %s", w.Code, w.Body.String())
		}
	}
	actor.auth.Scopes = nil
	r := httptest.NewRequest("GET", "/v1/learning/studio/artifacts", nil)
	r.Header.Set("Authorization", "Bearer studio-test")
	r.Header.Set(learningspace.Header, learningspace.DefaultID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
}
