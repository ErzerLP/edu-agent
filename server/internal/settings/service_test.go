package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testService(t *testing.T, endpoints ...string) (*Service, Options) {
	t.Helper()
	o := Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: endpoints}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, o
}
func key(value string) *string { return &value }
func saveModel(t *testing.T, s *Service, target Target, endpoint, secret string) View {
	t.Helper()
	c := Connection{Enabled: true, Provider: "openai_compatible", Endpoint: endpoint, Model: "test-model", AuthMode: "bearer"}
	if secret == "" {
		c.AuthMode = "none"
	}
	u := Update{ExpectedRevision: s.View().Revision, Target: target, Connection: &c}
	if secret != "" {
		u.NewKey = key(secret)
	}
	v, err := s.Update(u)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConfigurationLifecycleAndExplicitProbe(t *testing.T) {
	var calls atomic.Int32
	secret := "server-secret-sentinel-7839"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+secret || strings.Contains(string(body), secret) {
			t.Error("模型端点或凭据边界错误")
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil || payload["max_tokens"] != float64(64) || payload["stream"] != false {
			t.Error("探测未限制输出")
		}
		messages := payload["messages"].([]any)
		if len(messages) != 3 || messages[2].(map[string]any)["content"] != "Confirm the core profile." {
			t.Error("探测不是固定公开样本")
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"capability_probe\":true}"}}]}`)
	}))
	defer provider.Close()
	s, o := testService(t, provider.URL)
	if s.View().Teaching.Reason != "not_configured" || s.View().Policy.PrivateQueries {
		t.Fatal("初始配置不符")
	}
	v := saveModel(t, s, Teaching, provider.URL, secret)
	if !v.Teaching.Configured || v.Teaching.Status == "ready" || !v.TeachingRestartRequired {
		t.Fatal("保存被误报为已探测或已应用")
	}
	s.View()
	s.Capabilities()
	s.TeachingHealth(context.Background())
	if calls.Load() != 0 {
		t.Fatal("保存或只读查询触发了外部请求")
	}
	if _, err := s.Probe(context.Background(), Teaching, v.Revision, false); !errors.Is(err, ErrConsent) {
		t.Fatal("探测缺少显式确认")
	}
	v, err := s.Probe(context.Background(), Teaching, v.Revision, true)
	if err != nil || v.Teaching.Status != "ready" || v.Teaching.Probe.Requests != 1 || !v.Teaching.Probe.NativeSchema || v.Teaching.Probe.CheckedAt.IsZero() || v.Teaching.Probe.CostEstimate != "unknown" {
		t.Fatalf("模型确定性探测失败: %v %+v", err, v)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), secret) || strings.Contains(fmt.Sprintf("%+v %#v", s.state.Teaching, Update{NewKey: &secret}), secret) {
		t.Fatal("公开 DTO 或格式化泄漏秘密")
	}
	for _, path := range []string{o.Path, filepath.Dir(o.Path)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatal("秘密权限错误")
		}
	}
	reopened, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.View().TeachingRestartRequired || reopened.View().Teaching.Status != "ready" || !reopened.View().Teaching.HasKey {
		t.Fatal("重启未恢复配置和探测")
	}
	client, err := reopened.TeachingClient()
	if err != nil || client == nil {
		t.Fatal("保存配置未成为可用执行适配器")
	}
	if c := s.Capabilities(); c.Research.Available || c.WebMentor.Available || c.Answering.Available || !c.Schema.Available {
		t.Fatal("能力错误宣称研究可执行")
	}
	if _, err = s.Update(Update{ExpectedRevision: 0, Target: Teaching, ClearKey: true}); !errors.Is(err, ErrConflict) {
		t.Fatal("旧版本覆盖未拒绝")
	}
	v, err = s.Update(Update{ExpectedRevision: v.Revision, Target: Teaching, ClearKey: true})
	if err != nil || v.Teaching.HasKey || v.Teaching.Probe != nil || v.Teaching.Configured {
		t.Fatal("清除凭据未失效")
	}
	b, _ = os.ReadFile(o.Path)
	if strings.Contains(string(b), secret) {
		t.Fatal("清除后文件仍包含旧 Key")
	}
}

func TestEndpointCredentialBindingAndMentorReuse(t *testing.T) {
	s, _ := testService(t, "http://127.0.0.1:11434", "http://127.0.0.1:22434")
	saveModel(t, s, Teaching, "http://127.0.0.1:11434", "teaching-secret")
	saveModel(t, s, Mentor, "http://127.0.0.1:22434", "mentor-secret")
	yes := true
	v, err := s.Update(Update{ExpectedRevision: s.View().Revision, MentorUsesTeaching: &yes})
	if err != nil || v.EffectiveMentor.Endpoint != v.Teaching.Endpoint || v.Mentor.Endpoint == v.Teaching.Endpoint {
		t.Fatal("显式复用覆盖了独立导师配置")
	}
	c := v.Teaching.Connection
	c.Endpoint = "http://127.0.0.1:22434"
	v, err = s.Update(Update{ExpectedRevision: v.Revision, Target: Teaching, Connection: &c})
	if err != nil || v.Teaching.HasKey || !v.Mentor.HasKey {
		t.Fatal("换端点沿用旧凭据或修改导师 Key")
	}
	c.Endpoint = "http://127.0.0.1:9999"
	if _, err = s.Update(Update{ExpectedRevision: v.Revision, Target: Teaching, Connection: &c}); !errors.Is(err, ErrInvalid) {
		t.Fatal("未授权模型端点被保存")
	}
	search := Connection{Enabled: true, Provider: "brave", Endpoint: "http://127.0.0.1:11434", AuthMode: "bearer"}
	if _, err = s.Update(Update{ExpectedRevision: v.Revision, Target: Search, Connection: &search}); !errors.Is(err, ErrInvalid) {
		t.Fatal("模型许可变成搜索内网许可")
	}
	for _, endpoint := range []string{"https://user:secret@example.com", "https://example.com?key=secret", "https://example.com/#secret", "https://example.com/%2e%2e/private"} {
		if _, err := endpointURL(endpoint); err == nil {
			t.Errorf("接受危险端点: %s", endpoint)
		}
	}
}

func TestProbeFailureCategoriesAndNoAuthenticationMode(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body, reason string
	}{
		{"鉴权失败", 401, "provider-secret", "unauthorized"}, {"限流", 429, "provider-secret", "rate_limited"}, {"上游失败", 503, "provider-secret", "upstream_error"}, {"格式错误", 200, `{}`, "invalid_response"}, {"重定向", 302, "", "incompatible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "" {
					t.Error("无鉴权端点收到虚构凭据")
				}
				w.Header().Set("Location", "http://127.0.0.1:1/private")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			s, _ := testService(t, server.URL)
			v := saveModel(t, s, Teaching, server.URL, "")
			v, err := s.Probe(context.Background(), Teaching, v.Revision, true)
			if err != nil || v.Teaching.Status != "probe_failed" || v.Teaching.Reason != tc.reason || int(calls.Load()) != v.Teaching.Probe.Requests {
				t.Fatalf("分类或用量不符: %v %+v", err, v.Teaching)
			}
			b, _ := json.Marshal(v)
			if strings.Contains(string(b), "provider-secret") {
				t.Fatal("错误回显提供商正文")
			}
		})
	}
	s, _ := testService(t, "http://127.0.0.1:1")
	v := saveModel(t, s, Teaching, "http://127.0.0.1:1", "")
	v, err := s.Probe(context.Background(), Teaching, v.Revision, true)
	if err != nil || v.Teaching.Reason != "unavailable" {
		t.Fatal("端点不可达分类错误")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	v, err = s.Probe(ctx, Teaching, v.Revision, true)
	if err != nil || v.Teaching.Reason != "timeout" {
		t.Fatal("超时分类错误")
	}
}

func TestProbeDoesNotOverwriteConcurrentConfiguration(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"capability_probe\":true}"}}]}`)
	}))
	defer provider.Close()
	s, _ := testService(t, provider.URL)
	v := saveModel(t, s, Teaching, provider.URL, "")
	done := make(chan error, 1)
	go func() { _, err := s.Probe(context.Background(), Teaching, v.Revision, true); done <- err }()
	<-started
	if _, err := s.Probe(context.Background(), Teaching, v.Revision, true); !errors.Is(err, ErrBusy) {
		t.Fatal("并行探测未限制")
	}
	c := v.Teaching.Connection
	c.Enabled = false
	_, err := s.Update(Update{ExpectedRevision: v.Revision, Target: Teaching, Connection: &c})
	close(release)
	if err != nil || !errors.Is(<-done, ErrConflict) || s.View().Teaching.Probe != nil || s.View().Teaching.Reason != "not_enabled" {
		t.Fatal("过时探测覆盖新配置")
	}
}

