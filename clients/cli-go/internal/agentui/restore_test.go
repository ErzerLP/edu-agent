package agentui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestArchiveRestorePreviewPagingWithoutApprovalGate(t *testing.T) {
	source := ".edu-agent-archive/legacy/" + strings.Repeat("s", 180) + "/item"
	target := strings.Repeat("t", 180) + "/target"
	version := "entry-v1:" + strings.Repeat("a", 64)
	pending := &agentloop.PendingFileMutation{Operation: "restore_archive", Path: source, DestinationPath: target, EntryKind: "directory", Preview: "当前版本：" + version + "\n不覆盖，不清理空容器。\n最后安全说明"}
	before := *pending
	s := newFileMutationSelector(pending)
	m := model{selector: s, height: minimumHeight}
	view := m.renderSelector(40)
	if !s.optionalReview || s.copyReview || s.copyPages < 2 || strings.Contains(s.body, "复制") || strings.Contains(s.body, "源未修改") || strings.Contains(view, "必须查看末页") {
		t.Fatal(view, s.body)
	}
	action, _ := s.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationApprove {
		t.Fatal("new last-page gate", action)
	}
	s = newFileMutationSelector(pending)
	m.selector = s
	_ = m.renderSelector(40)
	var shown []string
	for i := 0; i < s.copyPages; i++ {
		shown = append(shown, s.copyPreviewPage(36, 4)...)
		if i < s.copyPages-1 {
			_, _ = s.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			page := s.copyPage
			_, _ = s.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
			if s.copyPage != page {
				t.Fatal("skipped unseen page")
			}
			_ = m.renderSelector(40)
		}
	}
	full := strings.Join(shown, "")
	for _, want := range []string{source, target, version, "最后安全说明"} {
		if !strings.Contains(full, want) {
			t.Fatal("truncated frozen preview", want)
		}
	}
	if !reflect.DeepEqual(before, *pending) {
		t.Fatal("paging changed frozen authorization")
	}
	for _, cancel := range []bool{false, true} {
		s = newFileMutationSelector(pending)
		s.focus = 1
		key := tea.KeyMsg{Type: tea.KeyEnter}
		if cancel {
			key = tea.KeyMsg{Type: tea.KeyEsc}
		}
		action, _ = s.handleKey(key)
		if cancel && action.kind != selectorCancel || !cancel && (action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationDecline) {
			t.Fatal(action)
		}
	}
	for _, operation := range []string{"copy", "move", "mkdir"} {
		old := newFileMutationSelector(&agentloop.PendingFileMutation{Operation: operation, Path: "source", DestinationPath: "target", Preview: strings.Repeat("review\n", 50)})
		m.selector = old
		_ = m.renderSelector(40)
		action, _ = old.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		if !old.copyReview || action.kind == selectorSubmit {
			t.Fatal("old approval gate changed", operation)
		}
	}
}

func TestArchiveRestoreActivityReportsActualEndpoints(t *testing.T) {
	for _, outcome := range []string{"completed", "unknown"} {
		text := strings.Join(renderFileActivityDetails(&agentloop.FileActivityDetail{Operation: "restore_archive", Path: ".edu-agent-archive/old/item", DestinationPath: "recovered", EntryKind: "directory", PublicationOutcome: outcome}, 100), "\n")
		for _, want := range []string{"归档恢复源", "恢复目标", ".edu-agent-archive/old/item", "recovered", outcome} {
			if !strings.Contains(text, want) {
				t.Fatal(text)
			}
		}
		if strings.Contains(text, "复制") || strings.Contains(text, "未修改源") || strings.Contains(text, "临时项") {
			t.Fatal("copy semantics leaked", text)
		}
		if outcome == "unknown" && !strings.Contains(text, "不会自动重试") {
			t.Fatal(text)
		}
	}
}
