package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepostgres "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgreSQLLearningChangeHTTPContract(t *testing.T) {
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
	schema := "change_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	actor := identity.Credential{Device: identity.Device{ID: uuid.NewString()}, TokenID: uuid.NewString(), Scopes: []string{"learning:read", "learning:write"}}
	if _, err = pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'变更 HTTP',now())`, actor.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at) VALUES($1,$2,decode(repeat('23',32),'hex'),$3,now())`, actor.TokenID, actor.Device.ID, actor.Scopes); err != nil {
		t.Fatal(err)
	}
	k := knowledgepostgres.New(pool)
	l := learningpostgres.New(pool, tutoringpostgres.New(pool), k)
	content, _ := learningcontent.New(pool, l, bytes.Repeat([]byte{42}, 32))
	changes, _ := learningchange.New(pool, l, k, content, bytes.Repeat([]byte{42}, 32))
	service, err := learning.NewService(l, l, goalHTTPResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	goal, sid := uuid.NewString(), uuid.NewString()
	op := func(kind, id string) learning.OperationEnvelope {
		return learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: kind, AggregateID: id, Payload: json.RawMessage(`{}`)}
	}
	g, err := service.CreateGoal(ctx, actor.Device.ID, learning.GoalCommand{Operation: op("goal", goal), GoalID: goal, Text: "学习概率", Source: "变更验收", Details: &learning.GoalDetails{Name: "学习概率"}})
	if err != nil {
		t.Fatal(err)
	}
	var revision learning.GoalRevision
	if err = json.Unmarshal(g.Result, &revision); err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateSession(ctx, actor.Device.ID, learning.SessionCommand{Operation: op("session", sid), GoalRevisionID: revision.ID}); err != nil {
		t.Fatal(err)
	}
	auth := &fakeIdentity{auth: actor}
	handler, err := New(Options{Identity: auth, LearningSpaces: spacepostgres.New(pool), Learning: service, LearningChanges: changes, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, space, protocol string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Learning-Space-ID", space)
		req.Header.Set("X-Learning-Change-Version", protocol)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	validate := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("HTTP %d：%s", w.Code, w.Body)
		}
		var v any
		if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatal(e)
		}
		if e := doc.Components.Schemas[name].Value.VisitJSON(v, openapi3.EnableJSONSchema2020()); e != nil {
			t.Fatalf("%s 不符合 OpenAPI：%v\n%s", name, e, w.Body)
		}
	}
	path := "/v1/learning/goals/" + goal
	if w := call("GET", path+"/change-context?session_id="+sid, learningspace.DefaultID, "", nil); w.Code != 400 {
		t.Fatal("未协商协议被接受")
	}
	w := call("GET", path+"/change-context?session_id="+sid, learningspace.DefaultID, "1", nil)
	validate("LearningChangeContext", w)
	var snap learningchange.Snapshot
	if err = json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	details := snap.Goal.GoalManagement().Details
	details.Scope = "条件概率"
	details.CompletionCriteria = "独立说明贝叶斯公式的条件"
	id := uuid.NewString()
	cmd := learningchange.Command{OperationID: uuid.NewString(), Action: "propose", Base: &snap.Base, Candidate: &learningchange.Candidate{Kind: "goal", Trigger: "user_request", Reason: "用户新增要求", Goal: &details}}
	body := func(cmd learningchange.Command) any {
		return struct {
			SessionID string `json:"session_id"`
			learningchange.Command
		}{sid, cmd}
	}
	w = call("POST", path+"/changes/"+id, learningspace.DefaultID, "1", body(cmd))
	validate("LearningChange", w)
	var change learningchange.Change
	if err = json.Unmarshal(w.Body.Bytes(), &change); err != nil {
		t.Fatal(err)
	}
	if change.Status != "waiting_approval" {
		t.Fatal("未确认写目标")
	}
	if w = call("GET", path+"/changes/"+id, uuid.NewString(), "1", nil); w.Code != 404 {
		t.Fatal("跨区候选未拒绝", w.Code)
	}
	approve := learningchange.Command{OperationID: uuid.NewString(), Action: "approve", ExpectedRevision: change.Revision, Hash: "错误摘要", InteractionID: change.InteractionID}
	if w = call("POST", path+"/changes/"+id, learningspace.DefaultID, "1", body(approve)); w.Code != 409 {
		t.Fatal("旧摘要被批准", w.Code)
	}
	approve.Hash = change.Hash
	w = call("POST", path+"/changes/"+id, learningspace.DefaultID, "1", body(approve))
	validate("LearningChange", w)
	if !strings.Contains(w.Body.String(), `"status":"applied"`) {
		t.Fatal("批准未生效")
	}
	if w = call("POST", path+"/changes/"+id, learningspace.DefaultID, "1", map[string]any{"mastery": 100}); w.Code != 400 {
		t.Fatal("接受未声明事实写入")
	}
}
