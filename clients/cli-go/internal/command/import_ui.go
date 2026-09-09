package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/importer"
)

type importMessage struct {
	generation int
	kind       string
	value      any
	err        error
}
type importDraft struct {
	space, spaceName              string
	collection                    api.KnowledgeCollection
	path, include, exclude, paste string
	reportSource                  string
	report                        importer.ScanReport
	request                       api.ImportRequest
	preview                       api.ImportPreview
	result                        *api.ImportResult
	unknown                       bool
}
type importModel struct {
	draft                                                 *importDraft
	client                                                *api.Client
	ctx                                                   context.Context
	cancel                                                context.CancelFunc
	requestCancel                                         context.CancelFunc
	newID                                                 func() (string, error)
	stage, busy, note, detailBack, search, editing        string
	inputs                                                []textinput.Model
	paste                                                 textarea.Model
	view                                                  viewport.Model
	query                                                 textinput.Model
	focus, cursor, generation, width, height, reviewIndex int
	spaces                                                []api.LearningSpace
	nextSpace                                             string
	collections                                           []api.KnowledgeCollection
	entries                                               []os.DirEntry
	browsePath                                            string
	navigate                                              string
	detailText                                            string
	completionPrefix                                      string
}

func (m *importModel) task(kind string, work func(context.Context) (any, error)) tea.Cmd {
	if m.requestCancel != nil {
		m.requestCancel()
	}
	m.generation++
	generation := m.generation
	ctx, cancel := context.WithCancel(m.ctx)
	m.requestCancel = cancel
	m.busy = kind
	return func() tea.Msg { value, err := work(ctx); return importMessage{generation, kind, value, err} }
}
func (m *importModel) scoped() *api.Client {
	return m.client.WithLearningSpace(m.draft.space).WithCollection(m.draft.collection.ID)
}
func (m *importModel) loadTargets() tea.Cmd {
	client := m.client.WithLearningSpace(m.draft.space)
	if m.draft.space == "" {
		cursor, search := m.nextSpace, m.search
		return m.task("读取学习区", func(ctx context.Context) (any, error) {
			return client.LearningSpaces(ctx, search, "active", cursor, 100)
		})
	}
	return m.task("读取资料集合", func(ctx context.Context) (any, error) { return client.KnowledgeCollections(ctx, false) })
}
func (m *importModel) Init() tea.Cmd { return m.loadTargets() }
func (m *importModel) invalidate() {
	m.draft.preview = api.ImportPreview{}
	m.draft.request = api.ImportRequest{}
	m.reviewIndex = 0
}
func (m *importModel) saveInputs() {
	m.draft.path, m.draft.include, m.draft.exclude, m.draft.paste = m.inputs[0].Value(), m.inputs[1].Value(), m.inputs[2].Value(), m.paste.Value()
}
func (m *importModel) scan() tea.Cmd {
	m.saveInputs()
	m.invalidate()
	options := importer.ScanOptions{Path: m.draft.path, Include: importPatterns(m.draft.include), Exclude: importPatterns(m.draft.exclude)}
	return m.task("本地扫描", func(ctx context.Context) (any, error) { return importer.Scan(ctx, options), nil })
}
func (m *importModel) previewRequest(fresh bool) tea.Cmd {
	if fresh {
		id, err := m.newID()
		if err != nil {
			m.note = err.Error()
			return nil
		}
		m.draft.request = api.ImportRequest{OperationID: id, Source: "go-cli-import-v1", Documents: m.draft.report.Documents()}
	}
	if len(m.draft.request.Documents) == 0 {
		m.note = "请至少选择一篇可导入资料"
		return nil
	}
	// 独立副本使迟到请求不能读取后来编辑的切片。
	raw, _ := json.Marshal(m.draft.request)
	var request api.ImportRequest
	_ = json.Unmarshal(raw, &request)
	client := m.scoped()
	return m.task("上传并检查", func(ctx context.Context) (any, error) {
		if fresh {
			head, err := client.KnowledgeHead(ctx)
			var apiErr *api.APIError
			if err != nil && (!errors.As(err, &apiErr) || apiErr.Code != "not_found") {
				return nil, err
			}
			if err == nil {
				request.ExpectedParentRevisionID = &head.RevisionID
			}
		}
		if err := validateImportRequestSize(request); err != nil {
			return nil, err
		}
		preview, err := client.PreviewImport(ctx, request)
		return struct {
			Request api.ImportRequest
			Preview api.ImportPreview
		}{request, preview}, err
	})
}
func (m *importModel) confirm() tea.Cmd {
	client := m.scoped()
	request := api.ConfirmImportRequest{Request: m.draft.request, Receipt: m.draft.preview.Receipt}
	if request.Receipt == "" {
		m.note = "请重新检查后确认"
		return nil
	}
	m.draft.unknown = true
	return m.task("正式提交", func(ctx context.Context) (any, error) { return client.ConfirmImport(ctx, request) })
}
func (m *importModel) showDetail(text, back string) {
	m.detailBack, m.stage = back, "detail"
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = safeText(lines[i])
	}
	m.detailText = ansi.Wrap(strings.Join(lines, "\n"), max(10, m.view.Width), "")
	m.view.SetContent(m.detailText)
	m.view.GotoTop()
}
func (m *importModel) browse(path string) tea.Cmd {
	m.browsePath = path
	return m.task("浏览目录", func(ctx context.Context) (any, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("请选择真实目录")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		entries, err := f.ReadDir(importer.MaxScanEntries + 1)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if len(entries) > importer.MaxScanEntries {
			return nil, fmt.Errorf("目录条目过多，请输入更具体路径")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return entries, err
	})
}

func (m *importModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.view.Width, m.view.Height = max(10, msg.Width-4), max(3, msg.Height-9)
		m.paste.SetWidth(max(10, msg.Width-4))
		m.paste.SetHeight(max(3, msg.Height-9))
		for i := range m.inputs {
			m.inputs[i].Width = max(10, msg.Width-8)
		}
	case importMessage:
		if msg.generation != m.generation {
			return m, nil
		}
		m.busy = ""
		if msg.err != nil {
			m.note = safeText(msg.err.Error())
			if msg.kind == "上传并检查" {
				m.stage = "files"
				m.invalidate()
				m.note += "；选择已保留，请修改路径或重新检查"
			}
			if msg.kind == "正式提交" {
				var apiErr *api.APIError
				if errors.As(msg.err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
					m.draft.unknown = false
					m.stage = "files"
					m.invalidate()
				} else {
					m.stage = "result"
					m.note = "提交结果未知。先核对原操作；不要新建操作重传。 " + m.note
				}
			}
			return m, nil
		}
		switch msg.kind {
		case "读取学习区":
			page := msg.value.(api.LearningSpacePage)
			m.spaces, m.nextSpace, m.stage, m.cursor = page.Items, page.NextCursor, "spaces", 0
		case "读取资料集合":
			m.collections, m.stage, m.cursor = msg.value.([]api.KnowledgeCollection), "target", 0
			for _, c := range m.collections {
				if c.ID == m.draft.collection.ID {
					m.draft.collection = c
					m.stage = "source"
					if m.draft.unknown || m.draft.result != nil {
						m.stage = "result"
					}
					break
				}
			}
			if m.draft.unknown || m.draft.result != nil {
				m.stage = "result"
			}
		case "浏览目录":
			m.entries, m.stage, m.cursor = msg.value.([]os.DirEntry), "browse", 0
			if m.completionPrefix != "" {
				var matched []os.DirEntry
				for _, entry := range m.entries {
					if strings.HasPrefix(entry.Name(), m.completionPrefix) {
						matched = append(matched, entry)
					}
				}
				m.entries = matched
				m.completionPrefix = ""
				if len(matched) == 1 {
					m.inputs[0].SetValue(filepath.Join(m.browsePath, matched[0].Name()))
					m.stage = "source"
				}
			}
		case "本地扫描":
			report := msg.value.(importer.ScanReport)
			previous := map[string]importer.ScanItem{}
			for _, item := range m.draft.report.Items {
				if m.draft.reportSource == m.draft.path {
					previous[item.Path] = item
				}
			}
			for i := range report.Items {
				item := &report.Items[i]
				if old, ok := previous[item.Path]; ok && old.Status == "ready" && item.Status == "ready" {
					item.Selected = old.Selected
					if old.Document != nil && item.Document != nil {
						item.Document.Path = old.Document.Path
					}
				}
			}
			m.draft.report, m.stage, m.cursor = report, "files", 0
			m.draft.reportSource = m.draft.path
			m.note = m.draft.report.Stopped
		case "上传并检查":
			result := msg.value.(struct {
				Request api.ImportRequest
				Preview api.ImportPreview
			})
			m.draft.request, m.draft.preview, m.reviewIndex = result.Request, result.Preview, 0
			m.stage = "preview"
			if result.Preview.Review != nil {
				m.stage = "review"
				m.refreshReview()
			} else {
				m.refreshPreview()
			}
		case "正式提交", "核对原操作":
			result := msg.value.(api.ImportResult)
			m.draft.result, m.draft.unknown, m.stage, m.note = &result, false, "result", ""
		case "读取资料详情", "读取原资料":
			back := "result"
			if msg.kind == "读取原资料" {
				back = "review"
			}
			body := ""
			if data, ok := msg.value.(map[string]any); ok {
				if docs, ok := data["documents"].([]any); ok {
					for _, value := range docs {
						if doc, ok := value.(map[string]any); ok {
							body += fmt.Sprint(doc["path"]) + "\n" + fmt.Sprint(doc["markdown"]) + "\n\n"
						}
					}
				}
			}
			m.showDetail(body, back)
		}
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			m.saveInputs()
			m.cancel()
			return m, tea.Quit
		}
		if key == "esc" {
			if m.busy != "" {
				kind := m.busy
				m.requestCancel()
				m.generation++
				m.busy = ""
				m.note = "已取消；输入保留"
				if kind == "正式提交" {
					m.stage = "result"
					m.note = "已停止等待，提交结果未知，请核对原操作"
				}
				return m, nil
			}
			if m.editing != "" {
				m.editing = ""
				return m, nil
			}
			switch m.stage {
			case "detail":
				m.stage = m.detailBack
				if m.stage == "review" {
					m.refreshReview()
				}
				if m.stage == "preview" {
					m.refreshPreview()
				}
			case "browse", "paste", "files":
				m.stage = "source"
			case "review", "preview":
				m.stage = "files"
				m.invalidate()
			default:
				m.saveInputs()
				return m, tea.Quit
			}
			return m, nil
		}
		if m.busy != "" {
			return m, nil
		}
		if m.editing != "" {
			if key == "enter" {
				value := m.query.Value()
				switch m.editing {
				case "rename":
					item := &m.draft.report.Items[m.cursor]
					if item.Document != nil {
						item.Document.Path = value
						m.invalidate()
					}
				case "search":
					m.search, m.cursor = value, 0
					indices := m.visibleIndices()
					if len(indices) > 0 {
						m.cursor = indices[0]
					}
					if m.stage == "review" || m.stage == "preview" || m.stage == "detail" {
						for i, line := range strings.Split(m.detailText, "\n") {
							if strings.Contains(strings.ToLower(line), strings.ToLower(value)) {
								m.view.SetYOffset(i)
								break
							}
						}
					}
				case "decision":
					m.editing = ""
					return m, m.decide(value)
				}
				m.editing = ""
				return m, nil
			}
			var cmd tea.Cmd
			m.query, cmd = m.query.Update(msg)
			return m, cmd
		}
		if m.stage == "detail" && key == "/" {
			m.editing = "search"
			m.query.SetValue("")
			return m, m.query.Focus()
		}
		if m.stage == "detail" {
			var cmd tea.Cmd
			m.view, cmd = m.view.Update(msg)
			return m, cmd
		}
		if m.stage == "source" {
			switch key {
			case "enter":
				return m, m.scan()
			case "down", "up":
				m.inputs[m.focus].Blur()
				m.focus = (m.focus + 1) % len(m.inputs)
				return m, m.inputs[m.focus].Focus()
			case "f2":
				p := m.inputs[0].Value()
				if p == "" {
					p = "."
				}
				if i, e := os.Lstat(p); e == nil && !i.IsDir() {
					p = filepath.Dir(p)
				}
				return m, m.browse(p)
			case "f3":
				m.draft.collection = api.KnowledgeCollection{}
				m.invalidate()
				return m, m.loadTargets()
			case "f4":
				m.stage = "paste"
				return m, m.paste.Focus()
			case "f5":
				m.draft.space, m.draft.spaceName = "", ""
				m.draft.collection = api.KnowledgeCollection{}
				m.nextSpace = ""
				m.search = ""
				m.invalidate()
				return m, m.loadTargets()
			case "f6":
				m.stage = "files"
				return m, nil
			case "tab":
				p := m.inputs[0].Value()
				m.completionPrefix = filepath.Base(p)
				return m, m.browse(filepath.Dir(p))
			}
			var cmd tea.Cmd
			m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
			return m, cmd
		}
		if m.stage == "paste" {
			if key == "ctrl+s" {
				text := m.paste.Value()
				if !utf8.ValidString(text) || len(text) > importer.MaxDocumentSize || text == "" {
					m.note = "粘贴内容必须是非空 UTF-8 且不超过4 MiB"
					return m, nil
				}
				doc := importer.TextDocument("粘贴文本", text, "paste; charset=utf-8")
				if len(doc.Markdown) > importer.MaxDocumentSize {
					m.note = "转换后的粘贴内容超过4 MiB预算，请缩小内容"
					return m, nil
				}
				m.draft.report = importer.ScanReport{Items: []importer.ScanItem{{Path: doc.Path, Bytes: int64(len(text)), Status: "ready", Selected: true, Document: &doc}}}
				m.draft.reportSource = ""
				m.invalidate()
				m.stage = "files"
				m.cursor = 0
				return m, nil
			}
			var cmd tea.Cmd
			m.paste, cmd = m.paste.Update(msg)
			return m, cmd
		}
		if key == "/" {
			m.editing = "search"
			m.query.SetValue("")
			return m, m.query.Focus()
		}
		if m.stage == "review" {
			if key == "v" && m.draft.request.ExpectedParentRevisionID != nil {
				client, id := m.scoped(), *m.draft.request.ExpectedParentRevisionID
				return m, m.task("读取原资料", func(ctx context.Context) (any, error) { return client.KnowledgeLibraryView(ctx, id, "export", false) })
			}
			if key == "enter" {
				m.editing = "decision"
				m.query.SetValue("")
				return m, m.query.Focus()
			}
			var cmd tea.Cmd
			m.view, cmd = m.view.Update(msg)
			return m, cmd
		}
		if m.stage == "preview" {
			if key == "ctrl+s" {
				return m, m.confirm()
			}
			var cmd tea.Cmd
			m.view, cmd = m.view.Update(msg)
			return m, cmd
		}
		if m.stage == "result" {
			switch key {
			case "c":
				client, id := m.scoped(), m.draft.request.OperationID
				return m, m.task("核对原操作", func(ctx context.Context) (any, error) { return client.ImportOperation(ctx, id) })
			case "r":
				if m.draft.unknown {
					return m, m.confirm()
				}
				m.draft.result = nil
				m.invalidate()
				m.stage = "files"
			case "v":
				if m.draft.result != nil {
					client, id := m.scoped(), m.draft.result.Revision.RevisionID
					return m, m.task("读取资料详情", func(ctx context.Context) (any, error) { return client.KnowledgeLibraryView(ctx, id, "export", false) })
				}
			case "g":
				if m.draft.result != nil {
					m.navigate = "goal"
					return m, tea.Quit
				}
			}
			var cmd tea.Cmd
			m.view, cmd = m.view.Update(msg)
			return m, cmd
		}
		indices := m.visibleIndices()
		if len(indices) == 0 && key == "enter" && m.stage != "files" {
			return m, nil
		}
		if key == "up" || key == "k" {
			m.moveCursor(indices, -1)
			return m, nil
		}
		if key == "down" || key == "j" {
			m.moveCursor(indices, 1)
			return m, nil
		}
		if key == "pgdown" {
			m.moveCursor(indices, 10)
			return m, nil
		}
		if key == "pgup" {
			m.moveCursor(indices, -10)
			return m, nil
		}
		switch m.stage {
		case "spaces":
			if key == "n" && m.nextSpace != "" {
				return m, m.loadTargets()
			}
			if key == "enter" && m.cursor < len(m.spaces) {
				s := m.spaces[m.cursor]
				m.draft.space, m.draft.spaceName = s.ID, s.Name
				m.search = ""
				return m, m.loadTargets()
			}
		case "target":
			if key == "enter" && m.cursor < len(m.collections) {
				m.draft.collection = m.collections[m.cursor]
				m.search = ""
				m.stage = "source"
			}
		case "browse":
			if key == "left" {
				return m, m.browse(filepath.Dir(m.browsePath))
			}
			if key == "s" {
				m.inputs[0].SetValue(m.browsePath)
				return m, m.scan()
			}
			if key == "enter" && m.cursor < len(m.entries) {
				e := m.entries[m.cursor]
				p := filepath.Join(m.browsePath, e.Name())
				if e.IsDir() {
					return m, m.browse(p)
				}
				m.inputs[0].SetValue(p)
				return m, m.scan()
			}
		case "files":
			if key == "enter" {
				return m, m.previewRequest(true)
			}
			if m.cursor >= len(m.draft.report.Items) {
				return m, nil
			}
			item := &m.draft.report.Items[m.cursor]
			if key == " " && item.Status == "ready" {
				item.Selected = !item.Selected
				m.invalidate()
			}
			if key == "ctrl+d" {
				dir := filepath.Dir(item.Path)
				selected := !item.Selected
				for i := range m.draft.report.Items {
					v := &m.draft.report.Items[i]
					if v.Status == "ready" && (dir == "." || filepath.Dir(v.Path) == dir || strings.HasPrefix(v.Path, dir+"/")) {
						v.Selected = selected
					}
				}
				m.invalidate()
			}
			if key == "v" && item.Document != nil {
				m.showDetail(item.Document.Markdown, "files")
			}
			if key == "e" && item.Document != nil {
				m.editing = "rename"
				m.query.SetValue(item.Document.Path)
				return m, m.query.Focus()
			}
		}
	}
	return m, nil
}

