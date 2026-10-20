package dashboard

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/terminal"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

type LocalState string

const (
	LocalStateUnpaired   LocalState = "unpaired"
	LocalStatePaired     LocalState = "paired"
	LocalStateIncomplete LocalState = "incomplete"
)

// Snapshot contains only non-secret local state that is safe to render.
type Snapshot struct {
	LearningSpaceName      string
	ServerURL              string
	Timeout                string
	Color                  string
	DeviceName             string
	LocalState             LocalState
	Models                 []modelgateway.Connection
	SelectedTarget         string
	PrivacyConfirmed       bool
	GatewayError           string
	AgentModel             string
	AgentContextWindow     int
	AgentMaxTokens         int
	AgentContextCompaction string
	AgentReasoningEffort   string
	AgentSessionHistory    string
}

type Runner struct {
	In              io.Reader
	Out             io.Writer
	Workbench       workbench.Service
	LearningSpaceID string
	workspace       *workbench.Model
	workspaceServer string
}

func (r *Runner) Run(ctx context.Context, snapshot Snapshot) ([]string, bool, error) {
	initial := newModel(snapshot)
	if r.Workbench != nil {
		if r.workspaceServer != snapshot.ServerURL {
			if r.workspace != nil {
				r.workspace.Suspend()
			}
			r.workspace = nil
			r.workspaceServer = snapshot.ServerURL
		}
		if r.workspace == nil {
			space := r.LearningSpaceID
			if space == "" {
				space = "00000000-0000-4000-8000-000000000001"
			}
			r.workspace = workbench.New(ctx, r.Workbench, space)
		}
		initial.workspace, initial.ctx = r.workspace, ctx
		defer r.workspace.Suspend()
	}
	program := tea.NewProgram(initial, tea.WithAltScreen(), tea.WithInput(r.In), tea.WithOutput(r.Out), tea.WithContext(ctx))
	result, err := program.Run()
	if err != nil {
		return nil, false, err
	}
	final, ok := result.(model)
	if !ok {
		return nil, false, fmt.Errorf("unexpected dashboard model %T", result)
	}
	return append([]string(nil), final.command...), final.quit, nil
}

func (r *Runner) SelectedLearningSpace() (string, string) {
	if r.workspace == nil {
		return "", ""
	}
	return r.workspace.Selection()
}

type screen int

const (
	screenMain screen = iota
	screenSettings
	screenGoal
	screenImport
	screenConnection
	screenTimeout
	screenColor
	screenRePair
	screenAgentSettings
	screenGatewayModels
	screenAgentReasoning
	screenAgentPrivacy
	screenAgentConfig

	minimumWidth  = 28
	minimumHeight = 18
)

type menuItem struct {
	key         string
	title       string
	description string
	command     []string
	next        screen
}

type model struct {
	ctx             context.Context
	workspace       *workbench.Model
	workspaceActive bool
	snapshot        Snapshot
	screen          screen
	cursor          int
	width           int
	height          int
	inputs          []textinput.Model
	inputLabels     []string
	focus           int
	command         []string
	quit            bool
}

func newModel(snapshot Snapshot) model {
	if snapshot.LocalState == "" {
		snapshot.LocalState = LocalStateUnpaired
	}
	return model{snapshot: snapshot, screen: screenMain, width: 80, height: 24}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if selection, ok := message.(workbench.AgentMsg); ok {
		m.command = []string{"agent", "--space", selection.Space}
		if selection.Goal != "" {
			m.command = append(m.command, "--goal", selection.Goal)
		}
		if selection.Session != "" {
			m.command = append(m.command, "--session", selection.Session)
		}
		return m, tea.Quit
	}
	if _, ok := message.(workbench.ExitMsg); ok {
		m.workspaceActive = false
		_, m.snapshot.LearningSpaceName = m.workspace.Selection()
		m.snapshot.LearningSpaceName = terminal.EscapeText(m.snapshot.LearningSpaceName)
		return m, nil
	}
	if m.workspaceActive {
		if size, ok := message.(tea.WindowSizeMsg); ok {
			m.width, m.height = size.Width, size.Height
		}
		if key, ok := message.(tea.KeyMsg); ok && key.String() == "ctrl+c" {
			m.quit = true
		}
		_, cmd := m.workspace.Update(message)
		return m, cmd
	}
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeInputs()
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.quit = true
			return m, tea.Quit
		}
		if m.terminalTooSmall() {
			if msg.String() == "q" {
				m.quit = true
				return m, tea.Quit
			}
			return m, nil
		}
		if m.isForm() {
			return m.updateForm(msg)
		}
		return m.updateMenu(msg)
	}
	return m, nil
}

