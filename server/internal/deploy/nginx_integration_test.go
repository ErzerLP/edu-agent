package deploy_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 复用 modelgateway/cli_nginx_integration_test.go 的只读容器、host 网络与原样 location。
// 后端回显方法和 RequestURI，证明每个样例都经过真实 nginx，且查询参数没有丢失。
func TestCLINginxProxy(t *testing.T) {
	if os.Getenv("EDU_AGENT_INTEGRATION_NGINX") != "1" {
		t.Skip("设置 EDU_AGENT_INTEGRATION_NGINX=1 启用 nginx:alpine 代理检查（需本机 Docker 和已有镜像）")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Docker host 网络检查需要 Linux")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-CLI-Nginx-Upstream", r.Method+" "+r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	config := readNginx(t)
	config = strings.ReplaceAll(config, "listen 443 ssl;", "listen "+address+";")
	config = regexp.MustCompile(`(?m)^\s*ssl_certificate(?:_key)? [^;]+;\n`).ReplaceAllString(config, "")
	config = strings.ReplaceAll(config, "http://127.0.0.1:8080", upstream.URL)
	config = "error_log stderr warn; pid /tmp/nginx.pid; events {} http {\n" + config + "\n}"
	path := filepath.Join(t.TempDir(), "nginx.conf")
	if err := os.WriteFile(path, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("cli-allowlist-nginx-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--pull", "never", "--detach", "--name", name, "--network", "host", "--read-only", "--tmpfs", "/var/cache/nginx", "--tmpfs", "/var/run", "--tmpfs", "/tmp", "--mount", "type=bind,src="+path+",dst=/etc/nginx/nginx.conf,readonly", "--entrypoint", "nginx", "nginx:alpine", "-g", "daemon off;")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("启动 nginx: %v %s", err, raw)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if t.Failed() {
			raw, err := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
			t.Logf("nginx 日志: %s (%v)", raw, err)
		}
		if raw, err := exec.CommandContext(ctx, "docker", "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("清理 nginx: %v %s", err, raw)
		}
	})
	endpoint := "http://" + address
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := client.Get(endpoint + "/")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusNotFound {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("nginx 未就绪: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, route := range cliRoutes {
		t.Run(route.source+"/"+route.method+route.path, func(t *testing.T) {
			path := samplePath(route.path)
			req, err := http.NewRequestWithContext(t.Context(), route.method, endpoint+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusNoContent || res.Header.Get("X-CLI-Nginx-Upstream") != route.method+" "+path {
				t.Fatalf("未收到后端回显: status=%d upstream=%q", res.StatusCode, res.Header.Get("X-CLI-Nginx-Upstream"))
			}
		})
	}
	for _, path := range rejectedPaths {
		t.Run("reject"+path, func(t *testing.T) {
			res, err := client.Get(endpoint + samplePath(path))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusNotFound || res.Header.Get("X-CLI-Nginx-Upstream") != "" {
				t.Fatalf("未知/管理路径被转发: status=%d upstream=%q", res.StatusCode, res.Header.Get("X-CLI-Nginx-Upstream"))
			}
		})
	}
	t.Logf("nginx:alpine 转发 %d 条 CLI 样例；拒绝 %d 条未知/管理路径", len(cliRoutes), len(rejectedPaths))
}
