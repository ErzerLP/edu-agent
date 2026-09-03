package agentui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontroller"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
)

type pickerConversation struct {
	*fakeConversation
	generation       uint64
	gate             agentcontroller.SwitchGate
	items            []agentcontroller.SessionListItem
	transcript       agentsession.TranscriptV1
	title            string
	outcomes         []agentcontroller.UnknownOutcome
	retried          string
	deleted          agentsession.DeleteTarget
	renamedID        string
	renamedTitle     string
	listRequests     []agentcontroller.SessionListRequest
	switchErr        error
	newErr           error
	switchCalls      int
	newCalls         int
	lastPlan         agentcontroller.SwitchPlan
	lastConfirmation agentcontroller.SwitchConfirmation
	planOverride     *agentcontroller.SwitchPlan
	nextTranscript   agentsession.TranscriptV1
	nextTitle        string
}

func (p *pickerConversation) Generation() uint64 { return p.generation }
func (p *pickerConversation) SwitchGate() agentcontroller.SwitchGate {
	return p.gate
}
func (p *pickerConversation) ListSessions(_ context.Context, request agentcontroller.SessionListRequest) ([]agentcontroller.SessionListItem, error) {
	p.listRequests = append(p.listRequests, request)
	return append([]agentcontroller.SessionListItem(nil), p.items...), nil
}
func (p *pickerConversation) PlanSwitch(summary agentsession.Summary) agentcontroller.SwitchPlan {
	if p.planOverride != nil {
		return *p.planOverride
	}
	return agentcontroller.SwitchPlan{SessionID: summary.SessionID, Title: summary.Title, ExpectedRevision: summary.RecordRevision}
}
func (p *pickerConversation) RenameSession(_ context.Context, sessionID, title string, _ uint64) (agentsession.Summary, error) {
	p.renamedID, p.renamedTitle = sessionID, title
	return agentsession.Summary{SessionID: sessionID, Title: title}, nil
}
func (p *pickerConversation) DeleteSession(_ context.Context, target agentsession.DeleteTarget) error {
	p.deleted = target
	return nil
}
func (p *pickerConversation) CommitSwitch(_ context.Context, plan agentcontroller.SwitchPlan, confirmation agentcontroller.SwitchConfirmation) (uint64, error) {
	p.switchCalls++
	p.lastPlan, p.lastConfirmation = plan, confirmation
	if p.switchErr != nil {
		return 0, p.switchErr
	}
	p.generation++
	p.transcript = p.nextTranscript
	p.title = p.nextTitle
	p.fileMode = agentloop.FileAuthorizationConfirm
	if plan.NeedProviderConfirm && !confirmation.Provider {
		p.startupNotices = []string{"[session_provider_confirmation_required] 模型提供商端点已变更；确认前不会发送历史文本或生成标题"}
	} else {
		p.startupNotices = nil
	}
	return p.generation, nil
}
func (p *pickerConversation) NewSession(context.Context) (uint64, error) {
	p.newCalls++
	if p.newErr != nil {
		return 0, p.newErr
	}
	p.generation++
	p.transcript = agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{}}
	p.title = "新 Session"
	p.fileMode = agentloop.FileAuthorizationConfirm
	return p.generation, nil
}
func (p *pickerConversation) UnknownOutcomes() []agentcontroller.UnknownOutcome {
	return append([]agentcontroller.UnknownOutcome(nil), p.outcomes...)
}
func (p *pickerConversation) RetryPreferenceReceipt(_ context.Context, id string) error {
	p.retried = id
	p.outcomes = nil
	return nil
}
func (p *pickerConversation) SessionTranscript() agentsession.TranscriptV1 { return p.transcript }
func (p *pickerConversation) SessionTitle() string                         { return p.title }

