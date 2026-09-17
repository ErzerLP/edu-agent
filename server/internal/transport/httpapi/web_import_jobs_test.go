package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLWebImportJobLifecycle(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if err = os.Chmod(staging, 0700); err != nil {
		t.Fatal(err)
	}
	store := knowledgedb.New(pool, knowledgedb.WithImportJobDirectory(staging))
	service, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	server.Config.Handler, err = New(Options{Identity: ids, Knowledge: service, LearningSpaces: spacedb.New(pool), Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute), WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	defer server.Close()
	var cookie *http.Cookie
	csrf, space, collection := "", learningspace.DefaultID, uuid.NewString()
	request := func(method, path string, body any, want int) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set(learningspace.Header, space)
		if collectionSelectionPath(path) {
			r.Header.Set("X-Knowledge-Collection-ID", collection)
		}
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		response, e := server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		data, e := io.ReadAll(response.Body)
		if e != nil || response.StatusCode != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s err=%v", method, path, response.StatusCode, want, data, e)
		}
		if method == "POST" && path == "/v1/web/pairings" {
			cookie = response.Cookies()[0]
			var session struct {
				CSRF string `json:"csrf_token"`
			}
			if json.Unmarshal(data, &session) != nil {
				t.Fatal("会话格式无效")
			}
			csrf = session.CSRF
		}
		return data
	}
	code, _, err := ids.CreatePairingCodeForProfile(ctx, identity.PairingProfileImport)
	if err != nil {
		t.Fatal(err)
	}
	request("POST", "/v1/web/pairings", map[string]string{"code": code, "display_name": "导入浏览器"}, 201)
	var created learningspace.Space
	if json.Unmarshal(request("POST", "/v1/learning-spaces", learningspace.Command{OperationID: uuid.NewString(), Name: "导入独立区", Status: "active"}, 200), &created) != nil {
		t.Fatal("无法读取学习区")
	}
	space = created.ID
	request("POST", "/v1/knowledge/collections", knowledge.CollectionCommand{ID: collection, Action: "create", Name: "浏览器资料", Source: "web-import-test"}, 200)
	id := uuid.NewString()
	docs := []knowledge.ImportDocument{{Path: "a.md", Markdown: "# Alpha\nfirst persistent source"}, {Path: "b.md", Markdown: "# Biology\n植物的细胞结构与物质运输。"}}
	items := []knowledge.ImportJobItem{}
	for _, doc := range docs {
		sum := sha256.Sum256([]byte(doc.Markdown))
		items = append(items, knowledge.ImportJobItem{Path: doc.Path, Bytes: len(doc.Markdown), Digest: hex.EncodeToString(sum[:])})
	}
	run := func(command knowledge.ImportJobCommand) knowledge.ImportJob {
		t.Helper()
		command.ID = id
		var job knowledge.ImportJob
		if json.Unmarshal(request("POST", "/v1/knowledge/import-jobs", command, 200), &job) != nil {
			t.Fatal("任务格式无效")
		}
		return job
	}
	run(knowledge.ImportJobCommand{Action: "create", Items: items})
	for i := range docs {
		run(knowledge.ImportJobCommand{Action: "upload", Batch: i, Document: &docs[i]})
	}
	job := run(knowledge.ImportJobCommand{Action: "preview"})
	if job.Status != "ready" || job.BatchCount != 2 {
		t.Fatalf("预览失败：%+v", job)
	}
	request("GET", "/v1/knowledge/revisions/head", nil, 404)
	run(knowledge.ImportJobCommand{Action: "confirm", PlanVersion: job.PlanVersion})
	run(knowledge.ImportJobCommand{Action: "continue"})
	if json.Unmarshal(request("GET", "/v1/knowledge/import-jobs/"+id, nil, 200), &job) != nil || job.Batches[0].Status != "completed" || job.Batches[1].Status != "ready" {
		t.Fatal("未按原任务恢复部分发布")
	}
	request("PUT", "/v1/learning-spaces/"+space, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: created.Version, Name: created.Name, Status: "archived"}, 200)
	request("POST", "/v1/knowledge/import-jobs", knowledge.ImportJobCommand{ID: id, Action: "continue"}, 409)
	if json.Unmarshal(request("DELETE", "/v1/knowledge/import-jobs/"+id, nil, 200), &job) != nil || job.Status != "cancelled" || job.Batches[0].Result == nil || job.Batches[1].Result != nil {
		t.Fatal("取消未保留真实发布结果")
	}
	// 新设备即使有相同权限，也不能从原任务 ID 取得所有权。
	code, _, err = ids.CreatePairingCodeForProfile(ctx, identity.PairingProfileImport)
	if err != nil {
		t.Fatal(err)
	}
	cookie = nil
	request("POST", "/v1/web/pairings", map[string]string{"code": code, "display_name": "另一导入设备"}, 201)
	request("GET", "/v1/knowledge/import-jobs/"+id, nil, 404)
}
