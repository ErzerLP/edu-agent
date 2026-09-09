package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestExplicitLearningSpaceMustNotFallBackToGlobal(t *testing.T) {
	service := &fakeLearning{}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &bytes.Buffer{})
	request := httptest.NewRequest(http.MethodGet, "/v1/tutoring/sessions/current", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("X-Learning-Space-ID", "forged-space")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusOK || service.calls != 0 {
		t.Fatalf("explicit scope silently used global service: status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}
}

func TestPostgreSQLLearningSpaceHTTPContract(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; real PostgreSQL HTTP scenario not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "space_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	actor := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: uuid.NewString()}, Scopes: []string{"learning:read", "learning:write"}}}
	legacy := &fakeLearning{}
	handler, err := New(Options{Identity: actor, LearningSpaces: spacedb.New(pool), Learning: legacy, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, header []string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		if header != nil {
			req.Header["X-Learning-Space-Id"] = header
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	caps := request("GET", "/v1/learning-spaces/capabilities", "", nil)
	if caps.Code != 200 || !strings.Contains(caps.Body.String(), space.DefaultID) {
		t.Fatalf("capabilities=%d %s", caps.Code, caps.Body)
	}
	c := space.Command{OperationID: uuid.NewString(), Name: "Go 后端", Description: "persistent", Status: "active"}
	raw, _ := json.Marshal(c)
	created := request("POST", "/v1/learning-spaces", string(raw), nil)
	var item space.Space
	if created.Code != 200 || json.Unmarshal(created.Body.Bytes(), &item) != nil || !space.ValidID(item.ID) {
		t.Fatalf("create=%d %s", created.Code, created.Body)
	}
	replay := request("POST", "/v1/learning-spaces", string(raw), nil)
	if replay.Body.String() != created.Body.String() {
		t.Fatalf("retry response changed: %s", replay.Body)
	}
	for _, tc := range []struct {
		name   string
		header []string
		path   string
		want   int
	}{
		{"omitted", nil, "/v1/tutoring/sessions/current", 200},
		{"default", []string{space.DefaultID}, "/v1/tutoring/sessions/current", 200},
		{"nondefault", []string{item.ID}, "/v1/tutoring/sessions/current", 501},
		{"empty", []string{""}, "/v1/tutoring/sessions/current", 400},
		{"malformed", []string{"forged"}, "/v1/tutoring/sessions/current", 400},
		{"missing", []string{uuid.NewString()}, "/v1/tutoring/sessions/current", 404},
		{"duplicate", []string{space.DefaultID, space.DefaultID}, "/v1/tutoring/sessions/current", 400},
		{"query alias", nil, "/v1/tutoring/sessions/current?learning_space_id=" + item.ID, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := legacy.calls
			rec := request("GET", tc.path, "", tc.header)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if tc.want != 200 && legacy.calls != before {
				t.Fatal("invalid scope called global service")
			}
		})
	}
	c.OperationID = uuid.NewString()
	c.ExpectedVersion = item.Version
	c.Status = "archived"
	raw, _ = json.Marshal(c)
	if rec := request("PUT", "/v1/learning-spaces/"+item.ID, string(raw), nil); rec.Code != 200 {
		t.Fatalf("archive=%d %s", rec.Code, rec.Body)
	}
	if rec := request("GET", "/v1/learning-spaces/"+item.ID, "", nil); rec.Code != 200 {
		t.Fatal("archived details unavailable")
	}
	if rec := request("POST", "/v1/learning/goals", "{}", []string{item.ID}); rec.Code != 409 {
		t.Fatalf("archived write=%d %s", rec.Code, rec.Body)
	}
	actor.auth.Scopes = []string{"learning:read"}
	if rec := request("PUT", "/v1/learning-spaces/"+item.ID, string(raw), nil); rec.Code != 403 {
		t.Fatalf("unauthorized edit=%d", rec.Code)
	}
	if rec := request("GET", "/v1/learning-spaces?search=absent", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty result=%d %s", rec.Code, rec.Body)
	}
}