func (m *importModel) visibleIndices() []int {
	var labels []string
	switch m.stage {
	case "spaces":
		for _, s := range m.spaces {
			labels = append(labels, s.Name)
		}
	case "target":
		for _, c := range m.collections {
			labels = append(labels, c.Name+" "+c.Source)
		}
	case "browse":
		for _, e := range m.entries {
			labels = append(labels, e.Name())
		}
	case "files":
		for _, i := range m.draft.report.Items {
			labels = append(labels, i.Path+" "+i.Reason)
		}
	}
	var indices []int
	for i, label := range labels {
		if strings.Contains(strings.ToLower(label), strings.ToLower(m.search)) {
			indices = append(indices, i)
		}
	}
	return indices
}
func (m *importModel) moveCursor(indices []int, delta int) {
	if len(indices) == 0 {
		return
	}
	position := 0
	for i, index := range indices {
		if index == m.cursor {
			position = i
			break
		}
	}
	m.cursor = indices[max(0, min(len(indices)-1, position+delta))]
}
func (m *importModel) refreshPreview() {
	p := m.draft.preview
	text := fmt.Sprintf("新增 %d · 更新 %d · 未变化 %d\n", p.Summary.Added, p.Summary.Updated, p.Summary.Unchanged)
	if p.ImpactKnown {
		text += fmt.Sprintf("可能关联 %d 条既有证据；历史依据不会自动替换。\n", p.AffectedEvidence)
	} else {
		text += "关联影响暂不可确定。\n"
	}
	for _, d := range p.Diff {
		text += "\n" + d.BeforePath + " → " + d.AfterPath + "\n" + d.UnifiedDiff
		if d.Truncated {
			text += "\n差异已截断，请在资料详情查看原文。\n"
		}
	}
	m.showDetail(text, "preview")
	m.stage = "preview"
}
func (m *importModel) refreshReview() {
	r := m.draft.preview.Review
	if r == nil {
		return
	}
	var path, reason string
	var candidates []api.IdentityCandidate
	if m.reviewIndex < len(r.Documents) {
		v := r.Documents[m.reviewIndex]
		path, reason, candidates = v.Path, v.ReasonCode, v.Candidates
	} else {
		v := r.Nodes[m.reviewIndex-len(r.Documents)]
		path, reason, candidates = v.Path, v.ReasonCode, v.Candidates
	}
	text := "身份待确认：" + path + "\n检查信息：" + reason + "\n\n输入候选序号：更新原资料并保留历史（章节须语义相同）；新资料：建立独立身份；跳过：本次不导入此文件。\n章节内容变化时输入“改写 1”；明确拆分或合并可输入“拆分 1”或“合并 1,2”，记录与原章节的承接关系，不复制学习证据。\n作为新资料时若原路径被占用，请返回清单修改目标路径。\n"
	for i, c := range candidates {
		text += fmt.Sprintf("\n%d. %s %s\n", i+1, c.ReasonCode, safeEvidence(c.Evidence))
	}
	for _, b := range m.draft.preview.Before {
		text += "\n原资料 " + b.Path + "\n" + b.Markdown
	}
	for _, d := range m.draft.request.Documents {
		if d.Path == path {
			text += "\n待导入内容\n" + d.Markdown
		}
	}
	m.showDetail(text, "review")
	m.stage = "review"
}
func (m *importModel) decide(value string) tea.Cmd {
	r := m.draft.preview.Review
	if r == nil {
		return nil
	}
	node := m.reviewIndex >= len(r.Documents)
	var path, locator string
	var candidates []api.IdentityCandidate
	if node {
		v := r.Nodes[m.reviewIndex-len(r.Documents)]
		path, locator, candidates = v.Path, v.Locator, v.Candidates
	} else {
		v := r.Documents[m.reviewIndex]
		path, locator, candidates = v.Path, v.Locator, v.Candidates
	}
	if value == "跳过" {
		for i := range m.draft.report.Items {
			v := &m.draft.report.Items[i]
			if v.Document != nil && v.Document.Path == path {
				v.Selected = false
			}
		}
		m.invalidate()
		m.stage = "files"
		return nil
	}
	action := "preserve"
	if value == "新资料" {
		action = "new"
	}
	for label, operation := range map[string]string{"改写 ": "rewrite", "拆分 ": "split", "合并 ": "merge"} {
		if node && strings.HasPrefix(value, label) {
			action = operation
			value = strings.TrimPrefix(value, label)
			break
		}
	}
	var selected []api.IdentityCandidate
	if action != "new" {
		for _, number := range strings.Split(value, ",") {
			index, err := strconv.Atoi(strings.TrimSpace(number))
			if err != nil || index < 1 || index > len(candidates) {
				m.note = "请输入有效候选序号、新资料或跳过"
				return nil
			}
			selected = append(selected, candidates[index-1])
		}
	}
	if (action == "merge" && len(selected) < 2) || (action != "new" && action != "merge" && len(selected) != 1) {
		m.note = "合并需至少两个候选，其他决定只选择一个候选"
		return nil
	}
	if node {
		v := api.NodeResolution{Locator: locator, Action: action, Reason: "用户在导入向导明确确认章节身份及后果"}
		if action != "new" {
			for _, candidate := range selected {
				v.SourceNodeRevisionIDs = append(v.SourceNodeRevisionIDs, candidate.RevisionID)
			}
		}
		m.draft.request.NodeResolutions = append(m.draft.request.NodeResolutions, v)
	} else {
		v := api.DocumentResolution{Locator: locator, Action: action, Reason: "用户在导入向导明确确认资料身份及后果"}
		if action != "new" {
			v.DocumentID = selected[0].StableID
		}
		m.draft.request.DocumentResolutions = append(m.draft.request.DocumentResolutions, v)
	}
	m.reviewIndex++
	if m.reviewIndex < len(r.Documents)+len(r.Nodes) {
		m.refreshReview()
		return nil
	}
	id, err := m.newID()
	if err != nil {
		m.note = err.Error()
		return nil
	}
	m.draft.request.OperationID = id
	m.draft.request.IdentityReviewBasisHash = r.BasisHash
	m.draft.request.IdentityReviewOperationID = r.OperationID
	m.draft.request.IdentityReviewReceipt = r.Receipt
	return m.previewRequest(false)
}