func TestAgentUIF2GatePickerAndMinimumTerminal(t *testing.T) {
	blocked := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Code: agentcontroller.SwitchBlockActiveTurn, Reason: "当前轮次仍在运行"}}
	value := newModel(t.Context(), blocked, "model")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyF2})
	value = updated.(model)
	if command != nil || value.sessionPicker != nil || !strings.Contains(value.status, "仍在运行") {
		t.Fatalf("blocked F2 changed state: picker=%+v status=%q command=%v", value.sessionPicker, value.status, command)
	}

	manager := &pickerConversation{
		fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}, title: "当前标题",
		items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{
			SessionID: "11111111-1111-4111-8111-111111111111", Title: "手动复习", TitleSource: "manual", RecordRevision: 2,
			UpdatedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), CommittedUserTurns: 7,
			WorkspaceLabel: "/secret/private/project", ProviderEndpoint: "https://provider.example/v1",
		}}},
	}
	value = newModel(t.Context(), manager, "model")
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyF2})
	value = updated.(model)
	if command == nil || value.sessionPicker == nil {
		t.Fatal("idle F2 did not open picker")
	}
	updated, _ = value.Update(command())
	value = updated.(model)
	updated, _ = value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	view := value.View()
	if !strings.Contains(view, "Session 选择器") || !strings.Contains(view, "手动复习") || !strings.Contains(view, "Enter 恢复") || !strings.Contains(view, "Esc") || !strings.Contains(view, "返回") {
		t.Fatalf("minimum picker is not operable: %s", view)
	}
	for _, forbidden := range []string{"/secret/private", "provider.example", "https://"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("picker leaked %q: %s", forbidden, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("picker line width=%d: %q", lipgloss.Width(line), line)
		}
	}
}

func TestSessionPickerScopeSearchRenameDeleteAndConfirmationReducer(t *testing.T) {
	source := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}}
	picker := newSessionPickerModel(false)
	picker.setItems([]agentcontroller.SessionListItem{{Summary: agentsession.Summary{SessionID: "11111111-1111-4111-8111-111111111111", Title: "代数", RecordRevision: 4}}}, nil)
	if intent := picker.handleKey(tea.KeyMsg{Type: tea.KeyTab}, source); intent.kind != pickerIntentRefresh || !picker.scopeAll {
		t.Fatalf("Tab scope intent=%+v all=%t", intent, picker.scopeAll)
	}
	picker.selected = 1
	picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR}, source)
	if picker.mode != pickerRename {
		t.Fatal("Ctrl+R did not open rename")
	}
	picker.edit = ""
	if intent := picker.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, source); intent.kind != pickerIntentRename || intent.title != "" {
		t.Fatalf("empty auto-title reset intent=%+v", intent)
	}
	picker.mode = pickerList
	picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}, source)
	if picker.mode != pickerDeleteConfirm {
		t.Fatal("Ctrl+D did not enter confirmation")
	}
	if intent := picker.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, source); intent.kind != pickerIntentNone {
		t.Fatal("Enter bypassed permanent-delete confirmation")
	}
	if intent := picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}, source); intent.kind != pickerIntentDelete {
		t.Fatalf("second Ctrl+D intent=%+v", intent)
	}
	picker.mode = pickerList
	picker.plan = agentcontroller.SwitchPlan{}
	plan := agentcontroller.SwitchPlan{SessionID: "11111111-1111-4111-8111-111111111111", NeedProviderConfirm: true, NeedWorkspaceConfirm: true, WorkspaceLabel: "project"}
	picker.mode, picker.plan = pickerSwitchConfirm, plan
	if intent := picker.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}, source); intent.kind != pickerIntentSwitch || intent.confirmation.Provider || !intent.confirmation.Workspace {
		t.Fatalf("local-only confirmation intent=%+v", intent)
	}
	if intent := picker.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, source); intent.kind != pickerIntentSwitch || !intent.confirmation.Provider || !intent.confirmation.Workspace {
		t.Fatalf("confirmation intent=%+v", intent)
	}
}

