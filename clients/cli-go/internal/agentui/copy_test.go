package agentui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"strings"
	"testing"
)

func TestCopyAuthorizationBothEndpointsAndUnknownDetails(t *testing.T) {
	pending := &agentloop.PendingFileMutation{CallID: "copy", Tool: "copy", Operation: "copy", Path: "input/source.bin", DestinationPath: "output/copy.bin", EntryKind: "file", PreviewKind: "copy", Preview: "源入口版本：entry-v1:confirmed\n普通权限：0640"}
	selector := newFileMutationSelector(pending)
	for _, text := range []string{pending.Path, pending.DestinationPath, "源入口版本", "不覆盖目标", "不创建父目录"} {
		if !strings.Contains(selector.body, text) {
			t.Fatal(selector.body)
		}
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
	text := strings.Join(renderFileActivityDetails(&agentloop.FileActivityDetail{Path: pending.Path, DestinationPath: pending.DestinationPath, Operation: "copy", EntryKind: "file", PublicationOutcome: "unknown"}, 120), "\n")
	for _, want := range []string{pending.Path, pending.DestinationPath, "未修改源", "不会自动重试", "恢复重放"} {
		if !strings.Contains(text, want) {
			t.Fatal(text)
		}
	}
	if toolDisplayName("copy") != "复制文件" {
		t.Fatal("missing label")
	}
	if !strings.Contains(newFileModeSelector(agentloop.FileAuthorizationYOLO).options[1].Description, "copy") {
		t.Fatal("missing mode")
	}
}
