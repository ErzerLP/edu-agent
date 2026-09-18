package identity

import "testing"

func TestMemoryWebRequiresExplicitProfile(t *testing.T) {
	profile, err := ParsePairingProfile("memory")
	if err != nil {
		t.Fatalf("缺少明确授权记忆与数据管理的配对入口：%v", err)
	}
	scopes, err := pairingProfileScopes(profile)
	if err != nil {
		t.Fatal(err)
	}
	web := WebScopes(scopes)
	for _, required := range []string{"memory:web", "memory:read", "memory:write", "privacy:read", "devices:read", "devices:manage"} {
		if !hasScope(web, required) {
			t.Fatalf("Web 丢失已明确授予的权限：%s", required)
		}
	}
	for _, forbidden := range []string{"privacy:erase", "privacy:device", "knowledge:write", "knowledge:approve", "learning:approve", "settings:write"} {
		if hasScope(web, forbidden) {
			t.Fatalf("记忆档案扩大了无关权限：%s", forbidden)
		}
	}
	for _, old := range []PairingProfile{PairingProfileUser, PairingProfileAgent, PairingProfileSettings, PairingProfileResearch, PairingProfileReferences, PairingProfileImport, PairingProfileAssessment} {
		oldScopes, err := pairingProfileScopes(old)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"memory:write", "devices:manage", "privacy:erase"} {
			if hasScope(WebScopes(oldScopes), forbidden) {
				t.Fatalf("旧档案 %s 意外增权 %s", old, forbidden)
			}
		}
	}
	withoutMarker := []string{"learning:read", "memory:read", "memory:write", "devices:manage"}
	if hasScope(WebScopes(withoutMarker), "memory:write") || hasScope(WebScopes(withoutMarker), "devices:manage") {
		t.Fatal("移除明确授权标记后仍保留写权限")
	}
}