func TestSessionPickerAllowsSafeCorruptDeleteAndRendersLocatorOnlyLabel(t *testing.T) {
	source := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}}
	locator := agentsession.Summary{StorageID: strings.Repeat("a", 32), Corrupt: true, LocatorOnly: true}
	picker := newSessionPickerModel(true)
	picker.setItems([]agentcontroller.SessionListItem{{Summary: locator}}, nil)
	picker.selected = 1
	picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}, source)
	if picker.mode != pickerDeleteConfirm {
		t.Fatal("unlocked locator-only corrupt item did not enter delete confirmation")
	}
	intent := picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}, source)
	if intent.kind != pickerIntentDelete {
		t.Fatalf("delete intent=%+v", intent)
	}
	target := deleteTargetForSummary(intent.item.Summary)
	if target.SessionID != "" || target.StorageID != locator.StorageID || target.ExpectedRecordRevision != 0 {
		t.Fatalf("delete target=%+v", target)
	}
	view := picker.render(46, 18)
	if !strings.Contains(view, "storage:aaaaaaaa") || strings.Contains(view, "01-01 00:00") {
		t.Fatalf("unsafe locator rendering: %s", view)
	}

	for _, item := range []agentcontroller.SessionListItem{
		{Summary: agentsession.Summary{StorageID: strings.Repeat("b", 32), Corrupt: true, LocatorOnly: true, Locked: true}},
		{Summary: agentsession.Summary{StorageID: strings.Repeat("c", 32), Corrupt: true, LocatorOnly: true}, Current: true},
		{Summary: agentsession.Summary{StorageID: strings.Repeat("d", 32), VersionUnsupported: true, Unavailable: true}},
	} {
		picker = newSessionPickerModel(true)
		picker.setItems([]agentcontroller.SessionListItem{item}, nil)
		picker.selected = 1
		picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}, source)
		if picker.mode != pickerList {
			t.Fatalf("blocked item entered delete confirmation: %+v", item)
		}
	}
}

func TestAgentUIGenerationFenceDropsLateTurnContextLearningAndPickerResults(t *testing.T) {
	manager := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 2, gate: agentcontroller.SwitchGate{Allowed: true}}
	value := newModel(t.Context(), manager, "model")
	value.activeTurnID = 7
	beforeEntries := len(value.entries)
	updated, _ := value.Update(turnMsg{generation: 1, turnID: 7, activity: &agentloop.Activity{Kind: agentloop.ActivityTextDelta, Delta: "late"}})
	value = updated.(model)
	if len(value.entries) != beforeEntries {
		t.Fatal("late turn delta crossed generation")
	}
	beforeContext := value.contextStatus
	updated, _ = value.Update(contextMsg{generation: 1, event: agentloop.ContextEvent{Status: agentloop.ContextStatus{WindowPercent: 99}}})
	value = updated.(model)
	if value.contextStatus != beforeContext {
		t.Fatal("late context crossed generation")
	}
	updated, _ = value.Update(learningMsg{generation: 1, status: agentloop.LearningStatus{Active: true}})
	value = updated.(model)
	if value.learningStatus.Active {
		t.Fatal("late learning refresh crossed generation")
	}
	value.sessionPicker = newSessionPickerModel(false)
	updated, _ = value.Update(pickerListMsg{generation: 1, items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{Title: "late"}}}})
	value = updated.(model)
	if len(value.sessionPicker.items) != 0 {
		t.Fatal("late picker result crossed generation")
	}
}

func openLoadedPicker(t *testing.T, value model) model {
	t.Helper()
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyF2})
	value = updated.(model)
	if command == nil || value.sessionPicker == nil {
		t.Fatalf("F2 did not open picker: command=%v picker=%+v status=%q", command, value.sessionPicker, value.status)
	}
	updated, _ = value.Update(command())
	return updated.(model)
}

