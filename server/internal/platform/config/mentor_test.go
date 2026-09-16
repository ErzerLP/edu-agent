package config

import (
	"testing"
	"time"
)

func TestMentorRuntimeConfiguration(t *testing.T) {
	cfg, err := load(env(baseEnv()))
	if err != nil || cfg.MentorKeyFile != "" || cfg.MentorHeartbeat != 10*time.Second || cfg.MentorWriteTimeout != 5*time.Second {
		t.Fatal("默认不保存正文，流配置必须有界")
	}
	for _, values := range []map[string]string{
		{"MENTOR_KEY_FILE": "relative/key"},
		{"MENTOR_KEY_FILE": "/private/../key"},
		{"MENTOR_KEY_FILE": "/private/key", "LEARNING_SETTINGS_FILE": "/private/key"},
		{"MENTOR_KEY_FILE": "/private/key", "ADMIN_UI_SETTINGS_FILE": "/private/key"},
		{"MENTOR_STREAM_HEARTBEAT": "0s"}, {"MENTOR_STREAM_HEARTBEAT": "61s"},
		{"MENTOR_STREAM_WRITE_TIMEOUT": "0s"}, {"MENTOR_STREAM_WRITE_TIMEOUT": "31s"},
	} {
		configuration := baseEnv()
		for key, value := range values {
			configuration[key] = value
		}
		if _, err := load(env(configuration)); err == nil {
			t.Fatalf("接受了不安全的导师配置 %v", values)
		}
	}
}
