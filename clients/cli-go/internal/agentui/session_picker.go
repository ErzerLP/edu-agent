package agentui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontroller"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/mattn/go-runewidth"
	"golang.org/x/text/unicode/norm"
)

type SessionPickerSource interface {
	Generation() uint64
	SwitchGate() agentcontroller.SwitchGate
	ListSessions(context.Context, agentcontroller.SessionListRequest) ([]agentcontroller.SessionListItem, error)
	PlanSwitch(agentsession.Summary) agentcontroller.SwitchPlan
	RenameSession(context.Context, string, string, uint64) (agentsession.Summary, error)
	DeleteSession(context.Context, agentsession.DeleteTarget) error
}

type sessionManager interface {
	SessionPickerSource
	CommitSwitch(context.Context, agentcontroller.SwitchPlan, agentcontroller.SwitchConfirmation) (uint64, error)
	NewSession(context.Context) (uint64, error)
	UnknownOutcomes() []agentcontroller.UnknownOutcome
	RetryPreferenceReceipt(context.Context, string) error
}

type pickerMode uint8

const (
	pickerList pickerMode = iota
	pickerRename
	pickerDeleteConfirm
	pickerSwitchConfirm
)

type pickerIntentKind uint8

const (
	pickerIntentNone pickerIntentKind = iota
	pickerIntentRefresh
	pickerIntentCancel
	pickerIntentSwitch
	pickerIntentNew
	pickerIntentRename
	pickerIntentDelete
)

type pickerIntent struct {
	kind         pickerIntentKind
	plan         agentcontroller.SwitchPlan
	item         agentcontroller.SessionListItem
	title        string
	confirmation agentcontroller.SwitchConfirmation
}

type sessionPickerModel struct {
	scopeAll bool
	query    string
	items    []agentcontroller.SessionListItem
	selected int
	mode     pickerMode
	edit     string
	plan     agentcontroller.SwitchPlan
	loading  bool
	status   string
	epoch    uint64
}

func newSessionPickerModel(all bool) *sessionPickerModel {
	return &sessionPickerModel{scopeAll: all, loading: true, status: "正在读取加密 Session 列表"}
}

func (p *sessionPickerModel) request() agentcontroller.SessionListRequest {
	return agentcontroller.SessionListRequest{All: p.scopeAll, Query: p.query}
}

func (p *sessionPickerModel) beginRequest() (agentcontroller.SessionListRequest, uint64) {
	p.epoch++
	p.loading = true
	return p.request(), p.epoch
}

func (p *sessionPickerModel) setItems(items []agentcontroller.SessionListItem, err error) {
	p.loading = false
	if err != nil {
		p.status = "Session 列表读取失败"
		return
	}
	p.items = append([]agentcontroller.SessionListItem(nil), items...)
	if p.selected > len(p.items) {
		p.selected = len(p.items)
	}
	p.status = fmt.Sprintf("已找到 %d 个 Session", len(items))
}

func (p *sessionPickerModel) selectedItem() (agentcontroller.SessionListItem, bool) {
	if p.selected == 0 || p.selected-1 >= len(p.items) {
		return agentcontroller.SessionListItem{}, false
	}
	return p.items[p.selected-1], true
}

