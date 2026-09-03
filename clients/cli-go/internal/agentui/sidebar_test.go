package agentui

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestSafeWorkspaceSidebarLabelUsesSafeBasename(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "normal", input: "project", want: "project"},
		{name: "unix", input: "/home/alice/private/project", want: "project"},
		{name: "windows", input: `C:\Users\alice\private\project`, want: "project"},
		{name: "unc", input: `\\server\share\alice\project`, want: "project"},
		{name: "repeated separators", input: `\\server\share\\alice\\project\\`, want: "project"},
		{name: "drive only", input: `C:\`, want: workspaceSidebarFallbackLabel},
		{name: "dot", input: "/home/alice/.", want: workspaceSidebarFallbackLabel},
		{name: "dotdot", input: "/home/alice/..", want: workspaceSidebarFallbackLabel},
		{name: "device namespace", input: `\\.\NUL`, want: workspaceSidebarFallbackLabel},
		{name: "extended device namespace", input: `\\?\C:\Users\alice\project`, want: workspaceSidebarFallbackLabel},
		{name: "device name", input: "/tmp/NUL.txt", want: workspaceSidebarFallbackLabel},
		{name: "empty", input: " \t\n", want: workspaceSidebarFallbackLabel},
		{name: "terminal control", input: "/home/alice/private/\x1b[31mproject\n", want: "�[31mproject"},
		{name: "bidi control", input: "/home/alice/private/\u202eproject", want: "�project"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := safeWorkspaceSidebarLabel(test.input)
			if got != test.want {
				t.Fatalf("safeWorkspaceSidebarLabel(%q)=%q, want %q", test.input, got, test.want)
			}
			assertSafeWorkspaceSidebarText(t, got)
		})
	}
}

func TestSafeWorkspaceSidebarLabelBoundsLongUnicode(t *testing.T) {
	t.Parallel()
	input := "/home/alice/private/" + strings.Repeat("学", 128)
	got := safeWorkspaceSidebarLabel(input)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("long workspace label=%q, want display truncation", got)
	}
	assertSafeWorkspaceSidebarText(t, got)
	if len(got) > workspaceSidebarLabelMaxBytes {
		t.Fatalf("label bytes=%d, limit=%d: %q", len(got), workspaceSidebarLabelMaxBytes, got)
	}
	if utf8.RuneCountInString(got) > workspaceSidebarLabelMaxRunes {
		t.Fatalf("label runes=%d, limit=%d: %q", utf8.RuneCountInString(got), workspaceSidebarLabelMaxRunes, got)
	}
	if width := lipgloss.Width(got); width > workspaceSidebarLabelMaxWidth {
		t.Fatalf("label display width=%d, limit=%d: %q", width, workspaceSidebarLabelMaxWidth, got)
	}
}

func TestWorkspaceSidebarSummaryKeepsMinimumLayoutBounded(t *testing.T) {
	t.Parallel()
	value := model{
		session: &fakeConversation{},
		status:  "就绪",
		workspaceStatus: agentloop.WorkspaceStatus{
			Available: true,
			Label:     "/home/alice/private/" + strings.Repeat("界", 128),
		},
	}
	if got := value.workspaceSidebarSummary(); strings.Contains(got, "/home/alice") || strings.Contains(got, "alice") {
		t.Fatalf("workspace summary leaked parent path: %q", got)
	}

	view := value.renderSidebar(sidebarMinWidth, minimumHeight)
	for _, line := range strings.Split(view, "\n") {
		if width := lipgloss.Width(line); width > sidebarMinWidth {
			t.Fatalf("sidebar line width=%d, limit=%d: %q", width, sidebarMinWidth, line)
		}
	}
	if strings.Contains(view, "/home/alice") || strings.Contains(view, "alice") {
		t.Fatalf("sidebar leaked parent path: %s", view)
	}
}

func assertSafeWorkspaceSidebarText(t *testing.T, value string) {
	t.Helper()
	if !utf8.ValidString(value) {
		t.Fatalf("workspace label is not valid UTF-8: %q", value)
	}
	if strings.ContainsAny(value, "/\\\x00\r\n\t") {
		t.Fatalf("workspace label contains path/control delimiter: %q", value)
	}
	for _, current := range value {
		if unicode.IsControl(current) || unicode.Is(unicode.Bidi_Control, current) {
			t.Fatalf("workspace label contains control/bidi rune U+%04X: %q", current, value)
		}
	}
}
