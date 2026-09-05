package agentui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestMkdirFullPreviewCannotAuthorizeUnseenCreationPaths(t *testing.T) {
	anchor := "existing/" + strings.Repeat("parent-", 24)
	target := anchor + "/" + strings.Repeat("child-", 24) + "/leaf"
	pending := &agentloop.PendingFileMutation{
		CallID: "mkdir-long", Tool: "mkdir", Operation: "mkdir", Path: target,
		EntryKind: "directory", PreviewKind: "mkdir",
		Preview: "创建目标：" + target + "\n已有父锚点：" + anchor + "\n创建范围：2 层\n最后说明：中途失败保留已创建项，不删除回滚。",
	}
	selector := newFileMutationSelector(pending)
	if action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); action.kind == selectorSubmit {
		t.Fatal("approved mkdir before rendering the frozen plan")
	}
	m := model{selector: selector, height: minimumHeight}
	const width = 40
	first := m.renderSelector(width)
	if selector.copyPages < 2 || !strings.Contains(first, "1/") {
		t.Fatal("long creation paths must be paginated", first)
	}
	if action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); action.kind == selectorSubmit {
		t.Fatal("approved a truncated creation path")
	}
	var seen strings.Builder
	for page := 0; page < selector.copyPages; page++ {
		_ = m.renderSelector(width)
		seen.WriteString(strings.Join(selector.copyPreviewPage(width-4, 4), ""))
		if page < selector.copyPages-1 {
			if action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); action.kind == selectorSubmit {
				t.Fatal("approved before the final page")
			}
			_, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			current := selector.copyPage
			_, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			if selector.copyPage != current {
				t.Fatal("skipped an unrendered creation-plan page")
			}
		}
	}
	for _, want := range []string{target, anchor, "创建范围：2 层", "不删除回滚"} {
		if !strings.Contains(seen.String(), want) {
			t.Fatalf("missing frozen detail %q in %q", want, seen.String())
		}
	}
	if action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationApprove {
		t.Fatal("full creation plan cannot be approved", action)
	}
	selector = newFileMutationSelector(pending)
	selector.focus = 1
	if action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationDecline {
		t.Fatal("decline must not require preview completion", action)
	}
	selector = newFileMutationSelector(pending)
	if action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); action.kind != selectorCancel {
		t.Fatal("cancel must not require preview completion", action)
	}
}
