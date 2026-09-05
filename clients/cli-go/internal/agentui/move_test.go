package agentui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"strings"
	"testing"
)

func TestMoveFullPreviewPagingAndHonestBothEndpointLabels(t *testing.T) {
	source := strings.Repeat("s", 180) + "/source"
	target := strings.Repeat("t", 180) + "/target"
	version := "entry-v1:" + strings.Repeat("a", 64)
	pending := &agentloop.PendingFileMutation{Operation: "move", Path: source, DestinationPath: target, EntryKind: "directory", Preview: "源入口版本：" + version + "\n不遍历目录内部链接；不是子树快照。\n最后安全说明"}
	selector := newFileMutationSelector(pending)
	m := model{selector: selector, height: minimumHeight}
	if strings.Contains(selector.body, "源未修改") || strings.Contains(selector.body, "复制") || !strings.Contains(selector.body, "移动目标") {
		t.Fatal(selector.body)
	}
	action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind == selectorSubmit {
		t.Fatal("unrendered approval")
	}
	_ = m.renderSelector(40)
	if selector.copyPages < 2 {
		t.Fatal("no paging")
	}
	action, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind == selectorSubmit {
		t.Fatal("truncated approval")
	}
	shown := []string{}
	for i := 0; i < selector.copyPages; i++ {
		shown = append(shown, selector.copyPreviewPage(36, 4)...)
		if i < selector.copyPages-1 {
			_, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			page := selector.copyPage
			_, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			if selector.copyPage != page {
				t.Fatal("skipped unseen page")
			}
			_ = m.renderSelector(40)
		}
	}
	full := strings.Join(shown, "")
	for _, want := range []string{source, target, version, "最后安全说明"} {
		if !strings.Contains(full, want) {
			t.Fatal("truncated frozen text", want)
		}
	}
	action, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != selectorSubmit {
		t.Fatal("approval blocked after review")
	}
	for _, cancel := range []bool{false, true} {
		selector = newFileMutationSelector(pending)
		key := tea.KeyMsg{Type: tea.KeyEnter}
		selector.focus = 1
		if cancel {
			key = tea.KeyMsg{Type: tea.KeyEsc}
		}
		action, _ = selector.handleKey(key)
		if !cancel && (action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationDecline) {
			t.Fatal("decline blocked")
		}
		if cancel && action.kind != selectorCancel {
			t.Fatal("cancel blocked", action)
		}
	}
	details := strings.Join(renderFileActivityDetails(&agentloop.FileActivityDetail{Operation: "move", Path: "source", DestinationPath: "target", EntryKind: "directory", PublicationOutcome: "unknown"}, 100), "\n")
	for _, want := range []string{"source", "target", "移动目标", "核查源与目标", "不会自动重试"} {
		if !strings.Contains(details, want) {
			t.Fatal(details)
		}
	}
	if strings.Contains(details, "未修改源") || strings.Contains(details, "临时项") {
		t.Fatal("copy semantics in move", details)
	}
}
