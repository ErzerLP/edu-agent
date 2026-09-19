package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestWebOfflineGateRequiresCompleteProfile(t *testing.T) {
	values := baseEnv()
	cfg, err := load(env(values))
	if err != nil || cfg.WebOfflineEnabled {
		t.Fatal("浏览器离线必须默认关闭")
	}
	values["WEB_OFFLINE_ENABLED"] = "true"
	if _, err := load(env(values)); err == nil {
		t.Fatal("缺少配套能力时不应启用")
	}
	values["WEB_UI_ENABLED"] = "true"
	values["OFFLINE_SIGNER_KEY_ID"] = "浏览器验收"
	values["OFFLINE_SIGNER_PRIVATE_KEY"] = base64.RawURLEncoding.EncodeToString(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{17}, 32)))
	values["OFFLINE_SIGNER_ISSUED_AT"] = "2026-01-01T00:00:00Z"
	values["OFFLINE_SIGNER_NOT_AFTER"] = "2030-01-01T00:00:00Z"
	if _, err := load(env(values)); err == nil {
		t.Fatal("缺少可恢复清除 challenge 密钥仍启用")
	}
	values["PRIVACY_OFFLINE_CHALLENGE_KEYS"] = "2:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{18}, 32))
	cfg, err = load(env(values))
	if err != nil || !cfg.WebOfflineEnabled {
		t.Fatalf("完整配置未启用：%v", err)
	}
}