func (p *sessionPickerModel) handleKey(msg tea.KeyMsg, source SessionPickerSource) pickerIntent {
	key := msg.String()
	switch p.mode {
	case pickerRename:
		switch key {
		case "esc":
			p.mode, p.edit = pickerList, ""
			return pickerIntent{}
		case "enter":
			item, ok := p.selectedItem()
			if !ok {
				p.mode = pickerList
				return pickerIntent{}
			}
			return pickerIntent{kind: pickerIntentRename, item: item, title: p.edit}
		case "backspace":
			runes := []rune(p.edit)
			if len(runes) > 0 {
				p.edit = string(runes[:len(runes)-1])
			}
			return pickerIntent{}
		}
		if msg.Type == tea.KeyRunes {
			p.edit = boundedPickerText(p.edit+string(msg.Runes), 80)
		}
		return pickerIntent{}
	case pickerDeleteConfirm:
		if key == "esc" {
			p.mode = pickerList
			return pickerIntent{}
		}
		if key == "ctrl+d" {
			item, ok := p.selectedItem()
			if ok {
				return pickerIntent{kind: pickerIntentDelete, item: item}
			}
		}
		return pickerIntent{}
	case pickerSwitchConfirm:
		if key == "esc" {
			p.mode = pickerList
			return pickerIntent{}
		}
		if key == "l" && p.plan.NeedProviderConfirm {
			return pickerIntent{
				kind: pickerIntentSwitch,
				plan: p.plan,
				confirmation: agentcontroller.SwitchConfirmation{
					Workspace: p.plan.NeedWorkspaceConfirm,
					Provider:  false,
				},
			}
		}
		if key == "enter" {
			return pickerIntent{kind: pickerIntentSwitch, plan: p.plan, confirmation: agentcontroller.SwitchConfirmation{Workspace: true, Provider: true}}
		}
		return pickerIntent{}
	}

	switch key {
	case "esc":
		return pickerIntent{kind: pickerIntentCancel}
	case "tab":
		p.scopeAll = !p.scopeAll
		p.selected, p.loading = 0, true
		return pickerIntent{kind: pickerIntentRefresh}
	case "up":
		if p.selected > 0 {
			p.selected--
		}
	case "down":
		if p.selected < len(p.items) {
			p.selected++
		}
	case "pgup":
		p.selected = max(0, p.selected-5)
	case "pgdown":
		p.selected = min(len(p.items), p.selected+5)
	case "backspace":
		runes := []rune(p.query)
		if len(runes) > 0 {
			p.query = string(runes[:len(runes)-1])
			p.loading = true
			return pickerIntent{kind: pickerIntentRefresh}
		}
	case "ctrl+r":
		item, ok := p.selectedItem()
		if ok && !item.Summary.Corrupt && !item.Summary.Unavailable {
			p.mode, p.edit = pickerRename, item.Summary.Title
		}
	case "ctrl+d":
		item, ok := p.selectedItem()
		if ok && !item.Current && !item.Summary.Locked && !item.Summary.Unavailable {
			p.mode = pickerDeleteConfirm
		}
	case "enter":
		item, ok := p.selectedItem()
		if !ok {
			return pickerIntent{kind: pickerIntentNew}
		}
		if item.Summary.Corrupt || item.Summary.Unavailable {
			p.status = "该 Session 损坏或不可用"
			return pickerIntent{}
		}
		if item.Summary.Locked && !item.Current {
			p.status = "该 Session 正由另一个进程使用"
			return pickerIntent{}
		}
		plan := source.PlanSwitch(item.Summary)
		if plan.SameTarget {
			return pickerIntent{kind: pickerIntentCancel}
		}
		if plan.NeedProviderConfirm || plan.NeedWorkspaceConfirm {
			p.mode, p.plan = pickerSwitchConfirm, plan
			return pickerIntent{}
		}
		return pickerIntent{kind: pickerIntentSwitch, plan: plan}
	default:
		if msg.Type == tea.KeyRunes {
			p.query = boundedPickerText(p.query+string(msg.Runes), 128)
			p.selected, p.loading = 0, true
			return pickerIntent{kind: pickerIntentRefresh}
		}
	}
	return pickerIntent{}
}

func sessionPickerItemLabel(summary agentsession.Summary) string {
	if summary.VersionUnsupported {
		if summary.SessionID != "" {
			return "不兼容 Session · " + truncateDisplayWidth(summary.SessionID, 8)
		}
		return "不兼容 Session · storage:" + shortStorageLabel(summary.StorageID)
	}
	if summary.LocatorOnly {
		return "损坏 Session · storage:" + shortStorageLabel(summary.StorageID)
	}
	if strings.TrimSpace(summary.Title) != "" {
		return summary.Title
	}
	if summary.SessionID != "" {
		return "损坏 Session · " + truncateDisplayWidth(summary.SessionID, 8)
	}
	return "损坏 Session · storage:" + shortStorageLabel(summary.StorageID)
}

