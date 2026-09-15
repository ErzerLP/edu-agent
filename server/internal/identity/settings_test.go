package identity

import "testing"

func TestSettingsScopesRequireExplicitPairingProfile(t *testing.T) {
	for _, profile := range []PairingProfile{PairingProfileUser, PairingProfileAgent, PairingProfileSettings} {
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
	}
}
