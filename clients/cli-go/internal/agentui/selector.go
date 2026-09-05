package agentui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type selectorKind int

const (
	selectorQuestion selectorKind = iota
	selectorPreference
	selectorPreferenceRetry
	selectorReasoning
	selectorFileMutation
	selectorFileMode
)

type selectorOption struct {
	ID          string
	Label       string
	Description string
	Selected    bool
}

type selectorModel struct {
	kind       selectorKind
	title      string
	body       string
	mode       agentloop.QuestionMode
	questionID string
	options    []selectorOption
	focus      int
	custom     textarea.Model
	hasCustom  bool
	submitted  bool
	width      int
}

type selectorActionKind int

const (
	selectorNoAction selectorActionKind = iota
	selectorSubmit
	selectorCancel
)

type selectorAction struct {
	kind           selectorActionKind
	optionIDs      []string
	custom         string
	resolution     agentloop.PreferenceResolution
	effort         modelclient.ReasoningEffort
	fileResolution agentloop.FileMutationResolution
	fileMode       agentloop.FileAuthorizationMode
}

func newQuestionSelector(question *agentloop.PendingQuestion) *selectorModel {
	custom := newCustomEditor()
	options := make([]selectorOption, 0, len(question.Options))
	for _, option := range question.Options {
		options = append(options, selectorOption{ID: option.ID, Label: option.Label, Description: option.Description})
	}
	return &selectorModel{
		kind: selectorQuestion, title: question.Header, body: question.Question,
		mode: question.Mode, questionID: question.ID, options: options, custom: custom, hasCustom: true,
	}
}

func newPreferenceSelector(pending *agentloop.PreferenceConfirmation) *selectorModel {
	return &selectorModel{
		kind:  selectorPreference,
		title: "长期偏好确认",
		body:  pending.Content,
		options: []selectorOption{
			{ID: string(agentloop.PreferenceSave), Label: "保存为长期偏好", Description: "写入长期记忆，并等待服务端确认"},
			{ID: string(agentloop.PreferenceSessionOnly), Label: "仅本次会话使用", Description: "继续当前会话，不写入长期记忆"},
			{ID: string(agentloop.PreferenceDecline), Label: "不保存", Description: "拒绝这次长期保存"},
		},
	}
}

func newPreferenceRetrySelector() *selectorModel {
	return &selectorModel{
		kind:  selectorPreferenceRetry,
		title: "长期偏好结果未知",
		body:  "只能使用原操作 ID 重试核对；不能改选为仅本次或不保存。",
		options: []selectorOption{
			{ID: string(agentloop.PreferenceRetry), Label: "重试核对原操作", Description: "沿用原操作 ID 查询或重试，不创建新操作"},
		},
	}
}

func newFileMutationSelector(pending *agentloop.PendingFileMutation) *selectorModel {
	body := fmt.Sprintf("操作：%s\n路径：%s\n%s：\n%s", pending.Operation, pending.Path, pending.PreviewKind, pending.Preview)
	if pending.ArchivePath != "" {
		body = fmt.Sprintf("源：%s\n归档目标：%s\n类型：%s\n仅整体移动，不永久删除；归档由用户手动恢复或清理。", pending.Path, pending.ArchivePath, pending.EntryKind)
	}
	if pending.Truncated {
		body += "\n预览已按安全上限截断。"
	}
	return &selectorModel{
		kind: selectorFileMutation, title: "文件修改授权", body: body,
		options: []selectorOption{
			{ID: string(agentloop.FileMutationApprove), Label: "允许此次修改", Description: "重新校验版本后只发布上方已冻结候选"},
			{ID: string(agentloop.FileMutationDecline), Label: "拒绝此次修改", Description: "文件保持不变，并把 authorization_denied 返回模型"},
		},
	}
}

