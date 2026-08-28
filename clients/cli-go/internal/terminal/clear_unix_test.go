//go:build !windows

package terminal

import (
	"bytes"
	"testing"
)

func TestPlatformClear(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	value := &IO{out: &output, outputTTY: true}
	if err := value.Clear(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\x1b[2J\x1b[H> "; got != want {
		t.Fatalf("Clear wrote %q, want %q", got, want)
	}
}