func TestAgentUIF2SwitchGateMatrixUsesStableRefusal(t *testing.T) {
	tests := []struct {
		code   string
		reason string
	}{
		{agentcontroller.SwitchBlockActiveTurn, "当前轮次仍在运行或停止中"},
		{agentcontroller.SwitchBlockPendingQuestion, "当前 Session 正在等待问题选择"},
		{agentcontroller.SwitchBlockPendingPreference, "当前 Session 正在等待长期偏好确认"},
		{agentcontroller.SwitchBlockPendingFile, "当前 Session 正在等待文件修改授权"},
		{agentcontroller.SwitchBlockUnknownOutcome, "先核对未知偏好结果；未知文件结果需重新读取、预览并授权"},
		{agentcontroller.SwitchBlockSaving, "当前 Session 正在保存"},
		{agentcontroller.SwitchBlockSwitching, "Session 正在切换"},
		{agentcontroller.SwitchBlockClosing, "Session 正在关闭"},
		{agentcontroller.SwitchBlockUnsaved, "当前 Session 未启用历史存储"},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			manager := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 4, gate: agentcontroller.SwitchGate{Code: test.code, Reason: test.reason}}
			value := newModel(t.Context(), manager, "model")
			updated, command := value.Update(tea.KeyMsg{Type: tea.KeyF2})
			value = updated.(model)
			if command != nil || value.sessionPicker != nil || value.status != test.reason || value.generation != 4 {
				t.Fatalf("gate=%s command=%v picker=%+v status=%q generation=%d", test.code, command, value.sessionPicker, value.status, value.generation)
			}
		})
	}
}

func TestAgentUISwitchFailurePreservesCurrentPresentationAndGeneration(t *testing.T) {
	failures := []struct {
		name string
		err  error
	}{
		{"target preflight", agentsession.ErrCorrupt},
		{"target lock", agentsession.ErrInUse},
		{"save before switch", agentsession.ErrCheckpointSaveFailed},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			manager := &pickerConversation{
				fakeConversation: &fakeConversation{fileMode: agentloop.FileAuthorizationYOLO}, generation: 7,
				gate: agentcontroller.SwitchGate{Allowed: true}, switchErr: failure.err, title: "当前标题",
				transcript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{{Sequence: 1, Kind: agentsession.TranscriptKindUser, Text: "当前正文"}}},
				items:      []agentcontroller.SessionListItem{{Summary: agentsession.Summary{SessionID: "11111111-1111-4111-8111-111111111111", Title: "目标", RecordRevision: 3}}},
			}
			value := openLoadedPicker(t, newModel(t.Context(), manager, "model"))
			updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyDown})
			value = updated.(model)
			updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
			value = updated.(model)
			if command == nil {
				t.Fatal("switch command missing")
			}
			updated, _ = value.Update(command())
			value = updated.(model)
			failureStatus := value.status
			updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
			value = updated.(model)
			if value.generation != 7 || manager.generation != 7 || value.sessionPicker != nil || manager.switchCalls != 1 || !strings.Contains(value.View(), "当前正文") || failureStatus != "Session 操作失败；当前 Session 保持不变" || manager.fileMode != agentloop.FileAuthorizationYOLO {
				t.Fatalf("generation=%d manager=%d picker=%+v calls=%d failureStatus=%q mode=%q view=%s", value.generation, manager.generation, value.sessionPicker, manager.switchCalls, failureStatus, manager.fileMode, value.View())
			}
		})
	}
}

