package agentui

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type fileArtifactRead struct {
	id     string
	offset int64
	limit  int
}

type fileArtifactSearch struct {
	fileArtifactRead
	query string
}

type fileArtifactConversation struct {
	*taskPanelConversation
	items                       []localartifact.Info
	bodies                      map[string][]byte
	reads                       []fileArtifactRead
	searches                    []fileArtifactSearch
	lists, sends, resolutions   int
	chunk                       int
	listErr, readErr, searchErr error
}

func (c *fileArtifactConversation) Send(ctx context.Context, text string) (agentloop.Result, error) {
	c.sends++
	return c.fakeConversation.Send(ctx, text)
}
func (c *fileArtifactConversation) ResolveFileMutation(ctx context.Context, id string, resolution agentloop.FileMutationResolution) (agentloop.Result, error) {
	c.resolutions++
	return c.fakeConversation.ResolveFileMutation(ctx, id, resolution)
}
func (c *fileArtifactConversation) LocalArtifacts() ([]localartifact.Info, error) {
	c.lists++
	return append([]localartifact.Info(nil), c.items...), c.listErr
}
func (c *fileArtifactConversation) ReadLocalArtifact(_ context.Context, id string, offset int64, limit int) (localartifact.Page, error) {
	c.reads = append(c.reads, fileArtifactRead{id, offset, limit})
	if c.readErr != nil {
		return localartifact.Page{}, c.readErr
	}
	body := c.bodies[id]
	if offset < 0 || offset > int64(len(body)) {
		return localartifact.Page{}, &localartifact.Error{Code: "artifact_invalid_arguments"}
	}
	if c.chunk > 0 {
		limit = min(limit, c.chunk)
	}
	end := min(offset+int64(limit), int64(len(body)))
	return localartifact.Page{Info: c.info(id), Data: body[offset:end], Offset: offset, NextOffset: end, More: end < int64(len(body))}, nil
}
func (c *fileArtifactConversation) info(id string) localartifact.Info {
	for _, info := range c.items {
		if info.ID == id {
			return info
		}
	}
	return localartifact.Info{}
}
func (c *fileArtifactConversation) SearchLocalArtifact(_ context.Context, id, query string, offset int64, limit int) (localartifact.SearchPage, error) {
	c.searches = append(c.searches, fileArtifactSearch{fileArtifactRead{id, offset, limit}, query})
	if c.searchErr != nil {
		return localartifact.SearchPage{}, c.searchErr
	}
	body := c.bodies[id]
	end := min(offset+64, int64(len(body)))
	page := localartifact.SearchPage{Info: c.info(id), Offset: offset, NextOffset: end, Scanned: end - offset, More: end < int64(len(body))}
	if index := strings.Index(string(body[offset:end]), query); index >= 0 {
		match := offset + int64(index)
		page.Offsets, page.NextOffset = []int64{match}, match+1
		page.More = page.NextOffset < int64(len(body))
	}
	return page, nil
}

func fileArtifactModel(t *testing.T) (model, *fileArtifactConversation) {
	t.Helper()
	c := &fileArtifactConversation{
		taskPanelConversation: &taskPanelConversation{fakeConversation: &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Available: true}, fileMode: agentloop.FileAuthorizationYOLO}, tasks: []localexec.Snapshot{{TaskID: "task_a", State: "running"}}},
		bodies: map[string][]byte{
			"diff_a":    []byte(strings.Repeat("a", 17) + "[x].*" + strings.Repeat("a", 51) + "[x].*" + strings.Repeat("line\n", 2000)),
			"receipt_b": []byte(strings.Repeat("b", 31) + "[x].*" + strings.Repeat("line\n", 2000)),
		},
	}
	for i, id := range []string{"diff_a", "receipt_b"} {
		kind := "diff"
		if i == 1 {
			kind = "receipt"
		}
		c.items = append(c.items, localartifact.Info{ID: id, Kind: kind, Hash: fmt.Sprintf("%x", sha256.Sum256(c.bodies[id])), Bytes: int64(len(c.bodies[id])), Saved: i == 1})
	}
	m := newModel(t.Context(), c, "model")
	m.generation = 7
	t.Cleanup(m.cancel)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
	return updated.(model), c
}

func fileArtifactKey(m model, key tea.KeyType) (model, tea.Cmd) {
	updated, cmd := m.Update(tea.KeyMsg{Type: key})
	return updated.(model), cmd
}
func fileArtifactRune(m model, text string) (model, tea.Cmd) {
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
	return updated.(model), cmd
}
func fileArtifactApply(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing artifact command")
	}
	for steps := 0; cmd != nil; steps++ {
		if steps == 4 {
			t.Fatal("artifact browsing must not poll or start a model turn")
		}
		updated, next := m.Update(cmd())
		m, cmd = updated.(model), next
	}
	return m
}

