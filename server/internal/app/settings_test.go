package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/settings"
)

func TestEnvironmentResponsesStyleReachesBothConstructors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Error("environment API style lost")
		}
		fmt.Fprint(w, `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{\"capability_probe\":true}"}]}]}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	cfg := config.Config{Model: config.ModelConfig{Enabled: true, BaseURL: u, APIStyle: "responses", Name: "test", APIKey: "test-key", ContextWindow: 8192, MinimumContext: 4096, Timeout: 5 * time.Second}}
	client, err := buildModelClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(t.Context()); !result.NativeJSONSchema {
		t.Fatalf("legacy constructor: %+v", result)
	}
	s, err := openLearningSettings(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err = s.TeachingClient()
	if err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(t.Context()); !result.NativeJSONSchema {
		t.Fatalf("settings constructor: %+v", result)
	}
}

func TestSavedTeachingSettingsAreConsumedAtStartup(t *testing.T) {
	cfg := config.Config{LearningSettingsFile: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpointAllowlist: []string{"http://127.0.0.1:11434/v1"}}
	s, err := openLearningSettings(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := s.TeachingClient()
	if err != nil || client != nil {
		t.Fatal("初始无模型被虚构为可运行")
	}
	connection := settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: cfg.ModelEndpointAllowlist[0], Model: "local-model", AuthMode: "none"}
	if _, err := s.Update(settings.Update{ExpectedRevision: 0, Target: settings.Teaching, Connection: &connection}); err != nil {
		t.Fatal(err)
	}
	client, _ = s.TeachingClient()
	if client != nil {
		t.Fatal("保存隐式切换了运行中教学")
	}
	restarted, err := openLearningSettings(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err = restarted.TeachingClient()
	if err != nil || client == nil || restarted.View().TeachingRestartRequired {
		t.Fatal("启动未消费保存的无鉴权本地模型配置")
	}
	if cfg.Model.Enabled || cfg.Model.APIKey != "" {
		t.Fatal("设置服务修改原始环境配置")
	}
}
