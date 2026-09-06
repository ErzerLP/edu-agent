package agentui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

func TestFileArtifactRejectsStaleLoadTokensAndClearsNavigationPage(t *testing.T) {
	m, c := fileArtifactModel(t)
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m, cmd = fileArtifactKey(m, tea.KeyPgDown)
	old := cmd().(localArtifactMsg)
	m, current := fileArtifactKey(m, tea.KeyDown)
	if len(m.artifactPanel.page.Data) != 0 || m.artifactPanel.page.NextOffset != 0 || m.artifactPanel.pageReady {
		t.Fatal("switch left an old body/cursor usable")
	}
	m, skipped := fileArtifactKey(m, tea.KeyPgDown)
	if skipped != nil || m.artifactPanel.cursor().offset != 0 {
		t.Fatal("repeated paging skipped bytes using stale NextOffset")
	}
	updated, _ := m.Update(old)
	m = updated.(model)
	if len(m.artifactPanel.page.Data) != 0 {
		t.Fatal("old request populated a different artifact")
	}
	message := current().(localArtifactMsg)
	for _, field := range []string{"generation", "epoch", "request"} {
		stale := message
		switch field {
		case "generation":
			stale.generation++
		case "epoch":
			stale.epoch++
		case "request":
			stale.request++
		}
		updated, _ = m.Update(stale)
		m = updated.(model)
		if len(m.artifactPanel.page.Data) != 0 {
			t.Fatalf("accepted wrong %s", field)
		}
	}
	updated, _ = m.Update(message)
	m = updated.(model)
	if !m.artifactPanel.pageReady || m.artifactPanel.id != "receipt_b" {
		t.Fatal("current reply rejected")
	}
	m, _ = fileArtifactKey(m, tea.KeyEsc)
	m, current = fileArtifactKey(m, tea.KeyF6)
	updated, _ = m.Update(message)
	m = updated.(model)
	if len(m.artifactPanel.items) != 0 {
		t.Fatal("closed epoch contaminated reopened catalog")
	}
	m = fileArtifactApply(t, m, current)
	m, current = fileArtifactRune(m, "r")
	message = current().(localArtifactMsg)
	m.resetAfterSessionSwap(m.generation + 1)
	updated, _ = m.Update(message)
	m = updated.(model)
	if m.artifactPanel != nil || m.pendingFileMutation != nil || c.sends != 0 {
		t.Fatal("session reset restored old panel or model state")
	}
}

func TestFileArtifactRejectsStaleSearchTokensCloseAndRefresh(t *testing.T) {
	m, _ := fileArtifactModel(t)
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m, _ = fileArtifactRune(m, "/")
	m, _ = fileArtifactRune(m, "[x].*")
	m, cmd = fileArtifactKey(m, tea.KeyEnter)
	message := cmd().(localArtifactSearchMsg)
	for _, field := range []string{"generation", "epoch", "request", "id"} {
		stale := message
		switch field {
		case "generation":
			stale.generation++
		case "epoch":
			stale.epoch++
		case "request":
			stale.request++
		case "id":
			stale.id = "receipt_b"
		}
		updated, _ := m.Update(stale)
		m = updated.(model)
		if m.artifactPanel.cursor().searchNext != 0 || !m.artifactPanel.searching {
			t.Fatalf("accepted search with wrong %s", field)
		}
	}
	m, cmd = fileArtifactRune(m, "r")
	updated, late := m.Update(message)
	m = updated.(model)
	if late != nil || m.artifactPanel.cursor().searchNext != 0 {
		t.Fatal("refresh accepted an older search")
	}
	m = fileArtifactApply(t, m, cmd)
	m, _ = fileArtifactKey(m, tea.KeyEsc)
	m, cmd = fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	updated, late = m.Update(message)
	m = updated.(model)
	if late != nil || m.artifactPanel.cursor().searched || m.artifactPanel.page.Offset != 0 {
		t.Fatal("reopened panel accepted old search")
	}
}

