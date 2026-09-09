package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type goalHTTPResolver struct{}

func (goalHTTPResolver) Resolve(context.Context, string, string) (learning.KnowledgeReference, error) {
	return learning.KnowledgeReference{}, &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid}
}

func TestPostgreSQLGoalHTTPManagementContract(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未配置真实 PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "goal_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
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
	actor := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'目标 HTTP',now())`, actor); err != nil {
		t.Fatal(err)
	}
	spaces := spacedb.New(pool)
	a, err := spaces.Mutate(ctx, actor, "", space.Command{OperationID: uuid.NewString(), Name: "Go 后端", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	store := learningdb.New(pool, tutoringdb.New(pool), kstore)
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := learning.NewService(store, store, goalHTTPResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	auth := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: actor}, Scopes: []string{"learning:read", "learning:write"}}}
	handler, err := New(Options{Identity: auth, LearningSpaces: spaces, Learning: service, Knowledge: ks, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, scope string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(payload)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(space.Header, scope)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	validate := func(schema string, rec *httptest.ResponseRecorder) {
		t.Helper()
		var value any
		if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas[schema].Value.VisitJSON(value, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("实际响应不符合 %s：%v %s", schema, err, rec.Body)
		}
	}
	goalID := uuid.NewString()
	body := map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "goal", "aggregate_id": goalID, "expected_version": 0, "text": "掌握并发", "source": "http-test"}
	created := request("POST", "/v1/learning/goals", a.ID, body)
	if created.Code != 201 {
		t.Fatalf("创建：%d %s", created.Code, created.Body)
	}
	validate("GoalOperationResult", created)
	var result struct {
		Result learning.GoalRevision `json:"result"`
	}
	_ = json.Unmarshal(created.Body.Bytes(), &result)
	if result.Result.Management.Status != "draft" {
		t.Fatal("新目标不是草稿")
	}
	if rec := request("POST", "/v1/learning/goals", a.ID, body); rec.Code != 200 {
		t.Fatalf("重试：%d %s", rec.Code, rec.Body)
	}
	if rec := request("GET", "/v1/learning/goals/"+goalID, space.DefaultID, nil); rec.Code != 404 {
		t.Fatalf("跨区读取：%d", rec.Code)
	}
	page := request("GET", "/v1/learning/goals?status=draft&search=并发&limit=1", a.ID, nil)
	if page.Code != 200 {
		t.Fatal(page.Body)
	}
	validate("GoalPage", page)
	body["operation_id"] = uuid.NewString()
	body["expected_version"] = 1
	body["previous_revision_id"] = result.Result.ID
	body["details"] = learning.GoalDetails{Name: "Go 并发", Scope: "Channel\nContext", SelfAssessment: "自述熟悉 goroutine"}
	updated := request("PUT", "/v1/learning/goals/"+goalID, a.ID, body)
	if updated.Code != 201 {
		t.Fatalf("修订：%d %s", updated.Code, updated.Body)
	}
	validate("GoalOperationResult", updated)
	// 同一真实 HTTP 服务按区创建和读取独立教学会话，列表响应同时核对公开 schema。
	sessionID := uuid.NewString()
	sessionBody := map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "session", "aggregate_id": sessionID, "expected_version": 0, "goal_revision_id": result.Result.ID}
	sessionCreated := request("POST", "/v1/tutoring/sessions", a.ID, sessionBody)
	if sessionCreated.Code != 201 {
		t.Fatalf("教学创建：%d %s", sessionCreated.Code, sessionCreated.Body)
	}
	validate("SessionOperationResult", sessionCreated)
	sessions := request("GET", "/v1/tutoring/sessions?goal_id="+goalID, a.ID, nil)
	if sessions.Code != 200 {
		t.Fatalf("教学列表：%d %s", sessions.Code, sessions.Body)
	}
	validate("TutoringSessionPage", sessions)
	var sessionPage learning.SessionPage
	if err := json.Unmarshal(sessions.Body.Bytes(), &sessionPage); err != nil || len(sessionPage.Items) != 1 || sessionPage.Items[0].SessionID != sessionID {
		t.Fatalf("教学列表归属：%s %v", sessions.Body, err)
	}
	for _, scope := range []string{a.ID, space.DefaultID} {
		detail := request("GET", "/v1/tutoring/sessions/"+sessionID, scope, nil)
		if scope == a.ID {
			if detail.Code != 200 {
				t.Fatal(detail.Body)
			}
			validate("SessionView", detail)
		} else if detail.Code != 404 {
			t.Fatalf("跨区教学读取未拒绝：%d", detail.Code)
		}
	}
	body["operation_id"] = uuid.NewString()
	if rec := request("PUT", "/v1/learning/goals/"+goalID, a.ID, body); rec.Code != 409 {
		t.Fatalf("过期修订未冲突：%d %s", rec.Code, rec.Body)
	}
	page = request("GET", "/v1/learning/goals/"+goalID+"/revisions", a.ID, nil)
	if page.Code != 200 {
		t.Fatal(page.Body)
	}
	validate("GoalPage", page)
	var history learning.GoalPage
	_ = json.Unmarshal(page.Body.Bytes(), &history)
	if len(history.Items) != 2 || history.Items[0].Management.Details.Scope != "" {
		t.Fatal("旧修订被覆盖")
	}
	body["expected_version"] = 2
	body["previous_revision_id"] = history.Items[1].ID
	body["operation_id"] = uuid.NewString()
	delete(body, "details")
	body["action"] = "complete"
	body["completion_reason"] = "自行确认已完成"
	completed := request("PUT", "/v1/learning/goals/"+goalID, a.ID, body)
	if completed.Code != 201 {
		t.Fatalf("完成：%d %s", completed.Code, completed.Body)
	}
	validate("GoalOperationResult", completed)
	detail := request("GET", "/v1/learning/goals/"+goalID, a.ID, nil)
	validate("GoalRevision", detail)
	for _, method := range []string{"GET", "PUT"} {
		auth.auth.Scopes = []string{}
		rec := request(method, "/v1/learning/goals/"+goalID, a.ID, body)
		if rec.Code != 403 {
			t.Fatalf("无权限调用：%s %d", method, rec.Code)
		}
	}
}
