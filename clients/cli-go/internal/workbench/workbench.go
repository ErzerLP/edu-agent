// Package workbench 提供不拥有业务状态的持续页面容器。
package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/terminal"
)

type Request struct {
	Space, Page, Action, Text, Search, Cursor string
	Session                                   string
	Version                                   int64
	Values                                    map[string]string
	Resource, Scope                           string
	Operation, Entity                         string
	Basis                                     string
}

type Field struct {
	ID, Label, Value string
	Choices          []string
}

type Action struct {
	ID, Label, Input, Confirmation string
	DraftKey                       string
	Fields                         []Field
}

type Entry struct{ ID, Label string }

// Page 是应用层的展示结果，容器不从终端输出推断任何业务状态。
type Page struct {
	Title, Content, SpaceName, Goal, Stage string
	Session                                string
	Version                                int64
	Entries                                []Entry
	Actions                                []Action
	NextCursor                             string
	ContextKnown                           bool
	Redirect, Scope                        string
	Basis                                  string
	Searchable                             bool
	Leaf                                   Leaf
}

// Leaf 由业务页拥有内容与规则；外壳只管理嵌入、返回及请求生命周期。
type Leaf interface {
	tea.Model
	Suspend()
	Resume(context.Context) tea.Cmd
	Destination(context.Context) func() (string, error)
}
type leafMessage struct {
	generation uint64
	value      tea.Msg
}

type Service interface {
	Load(context.Context, Request) (Page, error)
}

type ExitMsg struct{}
type Failure struct {
	Message     string
	ClearDrafts bool
}

func (e *Failure) Error() string { return e.Message }

type response struct {
	generation uint64
	page       Page
	err        error
}
type localState struct {
	drafts                        map[string]string
	cursor, offset                int
	search, pageCursor            string
	pendingKey, operation, entity string
}

type Model struct {
	service               Service
	ctx                   context.Context
	cancel                context.CancelFunc
	generation            uint64
	space, page           string
	spaceName             string
	goal, stage           string
	data                  Page
	states                map[string]*localState
	width, height, cursor int
	view                  viewport.Model
	input                 textarea.Model
	editing               bool
	action                Action
	confirm               bool
	loading               bool
	status                string
	pendingAction         string
	field                 int
	history               []string
	sessions, scopes      map[string]string
	leaves                map[string]Leaf
	leaf                  Leaf
}

func New(ctx context.Context, service Service, space string) *Model {
	in := textarea.New()
	in.CharLimit = 8000
	in.ShowLineNumbers = false
	in.SetHeight(4)
	return &Model{ctx: ctx, service: service, space: space, page: "overview", states: map[string]*localState{}, sessions: map[string]string{}, scopes: map[string]string{}, leaves: map[string]Leaf{}, width: 80, height: 24, input: in, view: viewport.New(76, 10)}
}

func (m *Model) Init() tea.Cmd { return m.refresh("") }

func (m *Model) Selection() (string, string) { return m.space, m.spaceName }

func (m *Model) state() *localState {
	key := m.space + "/" + m.page
	if m.states[key] == nil {
		m.states[key] = &localState{drafts: map[string]string{}}
	}
	return m.states[key]
}

func (m *Model) save() {
	s := m.state()
	s.cursor = m.cursor
	if !m.loading && m.data.Content != "" {
		s.offset = m.view.YOffset
	}
	if m.editing {
		s.drafts[m.draftKey()] = m.input.Value()
	}
}

func (m *Model) draftKey() string {
	key := m.action.ID + m.action.DraftKey
	if len(m.action.Fields) > 0 {
		key += "/" + m.action.Fields[m.field].ID
	}
	return key
}

// Suspend 仅释放本容器请求，不触碰 Agent 会话或受管任务。
func (m *Model) Suspend() { m.save(); m.stop(); m.data = Page{}; m.view.SetContent("") }