func (m *importModel) View() string {
	header := "资料导入 · " + safeText(m.draft.spaceName) + " / " + safeText(m.draft.collection.Name) + "\n"
	if m.draft.space != "" {
		header += "学习区：" + m.draft.space + "\n"
	}
	if m.draft.collection.ID != "" {
		header += "目标集合：" + m.draft.collection.ID + "\n"
	}
	header += "Esc 返回/取消 · Ctrl+C 退出（保留本进程草稿）\n"
	if m.busy != "" {
		return header + "\n" + m.busy + "…\n"
	}
	body := ""
	help := "↑↓/PgUp/PgDn 移动 · / 搜索 · Enter 选择"
	switch m.stage {
	case "source":
		body = "来源路径（Tab 补全）\n" + m.inputs[0].View() + "\n包含规则\n" + m.inputs[1].View() + "\n排除规则\n" + m.inputs[2].View()
		help = "↑↓ 字段 · F2 浏览 · F3 集合 · F4 粘贴 · F5 学习区 · F6 返回清单 · Enter 扫描"
	case "paste":
		body = m.paste.View()
		help = "粘贴 UTF-8 原文 · Ctrl+S 加入清单"
	case "detail":
		body = m.view.View()
		help = "↑↓/PgUp/PgDn 滚动 · Esc 返回"
	case "review":
		body = m.view.View()
		help = "↑↓/PgUp/PgDn 滚动 · / 搜索 · v 完整原资料 · Enter 输入决定"
	case "preview":
		body = m.view.View()
		help = "Ctrl+S 确认原子提交以上清单 · Esc 修改选择"
	case "result":
		if m.draft.unknown {
			body = "结果未知：" + m.draft.request.OperationID
			help = "c 核对原操作 · r 重试同一确认 · Esc 返回"
		} else if m.draft.result != nil {
			r := m.draft.result
			body = "已提交版本：" + r.Revision.RevisionID
			if r.Summary != nil {
				body += fmt.Sprintf("\n新增 %d · 更新 %d · 未变化 %d", r.Summary.Added, r.Summary.Updated, r.Summary.Unchanged)
			}
			body += fmt.Sprintf("\n未提交清单项：%d", len(m.draft.report.Items)-len(m.draft.report.Documents()))
			for _, i := range m.draft.report.Items {
				if !i.Selected || i.Status != "ready" {
					reason := i.Reason
					if reason == "" {
						reason = "用户取消选择"
					}
					body += "\n" + safeText(i.Path) + "：" + safeText(reason)
				}
			}
			help = "v 资料详情 · g 带资料进入目标管理 · r 返回导入 · Esc 返回"
		}
	default:
		indices := m.visibleIndices()
		position := 0
		for i, index := range indices {
			if index == m.cursor {
				position = i
			}
		}
		start := max(0, position-max(1, m.height-10)/2)
		end := min(len(indices), start+max(3, m.height-10))
		if len(indices) == 0 {
			body = "没有匹配项目；可修改搜索，或先在资料管理创建/关联集合。\n"
		}
		for _, i := range indices[start:end] {
			prefix := "  "
			if i == m.cursor {
				prefix = "> "
			}
			label := ""
			switch m.stage {
			case "spaces":
				label = m.spaces[i].Name
			case "target":
				label = m.collections[i].Name + " / " + m.collections[i].Source
			case "browse":
				label = m.entries[i].Name()
				if m.entries[i].IsDir() {
					label += "/"
				}
			case "files":
				v := m.draft.report.Items[i]
				check := "[ ]"
				if v.Selected {
					check = "[x]"
				}
				status := map[string]string{"ready": "可导入", "excluded": "已排除", "unsupported": "不支持", "error": "有问题"}[v.Status]
				label = fmt.Sprintf("%s %s (%d 字节) %s %s", check, v.Path, v.Bytes, status, v.Reason)
				if v.Document != nil && v.Document.Path != v.Path {
					label += " → " + v.Document.Path
				}
			}
			body += prefix + safeText(label) + "\n"
		}
		if m.stage == "files" {
			selected, bytes := 0, int64(0)
			for _, item := range m.draft.report.Items {
				if item.Selected && item.Status == "ready" {
					selected++
					bytes += item.Bytes
				}
			}
			body = fmt.Sprintf("选中 %d 项 · %d 字节 · 扫描报告 %d 项\n", selected, bytes, len(m.draft.report.Items)) + body
			help = "空格 多选 · Ctrl+D 切换同目录 · v 内容 · e 改目标路径 · Enter 服务端检查"
		}
		if m.stage == "browse" {
			help = "Enter 进入目录/选文件 · ← 上级 · s 扫描此目录后多选"
		}
		if m.stage == "spaces" {
			help += " · n 下一页"
		}
	}
	if m.editing != "" {
		help = "输入（Enter 确认，Esc 取消）：" + m.query.View()
	}
	if m.stage == "result" {
		m.view.SetContent(ansi.Wrap(body, max(10, m.view.Width), ""))
		body = m.view.View()
	}
	return header + "\n" + body + "\n" + safeText(m.note) + "\n" + help + "\n"
}