func (m model) isForm() bool {
	switch m.screen {
	case screenGoal, screenImport, screenConnection, screenTimeout, screenAgentConfig:
		return true
	default:
		return false
	}
}

func (m model) updateMenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.screen == screenAgentPrivacy {
		pages := sessionPrivacyPages()
		switch key {
		case "left", "up", "k", "h":
			m.cursor = (m.cursor - 1 + len(pages)) % len(pages)
		case "right", "down", "j", "l", "enter":
			m.cursor = (m.cursor + 1) % len(pages)
		case "esc", "backspace":
			m.goBack()
		}
		return m, nil
	}
	if m.screen == screenRePair {
		switch key {
		case "y", "Y":
			m.command = []string{"logout"}
			return m, tea.Quit
		case "f", "F":
			m.command = []string{"device", "forget-local"}
			return m, tea.Quit
		case "n", "N", "esc", "backspace":
			m.screen = screenSettings
			m.cursor = 0
		}
		return m, nil
	}
	items := m.items()
	if len(items) == 0 {
		return m, nil
	}
	switch key {
	case "up", "k":
		m.cursor = (m.cursor - 1 + len(items)) % len(items)
	case "down", "j":
		m.cursor = (m.cursor + 1) % len(items)
	case "esc", "backspace", "h":
		m.goBack()
	case "enter":
		return m.activate(items[m.cursor])
	default:
		for _, item := range items {
			if strings.EqualFold(key, item.key) {
				return m.activate(item)
			}
		}
	}
	return m, nil
}

func (m *model) goBack() {
	switch m.screen {
	case screenSettings:
		m.screen = screenMain
	case screenColor:
		m.screen = screenSettings
	case screenAgentSettings:
		m.screen = screenSettings
	case screenGatewayModels, screenAgentReasoning, screenAgentPrivacy:
		m.screen = screenAgentSettings
	}
	m.cursor = 0
}

func (m model) activate(item menuItem) (tea.Model, tea.Cmd) {
	if m.workspace != nil && m.screen == screenMain {
		page := map[string]string{"z": "spaces", "l": "learn", "g": "new-goal", "o": "goals", "i": "import", "t": "import", "v": "overview", "r": "learn", "w": "reviews", "b": "overview"}[item.key]
		if page != "" {
			m.workspaceActive = true
			_, _ = m.workspace.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			return m, m.workspace.Open(m.ctx, page)
		}
	}
	if item.key == "b" {
		m.goBack()
		return m, nil
	}
	if item.next != screenMain {
		if item.next == screenConnection && m.snapshot.LocalState == LocalStatePaired {
			m.screen = screenRePair
			return m, nil
		}
		m.open(item.next)
		return m, nil
	}
	if len(item.command) == 0 {
		m.quit = true
	} else {
		m.command = append([]string(nil), item.command...)
	}
	return m, tea.Quit
}

func (m *model) open(next screen) {
	m.screen, m.cursor, m.focus, m.inputs, m.inputLabels = next, 0, 0, nil, nil
	switch next {
	case screenGoal:
		m.inputs = []textinput.Model{newInput("例如：理解图论并能解决最短路径问题", "")}
		m.inputLabels = []string{"学习目标"}
	case screenImport:
		m.inputs = []textinput.Model{newInput("例如：notes/course", "")}
		m.inputLabels = []string{"Markdown文件或目录"}
	case screenConnection:
		server := display(m.snapshot.ServerURL, "http://127.0.0.1:8080")
		timeout := display(m.snapshot.Timeout, "30s")
		m.inputs = []textinput.Model{newInput("http://127.0.0.1:8080", server), newInput("30s", timeout)}
		m.inputLabels = []string{"服务器地址", "请求超时"}
	case screenTimeout:
		m.inputs = []textinput.Model{newInput("30s", display(m.snapshot.Timeout, "30s"))}
		m.inputLabels = []string{"请求超时"}
	case screenAgentConfig:
		m.inputs = []textinput.Model{newInput("auto", display(m.snapshot.AgentContextCompaction, config.DefaultAgentContextCompaction))}
		m.inputLabels = []string{"上下文压缩（auto / recent-only / off）"}

	}
	if len(m.inputs) > 0 {
		m.resizeInputs()
		m.inputs[0].Focus()
	}
}

