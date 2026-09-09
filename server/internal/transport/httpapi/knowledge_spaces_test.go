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
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLKnowledgeSpacesHTTPContract(t *testing.T) {
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
	schema := "knowledge_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	spaces := spacedb.New(pool)
	actor := uuid.NewString()
	a, err := spaces.Mutate(ctx, actor, "", space.Command{OperationID: uuid.NewString(), Name: "Go 后端", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := spaces.Mutate(ctx, actor, "", space.Command{OperationID: uuid.NewString(), Name: "英语", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := knowledge.NewService(knowledgedb.New(pool), knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: actor}, Scopes: []string{"learning:read", "knowledge:read", "knowledge:write", "knowledge:approve"}}}, LearningSpaces: spaces, Knowledge: service, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, spaceID, collection string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(payload)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set(space.Header, spaceID)
		if collection != "" {
			req.Header.Set(knowledge.CollectionHeader, collection)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	id := uuid.NewString()
	created := request("POST", "/v1/knowledge/collections", a.ID, "", knowledge.CollectionCommand{ID: id, Action: "create", Name: "Go 来源", Source: "repository-a"})
	if created.Code != 200 {
		t.Fatalf("创建集合: %d %s", created.Code, created.Body)
	}
	imported := request("POST", "/v1/knowledge/imports", a.ID, id, map[string]any{"operation_id": uuid.NewString(), "expected_parent_revision_id": nil, "source": "repository-a", "documents": []knowledge.ImportDocument{{Path: "README.md", Markdown: "# Channel\nchannel private example\n"}}})
	var result knowledge.ImportResult
	if imported.Code != 201 || json.Unmarshal(imported.Body.Bytes(), &result) != nil {
		t.Fatalf("导入集合: %d %s", imported.Code, imported.Body)
	}
	for _, path := range []string{"/v1/knowledge/revisions/" + result.Revision.ID + "/tree", "/v1/knowledge/revisions/" + result.Revision.ID + "/export"} {
		if rec := request("GET", path, b.ID, id, nil); rec.Code != 404 {
			t.Fatalf("跨区读取: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := request("POST", "/v1/knowledge/collections", b.ID, "", knowledge.CollectionCommand{ID: id, Action: "link"}); rec.Code != 404 {
		t.Fatalf("未共享集合被关联: %d %s", rec.Code, rec.Body)
	}
	for _, step := range []struct{ space, action string }{{a.ID, "share"}, {b.ID, "link"}} {
		if rec := request("POST", "/v1/knowledge/collections", step.space, "", knowledge.CollectionCommand{ID: id, Action: step.action, Shared: true, ExpectedVersion: 1}); rec.Code != 200 {
			t.Fatalf("共享关联: %d %s", rec.Code, rec.Body)
		}
	}
	snapshotID := uuid.NewString()
	snapshot := knowledge.ScopeSnapshot{ID: snapshotID, Entries: []knowledge.ScopeEntry{{CollectionID: id, RevisionID: result.Revision.ID}}}
	if rec := request("POST", "/v1/knowledge/scopes", b.ID, "", snapshot); rec.Code != 200 {
		t.Fatalf("冻结范围: %d %s", rec.Code, rec.Body)
	}
	if rec := request("POST", "/v1/knowledge/retrievals", b.ID, "", map[string]any{"query": "channel", "scope_snapshot_id": snapshotID}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "channel private example") {
		t.Fatalf("范围检索: %d %s", rec.Code, rec.Body)
	}
	if rec := request("GET", "/v1/knowledge/scopes/"+snapshotID+"/export", a.ID, "", nil); rec.Code != 404 {
		t.Fatalf("跨区范围: %d %s", rec.Code, rec.Body)
	}
	if rec := request("POST", "/v1/knowledge/retrievals", b.ID, id, map[string]any{"query": "channel", "scope_snapshot_id": snapshotID}); rec.Code != 400 {
		t.Fatalf("混用集合与冻结范围未被拒绝: %d %s", rec.Code, rec.Body)
	}
	if rec := request("GET", "/v1/knowledge/revisions/head?collection_id="+id, b.ID, "", nil); rec.Code != 400 {
		t.Fatalf("错误范围参数被静默忽略: %d %s", rec.Code, rec.Body)
	}
}
