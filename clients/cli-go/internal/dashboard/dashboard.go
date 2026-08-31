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
)

type LocalState string

const (
	LocalStateUnpaired   LocalState = "unpaired"
	LocalStatePaired     LocalState = "paired"
	LocalStateIncomplete LocalState = "incomplete"
)

// Snapshot contains only non-secret local state that is safe to render.
type Snapshot struct {
	ServerURL  string
	Timeout    string
	Color      string
	DeviceName string
	LocalState LocalState

	AgentProvider              string
	AgentBaseURL               string
	AgentModel                 string
	AgentContextWindow         int
	AgentContextCompaction     string
	AgentReasoningEffort       string
	AgentTimeout               string
	AgentMaxToolRounds         int
	AgentKeyConfigured         bool
	AgentKeyBackendUnavailable bool
}

type Runner struct {
	In       io.Reader
	Out      io.Writer
	modelKey string
}

func (r *Runner) Run(ctx context.Context, snapshot Snapshot) ([]string, bool, error) {
	r.modelKey = ""
	initial := newModel(snapshot)
	program := tea.NewProgram(initial, tea.WithAltScreen(), tea.WithInput(r.In), tea.WithOutput(r.Out), tea.WithContext(ctx))
	result, err := program.Run()
	if err != nil {
		return nil, false, err
	}
	final, ok := result.(model)
	if !ok {
		return nil, false, fmt.Errorf("unexpected dashboard model %T", result)
	}
	if final.modelKey != "" {
		r.modelKey = final.modelKey
		return nil, false, nil
	}
	return append([]string(nil), final.command...), final.quit, nil
}

// TakeModelKey transfers a form secret directly to the command layer without
// representing it as a CLI command or process argument.
func (r *Runner) TakeModelKey() (string, bool) {
	if r.modelKey == "" {
		return "", false
	}
	value := r.modelKey
	r.modelKey = ""
	return value, true
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
	screenAgentProvider
	screenAgentReasoning
	screenAgentConfig
	screenAgentKey
	screenAgentKeyDelete

	minimumWidth  = 28
	minimumHeight = 18
)

type menuItem struct {
	key         string
	title       string
	description string
	command     []string
	provider    string
	next        screen
}

type model struct {
	snapshot           Snapshot
	agentProviderDraft string
	screen             screen
	cursor             int
	width              int
	height             int
	inputs             []textinput.Model
	inputLabels        []string
	focus              int
	command            []string
	modelKey           string
	quit               bool
}

func newModel(snapshot Snapshot) model {
	if snapshot.LocalState == "" {
		snapshot.LocalState = LocalStateUnpaired
	}
	return model{snapshot: snapshot, screen: screenMain, width: 80, height: 24}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
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
	case screenGoal, screenImport, screenConnection, screenTimeout, screenAgentConfig, screenAgentKey:
		return true
	default:
		return false
	}
}

func (m model) updateMenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.screen == screenRePair || m.screen == screenAgentKeyDelete {
		switch key {
		case "y", "Y":
			if m.screen == screenRePair {
				m.command = []string{"logout"}
			} else {
				m.command = []string{"model", "key", "delete", "--confirmed"}
			}
			return m, tea.Quit
		case "f", "F":
			if m.screen == screenRePair {
				m.command = []string{"device", "forget-local"}
				return m, tea.Quit
			}
		case "n", "N", "esc", "backspace":
			if m.screen == screenRePair {
				m.screen = screenSettings
			} else {
				m.screen = screenAgentSettings
			}
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
	case screenAgentProvider, screenAgentReasoning:
		m.screen = screenAgentSettings
	}
	m.cursor = 0
}