func newFileModeSelector(current agentloop.FileAuthorizationMode) *selectorModel {
	if current == "" {
		current = agentloop.FileAuthorizationConfirm
	}
	options := []selectorOption{
		{ID: string(agentloop.FileAuthorizationConfirm), Label: "逐次确认", Description: "每个 write/edit/archive 都显示冻结预览并等待明确授权"},
		{ID: string(agentloop.FileAuthorizationYOLO), Label: "YOLO", Description: "当前 Session 后续 write/edit/archive 不再确认，但所有安全校验保持不变"},
	}
	focus := 0
	for index := range options {
		options[index].Selected = options[index].ID == string(current)
		if options[index].Selected {
			focus = index
		}
	}
	return &selectorModel{
		kind: selectorFileMode, title: "文件授权模式",
		body:    "YOLO 仅当前 Session 有效。归档目录禁止普通写入或清理；其他隐藏文件、.git、.comet 和秘密文件没有额外路径保护，内容可能发送给当前 provider。",
		options: options, focus: focus,
	}
}

func newReasoningSelector(current modelclient.ReasoningEffort) *selectorModel {
	if current == "" {
		current = modelclient.ReasoningEffortAuto
	}
	values := []modelclient.ReasoningEffort{
		modelclient.ReasoningEffortNone,
		modelclient.ReasoningEffortMinimal,
		modelclient.ReasoningEffortLow,
		modelclient.ReasoningEffortMedium,
		modelclient.ReasoningEffortHigh,
		modelclient.ReasoningEffortXHigh,
		modelclient.ReasoningEffortMax,
		modelclient.ReasoningEffortAuto,
	}
	options := make([]selectorOption, 0, len(values))
	focus := 0
	for index, value := range values {
		selected := value == current
		if selected {
			focus = index
		}
		options = append(options, selectorOption{ID: string(value), Label: string(value), Selected: selected})
	}
	return &selectorModel{
		kind: selectorReasoning, title: "推理强度", body: "选择后立即更新；已发出的模型请求保持原档位。",
		options: options, focus: focus,
	}
}

func newCustomEditor() textarea.Model {
	editor := textarea.New()
	editor.Placeholder = "输入自定义回答；Shift+Enter / Ctrl+J / Alt+Enter 换行"
	editor.CharLimit = 2000
	editor.SetHeight(2)
	editor.ShowLineNumbers = false
	editor.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter", "shift+enter")
	return editor
}

func (s *selectorModel) setWidth(width int) {
	if width < 16 {
		width = 16
	}
	s.width = width
	if s.hasCustom {
		s.custom.SetWidth(max(12, width-8))
	}
}