func (m *model) resizeInputs() {
	width := m.width - 8
	if width < 8 {
		width = 8
	}
	for index := range m.inputs {
		m.inputs[index].Width = width
	}
}

func newInput(placeholder, value string) textinput.Model {
	input := textinput.New()
	input.Placeholder = placeholder
	input.SetValue(value)
	input.CharLimit = 4096
	input.Prompt = "> "
	return input
}

func (m model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		previous := m.screen
		m.screen, m.cursor, m.inputs, m.inputLabels = screenMain, 0, nil, nil
		switch previous {
		case screenConnection, screenTimeout:
			m.screen = screenSettings
		case screenAgentConfig:
			m.screen = screenAgentSettings
		}
		return m, nil
	case "tab", "shift+tab", "up", "down":
		if len(m.inputs) > 1 {
			m.inputs[m.focus].Blur()
			if msg.String() == "shift+tab" || msg.String() == "up" {
				m.focus = (m.focus - 1 + len(m.inputs)) % len(m.inputs)
			} else {
				m.focus = (m.focus + 1) % len(m.inputs)
			}
			return m, m.inputs[m.focus].Focus()
		}
	case "enter":
		values := make([]string, len(m.inputs))
		for index := range m.inputs {
			values[index] = strings.TrimSpace(m.inputs[index].Value())
		}
		switch m.screen {
		case screenGoal:
			if values[0] != "" {
				m.command = append([]string{"goal", "set", "--"}, values[0])
				return m, tea.Quit
			}
		case screenImport:
			if values[0] != "" {
				m.command = []string{"knowledge", "import", "--", values[0]}
				return m, tea.Quit
			}
		case screenConnection:
			if values[0] != "" && values[1] != "" {
				m.command = []string{"pair", "--server", values[0], "--timeout", values[1]}
				return m, tea.Quit
			}
		case screenTimeout:
			if values[0] != "" {
				m.command = []string{"config", "set", "--timeout", values[0]}
				return m, tea.Quit
			}
		case screenAgentConfig:
			if allPresent(values) {
				m.command = []string{"model", "set", "--context-compaction", values[0]}
				return m, tea.Quit
			}

		}
	}
	var command tea.Cmd
	m.inputs[m.focus], command = m.inputs[m.focus].Update(msg)
	return m, command
}

func allPresent(values []string) bool {
	for _, value := range values {
		if value == "" {
			return false
		}
	}
	return true
}

