package identity

import "testing"

func TestReferencesNeedExplicitBrowserApproval(t *testing.T) {
	for _, profile := range []PairingProfile{PairingProfileUser, PairingProfileAgent, PairingProfileResearch, PairingProfileSettings, PairingProfileReferences} {
		scopes, err := pairingProfileScopes(profile)
		if err != nil {
			t.Fatal(err)
		}
		web := WebScopes(scopes)
		for _, permission := range []string{"references:manage", "knowledge:approve"} {
			if hasScope(web, permission) != (profile == PairingProfileReferences) {
				t.Fatalf("%s 意外继承或丢失 %s", profile, permission)
			}
		}
		if profile == PairingProfileReferences {
			for _, forbidden := range []string{"devices:manage", "settings:write", "learning:approve", "memory:write", "privacy:device"} {
				if hasScope(web, forbidden) {
					t.Fatalf("参考档案扩大了 %s", forbidden)
				}
			}
		}
	}
}
