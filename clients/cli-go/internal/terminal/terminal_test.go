package terminal

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestPlatformPairLineInput(t *testing.T) {
	t.Parallel()
	input := bytes.NewBufferString("one-line\nsecond\n")
	value := New(input, &bytes.Buffer{}, &bytes.Buffer{})
	secret, err := value.ReadSecret("")
	if err != nil || secret != "one-line" {
		t.Fatalf("ReadSecret = %q, %v", secret, err)
	}
}

func TestColorStaysPlainByDefault(t *testing.T) {
	t.Parallel()
	if ColorEnabled("never", true, "xterm", false) || ColorEnabled("always", false, "xterm", false) || ColorEnabled("auto", true, "dumb", false) || ColorEnabled("auto", true, "xterm", true) {
		t.Fatal("color was enabled outside the explicit TTY matrix")
	}
	if !ColorEnabled("auto", true, "xterm", false) {
		t.Fatal("auto color was not enabled for an explicit capable TTY")
	}
}

func TestPlatformControlL(t *testing.T) {
	t.Parallel()
	if !IsControlL([]byte{ControlL}) || IsControlL([]byte("clear")) {
		t.Fatal("Ctrl-L detection drifted")
	}
}

func TestEscapeTextReplacesTerminalControlCharacters(t *testing.T) {
	t.Parallel()
	got := EscapeText("safe\x1b[2J\nnext\t\u0085")
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\n') || strings.ContainsRune(got, '\t') || strings.ContainsRune(got, '\u0085') {
		t.Fatalf("EscapeText retained a control character in %q", got)
	}
	if want := `safe\u001b[2J\nnext\t\u0085`; got != want {
		t.Fatalf("EscapeText = %q, want %q", got, want)
	}
}

func TestClearRejectsNonTTYWithoutOutput(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	value := &IO{out: &output, outputTTY: false}
	if err := value.Clear(); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Clear error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("non-TTY clear wrote %q", output.String())
	}
}