func TestAgentUISwitchAndNewSessionInstallTargetAndClearTransientState(t *testing.T) {
	t.Run("switch", func(t *testing.T) {
		manager := &pickerConversation{
			fakeConversation: &fakeConversation{fileMode: agentloop.FileAuthorizationYOLO}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}, title: "旧标题",
			transcript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{{Sequence: 1, Kind: agentsession.TranscriptKindUser, Text: "旧正文"}}},
			nextTitle:  "目标标题", nextTranscript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{{Sequence: 1, Kind: agentsession.TranscriptKindUser, Text: "目标正文"}}},
			items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{SessionID: "22222222-2222-4222-8222-222222222222", Title: "目标标题", RecordRevision: 5}}},
		}
		value := openLoadedPicker(t, newModel(t.Context(), manager, "model"))
		value.pending = &agentloop.PreferenceConfirmation{Content: "旧偏好"}
		value.pendingQuestion = testQuestion(agentloop.QuestionSingle)
		value.pendingFileMutation = &agentloop.PendingFileMutation{CallID: "old"}
		value.input.SetValue("旧草稿")
		updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyDown})
		value = updated.(model)
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = updated.(model)
		updated, _ = value.Update(command())
		value = updated.(model)
		if value.generation != 2 || value.sessionPicker != nil || value.pending != nil || value.pendingQuestion != nil || value.pendingFileMutation != nil || value.selector != nil || value.input.Value() != "" || manager.fileMode != agentloop.FileAuthorizationConfirm || !strings.Contains(value.View(), "目标正文") || strings.Contains(value.View(), "旧正文") {
			t.Fatalf("generation=%d picker=%+v pending=%+v question=%+v file=%+v input=%q mode=%q view=%s", value.generation, value.sessionPicker, value.pending, value.pendingQuestion, value.pendingFileMutation, value.input.Value(), manager.fileMode, value.View())
		}
	})

	t.Run("new session from picker", func(t *testing.T) {
		manager := &pickerConversation{fakeConversation: &fakeConversation{fileMode: agentloop.FileAuthorizationYOLO}, generation: 3, gate: agentcontroller.SwitchGate{Allowed: true}, title: "旧标题", transcript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{{Kind: agentsession.TranscriptKindUser, Text: "旧正文"}}}}
		value := openLoadedPicker(t, newModel(t.Context(), manager, "model"))
		value.input.SetValue("旧草稿")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = updated.(model)
		updated, _ = value.Update(command())
		value = updated.(model)
		if manager.newCalls != 1 || value.generation != 4 || value.sessionPicker != nil || value.input.Value() != "" || manager.fileMode != agentloop.FileAuthorizationConfirm || strings.Contains(value.View(), "旧正文") {
			t.Fatalf("newCalls=%d generation=%d picker=%+v input=%q mode=%q view=%s", manager.newCalls, value.generation, value.sessionPicker, value.input.Value(), manager.fileMode, value.View())
		}
	})
}

