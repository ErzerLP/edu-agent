package identity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func assertExactScopes(t *testing.T, got []string, want string) {
	t.Helper()
	actual, expected := slices.Clone(got), strings.Fields(want)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("scopes = %v, want %v", actual, expected)
	}
}

func TestLearnerPairingExactScopesAndTokenAuthorization(t *testing.T) {
	profile, err := ParsePairingProfile("learner")
	if err != nil {
		t.Fatal(err)
	}
	want := "learning:read learning:write learning:approve knowledge:read knowledge:write knowledge:approve imports:web references:manage research:adopt assessment:web memory:read memory:write memory:web privacy:read devices:read devices:manage"
	scopes, err := pairingProfileScopes(profile)
	if err != nil {
		t.Fatal(err)
	}
	assertExactScopes(t, scopes, want)
	assertExactScopes(t, WebScopes(scopes), want)
	now := time.Now().UTC()
	store := newMemoryStore()
	service := testService(t, store, &now)
	code, _, err := service.CreatePairingCodeForProfile(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	assertExactScopes(t, store.codes[splitCode(t, code)[0]].Scopes, want)
	issued, err := service.ExchangePairingCode(context.Background(), code, "学习端全权")
	if err != nil {
		t.Fatal(err)
	}
	assertExactScopes(t, issued.Device.Scopes, want)
	for _, scope := range strings.Fields(want) {
		if _, err := service.Authenticate(context.Background(), issued.Token, scope); err != nil {
			t.Fatalf("learner missing %s: %v", scope, err)
		}
	}
	for _, scope := range []string{"settings:write", "settings:probe", "model:probe", "admin", "privacy:erase", "privacy:device", "*"} {
		if _, err := service.Authenticate(context.Background(), issued.Token, scope); !errors.Is(err, ErrForbidden) {
			t.Fatalf("learner must reject %s: %v", scope, err)
		}
	}
}

// Exact snapshots catch accidental expansion as well as loss of existing privileges.
func TestLegacyPairingProfilesKeepExactScopes(t *testing.T) {
	for _, test := range []struct{ profile, token, web string }{
		{"user", "devices:read devices:manage model:probe knowledge:read knowledge:write knowledge:approve learning:read learning:write learning:approve memory:read memory:write privacy:read privacy:device", "devices:read knowledge:read learning:read learning:write memory:read privacy:read"},
		{"agent", "knowledge:read knowledge:write learning:read learning:write memory:read", "knowledge:read learning:read learning:write memory:read"},
		{"settings", "learning:read learning:write settings:write settings:probe", "learning:read learning:write settings:write settings:probe"},
		{"research", "learning:read learning:write knowledge:read knowledge:write research:adopt", "learning:read learning:write knowledge:read knowledge:write research:adopt"},
		{"references", "learning:read learning:write knowledge:read knowledge:write knowledge:approve references:manage research:adopt", "learning:read learning:write knowledge:read knowledge:write knowledge:approve references:manage research:adopt"},
		{"import", "learning:read learning:write knowledge:read knowledge:write knowledge:approve imports:web", "learning:read learning:write knowledge:read knowledge:write knowledge:approve imports:web"},
		{"assessment", "learning:read learning:write learning:approve knowledge:read assessment:web", "learning:read learning:write learning:approve knowledge:read assessment:web"},
		{"memory", "learning:read learning:write knowledge:read memory:web memory:read memory:write privacy:read devices:read devices:manage", "learning:read learning:write knowledge:read memory:web memory:read memory:write privacy:read devices:read devices:manage"},
	} {
		t.Run(test.profile, func(t *testing.T) {
			profile, err := ParsePairingProfile(test.profile)
			if err != nil {
				t.Fatal(err)
			}
			scopes, err := pairingProfileScopes(profile)
			if err != nil {
				t.Fatal(err)
			}
			assertExactScopes(t, scopes, test.token)
			assertExactScopes(t, WebScopes(scopes), test.web)
		})
	}
}
