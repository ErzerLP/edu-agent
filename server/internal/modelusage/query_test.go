package modelusage

import (
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
)

func TestUsageFiltersValidateAndBindCursors(t *testing.T) {
	q := Query{From: time.Unix(1, 0), To: time.Unix(2, 0), Timezone: "UTC", Grain: "day", Limit: 20, Source: "cli", ActorDeviceID: "b2fde2cc-5a17-4ab0-958a-c5a90405112c", CLISessionID: "89c952df-b509-4e56-a2f9-59881b829eae"}
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
	actor := identity.Credential{TokenID: "token"}
	bind := binding(actor, 1, q, "")
	q.Cursor = nextPosition(time.Unix(1, 0), "00c2f77c-1959-4cab-9db4-54b95d912c9a", bind)
	for _, change := range []func(*Query){func(q *Query) { q.Source = "web" }, func(q *Query) { q.ActorDeviceID = "646382f5-07a6-44d6-a38e-896216261ca0" }, func(q *Query) { q.CLISessionID = "2072e033-0b2a-4c82-bb73-1035a6b84ddc" }} {
		next := q
		change(&next)
		if _, err := readPosition(next, binding(actor, 1, next, "")); err == nil {
			t.Fatal("cursor crossed filter binding")
		}
	}
	for _, change := range []func(*Query){func(q *Query) { q.Source = "untrusted" }, func(q *Query) { q.ActorDeviceID = "" }, func(q *Query) { q.CLISessionID = "invalid" }} {
		next := q
		change(&next)
		if err := next.Validate(); err == nil {
			t.Fatal("invalid attribution filter accepted")
		}
	}
}