func TestAgentUIPickerProviderLocalOpenAndFullConfirmationUseDistinctActions(t *testing.T) {
	for _, test := range []struct {
		name          string
		plan          agentcontroller.SwitchPlan
		wantWorkspace bool
	}{
		{"provider only", agentcontroller.SwitchPlan{SessionID: "target", NeedProviderConfirm: true}, false},
		{"provider and workspace", agentcontroller.SwitchPlan{SessionID: "target", NeedProviderConfirm: true, NeedWorkspaceConfirm: true, WorkspaceLabel: "project"}, true},
	} {
		t.Run(test.name+" local open", func(t *testing.T) {
			plan := test.plan
			manager := &pickerConversation{
				fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}, planOverride: &plan,
				nextTitle: "目标", nextTranscript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{{Kind: agentsession.TranscriptKindUser, Text: "本地历史正文"}}},
				items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{SessionID: "target", Title: "目标", RecordRevision: 3}}},
			}
			value := openLoadedPicker(t, newModel(t.Context(), manager, "model"))
			updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyDown})
			value = updated.(model)
			updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
			value = updated.(model)
			if command != nil || value.sessionPicker.mode != pickerSwitchConfirm {
				t.Fatalf("confirmation not shown: command=%v picker=%+v", command, value.sessionPicker)
			}
			confirmView := value.View()
			if !strings.Contains(confirmView, "仅本地打开") || !strings.Contains(confirmView, "拒绝向新 provider 发送") || !strings.Contains(confirmView, "Esc 取消切换") {
				t.Fatalf("local-open action was not explicit: %s", confirmView)
			}
			updated, command = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
			value = updated.(model)
			if command == nil {
				t.Fatal("local-open action did not start CommitSwitch")
			}
			updated, _ = value.Update(command())
			value = updated.(model)
			if manager.switchCalls != 1 || manager.lastConfirmation.Provider || manager.lastConfirmation.Workspace != test.wantWorkspace || value.generation != 2 || value.sessionPicker != nil {
				t.Fatalf("calls=%d confirmation=%+v generation=%d picker=%+v", manager.switchCalls, manager.lastConfirmation, value.generation, value.sessionPicker)
			}
			view := value.View()
			if !strings.Contains(view, "本地历史正文") || !strings.Contains(view, "session_provider_confirmation_required") {
				t.Fatalf("locally installed provider-blocked Session was not visible: %s", view)
			}

			manager.items = []agentcontroller.SessionListItem{
				{Current: true, Summary: agentsession.Summary{SessionID: "target", Title: "目标", RecordRevision: 4}},
				{Summary: agentsession.Summary{SessionID: "other", Title: "可删除", RecordRevision: 2}},
			}
			value = openLoadedPicker(t, value)
			updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyDown})
			value = updated.(model)
			updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
			value = updated.(model)
			if value.sessionPicker.mode != pickerRename {
				t.Fatal("provider-blocked Session could not enter rename")
			}
			value.sessionPicker.edit = "本地重命名"
			updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
			value = updated.(model)
			updated, _ = value.Update(command())
			value = updated.(model)
			if manager.renamedID != "target" || manager.renamedTitle != "本地重命名" {
				t.Fatalf("rename=%s %q", manager.renamedID, manager.renamedTitle)
			}
			updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyDown})
			value = updated.(model)
			updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
			value = updated.(model)
			updated, command = value.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
			value = updated.(model)
			updated, _ = value.Update(command())
			value = updated.(model)
			if manager.deleted.SessionID != "other" {
				t.Fatalf("delete target=%+v", manager.deleted)
			}
		})

		t.Run(test.name+" full confirmation", func(t *testing.T) {
			plan := test.plan
			manager := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}, planOverride: &plan, nextTitle: "目标", items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{SessionID: "target", Title: "目标"}}}}
			value := openLoadedPicker(t, newModel(t.Context(), manager, "model"))
			updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyDown})
			value = updated.(model)
			updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
			value = updated.(model)
			updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
			value = updated.(model)
			updated, _ = value.Update(command())
			value = updated.(model)
			if manager.switchCalls != 1 || value.generation != 2 || !manager.lastConfirmation.Provider || !manager.lastConfirmation.Workspace {
				t.Fatalf("calls=%d generation=%d confirmation=%+v", manager.switchCalls, value.generation, manager.lastConfirmation)
			}
		})
	}
}

func TestAgentUIPickerWorkspaceOnlyDeclineAndAcceptRemainFailClosed(t *testing.T) {
	plan := agentcontroller.SwitchPlan{SessionID: "target", NeedWorkspaceConfirm: true, WorkspaceLabel: "project"}
	manager := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}, planOverride: &plan, nextTitle: "目标", items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{SessionID: "target", Title: "目标"}}}}
	value := openLoadedPicker(t, newModel(t.Context(), manager, "model"))
	updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyDown})
	value = updated.(model)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	value = updated.(model)
	if command != nil || manager.switchCalls != 0 || value.sessionPicker.mode != pickerSwitchConfirm {
		t.Fatalf("local-provider key bypassed workspace confirmation: command=%v calls=%d picker=%+v", command, manager.switchCalls, value.sessionPicker)
	}
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
	value = updated.(model)
	if command != nil || manager.switchCalls != 0 || value.sessionPicker.mode != pickerList {
		t.Fatalf("Esc did not cancel workspace confirmation: command=%v calls=%d picker=%+v", command, manager.switchCalls, value.sessionPicker)
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	updated, _ = value.Update(command())
	value = updated.(model)
	if manager.switchCalls != 1 || !manager.lastConfirmation.Workspace || value.generation != 2 {
		t.Fatalf("workspace confirmation calls=%d confirmation=%+v generation=%d", manager.switchCalls, manager.lastConfirmation, value.generation)
	}
}