func TestFileArtifactErrorsDoNotMasqueradeAsEmptyHistory(t *testing.T) {
	m, c := fileArtifactModel(t)
	c.listErr = fmt.Errorf("backend /private/secret: %w", &localartifact.Error{Code: "artifact_corrupt"})
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	view := m.View()
	if !strings.Contains(view, "目录读取失败：artifact_corrupt") || strings.Contains(view, "无可访问产物") || strings.Contains(view, "private") {
		t.Fatalf("directory error was hidden or leaked: %s", view)
	}
	c.listErr, c.items = nil, nil
	m, cmd = fileArtifactRune(m, "r")
	m = fileArtifactApply(t, m, cmd)
	if !strings.Contains(m.View(), "无可访问产物") || strings.Contains(m.View(), "目录读取失败") {
		t.Fatal("successful empty catalog is not distinct from failure")
	}
	c.items = []localartifact.Info{{ID: "diff_a", Kind: "diff"}}
	c.readErr = errors.New("/private/path body secret")
	m, cmd = fileArtifactRune(m, "r")
	m = fileArtifactApply(t, m, cmd)
	if !strings.Contains(m.View(), "读取失败：artifact_unavailable") || strings.Contains(m.View(), "secret") || m.artifactPanel.pageReady {
		t.Fatal("read failure leaked details or retained an old page")
	}
	c.readErr = &localartifact.Error{Code: "artifact_version_unsupported"}
	m, cmd = fileArtifactKey(m, tea.KeyHome)
	m = fileArtifactApply(t, m, cmd)
	if !strings.Contains(m.View(), "artifact_version_unsupported") {
		t.Fatal("stable read error missing")
	}
}

func TestFileArtifactQueryPasteIsBoundedAndUTF8Safe(t *testing.T) {
	m, c := fileArtifactModel(t)
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m, _ = fileArtifactRune(m, "/")
	for _, invalid := range []string{"safe\x1b[2J", "safe\n", "safe\u202e", strings.Repeat("查", 171)} {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(invalid), Paste: true})
		m = updated.(model)
		if m.artifactPanel.queryDraft != "" || !strings.Contains(m.View(), "检索输入已拒绝") {
			t.Fatal("invalid paste was silently filtered or truncated")
		}
	}
	valid := strings.Repeat("查", 170) + "ab"
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(valid), Paste: true})
	m = updated.(model)
	if len(m.artifactPanel.queryDraft) != 512 || !utf8.ValidString(m.artifactPanel.queryDraft) {
		t.Fatal("valid 512-byte paste was rejected")
	}
	for range 3 {
		m, _ = fileArtifactKey(m, tea.KeyBackspace)
	}
	if len(m.artifactPanel.queryDraft) != 507 || !utf8.ValidString(m.artifactPanel.queryDraft) {
		t.Fatal("backspace split a rune")
	}
	m, _ = fileArtifactKey(m, tea.KeyEsc)
	if m.artifactPanel != nil || m.ctx.Err() != nil || c.sends != 0 || len(c.searches) != 0 {
		t.Fatal("Esc in query mode did not close without effects")
	}
}

func TestFileArtifactSafeMetadataBodyAndActualViewportResize(t *testing.T) {
	m, c := fileArtifactModel(t)
	unsafe := "\x1b]52;clipboard\a\x1b[2J\r\x00\u202e"
	c.bodies["diff_a"] = []byte(unsafe + "中文\n" + strings.Repeat("long content\n", 600))
	c.items[0].Kind += unsafe
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	for _, size := range []tea.WindowSizeMsg{{Width: 46, Height: 18}, {Width: 140, Height: 48}} {
		updated, _ := m.Update(size)
		m = updated.(model)
		p := m.artifactPanel
		view := m.View()
		if lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height {
			t.Fatalf("artifact view overflow at %+v: %dx%d", size, lipgloss.Width(view), lipgloss.Height(view))
		}
		if p.output.Height != size.Height-len(p.header(p.output.Width))-2 || p.output.Width != size.Width-4 {
			t.Fatal("viewport actual dimensions differ from rendered dimensions")
		}
		for _, control := range []string{"\x1b]52", "\x1b[2J", "\r", "\x00", "\u202e"} {
			if strings.Contains(view, control) {
				t.Fatalf("unsafe terminal content %q", control)
			}
		}
		before := p.cursor().offset
		for range 1000 {
			updated, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
			m = updated.(model)
		}
		if !p.output.AtBottom() || p.cursor().offset != before {
			t.Fatal("presentation scrolling cannot reach EOF or moved the byte cursor")
		}
	}
	if c.sends != 0 {
		t.Fatal("rendering or resizing invoked the model")
	}
}

