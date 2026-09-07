package agentui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestRecursiveCopyBrowser(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			m, c := fileArtifactModel(t)
			pending := &agentloop.PendingFileMutation{
				CallID:          "copy-plan-call",
				Tool:            "copy",
				Operation:       "copy",
				Path:            "source",
				DestinationPath: "target",
				EntryKind:       "directory",
				PreviewKind:     "copy",
				Preview:         "源入口版本：entry-v1\n仅复制源，不覆盖目标。",
				PlanID:          "receipt_b",
				PlanBytes:       c.items[1].Bytes,
				PlanSaved:       true,
			}
			m.pendingFileMutation, m.pendingFileTurnID = pending, 91
			m.selector = newFileMutationSelector(pending)
			selector := m.selector
			m.busy, m.activeCancelable = busy, busy
			m.input.SetValue("original draft")

			if !strings.Contains(selector.body, "F6 完整复制清单：receipt_b") || strings.Contains(selector.body, "F6 完整差异") {
				t.Fatalf("copy plan summary was missing or mislabeled: %s", selector.body)
			}
			initialSends, initialResolutions := c.sends, c.resolutions
			initialMode := c.FileAuthorizationMode()

			m, cmd := fileArtifactKey(m, tea.KeyF6)
			m = fileArtifactApply(t, m, cmd)
			if m.artifactPanel == nil || m.artifactPanel.id != pending.PlanID {
				t.Fatalf("F6 did not select the pending plan: panel=%v id=%q", m.artifactPanel != nil, m.artifactPanelID())
			}
			if len(c.reads) != 1 || c.reads[0].id != pending.PlanID || c.reads[0].offset != 0 || c.reads[0].limit != 4096 {
				t.Fatalf("unexpected raw plan read: %+v", c.reads)
			}
			if !m.artifactPanel.page.More {
				t.Fatal("fixture did not leave an unread final page for the F6 browser")
			}
			view := m.View()
			for _, want := range []string{"receipt_b", "receipt", "saved · 已加密保存", "原始字节（4096/页）"} {
				if !strings.Contains(view, want) {
					t.Fatalf("F6 plan view missing %q: %s", want, view)
				}
			}
			if strings.Contains(view, "F6 完整差异") {
				t.Fatalf("copy plan was presented as a diff: %s", view)
			}

			m, _ = fileArtifactKey(m, tea.KeyF6)
			if m.artifactPanel != nil || m.pendingFileMutation != pending || m.selector != selector || m.pendingFileTurnID != 91 || m.input.Value() != "original draft" {
				t.Fatal("closing the plan browser changed the pending interaction")
			}
			if m.busy != busy || m.activeCancelable != busy || c.FileAuthorizationMode() != initialMode || c.sends != initialSends || c.resolutions != initialResolutions || c.cancelledFileCall != "" {
				t.Fatalf("browser changed model state: busy=%t cancelable=%t sends=%d resolutions=%d cancelled=%q mode=%q", m.busy, m.activeCancelable, c.sends, c.resolutions, c.cancelledFileCall, c.FileAuthorizationMode())
			}

			if !busy {
				for {
					_ = m.renderSelector(100)
					if selector.copyPageRendered && selector.copyPage == selector.copyPages-1 {
						break
					}
					m, _ = fileArtifactKey(m, tea.KeyPgDown)
				}
				m, cmd = fileArtifactKey(m, tea.KeyEnter)
				m = runTurn(t, m, cmd)
				if c.resolutions != initialResolutions+1 || c.fileCallID != pending.CallID || c.fileResolution != agentloop.FileMutationApprove || m.pendingFileMutation != nil || m.selector != nil {
					t.Fatalf("approval did not use the original copy review: resolutions=%d call=%q resolution=%q pending=%v selector=%v", c.resolutions, c.fileCallID, c.fileResolution, m.pendingFileMutation != nil, m.selector != nil)
				}
				if c.sends != initialSends || c.FileAuthorizationMode() != initialMode {
					t.Fatal("approving the copy unexpectedly sent a model request or changed YOLO")
				}
			}
		})
	}

	t.Run("append-log", func(t *testing.T) {
		m, c := fileArtifactModel(t)
		id := "b_copy_log"
		body := []byte("plan: source=source target=target\npending: actual=unknown\n")
		c.bodies[id] = body
		c.items[1].ID = id
		c.items[1].Kind = "receipt"
		c.items[1].Bytes = int64(len(body))
		c.items[1].Saved = false

		pending := &agentloop.PendingFileMutation{
			CallID:          "copy-log-call",
			Tool:            "copy",
			Operation:       "copy",
			Path:            "source",
			DestinationPath: "target",
			EntryKind:       "directory",
			PreviewKind:     "copy",
			Preview:         "短授权摘要",
			PlanID:          id,
			PlanBytes:       int64(len(body)),
			PlanSaved:       false,
		}
		m.pendingFileMutation, m.pendingFileTurnID = pending, 92
		m.selector = newFileMutationSelector(pending)
		m.busy, m.activeCancelable = true, true

		m, cmd := fileArtifactKey(m, tea.KeyF6)
		m = fileArtifactApply(t, m, cmd)
		if m.artifactPanel == nil || m.artifactPanel.id != id {
			t.Fatalf("F6 did not select the append log: panel=%v id=%q", m.artifactPanel != nil, m.artifactPanelID())
		}
		if len(c.reads) != 1 || c.reads[0].id != id || c.reads[0].offset != 0 || c.reads[0].limit != 4096 || string(m.artifactPanel.page.Data) != string(body) {
			t.Fatalf("append log was not read as raw bytes: reads=%+v page=%q", c.reads, m.artifactPanel.page.Data)
		}
		view := m.View()
		for _, want := range []string{
			"追加日志：plan非执行；pending无actual为未知",
			"未完整保存（内存或部分保存）；恢复仅限认证范围",
			"plan: source=source target=target",
			"pending: actual=unknown",
		} {
			if !strings.Contains(view, want) {
				t.Fatalf("append-log view missing %q: %s", want, view)
			}
		}
		for _, misleading := range []string{"计划已完成", "pending已完成", "复制已完成"} {
			if strings.Contains(view, misleading) {
				t.Fatalf("append-log view presented pending data as completed: %s", view)
			}
		}
		if c.sends != 0 || c.resolutions != 0 || c.cancelledFileCall != "" {
			t.Fatalf("append-log browsing invoked an interaction: sends=%d resolutions=%d cancelled=%q", c.sends, c.resolutions, c.cancelledFileCall)
		}
		m, _ = fileArtifactKey(m, tea.KeyEsc)
		if m.pendingFileMutation != pending || m.selector == nil || m.input.Value() != "" || c.sends != 0 || c.resolutions != 0 {
			t.Fatal("closing append-log browser changed pending state")
		}
	})
}

func (m model) artifactPanelID() string {
	if m.artifactPanel == nil {
		return ""
	}
	return m.artifactPanel.id
}