func TestAgentUIPickerDropsSameGenerationSupersededRequestAndLateEvents(t *testing.T) {
	manager := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 2, gate: agentcontroller.SwitchGate{Allowed: true}}
	value := newModel(t.Context(), manager, "model")
	value.sessionPicker = newSessionPickerModel(false)
	_, firstEpoch := value.sessionPicker.beginRequest()
	_, secondEpoch := value.sessionPicker.beginRequest()
	updated, _ := value.Update(pickerListMsg{generation: 2, epoch: firstEpoch, items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{Title: "过期结果"}}}})
	value = updated.(model)
	if len(value.sessionPicker.items) != 0 || !value.sessionPicker.loading {
		t.Fatalf("superseded result applied: %+v", value.sessionPicker)
	}
	updated, _ = value.Update(pickerListMsg{generation: 2, epoch: secondEpoch, items: []agentcontroller.SessionListItem{{Summary: agentsession.Summary{Title: "最新结果"}}}})
	value = updated.(model)
	if len(value.sessionPicker.items) != 1 || value.sessionPicker.items[0].Summary.Title != "最新结果" {
		t.Fatalf("latest result missing: %+v", value.sessionPicker.items)
	}

	value.activeTurnID = 9
	beforeEntries := len(value.entries)
	activity := agentloop.Activity{Kind: agentloop.ActivityTool, Event: agentloop.Event{ID: "late", Tool: "read", Summary: "旧工具", Status: agentloop.EventSucceeded}}
	updated, _ = value.Update(turnMsg{generation: 1, turnID: 9, activity: &activity})
	value = updated.(model)
	updated, _ = value.Update(contextMsg{generation: 1, event: agentloop.ContextEvent{Kind: agentloop.ContextEventCompacted}})
	value = updated.(model)
	updated, _ = value.Update(learningMsg{generation: 1, status: agentloop.LearningStatus{Active: true}})
	value = updated.(model)
	updated, _ = value.Update(pickerOperationMsg{generation: 1, newGeneration: 3, kind: pickerIntentSwitch})
	value = updated.(model)
	if len(value.entries) != beforeEntries || value.generation != 2 || value.learningStatus.Active || strings.Contains(value.View(), "旧工具") {
		t.Fatalf("late event crossed generation: generation=%d entries=%d/%d view=%s", value.generation, len(value.entries), beforeEntries, value.View())
	}
}

func TestSessionPickerCurrentLockedAndCorruptRenameDeleteRules(t *testing.T) {
	source := &pickerConversation{fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}}
	for _, test := range []struct {
		name        string
		item        agentcontroller.SessionListItem
		allowRename bool
		allowDelete bool
	}{
		{"current", agentcontroller.SessionListItem{Current: true, Summary: agentsession.Summary{SessionID: "current", Title: "当前"}}, true, false},
		{"locked", agentcontroller.SessionListItem{Summary: agentsession.Summary{SessionID: "locked", Title: "锁定", Locked: true}}, true, false},
		{"corrupt", agentcontroller.SessionListItem{Summary: agentsession.Summary{StorageID: strings.Repeat("e", 32), Corrupt: true, LocatorOnly: true}}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			picker := newSessionPickerModel(false)
			picker.setItems([]agentcontroller.SessionListItem{test.item}, nil)
			picker.selected = 1
			picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR}, source)
			if got := picker.mode == pickerRename; got != test.allowRename {
				t.Fatalf("rename allowed=%t want=%t", got, test.allowRename)
			}
			picker.mode = pickerList
			picker.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}, source)
			if got := picker.mode == pickerDeleteConfirm; got != test.allowDelete {
				t.Fatalf("delete allowed=%t want=%t", got, test.allowDelete)
			}
		})
	}
}

