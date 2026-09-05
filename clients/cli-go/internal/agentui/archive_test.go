package agentui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestArchiveAuthorizationAndTerminalDetailsShowDestination(t *testing.T) {
	const destination = ".edu-agent-archive/20260905-unique/notes"
	pending := &agentloop.PendingFileMutation{
		CallID: "archive-call", Tool: "archive", Operation: "archive", Path: "notes",
		ArchivePath: destination, EntryKind: "directory", PreviewKind: "archive",
		Preview: "源：notes\n归档到：" + destination + "\n由用户手动恢复或清理",
	}
	selector := newFileMutationSelector(pending)
	for _, expected := range []string{"notes", destination, "directory", "不永久删除", "用户手动"} {
		if !strings.Contains(selector.body, expected) {
			t.Fatalf("selector missing %q: %s", expected, selector.body)
		}
	}
	action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationApprove {
		t.Fatalf("wrong authorization: %+v", action)
	}
	selector = newFileMutationSelector(pending)
	action, _ = selector.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if action.kind != selectorCancel {
		t.Fatalf("Esc did not stop: %+v", action)
	}
	text := strings.Join(renderFileActivityDetails(&agentloop.FileActivityDetail{
		Path: "notes", ArchivePath: destination, EntryKind: "directory", Operation: "archive",
		PreviewKind: "archive", Preview: pending.Preview, PublicationOutcome: "completed",
	}, 120), "\n")
	for _, expected := range []string{destination, "用户手动", "completed"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("details missing %q: %s", expected, text)
		}
	}
	if toolDisplayName("archive") != "归档文件或目录" {
		t.Fatal("archive shown as unknown tool")
	}
	mode := newFileModeSelector(agentloop.FileAuthorizationYOLO)
	if !strings.Contains(mode.options[1].Description, "archive") || !strings.Contains(mode.body, "归档目录禁止") {
		t.Fatalf("YOLO warning missing archive boundary: %+v", mode)
	}
}