func (m model) activate(item menuItem) (tea.Model, tea.Cmd) {
	if item.key == "b" {
		m.goBack()
		return m, nil
	}
	if item.provider != "" {
		m.agentProviderDraft = item.provider
		m.open(screenAgentConfig)
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
		agentConfig := m.snapshot
		if m.agentProviderDraft != "" {
			preset := config.DefaultAgentConfig(m.agentProviderDraft)
			agentConfig.AgentBaseURL = preset.BaseURL
			agentConfig.AgentModel = preset.Model
			agentConfig.AgentContextWindow = preset.ContextWindow
			agentConfig.AgentContextCompaction = preset.ContextCompaction
			agentConfig.AgentTimeout = preset.Timeout
			agentConfig.AgentMaxToolRounds = preset.MaxToolRounds
		}
		contextWindow := "32768"
		if agentConfig.AgentContextWindow > 0 {
			contextWindow = strconv.Itoa(agentConfig.AgentContextWindow)
		}
		toolRounds := "6"
		if agentConfig.AgentMaxToolRounds > 0 {
			toolRounds = strconv.Itoa(agentConfig.AgentMaxToolRounds)
		}
		m.inputs = []textinput.Model{
			newInput("https://api.openai.com/v1", agentConfig.AgentBaseURL),
			newInput("模型名称", agentConfig.AgentModel),
			newInput("32768", contextWindow),
			newInput("auto", display(agentConfig.AgentContextCompaction, config.DefaultAgentContextCompaction)),
			newInput("90s", display(agentConfig.AgentTimeout, "90s")),
			newInput("6", toolRounds),
		}
		m.inputLabels = []string{"OpenAI兼容Base URL", "模型名称", "上下文窗口", "上下文压缩（auto/recent-only/off）", "模型请求超时", "最大工具轮数"}
	case screenAgentKey:
		input := newInput("输入不会显示", "")
		input.EchoMode = textinput.EchoPassword
		input.EchoCharacter = '•'
		m.inputs = []textinput.Model{input}
		m.inputLabels = []string{"API Key（保存到系统钥匙串）"}
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
		if previous == screenAgentConfig {
			m.agentProviderDraft = ""
		}
		switch previous {
		case screenConnection, screenTimeout:
			m.screen = screenSettings
		case screenAgentConfig, screenAgentKey:
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
				m.command = []string{"model", "set"}
				provider := m.snapshot.AgentProvider
				reasoningEffort := display(m.snapshot.AgentReasoningEffort, config.DefaultAgentReasoningEffort)
				if m.agentProviderDraft != "" {
					provider = m.agentProviderDraft
					reasoningEffort = config.DefaultAgentConfig(provider).ReasoningEffort
				}
				if provider != "" {
					m.command = append(m.command, "--provider", provider)
				}
				m.command = append(m.command, "--base-url", values[0], "--model", values[1], "--context-window", values[2], "--context-compaction", values[3], "--reasoning-effort", reasoningEffort, "--timeout", values[4], "--max-tool-rounds", values[5])
				return m, tea.Quit
			}
		case screenAgentKey:
			if values[0] != "" {
				m.modelKey = values[0]
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
	case screenAgentProvider:
		return []menuItem{
			{key: "o", title: "OpenAI", description: "官方OpenAI API", provider: "openai"},
			{key: "d", title: "DeepSeek", description: "DeepSeek官方OpenAI兼容API", provider: "deepseek"},
			{key: "r", title: "OpenRouter", description: "OpenRouter多模型网关", provider: "openrouter"},
			{key: "l", title: "Ollama", description: "本机Ollama服务，可不配置API Key", provider: "ollama"},
			{key: "c", title: "自定义兼容服务", description: "任意OpenAI Chat Completions兼容端点", provider: "custom"},
			{key: "b", title: "返回", description: "返回AI助手设置", next: screenMain},
		}
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
			{key: "p", title: "选择提供商预设", description: "OpenAI、DeepSeek、OpenRouter、Ollama或自定义服务", next: screenAgentProvider},
			{key: "m", title: "编辑模型参数", description: "Base URL、模型、上下文窗口、压缩模式、超时和工具轮数", next: screenAgentConfig},
			{key: "r", title: "默认推理强度", description: "为新AI助手会话选择auto到max的默认档位", next: screenAgentReasoning},
			{key: "u", title: "更新API Key", description: "通过隐藏输入写入系统钥匙串", next: screenAgentKey},
		}
		if m.snapshot.AgentKeyConfigured {
			items = append(items, menuItem{key: "x", title: "删除API Key", description: "从系统钥匙串删除模型凭据", next: screenAgentKeyDelete})
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
				{key: "a", title: "AI助手与模型", description: "管理本地模型、API Key和Agent参数", next: screenAgentSettings},
				{key: "b", title: "返回", description: "返回主控制台", next: screenMain},
			}
		}
		pairTitle := "配置并配对设备"
		if m.snapshot.LocalState == LocalStatePaired {
			pairTitle = "重新配对或更换服务器"
		}
		items := []menuItem{
			{key: "a", title: "AI助手与模型", description: "管理本地模型、API Key和Agent参数", next: screenAgentSettings},
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
			{key: "a", title: "配置AI助手", description: "管理本地模型和系统钥匙串中的API Key", next: screenAgentSettings},
			{key: "q", title: "退出", description: "返回Shell"},
		}
	}
	if m.snapshot.LocalState == LocalStateUnpaired {
		return []menuItem{
			{key: "p", title: "配对设备", description: "连接到edu-agent服务", next: screenConnection},
			{key: "a", title: "配置AI助手", description: "先配置模型，也可在配对后使用", next: screenAgentSettings},
			{key: "s", title: "设置", description: "连接、客户端和模型设置", next: screenSettings},
			{key: "q", title: "退出", description: "返回Shell"},
		}
	}
	agentItem := menuItem{key: "a", title: "AI学习助手", description: "通过Agent Loop结合知识库、进度和长期偏好辅助学习", command: []string{"agent"}}
	if strings.TrimSpace(m.snapshot.AgentProvider) == "" {
		agentItem = menuItem{key: "a", title: "配置AI助手", description: "先选择模型提供商，再确认参数并保存", next: screenAgentProvider}
	} else {
		agentConfig := config.AgentConfig{Provider: m.snapshot.AgentProvider, BaseURL: m.snapshot.AgentBaseURL}
		if !agentConfig.APIKeyOptional() && !m.snapshot.AgentKeyConfigured {
			if m.snapshot.AgentKeyBackendUnavailable {
				agentItem = menuItem{key: "a", title: "修复AI助手配置", description: "系统钥匙串不可用；请启用系统凭据服务或改用本地模型", next: screenAgentSettings}
			} else {
				agentItem = menuItem{key: "a", title: "补全AI助手配置", description: "当前模型需要API Key，保存后才能启动", next: screenAgentKey}
			}
		}
	}
	return []menuItem{
		agentItem,
		{key: "l", title: "继续结构化学习", description: "恢复服务端教学状态机中的当前会话", command: []string{"learn"}},
		{key: "i", title: "导入知识", description: "导入Markdown文件或目录", next: screenImport},
		{key: "g", title: "设置学习目标", description: "创建或切换当前学习目标", next: screenGoal},
		{key: "v", title: "查看学习进度", description: "读取当前学习快照", command: []string{"progress"}},
		{key: "r", title: "查看学习路线", description: "显示当前路线", command: []string{"route"}},
		{key: "e", title: "查看学习证据", description: "检查已接受的学习证据", command: []string{"evidence"}},
		{key: "w", title: "查看复习安排", description: "显示待复习内容", command: []string{"reviews"}},
		{key: "d", title: "设备与服务状态", description: "检查设备绑定和服务就绪状态", command: []string{"device", "status"}},
		{key: "s", title: "设置", description: "连接、客户端和AI模型设置", next: screenSettings},
		{key: "q", title: "退出", description: "返回Shell"},
	}
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	labelStyle    = lipgloss.NewStyle().Bold(true)
)

