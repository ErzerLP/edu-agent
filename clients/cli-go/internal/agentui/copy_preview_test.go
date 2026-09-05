package agentui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"strings"
	"testing"
)

func TestCopyFullPreviewPagingCannotApproveTruncatedPath(t *testing.T) {
	source := strings.Repeat("s", 180) + "/source.bin"
	destination := strings.Repeat("d", 180) + "/target.bin"
	pending := &agentloop.PendingFileMutation{Operation: "copy", Path: source, DestinationPath: destination, Preview: "源入口版本：entry-v1:" + strings.Repeat("a", 64) + "\n普通权限：0640\n最后安全说明"}
	selector := newFileMutationSelector(pending)
	action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind == selectorSubmit {
		t.Fatal("approval before rendering")
	}
	m := model{selector: selector, height: minimumHeight}
	first := m.renderSelector(40)
	if selector.copyPages < 2 || !strings.Contains(first, "1/") {
		t.Fatal("not paginated", first)
	}
	action, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind == selectorSubmit {
		t.Fatal("truncated plan authorized")
	}
	var shown []string
	for i := 0; i < selector.copyPages; i++ {
		shown = append(shown, selector.copyPreviewPage(36, 4)...)
		if i < selector.copyPages-1 {
			_, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			// Repeated keys cannot skip unrendered pages.
			page := selector.copyPage
			_, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			if selector.copyPage != page {
				t.Fatal("skipped preview page")
			}
			_ = m.renderSelector(40)
		}
	}
	complete := strings.Join(shown, "")
	for _, want := range []string{source, destination, strings.Repeat("a", 64), "最后安全说明"} {
		if !strings.Contains(complete, want) {
			t.Fatal("lost preview bytes", want, complete)
		}
	}
	action, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != selectorSubmit {
		t.Fatal("full review not authorized")
	}
	selector = newFileMutationSelector(pending)
	m.selector = selector
	_ = m.renderSelector(40)
	selector.focus = 1
	action, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationDecline {
		t.Fatal("decline blocked")
	}
}
