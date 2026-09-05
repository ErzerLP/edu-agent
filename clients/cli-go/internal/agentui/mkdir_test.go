package agentui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestMkdirAuthorizationAndPartialDetails(t *testing.T) {
	path := "existing/" + strings.Repeat("a", 120) + "/leaf"
	pending := &agentloop.PendingFileMutation{CallID: "mkdir", Tool: "mkdir", Operation: "mkdir", Path: path, EntryKind: "directory", PreviewKind: "mkdir", Preview: "创建目标：" + path + "\n已有父锚点：existing\n创建范围：2 层；失败保留目录，不删除回滚"}
	selector := newFileMutationSelector(pending)
	if !strings.Contains(selector.body, path) || !strings.Contains(selector.body, "父锚点：existing") || !strings.Contains(selector.body, "不删除回滚") {
		t.Fatal(selector.body)
	}
	_ = (model{selector: selector, height: 30}).renderSelector(120)
	action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationApprove {
		t.Fatal(action)
	}
	selector = newFileMutationSelector(pending)
	action, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if action.kind != selectorCancel {
		t.Fatal(action)
	}
	text := strings.Join(renderFileActivityDetails(&agentloop.FileActivityDetail{Path: "a/b", Operation: "mkdir", EntryKind: "directory", PublicationOutcome: "unknown", CreationAnchor: ".", PlannedDirectories: 2, CreatedDirectories: 1, HasDirectoryPlan: true}, 120), "\n")
	for _, s := range []string{"a/b", "已知创建 1 层", "其余计划路径可能已创建", "不自动重试"} {
		if !strings.Contains(text, s) {
			t.Fatalf("missing %s: %s", s, text)
		}
	}
	if toolDisplayName("mkdir") != "创建目录" {
		t.Fatal("missing tool label")
	}
	mode := newFileModeSelector(agentloop.FileAuthorizationYOLO)
	if !strings.Contains(mode.options[1].Description, "mkdir") {
		t.Fatal("YOLO omitted mkdir")
	}
}