func TestFileArtifactBoundedSearchNoMatchEndAndErrors(t *testing.T) {
	m, c := fileArtifactModel(t)
	c.bodies["diff_a"] = []byte(strings.Repeat("x", 140))
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m, _ = fileArtifactRune(m, "/")
	m, _ = fileArtifactRune(m, "missing")
	m, cmd = fileArtifactKey(m, tea.KeyEnter)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.cursor().searchNext != 64 || !strings.Contains(m.View(), "尚未扫描完") {
		t.Fatal("bounded no-match window was mistaken for EOF")
	}
	for _, next := range []int64{128, 140} {
		m, cmd = fileArtifactRune(m, "n")
		m = fileArtifactApply(t, m, cmd)
		if m.artifactPanel.cursor().searchNext != next {
			t.Fatal("no-match scan did not continue")
		}
	}
	m, cmd = fileArtifactRune(m, "n")
	if cmd != nil || !strings.Contains(m.View(), "检索已到末尾") || len(c.searches) != 3 {
		t.Fatal("immutable artifact search restarted after EOF")
	}
	for _, failure := range []error{errors.New("raw /private/secret"), &localartifact.Error{Code: "artifact_corrupt"}} {
		c.searchErr = failure
		m, _ = fileArtifactRune(m, "/")
		m, _ = fileArtifactRune(m, "new")
		m, cmd = fileArtifactKey(m, tea.KeyEnter)
		m = fileArtifactApply(t, m, cmd)
		if !strings.Contains(m.View(), "检索失败："+localArtifactError(failure)) || strings.Contains(m.View(), "secret") || m.artifactPanel.cursor().searchNext != 0 {
			t.Fatal("search error leaked raw detail or advanced a cursor")
		}
	}
}

func TestFileArtifactRefreshPreservesSelectedOffsetAndReturnsToComposer(t *testing.T) {
	m, c := fileArtifactModel(t)
	m.input.SetValue("keep this draft")
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m, cmd = fileArtifactKey(m, tea.KeyPgDown)
	m = fileArtifactApply(t, m, cmd)
	m, cmd = fileArtifactRune(m, "r")
	m = fileArtifactApply(t, m, cmd)
	if c.lists != 2 || m.artifactPanel.page.Offset != 4096 {
		t.Fatal("refresh reset current artifact offset")
	}
	m, cmd = fileArtifactKey(m, tea.KeyPgUp)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 0 {
		t.Fatal("PgUp did not subtract one raw-byte page")
	}
	m, cmd = fileArtifactKey(m, tea.KeyPgDown)
	m = fileArtifactApply(t, m, cmd)
	m, cmd = fileArtifactKey(m, tea.KeyHome)
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.page.Offset != 0 {
		t.Fatal("Home did not return to byte zero")
	}
	c.items = c.items[1:]
	m, cmd = fileArtifactRune(m, "r")
	m = fileArtifactApply(t, m, cmd)
	if m.artifactPanel.id != "receipt_b" || m.artifactPanel.page.Offset != 0 {
		t.Fatal("refresh did not select the first available replacement")
	}
	m, _ = fileArtifactKey(m, tea.KeyEsc)
	if !m.input.Focused() || m.input.Value() != "keep this draft" || c.sends != 0 {
		t.Fatal("browser did not return to the unchanged composer")
	}
}