func TestFileArtifactBrowsingWhileBusyAndPendingNeverSendsOrAuthorizes(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			m, c := fileArtifactModel(t)
			pending := &agentloop.PendingFileMutation{CallID: "frozen", Operation: "patch", DiffID: "receipt_b", DiffBytes: c.items[1].Bytes, DiffSaved: true}
			m.pendingFileMutation, m.pendingFileTurnID = pending, 91
			m.selector = newFileMutationSelector(pending)
			selector := m.selector
			m.busy, m.activeCancelable = busy, busy
			m.input.SetValue("original draft")
			m, cmd := fileArtifactKey(m, tea.KeyF6)
			m = fileArtifactApply(t, m, cmd)
			if m.artifactPanel.id != pending.DiffID || len(c.reads) != 1 || c.reads[0].limit != 4096 {
				t.Fatal("pending artifact was not selected/read directly")
			}
			for _, text := range []string{"receipt", "receipt_b", "完整字节=", c.items[1].Hash, "saved", "已加密保存", "diff不代表修改已执行", "逐项事实", "offset=0", "next_offset=4096", "more=true"} {
				if !strings.Contains(m.View(), text) {
					t.Fatalf("missing %q in %s", text, m.View())
				}
			}
			m, cmd = fileArtifactKey(m, tea.KeyPgDown)
			m = fileArtifactApply(t, m, cmd)
			m, _ = fileArtifactRune(m, "/")
			m, _ = fileArtifactRune(m, "[x].*")
			m, cmd = fileArtifactKey(m, tea.KeyEnter)
			m = fileArtifactApply(t, m, cmd)
			if len(c.searches) != 1 || c.searches[0].query != "[x].*" || c.searches[0].offset != 0 || c.searches[0].limit != 1 || m.artifactPanel.page.Offset != 31 {
				t.Fatal("literal search used a page/model cursor")
			}
			m, cmd = fileArtifactKey(m, tea.KeyEnter)
			if cmd != nil {
				t.Fatal("Enter outside query mode performed an action")
			}
			m, _ = fileArtifactKey(m, tea.KeyF4)
			if c.sends != 0 || c.resolutions != 0 || c.cancelledFileCall != "" || m.busy != busy || c.FileAuthorizationMode() != agentloop.FileAuthorizationYOLO {
				t.Fatal("browsing changed model/authorization state")
			}
			m, _ = fileArtifactKey(m, tea.KeyF6)
			if m.pendingFileMutation != pending || m.selector != selector || m.pendingFileTurnID != 91 || m.input.Value() != "original draft" || m.ctx.Err() != nil {
				t.Fatal("closing did not restore the original pending interaction")
			}
			if !busy {
				// Deliberately never visited the final byte page.
				c.fileResolved = agentloop.Result{Text: "resolved"}
				m, cmd = fileArtifactKey(m, tea.KeyEnter)
				m = runTurn(t, m, cmd)
				if c.resolutions != 1 || c.fileResolution != agentloop.FileMutationApprove || c.fileCallID != "frozen" {
					t.Fatal("browser added an approval gate")
				}
			}
		})
	}
}

func TestFileArtifactIndependentOffsetsSearchContinuationAndTrueNextOffset(t *testing.T) {
	m, c := fileArtifactModel(t)
	m.busy = true
	c.chunk = 7
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.id != "diff_a" || !strings.Contains(m.View(), "仅内存") || !strings.Contains(m.View(), "退出不可恢复") {
		t.Fatal("first artifact/default memory state missing")
	}
	m, cmd = fileArtifactKey(m, tea.KeyPgDown)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 7 {
		t.Fatal("PgDn guessed 4096 instead of using returned NextOffset")
	}
	m, _ = fileArtifactRune(m, "/")
	m, _ = fileArtifactRune(m, "[x].*")
	m, cmd = fileArtifactKey(m, tea.KeyEnter)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.cursor().searchNext != 18 || m.artifactPanel.page.Offset != 17 {
		t.Fatal("first match cursor incorrect")
	}
	m, cmd = fileArtifactKey(m, tea.KeyDown)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 0 || m.artifactPanel.cursor().query != "" || m.artifactPanel.cursor().searchNext != 0 {
		t.Fatal("new artifact inherited another search/offset")
	}
	m, _ = fileArtifactRune(m, "/")
	m, _ = fileArtifactRune(m, "[x].*")
	m, cmd = fileArtifactKey(m, tea.KeyEnter)
	m = fileArtifactApply(t, m, cmd)
	m, cmd = fileArtifactKey(m, tea.KeyUp)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 17 || m.artifactPanel.cursor().searchNext != 18 {
		t.Fatal("artifact-local state was not restored")
	}
	m, cmd = fileArtifactRune(m, "n")
	m = fileArtifactApply(t, m, cmd)
	if c.searches[len(c.searches)-1].offset != 18 || m.artifactPanel.page.Offset != 73 {
		t.Fatal("n did not use the saved search continuation")
	}
	m, cmd = fileArtifactKey(m, tea.KeyPgDown)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 80 || m.artifactPanel.cursor().searchNext != 74 {
		t.Fatal("page navigation changed search continuation")
	}
	m, cmd = fileArtifactKey(m, tea.KeyPgUp)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 0 {
		t.Fatal("PgUp was not bounded raw-byte navigation")
	}
	m, cmd = fileArtifactKey(m, tea.KeyDown)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 31 || m.artifactPanel.cursor().searchNext != 32 {
		t.Fatal("second artifact continuation was lost")
	}
	if c.sends != 0 || !m.busy {
		t.Fatal("navigation affected the busy model")
	}
}
