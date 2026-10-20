//go:build linux

package modelgateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Reuse the shipped locations verbatim, changing only the test listen/upstream
// addresses. Docker is opt-in; the core integration suite needs no Docker.
func cliNginxSmoke(t *testing.T, f *cliService, upstream *cliUpstream) {
	t.Helper()
	if os.Getenv("EDU_AGENT_INTEGRATION_NGINX") != "1" {
		t.Skip("设置 EDU_AGENT_INTEGRATION_NGINX=1 启用 nginx:alpine smoke（需 Docker 和预先拉取镜像）")
	}
	production, err := os.ReadFile("../../../deploy/web/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	text := string(production)
	start := strings.Index(text, "    location = /v1/model-gateway/chat/completions {")
	end := strings.Index(text, "    # 公网只转发")
	if start < 0 || end < start {
		t.Fatal("未找到部署配置中的网关 location")
	}
	locations := strings.ReplaceAll(text[start:end], "http://127.0.0.1:8080", f.endpoint)
	address := cliFreeAddress(t)
	config := "error_log stderr warn; pid /tmp/nginx.pid; events {} http { server { listen " + address + ";\n" + locations + "location / { return 404; } } }"
	path := filepath.Join(t.TempDir(), "nginx.conf")
	if err = os.WriteFile(path, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	name := "ga122-nginx-" + uuid.NewString()
	cmd := exec.CommandContext(t.Context(), "docker", "run", "--detach", "--name", name, "--network", "host", "--read-only", "--tmpfs", "/var/cache/nginx", "--tmpfs", "/var/run", "--tmpfs", "/tmp", "--mount", "type=bind,src="+path+",dst=/etc/nginx/nginx.conf,readonly", "--entrypoint", "nginx", "nginx:alpine", "-g", "daemon off;")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("启动 nginx：%v %s", err, raw)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if t.Failed() {
			raw, _ := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
			t.Logf("nginx 日志：%s", raw)
		}
		if raw, err := exec.CommandContext(ctx, "docker", "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("清理 nginx：%v %s", err, raw)
		}
	})
	endpoint := "http://" + address
	probe := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := probe.Get(endpoint + "/v1/model-gateway/config")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 401 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("nginx 未就绪")
		}
		time.Sleep(50 * time.Millisecond)
	}
	c := f.pair(t)
	id := uuid.NewString()
	headers := c.headers(t, id)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, endpoint+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = headers.Clone()
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := do("GET", "/v1/model-gateway/config", "")
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("代理配置：%d", res.StatusCode)
	}
	before := upstream.calls.Load()
	startTime := time.Now()
	stream := do("POST", "/v1/model-gateway/chat/completions", `{"model":"mentor","messages":[{"role":"user","content":"GA122_BLOCK_nginx"}],"stream":true}`)
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)
	first, err := reader.ReadString('\n')
	elapsed := time.Since(startTime)
	if err != nil || stream.StatusCode != 200 || !strings.HasPrefix(first, "data: ") || elapsed > 2*time.Second {
		t.Fatalf("代理首帧：status=%d elapsed=%s err=%v data=%s", stream.StatusCode, elapsed, err, first)
	}
	for !strings.Contains(first, "GA122_流首帧已到达") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("首帧后没有模型内容：%v", err)
		}
		first += line
	}
	res = do("POST", "/v1/model-gateway/requests/"+id+"/cancel", "{}")
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || !strings.Contains(string(raw), `"cancelled"`) {
		t.Fatalf("代理 cancel：%d %s %v", res.StatusCode, raw, err)
	}
	cliWaitClosed(t, upstream)
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "request_cancelled") || strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("取消后的代理流：%s %v", rest, err)
	}
	res = do("GET", "/v1/model-gateway/requests/"+id, "")
	var state struct {
		State string `json:"state"`
	}
	err = json.NewDecoder(res.Body).Decode(&state)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || state.State != "cancelled" || upstream.calls.Load()-before != 1 {
		t.Fatalf("代理状态或外发次数：status=%d state=%s calls=%d err=%v", res.StatusCode, state.State, upstream.calls.Load()-before, err)
	}
	res = do("GET", "/v1/model-gateway/unknown", "")
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("代理开放了未知网关路径")
	}
	t.Logf("nginx:alpine 首帧 %s；配置、cancel、状态通过，取消后关闭上游且不重发", elapsed)
}