func (m model) items() []menuItem {
	switch m.screen {
	case screenColor:
		return []menuItem{
			{key: "n", title: "关闭颜色", description: "所有命令输出都不使用颜色", command: []string{"config", "set", "--color", "never"}},
			{key: "a", title: "自动检测", description: "终端支持时才使用颜色", command: []string{"config", "set", "--color", "auto"}},
			{key: "w", title: "始终启用", description: "所有命令输出都使用颜色", command: []string{"config", "set", "--color", "always"}},
			{key: "b", title: "返回", description: "返回设置", next: screenMain},
		}
	case screenGatewayModels:
		items := []menuItem{}
		for i, connection := range m.snapshot.Models {
			items = append(items, menuItem{key: strconv.Itoa(i + 1), title: terminal.EscapeText(connection.DisplayName), description: fmt.Sprintf("%s · 上下文 %d / 输出 %d", terminal.EscapeText(connection.ModelName), connection.ContextTokens, connection.OutputTokens), command: []string{"model", "select", connection.Target}})
		}
		items = append(items, menuItem{key: "r", title: "读取模型列表与隐私告知", description: "从配对服务端刷新可选连接", command: []string{"model", "list"}}, menuItem{key: "b", title: "返回", next: screenMain})
		return items
	case screenAgentReasoning:
		return []menuItem{
			{key: "a", title: "自动（推荐）", description: "由兼容端点决定；请求中省略推理强度字段", command: []string{"model", "set", "--reasoning-effort", "auto"}},
			{key: "n", title: "关闭", description: "不使用推理模式", command: []string{"model", "set", "--reasoning-effort", "none"}},
			{key: "m", title: "最小", description: "使用最少推理预算", command: []string{"model", "set", "--reasoning-effort", "minimal"}},
			{key: "l", title: "低", description: "较低推理强度", command: []string{"model", "set", "--reasoning-effort", "low"}},
			{key: "d", title: "中", description: "平衡速度与推理深度", command: []string{"model", "set", "--reasoning-effort", "medium"}},
			{key: "g", title: "高", description: "提高推理深度", command: []string{"model", "set", "--reasoning-effort", "high"}},
			{key: "x", title: "超高", description: "使用xhigh兼容档位", command: []string{"model", "set", "--reasoning-effort", "xhigh"}},
			{key: "z", title: "最大", description: "使用端点支持的最大推理强度", command: []string{"model", "set", "--reasoning-effort", "max"}},
			{key: "b", title: "返回", description: "返回AI助手设置", next: screenMain},
		}
	case screenAgentSettings:
		items := []menuItem{
			{key: "p", title: "选择服务端模型", description: "仅限网页启用的 teaching / mentor", next: screenGatewayModels},
			{key: "m", title: "上下文压缩偏好", description: "容量由服务端下发", next: screenAgentConfig},
			{key: "r", title: "默认推理强度", description: "为新AI助手会话选择auto到max的默认档位", next: screenAgentReasoning},
			{key: "v", title: "Session历史与隐私", description: "查看保存、服务端转发、恢复和清除边界", next: screenAgentPrivacy},
		}
		if display(m.snapshot.AgentSessionHistory, config.DefaultAgentSessionHistory) == "off" {
			items = append(items, menuItem{key: "s", title: "开启会话历史", description: "后续新Session默认本机加密保存；自动标题会发送有界对话片段", command: []string{"model", "set", "--session-history", "auto"}})
		} else {
			items = append(items, menuItem{key: "s", title: "关闭会话历史", description: "后续新Session仅在当前进程有效；已有历史仍可恢复和删除", command: []string{"model", "set", "--session-history", "off"}})
		}
		items = append(items,
			menuItem{key: "t", title: "测试模型连接", description: "发送最小请求验证当前模型配置", command: []string{"model", "test"}},
			menuItem{key: "b", title: "返回", description: "返回设置", next: screenMain},
		)
		return items
	case screenSettings:
		if m.snapshot.LocalState == LocalStateIncomplete {
			return []menuItem{
				{key: "f", title: "修复本地配对状态", description: "确认后删除不完整的本地状态", command: []string{"device", "forget-local"}},
				{key: "a", title: "AI助手与模型", description: "选择服务端模型与客户端偏好", next: screenAgentSettings},
				{key: "b", title: "返回", description: "返回主控制台", next: screenMain},
			}
		}
		pairTitle := "配置并配对设备"
		if m.snapshot.LocalState == LocalStatePaired {
			pairTitle = "重新配对或更换服务器"
		}
		items := []menuItem{
			{key: "a", title: "AI助手与模型", description: "选择服务端模型与客户端偏好", next: screenAgentSettings},
			{key: "t", title: "客户端请求超时", description: "调整访问edu-agent服务的HTTP超时", next: screenTimeout},
			{key: "c", title: "命令输出颜色", description: "选择关闭、自动或始终启用", next: screenColor},
		}
		items = append(items, menuItem{key: "p", title: pairTitle, description: "使用现有的一次性安全配对流程", next: screenConnection})
		if m.snapshot.LocalState == LocalStatePaired {
			items = append(items, menuItem{key: "d", title: "设备与服务状态", description: "检查连接、设备和服务就绪状态", command: []string{"device", "status"}})
		}
		items = append(items, menuItem{key: "b", title: "返回", description: "返回主控制台", next: screenMain})
		return items
	}
	if m.snapshot.LocalState == LocalStateIncomplete {
		return []menuItem{
			{key: "p", title: "修复本地配对状态", description: "确认后删除不完整的本地状态", command: []string{"device", "forget-local"}},
			{key: "a", title: "配置AI助手", description: "配对后选择服务端模型", next: screenAgentSettings},
			{key: "q", title: "退出", description: "返回Shell"},
		}
	}
	if m.snapshot.LocalState == LocalStateUnpaired {
		return []menuItem{
			{key: "p", title: "配对设备", description: "连接到edu-agent服务", next: screenConnection},
			{key: "a", title: "配置AI助手", description: "先配对，再选择服务端模型并确认隐私告知", next: screenAgentSettings},
			{key: "s", title: "设置", description: "连接、客户端和模型设置", next: screenSettings},
			{key: "q", title: "退出", description: "返回Shell"},
		}
	}
	agentItem := menuItem{key: "a", title: "AI学习助手", description: "通过Agent Loop结合知识库、进度和长期偏好辅助学习", command: []string{"agent"}}
	if m.snapshot.SelectedTarget == "" || !m.snapshot.PrivacyConfirmed {
		agentItem = menuItem{key: "a", title: "选择模型与隐私告知", description: "配对 → 选择服务端模型 → 确认告知", next: screenGatewayModels}
	}
	items := []menuItem{agentItem}
	if m.workspace != nil {
		items = append([]menuItem{{key: "b", title: "学习工作台", description: "持续页面、学习区切换、目标、教学和复习"}}, items...)
		items = append(items, menuItem{key: "t", title: "资料导入与身份审批", description: "在持续工作台中完成预览、审批与确认", next: screenImport})
	}
	if len(agentItem.command) > 0 {
		items = append(items, menuItem{key: "y", title: "恢复AI历史会话", description: "打开当前工作区已加密保存的Agent Session选择器", command: []string{"agent", "resume"}})
	}
	items = append(items,
		menuItem{key: "z", title: "学习区", description: "列表、搜索、详情与当前客户端选择", command: []string{"space", "browse"}},
		menuItem{key: "l", title: "继续结构化学习", description: "恢复服务端教学状态机中的当前会话", command: []string{"learn"}},
		menuItem{key: "i", title: "导入知识", description: "选择资料、预览变更并确认导入", command: []string{"knowledge", "import", "wizard"}},
		menuItem{key: "g", title: "保存学习目标", description: "保存一句话目标，独立于当前教学会话", next: screenGoal},
		menuItem{key: "o", title: "目标管理", description: "列表、草稿、资料选择与独立生命周期", command: []string{"goal", "browse"}},
		menuItem{key: "v", title: "查看学习进度", description: "读取当前学习快照", command: []string{"progress"}},
		menuItem{key: "r", title: "查看学习路线", description: "显示当前路线", command: []string{"route"}},
		menuItem{key: "e", title: "查看学习证据", description: "检查已接受的学习证据", command: []string{"evidence"}},
		menuItem{key: "w", title: "查看复习安排", description: "显示待复习内容", command: []string{"reviews"}},
		menuItem{key: "d", title: "设备与服务状态", description: "检查设备绑定和服务就绪状态", command: []string{"device", "status"}},
		menuItem{key: "s", title: "设置", description: "连接、客户端和AI模型设置", next: screenSettings},
		menuItem{key: "q", title: "退出", description: "返回Shell"},
	)
	return items
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	labelStyle    = lipgloss.NewStyle().Bold(true)
)