func (a *App) runImportWizard(ctx context.Context, client *api.Client, collection, initial string) error {
	if a.teachingInput == nil || a.teachingOutput == nil {
		return commandError("not_a_terminal", "全屏输入输出不可用", "使用 import preview/confirm", ExitInput)
	}
	key := a.learningSpace
	if a.importDrafts == nil {
		a.importDrafts = map[string]*importDraft{}
	}
	draft := a.importDrafts[key]
	if draft == nil {
		draft = &importDraft{space: a.learningSpace, spaceName: a.learningSpaceName, path: initial}
		draft.collection.ID = collection
		a.importDrafts[key] = draft
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	m := &importModel{draft: draft, client: client, ctx: child, cancel: cancel, newID: a.NewUUID, stage: "target", width: 80, height: 24, view: viewport.New(76, 15), query: textinput.New(), paste: textarea.New()}
	m.paste.CharLimit = importer.MaxDocumentSize
	m.paste.SetValue(draft.paste)
	for _, v := range []string{draft.path, draft.include, draft.exclude} {
		input := textinput.New()
		input.CharLimit = 4096
		input.SetValue(v)
		m.inputs = append(m.inputs, input)
	}
	m.inputs[0].Focus()
	_, err := tea.NewProgram(m, tea.WithContext(child), tea.WithInput(a.teachingInput), tea.WithOutput(a.teachingOutput), tea.WithAltScreen()).Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	if m.navigate == "goal" && draft.result != nil {
		// 只冻结资料上下文；目标由既有目标管理入口显式创建或编辑。
		id, idErr := a.NewUUID()
		if idErr != nil {
			return idErr
		}
		var entries []api.KnowledgeScopeEntry
		if draft.result.Summary != nil {
			for _, documentID := range draft.result.Summary.DocumentIDs {
				entries = append(entries, api.KnowledgeScopeEntry{CollectionID: draft.collection.ID, RevisionID: draft.result.Revision.RevisionID, DocumentID: documentID})
			}
		}
		if len(entries) == 0 {
			return commandError("invalid_import_result", "提交回执缺少资料上下文", "请在资料页选择范围", ExitUnavailable)
		}
		scope, scopeErr := client.WithLearningSpace(draft.space).FreezeKnowledgeScope(ctx, api.KnowledgeScopeSnapshot{ID: id, Entries: entries})
		if scopeErr != nil {
			return mapAPIError(scopeErr)
		}
		_, _ = fmt.Fprintf(a.Out, "本次资料上下文：%s\n", scope.ID)
		local := *a
		local.learningSpace, local.learningSpaceName = draft.space, draft.spaceName
		local.importedScope = scope.ID
		return local.runGoalManagement(ctx, []string{"browse"})
	}
	return nil
}