func (m *Model) stop() {
	if m.leaf != nil {
		m.leaf.Suspend()
		m.leaf = nil
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.generation++
	m.loading = false
}

func (m *Model) Open(ctx context.Context, page string) tea.Cmd {
	if len(m.states) >= 256 && m.states[m.space+"/"+page] == nil {
		m.status = "页面保留数量已达上限"
		return nil
	}
	m.ctx = ctx
	m.save()
	if m.page != page {
		if len(m.history) >= 64 {
			m.history = m.history[1:]
		}
		m.history = append(m.history, m.page)
	}
	m.page = page
	m.editing, m.confirm = false, false
	m.cursor = m.state().cursor
	return m.refresh("")
}

func (m *Model) refresh(action string) tea.Cmd {
	m.stop()
	m.pendingAction = action
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.loading = true
	m.status = "加载中；Esc 取消等待（不代表远端事务回滚）"
	s := m.state()
	req := Request{Space: m.space, Page: m.page, Action: action, Text: s.drafts[m.draftKey()], Search: s.search, Cursor: s.pageCursor, Session: m.data.Session, Version: m.data.Version}
	req.Page, req.Resource, _ = strings.Cut(m.page, "/")
	req.Scope = m.scopes[m.space]
	req.Basis = m.data.Basis
	if req.Session == "" {
		req.Session = m.sessions[m.space]
	}
	req.Values = map[string]string{}
	for _, f := range m.action.Fields {
		req.Values[f.ID] = s.drafts[m.action.ID+m.action.DraftKey+"/"+f.ID]
	}
	if len(m.action.Fields) > 0 {
		req.Text = req.Values["text"]
	}
	if action != "" {
		encoded, _ := json.Marshal(req)
		if s.pendingKey != string(encoded) {
			op, err := id.NewUUID()
			if err != nil {
				m.stop()
				m.status = err.Error()
				return nil
			}
			entity, err := id.NewUUID()
			if err != nil {
				m.stop()
				m.status = err.Error()
				return nil
			}
			s.pendingKey, s.operation, s.entity = string(encoded), op, entity
		}
		req.Operation, req.Entity = s.operation, s.entity
	}
	generation, service := m.generation, m.service
	m.data = Page{}
	m.view.SetContent("")
	m.cursor = min(m.cursor, len(m.options())-1)
	return func() tea.Msg { page, err := service.Load(ctx, req); return response{generation, page, err} }
}

var navigation = []Entry{{"overview", "概览"}, {"materials", "资料"}, {"goals", "目标"}, {"learn", "学习"}, {"reviews", "复习"}, {"spaces", "切换学习区（全局）"}, {"help", "帮助"}, {"exit", "返回主菜单"}}

func (m *Model) options() []Entry {
	items := append([]Entry(nil), navigation...)
	for _, e := range m.data.Entries {
		items = append(items, Entry{"select:" + e.ID, e.Label})
	}
	for _, a := range m.data.Actions {
		items = append(items, Entry{"action:" + a.ID, a.Label})
	}
	items = append(items, Entry{"refresh", "刷新"})
	if m.page == "spaces" || m.data.Searchable {
		items = append(items, Entry{"search", "搜索当前列表"})
	}
	if m.data.NextCursor != "" {
		items = append(items, Entry{"next", "下一页"})
	}
	if m.state().pageCursor != "" {
		items = append(items, Entry{"first", "返回第一页"})
	}
	return items
}

func (m *Model) begin(a Action) tea.Cmd {
	m.action = a
	m.field = 0
	m.confirm = false
	if a.Input != "" || len(a.Fields) > 0 {
		missing := 0
		keys := []string{m.draftKey()}
		if len(a.Fields) > 0 {
			keys = nil
			for _, f := range a.Fields {
				keys = append(keys, a.ID+a.DraftKey+"/"+f.ID)
			}
		}
		for _, key := range keys {
			if _, ok := m.state().drafts[key]; !ok {
				missing++
			}
		}
		if len(m.state().drafts)+missing > 32 {
			m.status = "本页草稿数量已达上限，请退出后重新打开"
			return nil
		}
		m.editing = true
		for _, f := range a.Fields {
			key := a.ID + a.DraftKey + "/" + f.ID
			if _, ok := m.state().drafts[key]; !ok {
				m.state().drafts[key] = f.Value
			}
		}
		m.input.SetValue(m.state().drafts[m.draftKey()])
		m.input.Placeholder = a.Input
		if len(a.Fields) > 0 {
			m.input.Placeholder = a.Fields[0].Label
		}
		return m.input.Focus()
	}
	if a.Confirmation != "" {
		m.confirm = true
		return nil
	}
	return m.refresh(a.ID)
}

func (m *Model) choose(id string) tea.Cmd {
	m.save()
	switch id {
	case "exit":
		m.Suspend()
		return func() tea.Msg { return ExitMsg{} }
	case "refresh":
		return m.refresh("")
	case "search":
		return m.begin(Action{ID: "search", Input: "输入名称或关键词；Ctrl+S 搜索"})
	case "next":
		m.state().pageCursor = m.data.NextCursor
		return m.refresh("")
	case "first":
		m.state().pageCursor = ""
		return m.refresh("")
	}
	if strings.HasPrefix(id, "action:") {
		for _, a := range m.data.Actions {
			if id == "action:"+a.ID {
				return m.begin(a)
			}
		}
		return nil
	}
	if strings.HasPrefix(id, "select:") {
		if target, ok := strings.CutPrefix(id, "select:resume:"); ok {
			space, session, valid := strings.Cut(target, "/")
			if !valid || space == "" || session == "" {
				m.status = "续学目标无效"
				return nil
			}
			m.space = space
			m.sessions[space] = session
			m.data = Page{}
			m.goal, m.stage, m.spaceName = "", "", ""
			m.history = nil
			return m.Open(m.ctx, "session/"+session)
		}
		if target, ok := strings.CutPrefix(id, "select:page:"); ok {
			return m.Open(m.ctx, target)
		}
		// 学习区数目有界，达到上限时明确拒绝，避免静默丢弃草稿。
		if len(m.states) >= 256 && m.states[strings.TrimPrefix(id, "select:")+"/overview"] == nil {
			m.status = "本进程页面保留数量已达上限，请退出后重新打开"
			return nil
		}
		m.space = strings.TrimPrefix(id, "select:")
		m.data = Page{}
		m.history = nil
		m.spaceName = ""
		m.goal, m.stage = "", ""
		m.page = "overview"
		m.editing, m.confirm = false, false
		m.cursor = m.state().cursor
		return m.refresh("")
	}
	return m.Open(m.ctx, id)
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if wrapped, ok := message.(leafMessage); ok {
		if wrapped.generation != m.generation || m.leaf == nil {
			return m, nil
		}
		if batch, ok := wrapped.value.(tea.BatchMsg); ok {
			cmds := []tea.Cmd{}
			for _, cmd := range batch {
				cmds = append(cmds, m.wrapLeaf(cmd))
			}
			return m, tea.Batch(cmds...)
		}
		if _, ok := wrapped.value.(tea.QuitMsg); ok {
			leaf, generation := m.leaf, m.generation
			ctx, cancel := context.WithCancel(m.ctx)
			m.cancel, m.loading = cancel, true
			m.status = "正在返回工作台；Esc 取消等待（不代表远端事务回滚）"
			leaf.Suspend()
			m.leaf = nil
			destination := leaf.Destination(ctx)
			return m, func() tea.Msg {
				target, err := destination()
				return response{generation, Page{Redirect: target}, err}
			}
		}
		_, cmd := m.leaf.Update(wrapped.value)
		return m, m.wrapLeaf(cmd)
	}
	if m.leaf != nil {
		if key, ok := message.(tea.KeyMsg); ok && key.String() == "ctrl+c" {
			m.Suspend()
			return m, tea.Quit
		}
		if key, ok := message.(tea.KeyMsg); ok && key.String() == "f10" {
			return m, m.Open(m.ctx, "materials")
		}
		if size, ok := message.(tea.WindowSizeMsg); ok {
			m.width, m.height = size.Width, size.Height
			m.view.Width, m.view.Height = max(1, size.Width-4), max(1, size.Height-12)
			m.input.SetWidth(max(1, size.Width-4))
			message = tea.WindowSizeMsg{Width: size.Width, Height: max(1, size.Height-1)}
		}
		_, cmd := m.leaf.Update(message)
		return m, m.wrapLeaf(cmd)
	}
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.view.Width, m.view.Height = max(1, msg.Width-4), max(1, msg.Height-12)
		m.input.SetWidth(max(1, msg.Width-4))
		m.renderContent()
	case response:
		if msg.generation != m.generation {
			return m, nil
		}
		m.stop()
		if msg.err != nil {
			if failure, ok := msg.err.(*Failure); ok && failure.ClearDrafts {
				m.states = map[string]*localState{}
				for _, leaf := range m.leaves {
					leaf.Suspend()
				}
				m.leaves = map[string]Leaf{}
				m.scopes, m.sessions = map[string]string{}, map[string]string{}
				m.input.Reset()
				m.goal, m.stage, m.spaceName = "", "", ""
			}
			m.status = "加载/操作失败：" + safeLine(msg.err.Error()) + "；可刷新或返回"
			return m, nil
		}
		m.data = msg.page
		if m.pendingAction != "" {
			m.state().pendingKey = ""
			m.pendingAction = ""
		}
		if msg.page.Redirect != "" {
			return m, m.Open(m.ctx, msg.page.Redirect)
		}
		if msg.page.Session != "" {
			m.sessions[m.space] = msg.page.Session
		}
		if msg.page.Scope != "" {
			m.scopes[m.space] = msg.page.Scope
		}
		if msg.page.Leaf != nil {
			key := m.space + "/" + m.page
			m.leaf = m.leaves[key]
			if m.leaf == nil {
				m.leaf = msg.page.Leaf
				m.leaves[key] = m.leaf
			} else if rebind, ok := m.leaf.(interface{ Rebind(Leaf) }); ok {
				rebind.Rebind(msg.page.Leaf)
			}
			_, _ = m.leaf.Update(tea.WindowSizeMsg{Width: m.width, Height: max(1, m.height-1)})
			return m, m.wrapLeaf(m.leaf.Resume(m.ctx))
		}
		if msg.page.ContextKnown {
			m.goal, m.stage = msg.page.Goal, msg.page.Stage
		}
		if msg.page.SpaceName != "" {
			m.spaceName = msg.page.SpaceName
		}
		m.status = ""
		m.cursor = min(m.state().cursor, len(m.options())-1)
		m.renderContent()
		m.view.SetYOffset(m.state().offset)
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			m.Suspend()
			return m, tea.Quit
		}
		if m.width < 28 || m.height < 16 {
			if key == "esc" {
				m.Suspend()
				return m, func() tea.Msg { return ExitMsg{} }
			}
			return m, nil
		}
		if m.editing {
			if len(m.action.Fields) > 0 && (key == "left" || key == "right") {
				choices := m.action.Fields[m.field].Choices
				if len(choices) > 0 {
					index := 0
					for i, v := range choices {
						if v == m.input.Value() {
							index = i
							break
						}
					}
					delta := 1
					if key == "left" {
						delta = -1
					}
					m.input.SetValue(choices[(index+delta+len(choices))%len(choices)])
					return m, nil
				}
			}
			if len(m.action.Fields) > 0 && (key == "tab" || key == "shift+tab") {
				m.save()
				delta := 1
				if key == "shift+tab" {
					delta = -1
				}
				m.field = (m.field + delta + len(m.action.Fields)) % len(m.action.Fields)
				m.input.SetValue(m.state().drafts[m.draftKey()])
				m.input.Placeholder = m.action.Fields[m.field].Label
				return m, nil
			}
			switch key {
			case "esc":
				m.save()
				m.editing = false
				m.input.Blur()
				return m, nil
			case "ctrl+s":
				m.save()
				m.editing = false
				m.input.Blur()
				if m.action.ID == "search" {
					m.state().search = strings.TrimSpace(m.input.Value())
					m.state().pageCursor = ""
					return m, m.refresh("")
				}
				if m.action.Confirmation != "" {
					m.confirm = true
					return m, nil
				}
				return m, m.refresh(m.action.ID)
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		if m.confirm {
			switch key {
			case "esc", "n":
				m.confirm = false
			case "y":
				m.confirm = false
				return m, m.refresh(m.action.ID)
			}
			return m, nil
		}
		switch key {
		case "esc":
			if m.loading {
				m.stop()
				m.status = "已取消等待；远端操作可能已提交，请刷新确认"
				return m, nil
			}
			if len(m.history) > 0 {
				target := m.history[len(m.history)-1]
				m.history = m.history[:len(m.history)-1]
				cmd := m.Open(m.ctx, target)
				if len(m.history) > 0 {
					m.history = m.history[:len(m.history)-1]
				}
				return m, cmd
			}
			m.Suspend()
			return m, func() tea.Msg { return ExitMsg{} }
		case "tab", "down", "j":
			m.cursor = (m.cursor + 1) % len(m.options())
		case "shift+tab", "up", "k":
			m.cursor = (m.cursor + len(m.options()) - 1) % len(m.options())
		case "enter":
			return m, m.choose(m.options()[m.cursor].ID)
		case "1", "2", "3", "4", "5", "6", "7":
			return m, m.choose(navigation[int(key[0]-'1')].ID)
		case "r":
			return m, m.refresh("")
		case "?":
			return m, m.Open(m.ctx, "help")
		case "pgdown":
			m.view.PageDown()
		case "pgup":
			m.view.PageUp()
		case "home":
			m.view.GotoTop()
		case "end":
			m.view.GotoBottom()
		}
	default:
		if m.editing {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m *Model) renderContent() {
	lines := strings.Split(m.data.Content, "\n")
	for i := range lines {
		lines[i] = safeLine(lines[i])
	}
	m.view.SetContent(lipgloss.NewStyle().Width(m.view.Width).Render(strings.Join(lines, "\n")))
}

func (m *Model) View() string {
	if m.leaf != nil {
		return "F10 返回学习工作台（保留导入草稿）\n" + m.leaf.View()
	}
	if m.width < 28 || m.height < 16 {
		return "终端过小，请放大\nEsc 返回 · Ctrl+C 退出"
	}
	safe := safeLine
	line := func(value string) string { return ansi.Truncate(safe(value), m.width, "…") }
	name := m.spaceName
	if name == "" {
		name = m.space
	}
	text := line("学习工作台 · 当前区："+name) + "\n"
	text += line("教学阶段："+m.stage+" · 当前教学目标："+m.goal) + "\n"
	text += line("1概览 2资料 3目标 4学习 5复习 6切区 7帮助") + "\n"
	text += line(m.data.Title) + "\n" + m.view.View() + "\n"
	if m.editing {
		if len(m.action.Fields) > 0 {
			text += line(fmt.Sprintf("字段 %d/%d：%s · Tab 切换", m.field+1, len(m.action.Fields), m.action.Fields[m.field].Label)) + "\n"
		}
		text += m.input.View() + "\nEnter 换行 · Ctrl+S 提交 · Esc 保留草稿返回"
	} else if m.confirm {
		text += safe(m.action.Confirmation) + "\ny 确认 · n/Esc 取消"
	} else {
		items := m.options()
		text += line(fmt.Sprintf("操作 %d/%d：%s", m.cursor+1, len(items), items[m.cursor].Label)) + "\n"
		text += "↑↓/Tab 选择 · Enter 执行 · PgUp/PgDn 滚动\nEsc 返回/取消 · r 刷新 · Ctrl+C 退出"
	}
	if m.status != "" {
		text += "\n" + safe(m.status)
	}
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(text)
}

func safeLine(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069' {
			return -1
		}
		return r
	}, value)
	return terminal.EscapeText(value)
}
