package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/importer"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func newImportModel(ctx context.Context, client *api.Client, draft *importDraft, newID func() (string, error)) *importModel {
	m := &importModel{draft: draft, client: client, ctx: ctx, cancel: func() {}, newID: newID, stage: "target", width: 80, height: 24, view: viewport.New(76, 15), query: textinput.New(), paste: textarea.New()}
	m.paste.CharLimit = importer.MaxDocumentSize
	m.paste.SetValue(draft.paste)
	for _, v := range []string{draft.path, draft.include, draft.exclude} {
		input := textinput.New()
		input.CharLimit = 4096
		input.SetValue(v)
		m.inputs = append(m.inputs, input)
	}
	m.inputs[0].Focus()
	return m
}

type embeddedImport struct {
	*importModel
	resumeStage                string
	resumeCursor, resumeOffset int
}

func (m *embeddedImport) View() string {
	return strings.ReplaceAll(m.importModel.View(), "F5 学习区", "F10 返回后切区")
}

func (m *embeddedImport) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "f5" && m.stage == "source" {
		m.note = "请按 F10 返回工作台切换学习区；各区分别保留导入草稿。"
		return m, nil
	}
	_, cmd := m.importModel.Update(msg)
	if result, ok := msg.(importMessage); ok && result.kind == "读取资料集合" && result.generation == m.generation && result.err == nil {
		if m.stage == "source" {
			switch m.resumeStage {
			case "files", "paste", "browse", "detail", "source":
				m.stage, m.cursor = m.resumeStage, m.resumeCursor
				m.view.SetYOffset(m.resumeOffset)
			case "preview":
				m.stage = "files"
			}
		}
		m.resumeStage = ""
	}
	return m, cmd
}

func (m *embeddedImport) Rebind(other workbench.Leaf) {
	if fresh, ok := other.(*embeddedImport); ok {
		m.client = fresh.client
		m.draft.spaceName = fresh.draft.spaceName
	}
}

func (m *embeddedImport) Suspend() {
	m.resumeStage, m.resumeCursor, m.resumeOffset = m.stage, m.cursor, m.view.YOffset
	m.saveInputs()
	if m.requestCancel != nil {
		m.requestCancel()
	}
	m.cancel()
	m.generation++
	m.busy = ""
}

func (m *embeddedImport) Resume(ctx context.Context) tea.Cmd {
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.navigate = ""
	if m.resumeStage == "review" || (m.resumeStage == "detail" && m.detailBack != "files") {
		m.resumeStage = "files"
		m.view.SetContent("")
	}
	// 保留扫描清单和输入；重新读取目标，不把旧预览当作当前确认凭证。
	if !m.draft.unknown {
		m.invalidate()
	}
	return m.loadTargets()
}

func (m *embeddedImport) Destination(ctx context.Context) func() (string, error) {
	if m.navigate != "goal" || m.draft.result == nil {
		return func() (string, error) { return "materials", nil }
	}
	if m.draft.result.Summary == nil {
		return func() (string, error) {
			return "", fmt.Errorf("提交回执缺少资料上下文，请在资料页选择范围")
		}
	}
	// 在事件循环内冻结回执上下文，异步请求不读取后来编辑的叶子状态。
	entries := []api.KnowledgeScopeEntry{}
	for _, doc := range m.draft.result.Summary.DocumentIDs {
		entries = append(entries, api.KnowledgeScopeEntry{CollectionID: m.draft.result.Summary.CollectionID, RevisionID: m.draft.result.Revision.RevisionID, DocumentID: doc})
	}
	client, newID := m.client.WithLearningSpace(m.draft.result.Summary.SpaceID), m.newID
	return func() (string, error) {
		if len(entries) == 0 {
			return "", fmt.Errorf("本次回执没有可绑定资料")
		}
		id, err := newID()
		if err != nil {
			return "", err
		}
		scope, err := client.FreezeKnowledgeScope(ctx, api.KnowledgeScopeSnapshot{ID: id, Entries: entries})
		if err != nil {
			return "", mapAPIError(err)
		}
		return "new-goal/" + scope.ID, nil
	}
}
