package config

import "testing"

func TestLearningSettingsOperatorConfiguration(t *testing.T) {
	values := baseEnv()
	values["LEARNING_SETTINGS_FILE"] = "/var/lib/edu-agent/learning-settings/settings.json"
	values["MODEL_ENDPOINT_ALLOWLIST"] = `["http://127.0.0.1:11434/v1"]`
	cfg, err := load(env(values))
	if err != nil || len(cfg.ModelEndpointAllowlist) != 1 || cfg.LearningSettingsFile != values["LEARNING_SETTINGS_FILE"] {
		t.Fatal("学习配置环境未加载")
	}
	for _, bad := range []string{"relative/settings.json", "/tmp/../settings.json"} {
		values["LEARNING_SETTINGS_FILE"] = bad
		if _, err := load(env(values)); err == nil {
			t.Fatal("接受非规范秘密路径")
		}
	}
	values["LEARNING_SETTINGS_FILE"] = ""
	values["MODEL_ENDPOINT_ALLOWLIST"] = `"http://127.0.0.1:11434"`
	if _, err := load(env(values)); err == nil {
		t.Fatal("允许列表类型错误未拒绝")
	}
	values["MODEL_ENDPOINT_ALLOWLIST"] = "[]"
	values["LEARNING_SETTINGS_FILE"] = "/tmp/shared/settings.json"
	values["ADMIN_UI_SETTINGS_FILE"] = "/tmp/shared/settings.json"
	if _, err := load(env(values)); err == nil {
		t.Fatal("允许覆盖管理设置文件")
	}
}
