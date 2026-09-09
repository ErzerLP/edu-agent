package command

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type teachingText string
type teachingPrompt struct {
	text  string
	reply chan string
}
type teachingDone struct{ err error }
type teachingFocus struct{ key string }

// 每次进入会话拥有独立程序、输入桥和输出接收器，迟到结果无法接触下一页面。
type teachingBridge struct {
	ctx  context.Context
	send func(tea.Msg)
}

func (b teachingBridge) Write(p []byte) (int, error) {
	b.send(teachingText(string(p)))
	return len(p), nil
}
func (b teachingBridge) ReadLine(prompt string) (string, error) {
	reply := make(chan string, 1)
	b.send(teachingPrompt{prompt, reply})
	select {
	case value := <-reply:
		return value, nil
	case <-b.ctx.Done():
		return "", b.ctx.Err()
	}
}
func (b teachingBridge) ReadSecret(string) (string, error) {
	return "", errors.New("教学页面不接受凭据输入")
}
func (b teachingBridge) Confirm(prompt string) (bool, error) {
	value, err := b.ReadLine(prompt + " [y/N] ")
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes"), err
}
func (b teachingBridge) Clear() error { b.send(teachingText("\x00")); return nil }
func (b teachingBridge) SetTeachingFocus(view api.SessionView) {
	b.send(teachingFocus{teachingDraftKey(view)})
}

func teachingDraftKey(view api.SessionView) string {
	key := view.Session.SessionID + "/"
	if view.WorkItem != nil && view.WorkItem.Activity != nil {
		return key + view.WorkItem.Activity.ActivityID
	}
	return key + view.Session.State
}

type teachingModel struct {
	title     string
	log       string
	prompt    string
	reply     chan string
	input     textinput.Model
	start     tea.Cmd
	cancel    context.CancelFunc
	switching bool
	exiting   bool
	finished  bool
	err       error
	height    int
	draftKey  string
	drafts    map[string]string
}

func (m *teachingModel) Init() tea.Cmd { return tea.Batch(textinput.Blink, m.start) }
func (m *teachingModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case teachingFocus:
		if msg.key != m.draftKey {
			m.drafts[m.draftKey] = m.input.Value()
			m.draftKey = msg.key
			m.input.SetValue(m.drafts[msg.key])
		}
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.input.Width = msg.Width - 4
	case teachingText:
		if string(msg) == "\x00" {
			m.log = ""
		} else {
			lines := strings.Split(string(msg), "\n")
			for i := range lines {
				lines[i] = safeText(lines[i])
			}
			m.log += strings.Join(lines, "\n")
			if len(m.log) > 32768 {
				m.log = m.log[len(m.log)-32768:]
			}
		}
	case teachingPrompt:
		m.prompt = safeText(msg.text)
		m.reply = msg.reply
	case teachingDone:
		m.finished = true
		m.err = msg.err
		if m.switching {
			m.err = errSelectTeachingSession
		} else if m.exiting {
			m.err = nil
		}
		return m, tea.Quit
	case tea.KeyMsg:
		switch msg.String() {
		case "f2":
			m.switching = true
			m.cancel()
			// 输入桥能立即退出；网络调用则独立收尾，不阻塞用户选择 B。
			if m.reply == nil {
				m.err = errSelectTeachingSession
				return m, tea.Quit
			}
			return m, nil
		case "ctrl+c", "esc":
			m.exiting = true
			m.cancel()
			m.err = nil
			if m.reply != nil {
				return m, nil
			}
			return m, tea.Quit
		case "enter":
			if m.reply != nil {
				m.reply <- m.input.Value()
				m.reply = nil
				m.input.SetValue("")
				m.prompt = "请求处理中；F2 可切换目标"
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(message)
	return m, cmd
}
func (m *teachingModel) View() string {
	lines := strings.Split(m.log, "\n")
	max := m.height - 7
	if max < 3 {
		max = 12
	}
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return m.title + "\nF2 切换会话 · Esc 返回（不会结束教学）\n\n" + strings.Join(lines, "\n") + "\n" + m.prompt + "\n" + m.input.View() + "\n"
}

func (a *App) runTeachingLoop(ctx context.Context, client APIClient, view api.SessionView) error {
	if a.teachingInput == nil || a.teachingOutput == nil || !a.interactiveTerminalAvailable() {
		return a.learnLoop(ctx, client, view)
	}
	local := *a
	local.learningDrafts = map[string][]string{}
	for key, lines := range a.learningDrafts {
		local.learningDrafts[key] = append([]string(nil), lines...)
	}
	local.learningSessions = map[string]string{}
	for key, id := range a.learningSessions {
		local.learningSessions[key] = id
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	key := teachingDraftKey(view)
	drafts := map[string]string{}
	for key, value := range a.learningInputDrafts {
		drafts[key] = value
	}
	input := textinput.New()
	input.CharLimit = 262144
	input.Focus()
	input.SetValue(a.learningInputDrafts[key])
	title := "学习区：" + safeText(a.teachingSpaceLabel())
	if view.WorkItem != nil && view.WorkItem.GoalRevision != nil {
		title += " · " + safeText(view.WorkItem.GoalRevision.GoalManagement().Details.Name)
	}
	m := &teachingModel{title: title, input: input, cancel: cancel, prompt: "正在恢复服务端状态", draftKey: key, drafts: drafts}
	program := tea.NewProgram(m, tea.WithInput(a.teachingInput), tea.WithOutput(a.teachingOutput), tea.WithAltScreen(), tea.WithContext(ctx))
	bridge := teachingBridge{ctx: child, send: program.Send}
	local.Terminal = bridge
	local.Out = io.Writer(bridge)
	local.Err = io.Writer(bridge)
	m.start = func() tea.Msg { return teachingDone{local.learnLoop(child, client, view)} }
	_, err := program.Run()
	if err != nil {
		return err
	}
	m.drafts[m.draftKey] = m.input.Value()
	a.learningInputDrafts = m.drafts
	if m.finished {
		a.learningDrafts = local.learningDrafts
		a.learningSpace = local.learningSpace
		a.learningSpaceName = local.learningSpaceName
	}
	return m.err
}