func (s *selectorModel) handleKey(msg tea.KeyMsg) (selectorAction, tea.Cmd) {
	if s == nil || s.submitted {
		return selectorAction{}, nil
	}
	key := msg.String()
	if key == "esc" {
		switch s.kind {
		case selectorQuestion:
			s.submitted = true
			return selectorAction{kind: selectorCancel}, nil
		case selectorPreference:
			s.submitted = true
			return selectorAction{kind: selectorSubmit, resolution: agentloop.PreferenceDecline}, nil
		case selectorFileMutation, selectorFileMode:
			return selectorAction{kind: selectorCancel}, nil
		case selectorReasoning:
			return selectorAction{kind: selectorCancel}, nil
		case selectorPreferenceRetry:
			return selectorAction{}, nil
		}
	}

	customIndex := len(s.options)
	if s.hasCustom && s.focus == customIndex {
		switch key {
		case "shift+tab":
			s.custom.Blur()
			s.focus = max(0, customIndex-1)
			return selectorAction{}, nil
		case "tab":
			s.custom.Blur()
			s.focus = 0
			return selectorAction{}, nil
		case "shift+enter", "ctrl+j", "alt+enter":
			return selectorAction{}, s.insertCustomNewline()
		case "enter":
			if strings.TrimSpace(s.custom.Value()) == "" && !s.hasSelectedOptions() {
				return selectorAction{}, nil
			}
			s.submitted = true
			return selectorAction{kind: selectorSubmit, optionIDs: s.selectedOptionIDs(), custom: s.custom.Value()}, nil
		}
		var cmd tea.Cmd
		s.custom, cmd = s.custom.Update(msg)
		return selectorAction{}, cmd
	}

	switch key {
	case "up", "shift+tab":
		if s.focus > 0 {
			s.focus--
		} else if s.hasCustom {
			s.focus = customIndex
			s.custom.Focus()
		}
		return selectorAction{}, nil
	case "down", "tab":
		if s.focus+1 < len(s.options) {
			s.focus++
		} else if s.hasCustom {
			s.focus = customIndex
			s.custom.Focus()
		}
		return selectorAction{}, nil
	case " ":
		if s.kind == selectorQuestion && s.mode == agentloop.QuestionMultiple && s.focus < len(s.options) {
			s.options[s.focus].Selected = !s.options[s.focus].Selected
		}
		return selectorAction{}, nil
	case "enter":
		if s.focus >= len(s.options) {
			return selectorAction{}, nil
		}
		if s.kind == selectorQuestion {
			if s.mode == agentloop.QuestionMultiple {
				if !s.hasSelectedOptions() {
					return selectorAction{}, nil
				}
				s.submitted = true
				return selectorAction{kind: selectorSubmit, optionIDs: s.selectedOptionIDs()}, nil
			}
			s.submitted = true
			return selectorAction{kind: selectorSubmit, optionIDs: []string{s.options[s.focus].ID}}, nil
		}
		s.submitted = true
		if s.kind == selectorReasoning {
			return selectorAction{kind: selectorSubmit, effort: modelclient.ReasoningEffort(s.options[s.focus].ID)}, nil
		}
		if s.kind == selectorFileMutation {
			return selectorAction{kind: selectorSubmit, fileResolution: agentloop.FileMutationResolution(s.options[s.focus].ID)}, nil
		}
		if s.kind == selectorFileMode {
			return selectorAction{kind: selectorSubmit, fileMode: agentloop.FileAuthorizationMode(s.options[s.focus].ID)}, nil
		}
		return selectorAction{kind: selectorSubmit, resolution: agentloop.PreferenceResolution(s.options[s.focus].ID)}, nil
	}

	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		index := int(key[0] - '1')
		if index >= 0 && index < len(s.options) {
			s.focus = index
		}
		return selectorAction{}, nil
	}
	if key == "0" && s.hasCustom {
		s.focus = customIndex
		s.custom.Focus()
		return selectorAction{}, nil
	}
	return selectorAction{}, nil
}

func (s *selectorModel) insertCustomNewline() tea.Cmd {
	// textarea treats Ctrl+J as a newline across terminals; normalizing the
	// alternatives here keeps Shift+Enter and Alt+Enter deterministic in tests.
	var cmd tea.Cmd
	s.custom, cmd = s.custom.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	return cmd
}

func (s *selectorModel) hasSelectedOptions() bool {
	for _, option := range s.options {
		if option.Selected {
			return true
		}
	}
	return false
}

func (s *selectorModel) selectedOptionIDs() []string {
	ids := make([]string, 0, len(s.options))
	for _, option := range s.options {
		if option.Selected {
			ids = append(ids, option.ID)
		}
	}
	return ids
}

func (s *selectorModel) helpText() string {
	if s == nil {
		return ""
	}
	switch s.kind {
	case selectorQuestion:
		if s.mode == agentloop.QuestionMultiple {
			return "↑/↓/Tab 移动 · 1-4/Space 多选 · 0 自定义 · Enter 提交 · Esc 取消"
		}
		return "↑/↓/Tab 移动 · 1-4 选择 · 0 自定义 · Enter 提交 · Esc 取消"
	case selectorPreference:
		return "↑/↓/Tab 或 1-3 · Enter 确认 · Esc 不保存"
	case selectorPreferenceRetry:
		return "Enter 重试原操作 · Esc 已禁用"
	case selectorReasoning:
		return "↑/↓/Tab 或数字 · Enter 应用 · Esc 返回"
	case selectorFileMutation:
		return "↑/↓/Tab 或 1-2 · Enter 确认 · Esc 停止当前轮次"
	case selectorFileMode:
		return "↑/↓/Tab 或 1-2 · Enter 应用 · Esc 返回"
	default:
		return fmt.Sprintf("%d 个选项", len(s.options))
	}
}
