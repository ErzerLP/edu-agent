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
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/companion"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

func TestPostgreSQLCompanionHTTPNativeEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("需要原生 Linux/macOS")
	}
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := settings.Open(settings.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := mentorrun.New(pool, configuration, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := ids.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cookie, principal, err := ids.ExchangeWebPairing(ctx, code, "本地连接验收")
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := runs.NewConversation(ctx, principal.Credential, learningspace.DefaultID, mentorrun.NewConversation{ID: uuid.NewString(), Saved: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	opts := Options{CompanionEnabled: true, Identity: ids, MentorRuns: runs, Settings: configuration, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute), WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}}
	handler, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	request := func(value any, alter func(*http.Request)) (int, []byte) {
		t.Helper()
		raw, _ := json.Marshal(value)
		r, _ := http.NewRequest("POST", origin+"/v1/companion/browser", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", webCSRF(cookie))
		r.AddCookie(&http.Cookie{Name: "edu_web_dev", Value: cookie})
		if alter != nil {
			alter(r)
		}
		response, e := server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response.StatusCode, data
	}
	for _, alter := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, func(r *http.Request) { r.Host = "rebound.example" }, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }} {
		if status, _ := request(map[string]string{"action": "pair"}, alter); status != 403 {
			t.Fatalf("跨站或 CSRF 未拒绝：%d", status)
		}
	}
	status, raw := request(map[string]string{"action": "pair"}, nil)
	if status != 200 {
		t.Fatalf("无法配对：%d %s", status, raw)
	}
	var pair companion.Pair
	json.Unmarshal(raw, &pair)
	body := func(action string) map[string]any {
		return map[string]any{"action": action, "id": pair.ID, "secret": pair.Secret}
	}
	unauthorized := body("submit")
	unauthorized["operation"] = companion.Operation{ID: "unauthorized", Tool: "shell", Arguments: json.RawMessage(`{"command":"true"}`)}
	if status, _ = request(unauthorized, nil); status != 403 {
		t.Fatal("未授权获得 Shell")
	}
	binary := filepath.Join(t.TempDir(), "edu-companion")
	build := exec.Command("go", "build", "-o", binary, "./cmd/edu-companion")
	build.Dir = "../../../../clients/cli-go"
	if output, e := build.CombinedOutput(); e != nil {
		t.Fatalf("构建原生 companion：%v %s", e, output)
	}
	workspace := t.TempDir()
	process := exec.Command(binary, "--server", origin, "--workspace", workspace, "--allow-loopback-http")
	process.Stdin = strings.NewReader(pair.ID + "." + pair.Code + "\n连接\n")
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		_ = process.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			_ = process.Process.Kill()
			<-done
		}
	})
	var view companion.View
	deadline := time.Now().Add(8 * time.Second)
	for {
		status, raw = request(body("status"), nil)
		if status != 200 {
			t.Fatalf("状态读取失败 %s", raw)
		}
		json.Unmarshal(raw, &view)
		if view.Device.ID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("原生程序未连接")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if view.Device.Workspace != workspace || view.Device.OS != runtime.GOOS || view.Device.User == "" {
		t.Fatalf("实际执行身份不一致：%+v", view.Device)
	}
	grant := body("grant")
	grant["device"] = view.Device.ID
	grant["grant"] = companion.Grant{Generation: principal.Generation, Space: learningspace.DefaultID, Conversation: conversation, Files: true, Shell: true}
	if status, raw = request(grant, nil); status != 200 {
		t.Fatalf("授权未成功：%d %s", status, raw)
	}
	submit := body("submit")
	submit["operation"] = companion.Operation{ID: "real-shell", Run: "forged-run", Tool: "shell", Arguments: json.RawMessage(`{"command":"printf '真实原生结果' > from-web.txt","wait_ms":1000}`)}
	if status, raw = request(submit, nil); status != 200 {
		t.Fatalf("实际命令未受理：%d %s", status, raw)
	}
	query := body("receipt")
	query["operation_id"] = "real-shell"
	var receipt companion.Receipt
	deadline = time.Now().Add(6 * time.Second)
	for {
		status, raw = request(query, nil)
		json.Unmarshal(raw, &receipt)
		if status == 200 && receipt.State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没有原始回执：%d %s", status, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if receipt.Error != "" || receipt.Task == "" || receipt.Device != view.Device.ID || receipt.Conversation != conversation || receipt.Run != "manual:real-shell" || receipt.TaskRun != receipt.Run {
		t.Fatalf("操作归属或真实结果错误：%+v", receipt)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "from-web.txt"))
	if err != nil || string(data) != "真实原生结果" {
		t.Fatal("文件并非来自真实本地程序", err)
	}
	if err = ids.RevokeDevice(ctx, principal.Device.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		done <- nil
	case <-time.After(6 * time.Second):
		t.Fatal("浏览器设备撤销没有使原生通道退出")
	}
	if status, _ = request(query, nil); status != 401 {
		t.Fatal("撤销后还能读私人回执")
	}
	// 关闭 feature gate 时保留能力查询，通道路径直接不存在。
	opts.CompanionEnabled = false
	disabled, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", origin+"/v1/companion/attach", strings.NewReader(`{}`))
	r.Header.Set("Origin", origin)
	w := httptest.NewRecorder()
	disabled.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("关闭开关仍暴露通道：%d", w.Code)
	}
}