func shortStorageLabel(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func deleteTargetForSummary(summary agentsession.Summary) agentsession.DeleteTarget {
	return agentsession.DeleteTarget{
		SessionID: summary.SessionID, StorageID: summary.StorageID,
		ExpectedRecordRevision: summary.RecordRevision,
	}
}

func pickerErrorText(err error) string {
	code, reason, next := "session_operation_failed", "Session 操作未完成", "保留当前 Session；稍后重试"
	switch {
	case errors.Is(err, agentsession.ErrInUse):
		code, reason, next = "session_in_use", "目标 Session 正在其他进程中使用", "关闭另一进程后刷新列表"
	case errors.Is(err, agentsession.ErrCorrupt):
		code, reason, next = "session_corrupt", "目标 Session 已损坏或无法验证", "保留当前 Session；可删除损坏记录"
	case errors.Is(err, agentsession.ErrVersionUnsupported):
		code, reason, next = "session_version_unsupported", "目标 Session 版本不受支持", "升级客户端后重试；当前版本不会删除该记录"
	case errors.Is(err, agentsession.ErrCheckpointConflict):
		code, reason, next = "session_checkpoint_conflict", "Session 已被并发更新", "刷新列表后重试"
	case errors.Is(err, agentsession.ErrStoreFull):
		code, reason, next = "session_store_full", "加密 Session 存储已达到硬上限", "手动删除不再需要的 Session"
	case errors.Is(err, agentsession.ErrKeyUnavailable):
		code, reason, next = "session_store_unavailable", "系统钥匙串或加密存储不可用", "检查平台密钥服务后重试"
	case errors.Is(err, agentcontroller.ErrProviderConfirmationRequired):
		code, reason, next = "session_provider_confirmation_required", "需要确认新的模型 provider 端点", "返回确认面板后再继续"
	case errors.Is(err, agentcontroller.ErrWorkspaceConfirmationRequired):
		code, reason, next = "session_workspace_confirmation_required", "需要确认历史工作区", "返回确认面板后再继续"
	case errors.Is(err, agentcontroller.ErrCurrentSession):
		code, reason, next = "session_current", "不能删除当前 active Session", "先切换到其他 Session"
	case errors.Is(err, agentcontroller.ErrSwitchUnavailable):
		code, reason, next = "session_switch_unavailable", "当前状态不允许切换 Session", "完成或停止当前操作后重试"
	case errors.Is(err, agentsession.ErrInvalid):
		code, reason, next = "session_invalid", "Session 输入不符合安全边界", "修改搜索词或标题后重试"
	}
	return fmt.Sprintf("[%s] %s；%s", code, reason, next)
}

func boundedPickerText(value string, maxRunes int) string {
	value = norm.NFC.String(value)
	clean := make([]rune, 0, min(utf8.RuneCountInString(value), maxRunes))
	for _, current := range value {
		if len(clean) >= maxRunes {
			break
		}
		if unicode.IsControl(current) || current == '\u202e' || current == '\u202d' || current == '\u2066' || current == '\u2067' || current == '\u2068' || current == '\u2069' {
			continue
		}
		clean = append(clean, current)
	}
	return string(clean)
}

func (p *sessionPickerModel) render(width, height int) string {
	width = max(minimumWidth, width)
	height = max(minimumHeight, height)
	inner := max(20, width-6)
	scope := "当前工作区"
	if p.scopeAll {
		scope = "全部工作区"
	}
	lines := []string{
		assistantLabelStyle.Render("Session 选择器"),
		mutedStyle.Render("范围 " + scope + " · Tab 切换"),
		"搜索 › " + truncateDisplayWidth(p.query, max(8, inner-9)),
	}
	if p.mode == pickerSwitchConfirm {
		lines = append(lines, "", confirmLabelStyle.Render("确认安全切换"))
		if p.plan.NeedWorkspaceConfirm {
			lines = append(lines, "工作区："+truncateDisplayWidth(safeWorkspaceLabel(p.plan.WorkspaceLabel), inner-8), "将重新打开原工作区；失败时文件工具保持禁用。")
		}
		if p.plan.NeedProviderConfirm {
			lines = append(lines, "当前 provider 端点与历史不同。", "确认前不会发送任何历史正文或标题输入。")
			if p.plan.NeedWorkspaceConfirm {
				lines = append(lines, "", "L 确认原工作区并仅本地打开（拒绝向新 provider 发送）")
			} else {
				lines = append(lines, "", "L 仅本地打开（拒绝向新 provider 发送）")
			}
		}
		confirmText := "Enter 明确确认"
		if p.plan.NeedProviderConfirm {
			confirmText = "Enter 确认并允许发送"
		}
		lines = append(lines, "", confirmText+" · Esc 取消切换")
		return lipgloss.NewStyle().Width(inner).Render(strings.Join(lines, "\n"))
	}
	if p.mode == pickerRename {
		lines = append(lines, "", confirmLabelStyle.Render("重命名 Session"), "标题 › "+truncateDisplayWidth(p.edit, max(8, inner-9)), "空标题恢复自动命名", "Enter 保存 · Esc 取消")
		return lipgloss.NewStyle().Width(inner).Render(strings.Join(lines, "\n"))
	}
	if p.mode == pickerDeleteConfirm {
		item, _ := p.selectedItem()
		lines = append(lines, "", errorLabelStyle.Render("永久删除"), truncateDisplayWidth(sessionPickerItemLabel(item.Summary), inner), "删除会销毁 wrapped Session key，且不可撤销。", "再次按 Ctrl+D 确认 · Esc 取消")
		return lipgloss.NewStyle().Width(inner).Render(strings.Join(lines, "\n"))
	}

	visibleRows := max(3, min(9, height-9))
	start := 0
	if p.selected >= visibleRows {
		start = p.selected - visibleRows + 1
	}
	end := min(len(p.items)+1, start+visibleRows)
	for index := start; index < end; index++ {
		selected := index == p.selected
		prefix := "  "
		if selected {
			prefix = "› "
		}
		if index == 0 {
			lines = append(lines, prefix+"新建 Session  不复制当前上下文")
			continue
		}
		item := p.items[index-1]
		summary := item.Summary
		source := "自动"
		if summary.TitleSource == "manual" {
			source = "手动"
		}
		state := ""
		switch {
		case item.Current:
			state = " · current"
		case summary.VersionUnsupported:
			state = " · unsupported"
		case summary.Corrupt:
			state = " · corrupt"
		case summary.Unavailable:
			state = " · unavailable"
		case summary.Locked:
			state = " · locked"
		}
		label := sessionPickerItemLabel(summary)
		primary := fmt.Sprintf("%s%s%s", prefix, label, state)
		if !summary.LocatorOnly && strings.TrimSpace(summary.Title) != "" {
			primary = fmt.Sprintf("%s%s [%s]%s", prefix, label, source, state)
		}
		lines = append(lines, truncateDisplayWidth(primary, inner))
		secondary := ""
		if !summary.UpdatedAt.IsZero() {
			secondary = fmt.Sprintf("   %s · %d 轮", summary.UpdatedAt.Local().Format("01-02 15:04"), summary.CommittedUserTurns)
		} else {
			secondary = "   storage:" + shortStorageLabel(summary.StorageID)
		}
		if p.scopeAll && strings.TrimSpace(summary.WorkspaceLabel) != "" {
			secondary += " · " + safeWorkspaceLabel(summary.WorkspaceLabel)
		}
		if runewidth.StringWidth(secondary) <= inner && height >= 20 {
			lines = append(lines, mutedStyle.Render(truncateDisplayWidth(secondary, inner)))
		}
	}
	status := p.status
	if p.loading {
		status = "正在刷新…"
	}
	lines = append(lines, "", mutedStyle.Render(truncateDisplayWidth(status, inner)), "↑↓/PgUp/PgDn 选择 · Enter 恢复 · Esc 返回", "Ctrl+R 重命名 · Ctrl+D 永久删除")
	return lipgloss.NewStyle().Width(inner).Render(strings.Join(lines, "\n"))
}

type pickerListMsg struct {
	generation uint64
	epoch      uint64
	items      []agentcontroller.SessionListItem
	err        error
}

type pickerOperationMsg struct {
	generation    uint64
	newGeneration uint64
	kind          pickerIntentKind
	err           error
}

type preferenceRetryMsg struct {
	generation uint64
	err        error
}

func listSessionsCmd(ctx context.Context, source SessionPickerSource, generation, epoch uint64, request agentcontroller.SessionListRequest) tea.Cmd {
	return func() tea.Msg {
		items, err := source.ListSessions(ctx, request)
		return pickerListMsg{generation: generation, epoch: epoch, items: items, err: err}
	}
}

func pickerOperationCmd(ctx context.Context, manager sessionManager, generation uint64, intent pickerIntent) tea.Cmd {
	return func() tea.Msg {
		var next uint64
		var err error
		switch intent.kind {
		case pickerIntentSwitch:
			next, err = manager.CommitSwitch(ctx, intent.plan, intent.confirmation)
		case pickerIntentNew:
			next, err = manager.NewSession(ctx)
		case pickerIntentRename:
			_, err = manager.RenameSession(ctx, intent.item.Summary.SessionID, intent.title, intent.item.Summary.RecordRevision)
		case pickerIntentDelete:
			err = manager.DeleteSession(ctx, deleteTargetForSummary(intent.item.Summary))
		}
		return pickerOperationMsg{generation: generation, newGeneration: next, kind: intent.kind, err: err}
	}
}

func retryPreferenceCmd(ctx context.Context, manager sessionManager, generation uint64, receiptID string) tea.Cmd {
	return func() tea.Msg {
		return preferenceRetryMsg{generation: generation, err: manager.RetryPreferenceReceipt(ctx, receiptID)}
	}
}

type PickerChoice struct {
	Cancelled bool
	New       bool
	SessionID string
	Workspace bool
	Provider  bool
}

type standalonePicker struct {
	ctx        context.Context
	source     SessionPickerSource
	picker     *sessionPickerModel
	generation uint64
	choice     *PickerChoice
	width      int
	height     int
}

func RunSessionPicker(ctx context.Context, in io.Reader, out io.Writer, source SessionPickerSource, all bool) (PickerChoice, error) {
	if source == nil {
		return PickerChoice{}, agentsession.ErrInvalid
	}
	choice := &PickerChoice{}
	model := standalonePicker{ctx: ctx, source: source, picker: newSessionPickerModel(all), generation: source.Generation(), choice: choice, width: 80, height: 24}
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx))
	_, err := program.Run()
	return *choice, err
}