func (m model) View() string {
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
	body.WriteString("\n\n")

	switch m.screen {
	case screenGoal:
		body.WriteString(labelStyle.Render("设置学习目标"))
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
	case screenAgentKey:
		body.WriteString(labelStyle.Render("更新模型API Key"))
		body.WriteString("\n" + mutedStyle.Render("密钥只写入操作系统钥匙串，不进入config.json或日志。"))
		m.renderInputs(&body)
		body.WriteString("\n" + mutedStyle.Render("Enter保存  Esc取消"))
	case screenRePair:
		body.WriteString(labelStyle.Render("重新配对设备"))
		body.WriteString("\n\n[Y] 安全注销旧设备\n需要旧服务器在线，会先撤销远端设备。\n\n[F] 仅清除本地配对\n旧服务器不可用时用于恢复；远端设备可能仍有效。\n\n[N] 取消")
	case screenAgentKeyDelete:
		body.WriteString(labelStyle.Render("删除模型API Key"))
		body.WriteString("\n\n这会从操作系统钥匙串中删除当前模型凭据。\n普通模型设置不会被删除。\n\n")
		body.WriteString(selectedStyle.Render("Y 确认删除"))
		body.WriteString("   " + mutedStyle.Render("N 取消"))
	default:
		m.renderMenuHeader(&body)
		for index, item := range m.items() {
			marker := "  "
			style := lipgloss.NewStyle()
			if index == m.cursor {
				marker = "> "
				style = selectedStyle
			}
			body.WriteString(style.Render(fmt.Sprintf("%s[%s] %s", marker, item.key, item.title)))
			body.WriteString("\n")
			if m.width >= 52 && m.height >= 30 {
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
		body.WriteString(fmt.Sprintf("提供商：%s\n模型：%s\nBase URL：%s\n上下文窗口：%s\n上下文压缩：%s\n默认推理强度：%s\n模型超时：%s\n工具轮数：%s\nAPI Key：%s\n\n",
			providerDisplay(m.snapshot.AgentProvider), display(m.snapshot.AgentModel, "未配置"), display(m.snapshot.AgentBaseURL, "未配置"),
			positiveNumber(m.snapshot.AgentContextWindow), display(m.snapshot.AgentContextCompaction, config.DefaultAgentContextCompaction), display(m.snapshot.AgentReasoningEffort, config.DefaultAgentReasoningEffort), display(m.snapshot.AgentTimeout, "90s"), positiveNumber(m.snapshot.AgentMaxToolRounds), keyStatus(m.snapshot.AgentKeyConfigured, m.snapshot.AgentKeyBackendUnavailable)))
	case screenAgentProvider:
		body.WriteString(labelStyle.Render("选择模型提供商"))
		body.WriteString("\n")
		body.WriteString(mutedStyle.Render("选择后会进入参数表单；只有在表单中按Enter保存，配置才会生效。"))
		body.WriteString("\n\n")
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

func providerDisplay(value string) string {
	switch value {
	case "openai":
		return "OpenAI"
	case "deepseek":
		return "DeepSeek"
	case "openrouter":
		return "OpenRouter"
	case "ollama":
		return "Ollama"
	case "custom":
		return "自定义OpenAI兼容服务"
	default:
		return display(value, "未配置")
	}
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

func keyStatus(configured, backendUnavailable bool) string {
	if configured {
		return "已存入系统钥匙串"
	}
	if backendUnavailable {
		return "系统钥匙串不可用（请启用系统凭据服务或改用本地模型）"
	}
	return "未配置"
}
