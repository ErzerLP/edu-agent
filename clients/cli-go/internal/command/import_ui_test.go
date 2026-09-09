package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/importer"
)

func newImportTestModel(t *testing.T) *importModel {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	m := &importModel{ctx: ctx, cancel: cancel, client: api.NewClient("http://127.0.0.1:1", "test", 0, nil), draft: &importDraft{space: api.DefaultLearningSpaceID, collection: api.KnowledgeCollection{ID: "00000000-0000-4000-8000-000000000002", Name: "Go"}}, stage: "files", height: 24, width: 80, view: viewport.New(76, 15), paste: textarea.New(), query: textinput.New(), newID: func() (string, error) { return "10000000-0000-4000-8000-000000000001", nil }}
	for range 3 {
		m.inputs = append(m.inputs, textinput.New())
	}
	for _, name := range []string{"a.md", "b.md"} {
		doc := api.ImportDocument{Path: name, Markdown: "# " + name}
		m.draft.report.Items = append(m.draft.report.Items, importer.ScanItem{Path: name, Status: "ready", Selected: true, Document: &doc})
	}
	return m
}

func TestImportDraftSelectionAndLateResults(t *testing.T) {
	m := newImportTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if len(m.draft.report.Documents()) != 1 {
		t.Fatal("多选未影响最终清单")
	}
	m.previewRequest(true)
	if len(m.draft.request.Documents) != 1 || m.draft.request.Documents[0].Path != "b.md" {
		t.Fatal("请求包含已排除项")
	}
	old := m.generation
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.draft.space = "20000000-0000-4000-8000-000000000001"
	m.Update(importMessage{generation: old, kind: "本地扫描", value: importer.ScanReport{}})
	if len(m.draft.report.Items) != 2 || m.draft.space == api.DefaultLearningSpaceID {
		t.Fatal("迟到结果改写了新草稿")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.stage != "source" || m.draft.report.Items[0].Selected {
		t.Fatal("返回丢失了选择")
	}
}

func TestImportUnknownOutcomeKeepsOriginalOperation(t *testing.T) {
	m := newImportTestModel(t)
	m.previewRequest(true)
	m.busy = ""
	m.draft.preview.Receipt = "signed-receipt"
	op := m.draft.request.OperationID
	m.confirm()
	m.Update(importMessage{generation: m.generation, kind: "正式提交", err: errors.New("连接在响应前关闭")})
	if !m.draft.unknown || m.stage != "result" || m.draft.request.OperationID != op || m.draft.preview.Receipt != "signed-receipt" {
		t.Fatal("未知结果丢失原操作")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.draft.request.OperationID != op || !m.draft.unknown {
		t.Fatal("重试创建了新操作")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.stage != "result" || !m.draft.unknown {
		t.Fatal("取消正式提交被当作确定失败")
	}
}

func TestImportFailedReviewKeepsFilesAndSearchSelectsVisibleItem(t *testing.T) {
	m := newImportTestModel(t)
	m.stage = "review"
	m.reviewIndex = 9
	m.Update(importMessage{generation: m.generation, kind: "上传并检查", err: &api.APIError{Code: "path_occupied", Status: 409}})
	if m.stage != "files" || m.reviewIndex != 0 || len(m.draft.report.Items) != 2 {
		t.Fatal("身份失败未回到可修正清单")
	}
	m.editing = "search"
	m.query.SetValue("b.md")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.cursor != 1 {
		t.Fatal("搜索后焦点仍指向不可见文件")
	}
	m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if m.draft.report.Items[1].Selected == true || !m.draft.report.Items[0].Selected {
		t.Fatal("搜索后取消了错误文件")
	}
}

func TestImportNonTTYNeverReadsInteractiveInput(t *testing.T) {
	app, out, _ := newTestApp(nil, nil, nil)
	if err := app.runImportWorkflow(t.Context(), []string{"wizard"}); err == nil {
		t.Fatal("非 TTY 启动了向导")
	}
	if _, _, err := app.collectIdentityResolutions(api.IdentityReview{}); err == nil {
		t.Fatal("非 TTY 等待身份输入")
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatal("非 TTY 输出控制序列")
	}
}