func (m model) View() string {
	if m.workspaceActive {
		return m.workspace.View()
	}
	if m.terminalTooSmall() {
		return smallTerminalView(m.width, m.height)
	}
	width := m.width - 4
	if width < 12 {
		width = 12
	}
	var body strings.Builder
	body.WriteString(titleStyle.Render("edu-agent 学习工作台"))
	body.WriteString("\n")
	body.WriteString(mutedStyle.Render("中文优先 · 直接学习与AI辅助学习"))
	if m.snapshot.LearningSpaceName != "" {
		body.WriteString("\n" + mutedStyle.Render("当前学习区："+m.snapshot.LearningSpaceName))
	}
	body.WriteString("\n\n")

	switch m.screen {
	case screenGoal:
		body.WriteString(labelStyle.Render("保存学习目标"))
		m.renderInputs(&body)
		body.WriteString("\n" + mutedStyle.Render("Enter提交  Esc取消"))
	case screenImport:
		body.WriteString(labelStyle.Render("导入知识"))
		m.renderInputs(&body)
		body.WriteString("\n" + mutedStyle.Render("Enter导入  Esc取消"))
	case screenConnection:
		body.WriteString(labelStyle.Render("连接并配对"))
		m.renderInputs(&body)
		body.WriteString("\n" + mutedStyle.Render("Tab切换字段  Enter继续配对  Esc取消"))
	case screenTimeout:
		body.WriteString(labelStyle.Render("客户端请求超时"))
		m.renderInputs(&body)
		body.WriteString("\n" + mutedStyle.Render("Enter保存  Esc取消"))
	case screenAgentConfig:
		body.WriteString(labelStyle.Render("AI模型参数"))
		m.renderInputs(&body)
		body.WriteString("\n" + mutedStyle.Render("Tab切换字段  Enter保存  Esc取消"))
	case screenAgentPrivacy:
		m.renderSessionPrivacy(&body)
	case screenRePair:
		body.WriteString(labelStyle.Render("重新配对设备"))
		body.WriteString("\n\n[Y] 安全注销旧设备\n需要旧服务器在线，会先撤销远端设备。\n\n[F] 仅清除本地配对\n旧服务器不可用时用于恢复；远端设备可能仍有效。\n\n[N] 取消")
	default:
		m.renderMenuHeader(&body)
		items := m.items()
		start, end := 0, len(items)
		descriptions := m.width >= 52 && m.height >= 30
		if m.workspace != nil && m.screen == screenMain {
			available := max(1, m.height-lipgloss.Height(body.String())-5)
			descriptions = descriptions && len(items)*2 <= available
			if len(items) > available {
				start = max(0, m.cursor-available+1)
				end = min(len(items), start+available)
			}
		}
		for index := start; index < end; index++ {
			item := items[index]
			marker := "  "
			style := lipgloss.NewStyle()
			if index == m.cursor {
				marker = "> "
				style = selectedStyle
			}
			body.WriteString(style.MaxWidth(width - 4).MaxHeight(1).Render(fmt.Sprintf("%s[%s] %s", marker, item.key, item.title)))
			body.WriteString("\n")
			if descriptions {
				body.WriteString("    " + mutedStyle.Render(item.description) + "\n")
			}
		}
		body.WriteString("\n" + mutedStyle.Render("方向键或j/k移动  Enter选择  字母键快捷打开"))
	}
	return lipgloss.NewStyle().Width(width).MaxWidth(width).Padding(1, 2).Render(body.String())
}