func TestSessionPickerMinimumTerminalAllModesStayBounded(t *testing.T) {
	picker := newSessionPickerModel(true)
	picker.setItems([]agentcontroller.SessionListItem{{Summary: agentsession.Summary{SessionID: "target", Title: strings.Repeat("长标题", 30), RecordRevision: 1}}}, nil)
	picker.selected = 1
	modes := []pickerMode{pickerList, pickerRename, pickerDeleteConfirm, pickerSwitchConfirm}
	for _, mode := range modes {
		picker.mode = mode
		picker.edit = strings.Repeat("重命名", 30)
		picker.plan = agentcontroller.SwitchPlan{NeedProviderConfirm: true, NeedWorkspaceConfirm: true, WorkspaceLabel: strings.Repeat("工作区", 30)}
		view := picker.render(minimumWidth, minimumHeight)
		for _, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) > minimumWidth {
				t.Fatalf("mode=%d width=%d line=%q view=%s", mode, lipgloss.Width(line), line, view)
			}
		}
	}
}

func TestPickerErrorTextClassifiesSwitchFailures(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{agentsession.ErrInUse, "session_in_use"},
		{agentsession.ErrCorrupt, "session_corrupt"},
		{agentsession.ErrCheckpointConflict, "session_checkpoint_conflict"},
		{agentsession.ErrCheckpointSaveFailed, "session_operation_failed"},
		{agentcontroller.ErrProviderConfirmationRequired, "session_provider_confirmation_required"},
		{agentcontroller.ErrWorkspaceConfirmationRequired, "session_workspace_confirmation_required"},
		{errors.New("preflight failed"), "session_operation_failed"},
	} {
		if got := pickerErrorText(test.err); !strings.Contains(got, "["+test.code+"]") {
			t.Fatalf("error=%v got=%q want code=%q", test.err, got, test.code)
		}
	}
}

func TestAgentUIRestoresDurablePresentationTranscriptAndRetryOnlyEntry(t *testing.T) {
	manager := &pickerConversation{
		fakeConversation: &fakeConversation{}, generation: 1, gate: agentcontroller.SwitchGate{Allowed: true}, title: "历史标题",
		outcomes: []agentcontroller.UnknownOutcome{{ReceiptID: "11111111-1111-4111-8111-111111111111", Kind: "preference", Label: "待核对"}},
		transcript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{
			{Sequence: 1, PresentationTurn: 1, Kind: agentsession.TranscriptKindUser, Text: "历史问题"},
			{Sequence: 2, PresentationTurn: 1, Kind: agentsession.TranscriptKindTool, Tools: []agentsession.TerminalToolActivityV1{{Name: "read", State: agentsession.ToolStateCompleted, Summary: "工具调用已完成"}}},
			{Sequence: 3, PresentationTurn: 1, Kind: agentsession.TranscriptKindAssistant, Text: "历史答案", AssistantState: agentsession.AssistantStateFinal, ModelCommitted: true},
			{Sequence: 4, Kind: agentsession.TranscriptKindSessionNotice, Notice: &agentsession.TypedNoticeV1{Code: "transcript_compacted", Outcome: agentsession.NoticeOutcomeInformational, Message: "较早的 3 条展示记录已收起", Count: 3}},
		}},
	}
	value := newModel(t.Context(), manager, "model")
	view := value.View()
	for _, expected := range []string{"历史问题", "历史答案", "工具调用", "较早的 3 条展示记录已收起", "Session 历史标题"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("restored transcript/footer omitted %q: %s", expected, view)
		}
	}
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	value = updated.(model)
	if command == nil {
		t.Fatal("Ctrl+P did not start explicit retry-only reconciliation")
	}
	updated, _ = value.Update(command())
	value = updated.(model)
	if manager.retried == "" || !strings.Contains(value.status, "已核对") {
		t.Fatalf("retry id=%q status=%q", manager.retried, value.status)
	}
}
