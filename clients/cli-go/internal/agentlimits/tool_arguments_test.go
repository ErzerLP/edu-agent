package agentlimits

import "testing"

func TestLargeArgumentsToolPolicy(t *testing.T) {
	for _, name := range []string{"write", "edit", "read", "archive", "remember_preference", "WRITE", "unknown", ""} {
		want := 8192
		if name == "write" || name == "edit" {
			want = 65536
		}
		if got := ToolArgumentsBytes(name); got != want {
			t.Fatalf("%q budget=%d want=%d", name, got, want)
		}
	}
	if MaxToolCallArgumentsTotal != 131072 {
		t.Fatal("unexpected aggregate argument budget")
	}
}