func (m model) renderInputs(body *strings.Builder) {
	for index := range m.inputs {
		body.WriteString("\n\n" + mutedStyle.Render(m.inputLabels[index]))
		body.WriteString("\n" + m.inputs[index].View())
	}
}

func (m model) renderMenuHeader(body *strings.Builder) {
	switch m.screen {
	case screenSettings:
		body.WriteString(labelStyle.Render("设置"))
		body.WriteString("\n")
		body.WriteString(fmt.Sprintf("服务器：%s\n客户端超时：%s\n输出颜色：%s\n本地状态：%s",
			display(m.snapshot.ServerURL, "未配置"), display(m.snapshot.Timeout, "30s"), colorName(m.snapshot.Color), localStateName(m.snapshot.LocalState)))
		if m.snapshot.DeviceName != "" {
			body.WriteString("\n设备：" + m.snapshot.DeviceName)
		}
		body.WriteString("\n\n")
	case screenAgentSettings:
		body.WriteString(labelStyle.Render("AI助手与模型"))
		body.WriteString("\n")
		body.WriteString(fmt.Sprintf("所选连接：%s\n模型：%s\n上下文窗口：%s\n最大输出 tokens：%s\n上下文压缩：%s\n默认推理强度：%s\n会话历史：%s\n\n", display(m.snapshot.SelectedTarget, "未选择"), display(m.snapshot.AgentModel, "请刷新服务端配置"), positiveNumber(m.snapshot.AgentContextWindow), positiveNumber(m.snapshot.AgentMaxTokens), display(m.snapshot.AgentContextCompaction, config.DefaultAgentContextCompaction), display(m.snapshot.AgentReasoningEffort, config.DefaultAgentReasoningEffort), sessionHistoryName(m.snapshot.AgentSessionHistory)))
	case screenGatewayModels:
		body.WriteString(labelStyle.Render("选择服务端模型"))
		body.WriteString("\n仅显示网页启用的教学 / AI 助手连接。\n")
		if m.snapshot.GatewayError != "" {
			body.WriteString(terminal.EscapeText(m.snapshot.GatewayError) + "\n")
		}
		if !m.snapshot.PrivacyConfirmed {
			body.WriteString("首次读取前将显示隐私告知并等待确认。\n")
		}
	case screenAgentReasoning:
		body.WriteString(labelStyle.Render("默认推理强度"))
		body.WriteString("\n")
		body.WriteString(fmt.Sprintf("当前：%s\n", display(m.snapshot.AgentReasoningEffort, config.DefaultAgentReasoningEffort)))
		body.WriteString(mutedStyle.Render("只影响以后新建的AI助手会话；会话内临时覆盖不会改写这里。"))
		body.WriteString("\n\n")
	case screenColor:
		body.WriteString(labelStyle.Render("命令输出颜色"))
		body.WriteString("\n\n")
	}
}

