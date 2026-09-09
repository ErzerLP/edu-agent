package command

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestEmbeddedImportResumePreservesLocalPositionAndRevalidates(t *testing.T) {
	m := &embeddedImport{importModel: newImportTestModel(t)}
	m.cursor = 1
	m.inputs[0].SetValue("中文目录")
	m.paste.SetValue("中文\n多行")
	m.view.SetContent(strings.Repeat("正文\n", 40))
	m.view.SetYOffset(7)
	m.draft.preview.Receipt = "旧凭证"
	m.Suspend()
	m.Resume(t.Context())
	m.Update(importMessage{generation: m.generation, kind: "读取资料集合", value: []api.KnowledgeCollection{m.draft.collection}})
	if m.stage != "files" || m.cursor != 1 || m.view.YOffset != 7 || m.draft.path != "中文目录" || m.draft.paste != "中文\n多行" || m.draft.preview.Receipt != "" {
		t.Fatalf("返回丢失本地位置或复用了旧凭证：%+v", m)
	}
	m.stage = "source"
	m.Update(tea.KeyMsg{Type: tea.KeyF5})
	if m.draft.space != api.DefaultLearningSpaceID || !strings.Contains(m.note, "F10") {
		t.Fatal("嵌入页绕过外壳切区，可能污染草稿")
	}
	m.draft.unknown, m.draft.request.OperationID, m.draft.preview.Receipt = true, "原操作", "原凭证"
	m.Suspend()
	m.Resume(t.Context())
	if m.draft.request.OperationID != "原操作" || m.draft.preview.Receipt != "原凭证" {
		t.Fatal("未知提交丢失核对身份")
	}
	m.Suspend()
}
