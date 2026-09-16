package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLMentorCookieHTTPAndSSERecovery(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	store := learningdb.New(pool, tutoringdb.New(pool))
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	learningService, err := learning.NewService(store, store, goalHTTPResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	const privateGoal = "浏览器绑定的概率目标"
	const privateOutput = "针对概率目标的真实夹具回答"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte(privateGoal)) {
			t.Error("共享核心没有读取绑定目标")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", privateOutput)
	}))
	defer provider.Close()
	configuration, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{provider.URL}})
	if err != nil {
		t.Fatal(err)
	}
	limits := settings.DefaultLimits()
	limits.OutputTokens = 64
	if _, err = configuration.Update(settings.Update{ExpectedRevision: 0, Target: settings.Mentor, Connection: &settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: provider.URL, Model: "http-fixture", AuthMode: "none"}, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	runtime, err := mentorrun.New(pool, configuration, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	var logs bytes.Buffer
	handler, err := New(Options{Identity: ids, Learning: learningService, LearningSpaces: spacedb.New(pool), Settings: configuration, MentorRuns: runtime, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(&logs, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute), WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	var cookie *http.Cookie
	csrf := ""
	request := func(method, path, scope string, body any, alter func(*http.Request)) (*http.Response, []byte) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		if scope != "" {
			r.Header.Set(learningspace.Header, scope)
		}
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if alter != nil {
			alter(r)
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, data
	}
	code, _, err := ids.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	response, data := request("POST", "/v1/web/pairings", "", map[string]string{"code": code, "display_name": "导师浏览器验收"}, nil)
	if response.StatusCode != 201 {
		t.Fatalf("配对失败 %d", response.StatusCode)
	}
	cookie = response.Cookies()[0]
	var session struct {
		CSRF   string          `json:"csrf_token"`
		Device identity.Device `json:"device"`
	}
	if err = json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	csrf = session.CSRF
	response, data = request("POST", "/v1/learning-spaces", "", learningspace.Command{OperationID: uuid.NewString(), Name: "导师独立区", Status: "active"}, nil)
	if response.StatusCode != 200 {
		t.Fatalf("创建学习区失败 %d", response.StatusCode)
	}
	var space learningspace.Space
	json.Unmarshal(data, &space)
	goal := uuid.NewString()
	response, _ = request("POST", "/v1/learning/goals", space.ID, map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "goal", "aggregate_id": goal, "expected_version": 0, "text": privateGoal, "source": "web"}, nil)
	if response.StatusCode != 201 {
		t.Fatalf("真实目标未创建 %d", response.StatusCode)
	}
	create := mentorrun.Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), ExpectedVersion: 1, Prompt: "解释目标", Save: true, RequestBudget: 2, TokenBudget: 10000}
	path := "/v1/learning/goals/" + goal + "/runs"
	response, _ = request("POST", path, space.ID, create, func(r *http.Request) { r.Header.Del("X-CSRF-Token") })
	if response.StatusCode != 403 {
		t.Fatal("运行创建绕过 CSRF")
	}
	response, _ = request("POST", path, "", create, nil)
	if response.StatusCode != 400 {
		t.Fatal("运行允许猜测学习区")
	}
	response, data = request("POST", path, space.ID, create, nil)
	if response.StatusCode != 202 {
		t.Fatalf("运行未受理 %d %s", response.StatusCode, data)
	}
	var receipt mentorrun.Receipt
	json.Unmarshal(data, &receipt)
	response, retry := request("POST", path, space.ID, create, nil)
	if response.StatusCode != 202 || !bytes.Equal(data, retry) {
		t.Fatal("重试没有返回原回执")
	}
	create.Prompt = "不同载荷"
	response, _ = request("POST", path, space.ID, create, nil)
	if response.StatusCode != 409 {
		t.Fatal("载荷冲突未拒绝")
	}
	runPath := "/v1/learning/runs/" + receipt.RunID
	response, data = request("GET", runPath, space.ID, nil, nil)
	var snapshot mentorrun.Snapshot
	json.Unmarshal(data, &snapshot)
	if response.StatusCode != 200 || snapshot.Status != "queued" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("快照合同错误")
	}
	// 先快照，后发生工作，再订阅；中间事件必须全部可补读，观察不能执行模型。
	if n, err := runtime.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("共享核心未完成 %d %v", n, err)
	}
	eventsCtx, cancelEvents := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEvents()
	r, _ := http.NewRequestWithContext(eventsCtx, "GET", origin+runPath+fmt.Sprintf("/events?after=%d", snapshot.Watermark), nil)
	r.AddCookie(cookie)
	r.Header.Set(learningspace.Header, space.ID)
	stream, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if stream.StatusCode != 200 || stream.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("SSE 不可观察或启用了缓冲")
	}
	scanner := bufio.NewScanner(stream.Body)
	seq := snapshot.Watermark
	completed := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event mentorrun.Event
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.Seq != seq+1 || event.RunID != receipt.RunID || event.GoalID != goal || event.SpaceID != space.ID || strings.Contains(line, privateOutput) {
			t.Fatal("事件有缝隙、错误上下文或正文")
		}
		seq = event.Seq
		if event.Type == "completed" {
			completed = true
			break
		}
	}
	stream.Body.Close()
	if !completed {
		t.Fatal("未收到持久完成事件")
	}
	response, data = request("GET", runPath, space.ID, nil, nil)
	json.Unmarshal(data, &snapshot)
	if response.StatusCode != 200 || snapshot.Status != "succeeded" || snapshot.Output != privateOutput || snapshot.Watermark != seq || calls.Load() != 1 {
		t.Fatal("真实结果或观察纯读语义失败")
	}
	response, _ = request("GET", runPath, learningspace.DefaultID, nil, nil)
	if response.StatusCode != 404 {
		t.Fatal("跨区运行泄漏")
	}
	response, _ = request("GET", runPath+"/events?after=999999", space.ID, nil, nil)
	if response.StatusCode != 409 {
		t.Fatal("无效游标未要求重取快照")
	}
	response, data = request("GET", "/v1/learning/operations/"+receipt.OperationID, space.ID, nil, nil)
	var recovered mentorrun.Receipt
	json.Unmarshal(data, &recovered)
	if response.StatusCode != 200 || recovered != receipt {
		t.Fatal("原操作查询失效")
	}
	if strings.Contains(logs.String(), privateGoal) || strings.Contains(logs.String(), privateOutput) || strings.Contains(logs.String(), cookie.Value) {
		t.Fatal("日志含私人正文或 Cookie")
	}
}
