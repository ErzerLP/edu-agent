package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLLearnerAdminIssuanceAndHTTPPermissionMatrix(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lstore := learningdb.New(pool, tutoringdb.New(pool), kstore)
	ls, err := learning.NewService(lstore, lstore, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{42}, 32)
	changes, err := learningchange.New(pool, lstore, kstore, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	content, err := learningcontent.New(pool, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json")})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := mentorrun.New(pool, configuration, key)
	if err != nil {
		t.Fatal(err)
	}
	memories, err := memory.NewService(memorydb.New(pool), memory.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("http://127.0.0.1:8080")
	h, err := New(Options{
		Identity: ids, Knowledge: ks, Learning: ls, LearningChanges: changes, LearningContent: content,
		LearningSpaces: spacedb.New(pool), Settings: configuration, MentorRuns: runs, Memory: memories, MemoryExporter: &fakeMemoryExporter{},
		Privacy:   &fakePrivacyHTTP{receiptErr: &privacy.Error{Code: privacy.CodeNotFound}},
		Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute),
		AdminUI: AdminUIOptions{Enabled: true, Identity: ids, PublicBaseURL: base, Token: adminTestToken, TrustedLoopbackProxy: true,
			AuthLimiter: NewFixedWindowLimiter(100, time.Minute), WriteLimiter: NewFixedWindowLimiter(100, time.Minute)},
		WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids,
			Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := loginAdmin(t, h)
	for _, header := range []string{"Origin", adminCSRFHeader} {
		r := authenticatedAdminRequest("POST", "/admin/api/pairing-codes", `{"profile":"learner"}`, admin)
		r.Header.Del(header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("admin missing %s: %d %s", header, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authenticatedAdminRequest("POST", "/admin/api/pairing-codes", `{"profile":"learner"}`, admin))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var issued struct {
		Code    string `json:"code"`
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil || issued.Profile != "learner" {
		t.Fatal(issued, err)
	}
	var cookie *http.Cookie
	csrf := ""
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := adminRequest(method, path, body, "application/json")
		r.Header.Set(learningspace.Header, learningspace.DefaultID)
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, r)
		if response.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, want, response.Body.String())
		}
		return response
	}
	raw, _ := json.Marshal(map[string]string{"code": issued.Code, "display_name": "学习端全权"})
	w = call("POST", "/v1/web/pairings", string(raw), 201)
	var session struct {
		Device       identity.Device `json:"device"`
		CSRF         string          `json:"csrf_token"`
		Capabilities struct {
			References bool `json:"references"`
			ImportJobs bool `json:"import_jobs"`
			SaveGoal   bool `json:"save_goal"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookie, csrf = w.Result().Cookies()[0], session.CSRF
	if !session.Capabilities.References || !session.Capabilities.ImportJobs || !session.Capabilities.SaveGoal {
		t.Fatal("learner capabilities unavailable", session.Capabilities)
	}
	var original []string
	if err := pool.QueryRow(ctx, `SELECT scopes FROM device_tokens WHERE device_id=$1`, session.Device.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	entity := uuid.NewString()
	// Malformed writes must reach input validation (400) with learner, and fail
	// at the real route's scope gate (403) after removing each required scope.
	// Successful import/Studio/memory writes are exercised by learner.spec.ts.
	for _, test := range []struct {
		name, method, path string
		status             int
		scopes             []string
	}{
		{"spaces read", "GET", "/v1/learning-spaces", 200, []string{"learning:read"}},
		{"spaces write", "POST", "/v1/learning-spaces", 400, []string{"learning:write"}},
		{"goals", "POST", "/v1/learning/goals", 400, []string{"learning:write"}},
		{"conversation", "POST", "/v1/learning/goals/" + entity + "/runs", 400, []string{"learning:write"}},
		{"practice", "POST", "/v1/tutoring/sessions/" + entity + "/actions", 400, []string{"learning:write"}},
		{"sources", "GET", "/v1/knowledge/sources", 200, []string{"knowledge:read"}},
		{"import", "POST", "/v1/knowledge/imports/previews", 400, []string{"knowledge:read", "knowledge:write"}},
		{"import approval", "POST", "/v1/knowledge/imports/confirm", 400, []string{"knowledge:write", "knowledge:approve"}},
		{"import jobs", "POST", "/v1/knowledge/import-jobs", 400, []string{"knowledge:read", "knowledge:write"}},
		{"references", "POST", "/v1/learning/goals/" + entity + "/references/previews", 400, []string{"references:manage", "knowledge:approve", "learning:write"}},
		{"studio read", "GET", "/v1/learning/studio/artifacts", 200, []string{"learning:read"}},
		{"studio note", "POST", "/v1/learning/studio/artifacts", 400, []string{"learning:write"}},
		{"studio generate", "POST", "/v1/learning/studio/generations", 400, []string{"learning:write"}},
		{"assessment", "POST", "/v1/learning/assessments/" + entity + "/decisions", 400, []string{"learning:write", "learning:approve"}},
		{"evidence review", "POST", "/v1/learning/evidence-carryovers/" + entity + "/approve", 400, []string{"learning:approve"}},
		{"memory read", "GET", "/v1/memory/candidates", 200, []string{"memory:read"}},
		{"memory candidate", "POST", "/v1/memory/candidates", 400, []string{"memory:write"}},
		{"memory review", "POST", "/v1/memory/candidates/" + entity + "/decisions", 400, []string{"memory:write"}},
		{"privacy receipts", "GET", "/v1/privacy/erasures/" + entity, 404, []string{"privacy:read"}},
		{"devices", "GET", "/v1/devices", 200, []string{"devices:read"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			call(test.method, test.path, "{", test.status)
			for _, scope := range test.scopes {
				if _, err := pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,$2) WHERE device_id=$1`, session.Device.ID, scope); err != nil {
					t.Fatal(err)
				}
				call(test.method, test.path, "{", 403)
				if _, err := pool.Exec(ctx, `UPDATE device_tokens SET scopes=$2 WHERE device_id=$1`, session.Device.ID, original); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, test := range []struct{ method, path string }{
		{"PUT", "/v1/settings"}, {"POST", "/v1/settings/probes"}, {"GET", "/v1/model/capabilities"},
		{"GET", "/admin/"}, {"POST", "/admin/api/pairing-codes"},
		{"GET", "/v1/privacy/erasures/" + entity + "/offline-device-purge"},
	} {
		call(test.method, test.path, "{}", 403)
	}
	call("GET", "/v1/settings", "", 200)
	call("POST", "/v1/privacy/erasures", `{"operation_id":"`+uuid.NewString()+`","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`, 403)
	// A rejected erase must leave the session and data generation usable.
	call("GET", "/v1/web/session", "", 200)
	if _, err := pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'devices:manage') WHERE device_id=$1`, session.Device.ID); err != nil {
		t.Fatal(err)
	}
	call("DELETE", "/v1/devices/"+session.Device.ID, "", 403)
	if _, err := pool.Exec(ctx, `UPDATE device_tokens SET scopes=$2 WHERE device_id=$1`, session.Device.ID, original); err != nil {
		t.Fatal(err)
	}
	call("DELETE", "/v1/devices/"+session.Device.ID, "", 204)
	call("GET", "/v1/web/session", "", 401)
}
