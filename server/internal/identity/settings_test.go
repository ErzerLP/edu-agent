package identity

import "testing"

func TestSettingsScopesRequireExplicitPairingProfile(t *testing.T) {
	for _, profile := range []PairingProfile{PairingProfileUser, PairingProfileAgent, PairingProfileSettings, PairingProfileResearch, PairingProfileImport} {
		scopes, err := pairingProfileScopes(profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []string{"settings:write", "settings:probe"} {
			want := profile == PairingProfileSettings
			if hasScope(scopes, scope) != want || hasScope(WebScopes(scopes), scope) != want {
				t.Fatalf("%s 的 %s 权限不符", profile, scope)
			}
		}
		if hasScope(WebScopes(scopes), "knowledge:read") != hasScope(scopes, "knowledge:read") {
			t.Fatalf("%s 未保留教学所需的已有资料读取权限", profile)
		}
		for _, scope := range []string{"knowledge:write", "research:adopt"} {
			want := profile == PairingProfileResearch || profile == PairingProfileImport && scope == "knowledge:write"
			if hasScope(WebScopes(scopes), scope) != want {
				t.Fatalf("%s 意外获得浏览器来源采纳权限 %s", profile, scope)
			}
		}
		if hasScope(WebScopes(scopes), "knowledge:approve") != (profile == PairingProfileImport) {
			t.Fatalf("%s 的浏览器资料审批未遵守显式档案", profile)
		}
		for _, forbidden := range []string{"devices:manage", "learning:approve", "memory:write", "privacy:device"} {
			if hasScope(WebScopes(scopes), forbidden) {
				t.Fatalf("浏览器获得无关权限：%s", forbidden)
			}
		}
	}
}
