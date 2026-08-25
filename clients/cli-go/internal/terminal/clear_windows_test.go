//go:build windows

package terminal

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	nativeClearHelperEnvironment = "EDU_AGENT_NATIVE_CLEAR_HELPER"
	nativeClearVTMode            = "vt"
	nativeClearFallbackMode      = "fallback"
	nativeClearVTMethod          = "windows-conpty+production-clearscreen+vt-clear+success-marker"
	nativeClearFallbackMethod    = "windows-conpty+production-clearscreen+forced-vt-unavailable+fillconsole-cursor-fallback+success-marker"
	nativeClearPrompt            = "> "
)

func TestPlatformClear(t *testing.T) {
	if mode := os.Getenv(nativeClearHelperEnvironment); mode != "" {
		runNativeClearHelper(mode)
		return
	}

	for _, testCase := range []struct {
		name            string
		mode            string
		method          string
		requireVTOutput bool
	}{
		{name: "virtual-terminal", mode: nativeClearVTMode, method: nativeClearVTMethod, requireVTOutput: true},
		{name: "console-api-fallback", mode: nativeClearFallbackMode, method: nativeClearFallbackMethod},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testNativeClearBranch(t, testCase.mode, testCase.method, testCase.requireVTOutput)
		})
	}
}

func testNativeClearBranch(t *testing.T, mode, method string, requireVTOutput bool) {
	t.Helper()
	process, err := startWindowsConPTYTestHelper(nativeClearHelperEnvironment, mode, "TestPlatformClear")
	if err != nil {
		t.Fatalf("start native clear helper using %s: %v", method, err)
	}
	defer process.close()

	var output bytes.Buffer
	if err := process.readUntil(&output, []byte(">"), 10*time.Second); err != nil {
		t.Fatalf("native clear helper omitted neutral prompt using %s: %v", method, err)
	}
	if requireVTOutput {
		clearIndex := bytes.Index(output.Bytes(), []byte("\x1b[2J"))
		homeIndex := bytes.Index(output.Bytes(), []byte("\x1b[H"))
		if clearIndex < 0 || homeIndex < clearIndex {
			t.Fatalf("native clear helper omitted ordered VT viewport control output using %s", method)
		}
	}
	if got := string(bytes.TrimSpace(windowsVisibleConPTYText(output.Bytes()))); got != strings.TrimSpace(nativeClearPrompt) {
		t.Fatalf("native clear helper did not leave the neutral prompt using %s", method)
	}

	if _, err := process.Write([]byte{'\r'}); err != nil {
		t.Fatalf("acknowledge native clear output using %s: %v", method, err)
	}
	marker := nativeClearSuccessMarker(mode)
	if err := process.readUntil(&output, []byte(marker), 10*time.Second); err != nil {
		t.Fatalf("native clear helper omitted success marker using %s: %v", method, err)
	}
	if err := process.waitAndDrain(&output, 10*time.Second); err != nil {
		t.Fatalf("native clear helper failed using %s: %v", method, err)
	}
	visible := strings.Join(strings.Fields(string(windowsVisibleConPTYText(output.Bytes()))), " ")
	if visible != ">"+marker && visible != "> "+marker {
		t.Fatalf("native clear helper emitted unexpected visible output %q using %s", visible, method)
	}
	t.Logf("native_clear_method=%s", method)
	t.Log("native_clear_result=pass")
}

func nativeClearSuccessMarker(mode string) string {
	return "native-clear-" + mode + "-ok"
}

func runNativeClearHelper(mode string) {
	restoreVTEnabler := func() {}
	switch mode {
	case nativeClearVTMode:
	case nativeClearFallbackMode:
		original := setConsoleModeForClear
		setConsoleModeForClear = func(windows.Handle, uint32) error {
			return windows.ERROR_INVALID_FUNCTION
		}
		restoreVTEnabler = func() {
			setConsoleModeForClear = original
		}
	default:
		_, _ = fmt.Fprintln(os.Stderr, "native clear mode invalid")
		os.Exit(1)
	}

	if err := clearScreen(os.Stdout); err != nil {
		restoreVTEnabler()
		_, _ = fmt.Fprintln(os.Stderr, "native clear failed")
		os.Exit(1)
	}
	var acknowledgement [1]byte
	if _, err := os.Stdin.Read(acknowledgement[:]); err != nil {
		restoreVTEnabler()
		_, _ = fmt.Fprintln(os.Stderr, "native clear acknowledgement failed")
		os.Exit(1)
	}
	restoreVTEnabler()
	_, _ = fmt.Fprintln(os.Stdout, nativeClearSuccessMarker(mode))
	os.Exit(0)
}
