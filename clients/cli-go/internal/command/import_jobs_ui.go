package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type jobMessage struct {
	generation int
	value      any
	err        error
}
type importJobsModel struct {
	ctx                       context.Context
	client                    *api.Client
	newID                     func() (string, error)
	jobs                      []api.ImportJob
	nextCursor                string
	job                       *api.ImportJob
	cursor, batch, generation int
	busy, newJob              bool
	resumeJob                 bool
	running                   bool
	note                      string
	view                      viewport.Model
	review                    *importModel
	requestCancel             context.CancelFunc
}

func (m *importJobsModel) task(work func(context.Context) (any, error)) tea.Cmd {
	if m.requestCancel != nil {
		m.requestCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.requestCancel = cancel
	m.generation++
	g := m.generation
	m.busy = true
	return func() tea.Msg { value, err := work(ctx); return jobMessage{g, value, err} }
}
func (m *importJobsModel) Init() tea.Cmd {
	return m.task(func(ctx context.Context) (any, error) { return m.client.ImportJobs(ctx, "") })
}
func (m *importJobsModel) action(action string) tea.Cmd {
	c := api.ImportJobCommand{ID: m.job.ID, Action: action, PlanVersion: m.job.PlanVersion}
	return m.task(func(ctx context.Context) (any, error) { return m.client.RunImportJob(ctx, c) })
}
func (m *importJobsModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case jobMessage:
		if msg.generation != m.generation {
			return m, nil
		}
		m.busy = false
		if msg.err != nil {
			m.running = false
			m.note = safeText(msg.err.Error())
			return m, nil
		}
		m.note = ""
		switch value := msg.value.(type) {
		case api.ImportJobPage:
			m.jobs = value.Items
			m.nextCursor = value.NextCursor
			m.job = nil
			m.cursor = min(m.cursor, max(0, len(value.Items)-1))
		case api.ImportJob:
			m.job = &value
			m.review = nil
			m.batch = min(m.batch, len(value.Batches)-1)
		case api.ImportJobBatch:
			if value.Preview != nil && value.Preview.Review != nil {
				draft := &importDraft{space: m.job.Space, preview: *value.Preview}
				draft.collection.ID = m.job.Collection
				child, cancel := context.WithCancel(m.ctx)
				review := &importModel{draft: draft, client: m.client, ctx: child, cancel: cancel, newID: m.newID, stage: "review", width: m.view.Width + 4, height: m.view.Height + 8, view: viewport.New(m.view.Width, m.view.Height), query: textinput.New(), paste: textarea.New()}
				for i := 0; i < 3; i++ {
					review.inputs = append(review.inputs, textinput.New())
				}
				id, index := m.job.ID, m.batch
				review.jobResolve = func(request api.ImportRequest) tea.Cmd {
					return m.task(func(ctx context.Context) (any, error) {
						return m.client.RunImportJob(ctx, api.ImportJobCommand{ID: id, Action: "resolve", Batch: index, DocumentResolutions: request.DocumentResolutions, NodeResolutions: request.NodeResolutions})
					})
				}
				review.refreshReview()
				m.review = review
			} else {
				raw, _ := json.MarshalIndent(value, "", "  ")
				m.note = "批次详情（Esc 返回任务）"
				m.setContent(string(raw))
				m.view.GotoTop()
				return m, nil
			}
		}
		m.refresh()
		if m.running && m.job != nil {
			if !m.job.Approved || m.job.Status == "completed" {
				m.running = false
			} else {
				for _, b := range m.job.Batches {
					if b.Status == "unknown" {
						m.running = false
						m.note = "本批结果未知，已停止后续批次；按g核对原操作"
					}
				}
				if m.running {
					return m, m.action("continue")
				}
			}
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.view.Width = max(20, msg.Width-4)
		m.view.Height = max(5, msg.Height-8)
		m.refresh()
	case tea.KeyMsg:
		key := msg.String()
		if m.review != nil {
			if key == "esc" || key == "ctrl+c" {
				m.review.cancel()
				m.review = nil
				m.refresh()
				return m, nil
			}
			_, cmd := m.review.Update(message)
			return m, cmd
		}
		if key == "ctrl+c" {
			if m.requestCancel != nil {
				m.requestCancel()
			}
			return m, tea.Quit
		}
		if key == "esc" {
			m.running = false
			if m.requestCancel != nil {
				m.requestCancel()
			}
			m.generation++
			m.busy = false
			if m.job == nil {
				return m, tea.Quit
			}
			m.job = nil
			return m, m.Init()
		}
		if m.busy {
			if key == "x" {
				m.running = false
				if m.requestCancel != nil {
					m.requestCancel()
				}
				m.generation++
				m.busy = false
				m.note = "已停止后续请求；按g核对当前批次，再按x取消任务。已入库内容不会回滚"
			}
			return m, nil
		}
		if m.job == nil {
			switch key {
			case "n":
				m.newJob = true
				return m, tea.Quit
			case "r":
				return m, m.Init()
			case "pgdown":
				if m.nextCursor != "" {
					cursor := m.nextCursor
					return m, m.task(func(ctx context.Context) (any, error) { return m.client.ImportJobs(ctx, cursor) })
				}
			case "up", "k":
				m.cursor = max(0, m.cursor-1)
			case "down", "j":
				m.cursor = min(len(m.jobs)-1, m.cursor+1)
			case "enter":
				if len(m.jobs) > 0 {
					id := m.jobs[m.cursor].ID
					return m, m.task(func(ctx context.Context) (any, error) { return m.client.ImportJob(ctx, id) })
				}
			}
		} else {
			switch key {
			case "p":
				return m, m.action("preview")
			case "ctrl+s":
				return m, m.action("confirm")
			case "r":
				m.running = true
				return m, m.action("continue")
			case "x":
				return m, m.action("cancel")
			case "g":
				id := m.job.ID
				return m, m.task(func(ctx context.Context) (any, error) { return m.client.ImportJob(ctx, id) })
			case "u":
				m.resumeJob = true
				return m, tea.Quit
			case "left":
				m.batch = max(0, m.batch-1)
			case "right":
				m.batch = min(len(m.job.Batches)-1, m.batch+1)
			case "v":
				id, index := m.job.ID, m.batch
				return m, m.task(func(ctx context.Context) (any, error) { return m.client.ImportJobBatch(ctx, id, index) })
			}
		}
		if key == "up" || key == "down" || key == "pgup" || key == "pgdown" {
			var cmd tea.Cmd
			m.view, cmd = m.view.Update(message)
			return m, cmd
		}
		m.refresh()
	}
	return m, nil
}
func (m *importJobsModel) refresh() {
	var text strings.Builder
	if m.job == nil {
		for i, j := range m.jobs {
			mark := " "
			if i == m.cursor {
				mark = ">"
			}
			fmt.Fprintf(&text, "%s %s  %s  %d批\n", mark, j.ID, j.Status, j.BatchCount)
		}
		if len(m.jobs) == 0 {
			text.WriteString("此集合暂无导入任务。")
		}
	} else {
		j := m.job
		fmt.Fprintf(&text, "任务 %s\n学习区 %s\n集合 %s\n状态 %s · 预览版本 %d · 已授权 %t\n到期 %s\n", j.ID, j.Space, j.Collection, j.Status, j.PlanVersion, j.Approved, j.Expires.Format("2006-01-02 15:04 UTC"))
		added, updated, unchanged, failed, unknown, unsubmitted, uploaded := 0, 0, 0, 0, 0, 0, 0
		for i, b := range j.Batches {
			mark := " "
			if i == m.batch {
				mark = ">"
			}
			fmt.Fprintf(&text, "%s [%d] %s · %s · %s\n", mark, i, b.Item.Path, b.Status, b.Error)
			if b.Status != "pending" && b.Status != "missing" {
				uploaded++
			}
			if b.Result != nil && b.Result.Summary != nil {
				r := b.Result.Summary
				added += r.Added
				updated += r.Updated
				unchanged += r.Unchanged
				fmt.Fprintf(&text, "    正式版本 %s\n", b.Result.Revision.RevisionID)
			} else {
				switch b.Status {
				case "unknown":
					unknown++
				case "failed":
					failed++
				default:
					unsubmitted++
				}
			}
		}
		fmt.Fprintf(&text, "\n已传输 %d/%d · 实际新增 %d · 更新 %d · 未变化 %d\n失败 %d · 未知 %d · 未提交 %d\n", uploaded, len(j.Batches), added, updated, unchanged, failed, unknown, unsubmitted)
		if j.CleanupPending {
			text.WriteString("文件清理尚未完成；密钥已删除，可按x重试清理。\n")
		}
		text.WriteString("确认绑定以上完整清单与逐文件批次计划；跨批不承诺全有或全无。\n取消保留已入库版本。未传输文件未备份；按u校验来源并续传。")
	}
	m.setContent(text.String())
}
func (m *importJobsModel) setContent(text string) {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = safeText(lines[i])
	}
	m.view.SetContent(ansi.Wrap(strings.Join(lines, "\n"), max(10, m.view.Width), ""))
}
func (m *importJobsModel) View() string {
	if m.review != nil {
		return m.review.View()
	}
	help := "↑↓ 选择 · Enter 详情 · n 使用导入扫描向导创建 · PgDown 下一页 · r 刷新 · Esc 退出"
	if m.job != nil {
		help = "p 预览剩余 · Ctrl+S 确认完整计划 · r 逐批继续 · g 核对 · x 取消/清理\n←→ 选择批次 · v 差异/身份审阅 · u 校验来源并续传 · ↑↓ 滚动 · Esc 返回"
	}
	busy := ""
	if m.busy {
		busy = "正在读取或处理；进度以返回的持久状态为准。\n"
	}
	return "可恢复导入任务\n" + busy + m.view.View() + "\n" + m.note + "\n" + help + "\n"
}
func (a *App) runImportJobsUI(ctx context.Context, client *api.Client, collection string) error {
	if !a.interactiveTerminalAvailable() || a.teachingInput == nil || a.teachingOutput == nil {
		return commandError("not_a_terminal", "任务页需要交互终端", "使用 jobs list/show/resume", ExitInput)
	}
	for {
		child, cancel := context.WithCancel(ctx)
		m := &importJobsModel{ctx: child, client: client, newID: a.NewUUID, view: viewport.New(76, 16)}
		_, err := tea.NewProgram(m, tea.WithContext(child), tea.WithInput(a.teachingInput), tea.WithOutput(a.teachingOutput), tea.WithAltScreen()).Run()
		cancel()
		if err != nil {
			return err
		}
		if m.resumeJob {
			if err = a.runImportWizardMode(ctx, client, collection, "", true, *m.job); err != nil {
				return err
			}
			continue
		}
		if !m.newJob {
			return nil
		}
		if err = a.runImportWizardMode(ctx, client, collection, "", true); err != nil {
			return err
		}
	}
}