func (m standalonePicker) Init() tea.Cmd {
	request, epoch := m.picker.beginRequest()
	return listSessionsCmd(m.ctx, m.source, m.generation, epoch, request)
}

func (m standalonePicker) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case pickerListMsg:
		if msg.generation == m.generation && msg.epoch == m.picker.epoch {
			m.picker.setItems(msg.items, msg.err)
		}
		return m, nil
	case pickerOperationMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		m.picker.loading = false
		if msg.err != nil {
			m.picker.status = pickerErrorText(msg.err)
			m.picker.mode = pickerList
			return m, nil
		}
		m.picker.mode = pickerList
		request, epoch := m.picker.beginRequest()
		return m, listSessionsCmd(m.ctx, m.source, m.generation, epoch, request)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		intent := m.picker.handleKey(msg, m.source)
		switch intent.kind {
		case pickerIntentCancel:
			m.choice.Cancelled = true
			return m, tea.Quit
		case pickerIntentRefresh:
			request, epoch := m.picker.beginRequest()
			return m, listSessionsCmd(m.ctx, m.source, m.generation, epoch, request)
		case pickerIntentNew:
			m.choice.New = true
			return m, tea.Quit
		case pickerIntentSwitch:
			m.choice.SessionID = intent.plan.SessionID
			m.choice.Workspace = intent.confirmation.Workspace
			m.choice.Provider = intent.confirmation.Provider
			return m, tea.Quit
		case pickerIntentRename, pickerIntentDelete:
			m.picker.loading = true
			return m, func() tea.Msg {
				var err error
				if intent.kind == pickerIntentRename {
					_, err = m.source.RenameSession(m.ctx, intent.item.Summary.SessionID, intent.title, intent.item.Summary.RecordRevision)
				} else {
					err = m.source.DeleteSession(m.ctx, deleteTargetForSummary(intent.item.Summary))
				}
				return pickerOperationMsg{generation: m.generation, kind: intent.kind, err: err}
			}
		}
	}
	return m, nil
}

func (m standalonePicker) View() string {
	return m.picker.render(m.width, m.height)
}

var _ = time.Second
