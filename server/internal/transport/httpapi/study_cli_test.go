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
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

// 生产 CLI 与真实 Cookie/CSRF HTTP 共用 PG 权威记录；此用例不冒充渲染浏览器验收。
func TestPostgreSQLStudyCLIAndWebShareChanges(t *testing.T) {
	binary := os.Getenv("EDU_AGENT_INTEGRATION_CLI")
	if binary == "" {
		t.Skip("需要生产 CLI 二进制 EDU_AGENT_INTEGRATION_CLI")
	}
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	k := knowledgedb.New(pool)
	l := learningdb.New(pool, tutoringdb.New(pool), k)
	if _, err = l.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	content, err := learningcontent.New(pool, l, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	l.ConfigureContent(content)
	changes, err := learningchange.New(pool, l, k, content, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := learning.NewService(l, l, goalHTTPResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	handler, err := New(Options{Identity: ids, Learning: service, LearningSpaces: spacedb.New(pool), LearningContent: content, LearningChanges: changes, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute), WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	var cookie *http.Cookie
	csrf := ""
	web := func(method, path string, body any) map[string]json.RawMessage {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Learning-Space-ID", learningspace.DefaultID)
		r.Header.Set("X-Learning-Change-Version", "1")
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		response, e := server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("Web %s：%d %s", path, response.StatusCode, data)
		}
		if cookies := response.Cookies(); len(cookies) > 0 {
			cookie = cookies[0]
		}
		var result map[string]json.RawMessage
		if json.Unmarshal(data, &result) != nil {
			t.Fatal("Web 响应无效")
		}
		return result
	}
	code, _, err := ids.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paired := web("POST", "/v1/web/pairings", map[string]string{"code": code, "display_name": "Web 正式课堂"})
	_ = json.Unmarshal(paired["csrf_token"], &csrf)
	goal, sid := uuid.NewString(), uuid.NewString()
	g := web("POST", "/v1/learning/goals", map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "goal", "aggregate_id": goal, "expected_version": 0, "text": "概率基础", "source": "web"})
	var revision learning.GoalRevision
	_ = json.Unmarshal(g["result"], &revision)
	web("POST", "/v1/tutoring/sessions", map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "session", "aggregate_id": sid, "expected_version": 0, "goal_revision_id": revision.ID})
	other := uuid.NewString()
	web("POST", "/v1/learning/goals", map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "goal", "aggregate_id": other, "expected_version": 0, "text": "英语阅读", "source": "web"})
	cliRoot := t.TempDir()
	cli := func(input string, args ...string) []byte {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, args...)
		command.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cliRoot, "TERM=dumb", "NO_COLOR=1")
		command.Stdin = strings.NewReader(input)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, e := command.Output()
		if e != nil {
			t.Fatalf("生产 CLI %v：%v %s", args, e, stderr.String())
		}
		return output
	}
	code, _, err = ids.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cli(code+"\n", "pair", "--server", origin, "--name", "CLI 跨端继续")
	var snapshot learningchange.Snapshot
	if json.Unmarshal(cli("", "study", "change-context", "--goal", goal, "--session", sid), &snapshot) != nil || snapshot.Session.ID != sid {
		t.Fatal("CLI 未读取 Web 原会话")
	}
	details := snapshot.Goal.GoalManagement().Details
	details.Scope = "条件概率"
	cid := uuid.NewString()
	proposal := map[string]any{"operation_id": uuid.NewString(), "session_id": sid, "action": "propose", "expected_revision": 0, "hash": "", "interaction_id": "", "immediate": false, "base": snapshot.Base, "candidate": learningchange.Candidate{Kind: "goal", Trigger: "user_request", Reason: "用户明确扩展学习范围", Goal: &details}}
	raw, _ := json.Marshal(proposal)
	var change learningchange.Change
	if json.Unmarshal(cli(string(raw), "study", "change-command", "--goal", goal, "--change", cid, "--input", "-"), &change) != nil || change.Status != "waiting_approval" {
		t.Fatal("CLI 候选没有进入明确审阅")
	}
	read := web("GET", "/v1/learning/goals/"+goal+"/changes/"+cid, nil)
	if string(read["hash"]) != "\""+change.Hash+"\"" {
		t.Fatal("Web 未看到同一具体候选")
	}
	approval := map[string]any{"operation_id": uuid.NewString(), "session_id": sid, "action": "approve", "expected_revision": change.Revision, "hash": change.Hash, "interaction_id": change.InteractionID, "immediate": false}
	raw, _ = json.Marshal(approval)
	first := cli(string(raw), "study", "change-command", "--goal", goal, "--change", cid, "--input", "-")
	second := cli(string(raw), "study", "change-command", "--goal", goal, "--change", cid, "--input", "-")
	if !bytes.Equal(first, second) {
		t.Fatal("同操作重试没有返回原正式结果")
	}
	read = web("GET", "/v1/learning/goals/"+goal+"/changes/"+cid, nil)
	if string(read["status"]) != "\"applied\"" {
		t.Fatal("Web 未读取 CLI 采用后的状态", string(read["status"]))
	}
	var otherResult struct {
		Revision int64 `json:"revision"`
	}
	otherRaw, _ := json.Marshal(web("GET", "/v1/learning/goals/"+other, nil))
	_ = json.Unmarshal(otherRaw, &otherResult)
	if otherResult.Revision != 1 {
		t.Fatal("另一目标被修改")
	}
	var cliProgress, webProgress map[string]json.RawMessage
	_ = json.Unmarshal(cli("", "progress", "--status", "all", "--json"), &cliProgress)
	webProgress = web("GET", "/v1/learning/progress?status=all", nil)
	if string(cliProgress["total"]) != string(webProgress["total"]) || string(cliProgress["total"]) != "2" {
		t.Fatal("两端进度不一致")
	}
	var sessions, attempts, evidence int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tutoring_sessions),(SELECT count(*) FROM learning_attempts),(SELECT count(*) FROM learning_evidence)`).Scan(&sessions, &attempts, &evidence); err != nil || sessions != 1 || attempts != 0 || evidence != 0 {
		t.Fatal("跨端复制了会话、答案或证据", sessions, attempts, evidence, err)
	}
}