func TestFileArtifactMutualExclusionAndGlobalExit(t *testing.T) {
	m, _ := fileArtifactModel(t)
	m, cmd := fileArtifactKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, cmd)
	m, _ = fileArtifactKey(m, tea.KeyF6)
	if m.taskPanel == nil || m.artifactPanel != nil {
		t.Fatal("F6 opened on top of F5")
	}
	m, _ = fileArtifactKey(m, tea.KeyF5)
	m, cmd = fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m, _ = fileArtifactKey(m, tea.KeyF5)
	m, _ = fileArtifactKey(m, tea.KeyF2)
	if m.taskPanel != nil || m.sessionPicker != nil || m.artifactPanel == nil {
		t.Fatal("another panel opened on top of artifacts")
	}
	for _, key := range []tea.KeyType{tea.KeyCtrlC, tea.KeyCtrlQ} {
		value, c := fileArtifactModel(t)
		pending := &agentloop.PendingFileMutation{CallID: "pending", DiffID: "diff_a"}
		value.pendingFileMutation = pending
		value, cmd = fileArtifactKey(value, tea.KeyF6)
		value = fileArtifactApply(t, value, cmd)
		value, _ = fileArtifactRune(value, "/")
		value, cmd = fileArtifactKey(value, key)
		if cmd == nil || value.ctx.Err() == nil || c.cancelledFileCall != "" || c.sends != 0 {
			t.Fatal("global quit failed or performed a pending action")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatal("global exit did not return tea.Quit")
		}
	}
}

func TestFileArtifactDoesNotAlterExistingApprovalReviewRules(t *testing.T) {
	for _, operation := range []string{"write", "edit", "patch", "copy", "move", "mkdir"} {
		t.Run(operation, func(t *testing.T) {
			m, _ := fileArtifactModel(t)
			pending := &agentloop.PendingFileMutation{Operation: operation, DiffID: "diff_a", Preview: strings.Repeat("frozen preview\n", 60)}
			m.pendingFileMutation = pending
			m.selector = newFileMutationSelector(pending)
			selector := m.selector
			_ = m.renderSelector(40)
			page, rendered := selector.copyPage, selector.copyPageRendered
			m, cmd := fileArtifactKey(m, tea.KeyF6)
			m = fileArtifactApply(t, m, cmd)
			m, cmd = fileArtifactKey(m, tea.KeyPgDown)
			m = fileArtifactApply(t, m, cmd)
			m, _ = fileArtifactKey(m, tea.KeyEsc)
			if m.selector != selector || selector.copyPage != page || selector.copyPageRendered != rendered {
				t.Fatal("F6 changed selector review state")
			}
			action, _ := selector.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
			legacyReview := operation == "copy" || operation == "move" || operation == "mkdir"
			if legacyReview && action.kind == selectorSubmit {
				t.Fatal("F6 bypassed a legacy review gate")
			}
			if !legacyReview && (action.kind != selectorSubmit || action.fileResolution != agentloop.FileMutationApprove) {
				t.Fatal("F6 introduced a new final-page gate")
			}
		})
	}
}

func TestFileArtifactPreservesOtherPendingInteractionsAndBusyCompletion(t *testing.T) {
	m, _ := fileArtifactModel(t)
	m.pending = &agentloop.PreferenceConfirmation{Content: "original preference"}
	m.selector = newPreferenceSelector(m.pending)
	m.selector.focus = 1
	original := m.selector
	m, cmd := fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m, _ = fileArtifactKey(m, tea.KeyEsc)
	if m.selector != original || m.selector.focus != 1 || m.pending.Content != "original preference" {
		t.Fatal("F6 replaced preference selection")
	}
	m.pending, m.selector = nil, nil
	m.busy = true
	m, cmd = fileArtifactKey(m, tea.KeyF6)
	m = fileArtifactApply(t, m, cmd)
	m.finishTurn(agentloop.Result{}, nil)
	if m.input.Focused() || m.artifactPanel == nil {
		t.Fatal("busy completion stole browser focus")
	}
	m, _ = fileArtifactKey(m, tea.KeyEsc)
	if !m.input.Focused() {
		t.Fatal("closing after busy completion did not restore composer")
	}
	m.sessionPicker = newSessionPickerModel(false)
	picker := m.sessionPicker
	m, _ = fileArtifactKey(m, tea.KeyF6)
	if m.sessionPicker != picker || m.artifactPanel != nil {
		t.Fatal("F6 opened over the session picker")
	}
}
