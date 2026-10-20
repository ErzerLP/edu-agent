//go:build linux

package modelgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/modelgateway"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/testutil/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"
)

// These tests run both shipped executables. Only the provider is an HTTP fake.
// Each run owns a schema and temporary local CLI/settings state.
type cliService struct {
	pool                  *pgxpool.Pool
	endpoint, server, cli string
	env                   []string
	browser               *http.Cookie
	csrf                  string
	logs                  lockedBuffer
}

func newCLIService(t *testing.T, upstream string) *cliService {
	t.Helper()
	server, cli := os.Getenv("EDU_AGENT_INTEGRATION_SERVER"), os.Getenv("EDU_AGENT_INTEGRATION_CLI")
	if server == "" || cli == "" || os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("需要 TEST_DATABASE_URL、EDU_AGENT_INTEGRATION_SERVER 和 EDU_AGENT_INTEGRATION_CLI（两个二进制须为绝对路径）")
	}
	for _, path := range []string{server, cli} {
		if !filepath.IsAbs(path) {
			t.Fatal("集成测试二进制须为绝对路径")
		}
	}
	f := &cliService{pool: dbtest.Pool(t), server: server, cli: cli}
	dsn, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("search_path", f.pool.Config().ConnConfig.RuntimeParams["search_path"])
	dsn.RawQuery = query.Encode()
	addr := cliFreeAddress(t)
	f.endpoint = "http://" + addr
	allowlist, _ := json.Marshal([]string{upstream})
	f.env = []string{
		"DATABASE_URL=" + dsn.String(), "LISTEN_ADDR=" + addr, "PUBLIC_BASE_URL=" + f.endpoint,
		"MIGRATE_ON_START=false", "MODEL_REQUIRED=false", "NOCTURNE_ENABLED=false", "ADMIN_UI_ENABLED=false",
		"WEB_UI_ENABLED=true", "WEB_UI_ALLOW_LOOPBACK_HTTP=true", "SHUTDOWN_TIMEOUT=2s",
		"LEARNING_SETTINGS_FILE=" + filepath.Join(t.TempDir(), "private", "settings.json"),
		"MODEL_ENDPOINT_ALLOWLIST=" + string(allowlist),
		"PAIRING_RATE_LIMIT_PER_MINUTE=1000", "AUTH_FAILURE_LIMIT_PER_MINUTE=1000", "DEVICE_RATE_LIMIT_PER_MINUTE=10000",
	}
	ctx, stop := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, server, "serve")
	cmd.Env, cmd.Stdout, cmd.Stderr = f.env, &f.logs, &f.logs
	if err = cmd.Start(); err != nil {
		stop()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			stop()
			<-done
		}
		stop()
		if t.Failed() {
			t.Logf("服务端日志：%s", f.logs.text())
		}
	})
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := client.Get(f.endpoint + "/livez")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
			if res.StatusCode == 404 {
				t.Fatal("真实服务未注册健康检查路径")
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("真实服务未就绪：%s", f.logs.text())
		}
		time.Sleep(100 * time.Millisecond)
	}
	response, raw := f.http(t, "POST", "/v1/web/pairings", "", map[string]any{"code": f.code(t, "settings"), "display_name": "G-A 联调网页设置"}, nil)
	if response.StatusCode != 201 || len(response.Cookies()) != 1 {
		t.Fatalf("网页配对：%d %s", response.StatusCode, raw)
	}
	f.browser = response.Cookies()[0]
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	cliDecode(t, raw, &session)
	f.csrf = session.CSRF
	return f
}

func cliFreeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func (f *cliService) code(t *testing.T, profile string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), f.server, "pairing-code", "create", "--profile", profile)
	cmd.Env = f.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("签发 %s 配对码：%v %s", profile, err, stderr.String())
	}
	return strings.TrimSpace(string(raw))
}

func (f *cliService) http(t *testing.T, method, path, token string, body any, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, f.endpoint+path, input)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header[key] = value
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("Origin", f.endpoint)
		if f.browser != nil {
			req.AddCookie(f.browser)
			req.Header.Set("X-CSRF-Token", f.csrf)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, raw
}

func (f *cliService) settings(t *testing.T, target, style, upstream, model string, enabled bool) {
	t.Helper()
	res, raw := f.http(t, "GET", "/v1/settings", "", nil, nil)
	var view settings.View
	cliDecode(t, raw, &view)
	if res.StatusCode != 200 || !view.Writable {
		t.Fatalf("网页设置不可写：%d %s", res.StatusCode, raw)
	}
	res, raw = f.http(t, "PUT", "/v1/settings", "", map[string]any{
		"expected_revision": view.Revision, "target": target, "new_key": "ga122-upstream-secret",
		"connection": settings.Connection{Enabled: enabled, Provider: "openai_compatible", Endpoint: upstream, Model: model, AuthMode: "bearer", APIStyle: style},
	}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("网页配置 %s/%s：%d %s", target, style, res.StatusCode, raw)
	}
}

