package identity

import "testing"

func TestCLIProfileInvokesOnlyWithDeviceCredential(t *testing.T) {
	p, err := ParsePairingProfile("cli")
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := pairingProfileScopes(p)
	if err != nil {
		t.Fatal(err)
	}
	if !hasScope(scopes, "model:invoke") || hasScope(WebScopes(scopes), "model:invoke") {
		t.Fatal("CLI invocation must require a device token")
	}
	for _, scope := range userPairingScopes {
		if !hasScope(scopes, scope) {
			t.Fatalf("missing user scope %s", scope)
		}
	}
	for _, profile := range []PairingProfile{PairingProfileUser, PairingProfileAgent, PairingProfileLearner, PairingProfileSettings} {
		other, err := pairingProfileScopes(profile)
		if err != nil || hasScope(other, "model:invoke") {
			t.Fatalf("profile %s gained invocation", profile)
		}
	}
}