type privacyPage struct {
	title string
	lines []string
}

func sessionPrivacyPages() []privacyPage {
	return []privacyPage{
		{
			title: "保存与保留",
			lines: []string{
				"• 默认本机加密保存对话与有界",
				"  工具/context evidence。",
				"• 无时间 TTL；需手动 delete/clear。",
				"• 硬上限不淘汰旧 Session；",
				"  最近内容可能保存失败并明确提示。",
			},
		},
		{
			title: "发送与恢复",
			lines: []string{
				"• 自动标题会把有界近期片段发送",
				"  到所选服务端模型。",
				"• 恢复后下一请求会把历史发送到",
				"  所选服务端模型；目的地变化先确认。",
				"• workspace 正文可能在加密历史中；",
				"  恢复不代表当前磁盘内容。",
			},
		},
		{
			title: "降级与清除",
			lines: []string{
				"• key backend 不可用绝不落明文；",
				"  Session 仅当前进程有效。",
				"• clear 只清本地 Agent Session store。",
				"• 不清 server/Nocturne/terminal、",
				"  Shell/模型服务/OS 备份。",
				"• YOLO、旧文件授权和 pending 交互",
				"  均不恢复。",
			},
		},
	}
}

func (m model) renderSessionPrivacy(body *strings.Builder) {
	pages := sessionPrivacyPages()
	page := m.cursor
	if page < 0 || page >= len(pages) {
		page = 0
	}
	body.WriteString(labelStyle.Render("Session 历史与隐私"))
	body.WriteString("\n" + mutedStyle.Render(pages[page].title))
	body.WriteString("\n\n" + strings.Join(pages[page].lines, "\n"))
	body.WriteString(fmt.Sprintf("\n\n%s", mutedStyle.Render(fmt.Sprintf("←/→ 或 j/k 翻页  Esc返回  %d/%d", page+1, len(pages)))))
}

func (m model) terminalTooSmall() bool {
	return m.width < minimumWidth || m.height < minimumHeight
}

func smallTerminalView(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := []string{"edu-agent", "", "终端尺寸过小", "调整窗口后继续"}
	if len(lines) > height {
		lines = lines[:height]
	}
	for index, line := range lines {
		runes := []rune(line)
		if len(runes) > width {
			lines[index] = string(runes[:width])
		}
	}
	return strings.Join(lines, "\n")
}

func display(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func positiveNumber(value int) string {
	if value < 1 {
		return "未配置"
	}
	return strconv.Itoa(value)
}

func toolRoundLimit(value int) string {
	if value <= 0 {
		return "不限制"
	}
	return strconv.Itoa(value)
}

func localStateName(value LocalState) string {
	switch value {
	case LocalStatePaired:
		return "已配对"
	case LocalStateIncomplete:
		return "需要修复"
	default:
		return "未配对"
	}
}

func colorName(value string) string {
	switch value {
	case "auto":
		return "自动"
	case "always":
		return "始终启用"
	default:
		return "关闭"
	}
}

func sessionHistoryName(value string) string {
	if display(value, config.DefaultAgentSessionHistory) == "off" {
		return "关闭（后续新Session不保存）"
	}
	return "自动（本机加密保存）"
}