type realCLI struct {
	service *cliService
	root    string
	device  string
	token   string
}

func (f *cliService) pair(t *testing.T) *realCLI {
	t.Helper()
	return f.pairAt(t, f.endpoint)
}

func (f *cliService) pairAt(t *testing.T, endpoint string) *realCLI {
	t.Helper()
	c := &realCLI{service: f, root: t.TempDir()}
	cmd := exec.CommandContext(t.Context(), f.cli, "pair", "--server", endpoint, "--name", "G-A 真实 CLI")
	cmd.Env = c.env()
	cmd.Stdin = strings.NewReader(f.code(t, "cli") + "\n")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("真实 CLI 配对：%v %s", err, raw)
	}
	stored, err := os.ReadFile(filepath.Join(c.root, "edu-agent", "credential.json"))
	if err != nil {
		t.Fatal(err)
	}
	var credential struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	cliDecode(t, stored, &credential)
	c.device, c.token = credential.DeviceID, credential.Token
	res, raw := f.http(t, "GET", "/v1/model-gateway/config", c.token, nil, nil)
	var cfg modelgateway.Config
	cliDecode(t, raw, &cfg)
	if res.StatusCode != 200 || !cfg.InvokeAllowed || len(cfg.Models) != 2 {
		t.Fatalf("cli profile 或配置投影：%d %s", res.StatusCode, raw)
	}
	for _, secret := range []string{"endpoint", "api_key", "ga122-upstream-secret"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("配置投影泄露 %s", secret)
		}
	}
	return c
}

func (c *realCLI) env() []string {
	return []string{"XDG_CONFIG_HOME=" + c.root, "TERM=xterm-256color", "LANG=C.UTF-8", "NO_COLOR=1"}
}

func (c *realCLI) command(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), c.service.cli, args...)
	cmd.Env = c.env()
	raw, err := cmd.CombinedOutput()
	return string(raw), err
}

// A Linux PTY avoids introducing a server dependency just for terminal tests.
type cliTerminal struct {
	master *os.File
	chunks chan string
	done   chan error
	output string
}

func (c *realCLI) terminal(t *testing.T, args ...string) *cliTerminal {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	if err = unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 55, Col: 180}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, c.service.cli, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.env(), slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	p := &cliTerminal{master: master, chunks: make(chan string, 256), done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait(); close(p.done) }()
	go func() {
		defer close(p.chunks)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				select {
				case p.chunks <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-p.done })
	return p
}

func (p *cliTerminal) write(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.master, s); err != nil {
		t.Fatal(err)
	}
}

func (p *cliTerminal) wait(t *testing.T, text string) {
	t.Helper()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	for !strings.Contains(p.output, text) {
		select {
		case chunk, ok := <-p.chunks:
			p.output += chunk
			if !ok {
				t.Fatalf("CLI 提前关闭，等待 %q：%s", text, p.output)
			}
		case <-deadline.C:
			t.Fatalf("CLI 未显示 %q：%s", text, p.output)
		}
	}
}

func (p *cliTerminal) exit(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("CLI 退出：%v %s", err, p.output)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("CLI 未退出：%s", p.output)
	}
}

func (c *realCLI) selectModel(t *testing.T, target string, first bool) {
	t.Helper()
	p := c.terminal(t, "model", "select", target)
	if first {
		p.wait(t, "是否同意经此服务端使用所选模型")
		if !strings.Contains(p.output, "工具返回的本地内容") {
			t.Fatal("首次告知遗漏本机内容外发")
		}
		p.write(t, "y\n")
	}
	p.wait(t, "已选择")
	p.exit(t)
	output, err := c.command(t, "model", "show")
	if err != nil || !strings.Contains(output, "（"+target+"）") {
		t.Fatalf("新进程未记住连接：%v %s", err, output)
	}
}

func (c *realCLI) config(t *testing.T) modelgateway.Config {
	t.Helper()
	res, raw := c.service.http(t, "GET", "/v1/model-gateway/config", c.token, nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("配置：%d %s", res.StatusCode, raw)
	}
	var cfg modelgateway.Config
	cliDecode(t, raw, &cfg)
	return cfg
}

func cliDecode(t *testing.T, raw []byte, value any) {
	t.Helper()
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatalf("JSON 解码：%v %s", err, raw)
	}
}
