package api_test

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestSettingsContractMatchesDefaultsAndScopeSeparation(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := settings.Open(settings.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"LearningSettings": s.View(), "LearningCapabilities": s.Capabilities(), "SettingsLimits": settings.DefaultLimits()} {
		data, _ := json.Marshal(value)
		var decoded any
		json.Unmarshal(data, &decoded)
		if err := doc.Components.Schemas[name].Value.VisitJSON(decoded, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("%s 与实现不一致: %v", name, err)
		}
	}
	if doc.Paths.Find("/v1/settings").Put.Extensions["x-required-scope"] != "settings:write" || doc.Paths.Find("/v1/settings/probes").Post.Extensions["x-required-scope"] != "settings:probe" {
		t.Fatal("配置操作缺少独立 scope")
	}
	if !doc.Components.Schemas["LearningSettingsUpdate"].Value.Properties["new_key"].Value.WriteOnly {
		t.Fatal("秘密未声明只写")
	}
	if doc.Paths.Find("/v1/model/capabilities") == nil {
		t.Fatal("破坏旧客户端能力入口")
	}
}
