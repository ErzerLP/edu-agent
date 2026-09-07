package agentui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestArchivePurgeConfirmationAndIndependentPlan(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			m, c := fileArtifactModel(t)
			pending := &agentloop.PendingFileMutation{CallID: "purge", Tool: "purge_archive", Operation: "purge_archive", Path: ".edu-agent-archive/selected", EntryKind: "directory", PreviewKind: "purge_archive", Preview: strings.Repeat("冻结范围说明\n", 30) + "末尾不可撤销说明", PlanID: "receipt_b", PlanBytes: c.items[1].Bytes, PlanSaved: true}
			before := *pending
			m.pendingFileMutation = pending
			m.pendingFileTurnID = 77
			m.selector = newFileMutationSelector(pending)
			m.busy, m.activeCancelable = busy, busy
			selector := m.selector
			m.input.SetValue("draft")
			sends, resolves, mode := c.sends, c.resolutions, c.FileAuthorizationMode()
			view := m.renderSelector(46)
			if !selector.optionalReview || selector.copyReview || selector.copyPages < 2 || !strings.Contains(selector.title, "永久") || !strings.Contains(selector.body, "不可撤销") || !strings.Contains(selector.body, "即使YOLO") || selector.options[0].Label != "永久删除选定范围" || strings.Contains(view, "必须查看末页") {
				t.Fatal(selector, view)
			}
			m, cmd := fileArtifactKey(m, tea.KeyF6)
			m = fileArtifactApply(t, m, cmd)
			if m.artifactPanel == nil || m.artifactPanel.id != pending.PlanID || len(c.reads) != 1 || c.reads[0].offset != 0 || c.reads[0].limit != 4096 || !m.artifactPanel.page.More {
				t.Fatal("missing independent plan browser", c.reads)
			}
			m, _ = fileArtifactKey(m, tea.KeyF6)
			if m.artifactPanel != nil || m.selector != selector || m.pendingFileMutation != pending || !reflect.DeepEqual(before, *pending) || m.input.Value() != "draft" || c.sends != sends || c.resolutions != resolves || c.FileAuthorizationMode() != mode {
				t.Fatal("browser changed approval/model state")
			}
			// Reading an unread final F6 page is not required for explicit approval.
			action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
			if action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationApprove {
				t.Fatal("new page gate", action)
			}
			for _, cancel := range []bool{false, true} {
				s := newFileMutationSelector(pending)
				s.focus = 1
				key := tea.KeyMsg{Type: tea.KeyEnter}
				if cancel {
					key.Type = tea.KeyEsc
				}
				action, _ := s.handleKey(key)
				if cancel && action.kind != selectorCancel || !cancel && (action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationDecline) {
					t.Fatal(action)
				}
			}
		})
	}
	mode := newFileModeSelector(agentloop.FileAuthorizationYOLO)
	if !strings.Contains(mode.body, "永久归档清理始终另需一次明确确认") {
		t.Fatal(mode.body)
	}
}

func TestArchivePurgeActivityNeverClaimsReversibleOrFreedSpace(t *testing.T) {
	for _, outcome := range []string{"completed", "unknown"} {
		text := strings.Join(renderFileActivityDetails(&agentloop.FileActivityDetail{Operation: "purge_archive", Path: ".edu-agent-archive/selected", EntryKind: "directory", PublicationOutcome: outcome}, 100), "\n")
		for _, want := range []string{"永久归档清理范围", "不可撤销", "物理释放量未知", outcome} {
			if !strings.Contains(text, want) {
				t.Fatal(want, text)
			}
		}
		if strings.Contains(text, "未修改源") || strings.Contains(text, "复制目标") || strings.Contains(text, "恢复目标") {
			t.Fatal(text)
		}
		if outcome == "unknown" && !strings.Contains(text, "可能已部分删除") {
			t.Fatal(text)
		}
	}
}