func TestLimitsAndProtectedStorage(t *testing.T) {
	defaults := DefaultLimits()
	if defaults.Validate() != nil {
		t.Fatal("默认限额无效")
	}
	for i := 0; i < reflect.TypeOf(defaults).NumField(); i++ {
		for _, bad := range []int{0, -1, 1000000000} {
			l := defaults
			reflect.ValueOf(&l).Elem().Field(i).SetInt(int64(bad))
			if l.Validate() == nil {
				t.Errorf("限额字段 %d 接受 %d", i, bad)
			}
		}
	}
	l := defaults
	l.OutputTokens = l.ContextTokens
	if l.Validate() == nil {
		t.Fatal("输出等于上下文未拒绝")
	}
	s, o := testService(t)
	if err := os.WriteFile(o.Path, []byte(`{"key":"sentinel"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(o); !errors.Is(err, ErrStorage) || strings.Contains(err.Error(), "sentinel") {
		t.Fatal("不安全文件未失败关闭")
	}
	if _, err := s.Update(Update{ExpectedRevision: 0, Limits: &defaults}); !errors.Is(err, ErrStorage) {
		t.Fatal("覆盖了不安全文件")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Dir(o.Path), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Path: filepath.Join(link, "secret.json")}); !errors.Is(err, ErrStorage) {
		t.Fatal("接受秘密目录符号链接")
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "64:ff9b::7f00:1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("公共搜索网络接受 %s", raw)
		}
	}
	if !publicIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("公共 IP 被误拒绝")
	}
	if _, err := searchTransport().DialContext(context.Background(), "tcp", "localhost:80"); err == nil {
		t.Fatal("搜索网络接受本机连接")
	}
}

func TestEnvironmentKeysAreNotMigratedByOtherSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "settings.json")
	o := Options{Path: path, Teaching: Connection{Enabled: true, Provider: "openai_compatible", Endpoint: "http://127.0.0.1:11434", Model: "env-model", AuthMode: "bearer"}, TeachingKey: "environment-secret"}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	l := DefaultLimits()
	if _, err := s.Update(Update{ExpectedRevision: 0, Limits: &l}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), o.TeachingKey) {
		t.Fatal("保存预算迁移了环境 Key")
	}
	o.TeachingKey = "rotated-environment-secret"
	reopened, err := Open(o)
	if err != nil || reopened.state.Teaching.Key != o.TeachingKey {
		t.Fatal("重启未继续使用当前环境配置")
	}
}
