package agentui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type fakeConversation struct {
	result             agentloop.Result
	resolved           agentloop.Result
	questionResolved   agentloop.Result
	fileResolved       agentloop.Result
	activities         []agentloop.Activity
	resolvedActivities []agentloop.Activity
	sendErr            error
	sent               string
	approved           bool
	resolution         agentloop.PreferenceResolution
	questionAnswer     agentloop.QuestionAnswer
	fileResolution     agentloop.FileMutationResolution
	fileCallID         string
	cancelledFileCall  string
	fileMode           agentloop.FileAuthorizationMode
	resolveCalls       int
	questionCalls      int
	resolveErrors      []error
	resolveStarted     chan struct{}
	resolveRelease     chan struct{}
	questionStarted    chan struct{}
	questionRelease    chan struct{}
	reasoningEffort    modelclient.ReasoningEffort
	setReasoningErr    error
	contextStatus      agentloop.ContextStatus
	contextUpdates     chan agentloop.ContextEvent
	workspaceStatus    agentloop.WorkspaceStatus
	learningStatus     agentloop.LearningStatus
	learningErr        error
	persistenceState   string
	persistenceDetail  string
	startupNotices     []string
}

func (c *fakeConversation) SessionStartupNotices() []string {
	return append([]string(nil), c.startupNotices...)
}

func (c *fakeConversation) SessionPersistenceStatus() (string, string) {
	return c.persistenceState, c.persistenceDetail
}

func (c *fakeConversation) Send(ctx context.Context, input string) (agentloop.Result, error) {
	c.sent = input
	for _, activity := range c.activities {
		agentloop.PublishActivity(ctx, activity)
	}
	return c.result, c.sendErr
}
func (c *fakeConversation) ResolvePreference(ctx context.Context, resolution agentloop.PreferenceResolution) (agentloop.Result, error) {
	c.resolution, c.approved, c.resolveCalls = resolution, resolution == agentloop.PreferenceSave || resolution == agentloop.PreferenceRetry, c.resolveCalls+1
	for _, activity := range c.resolvedActivities {
		agentloop.PublishActivity(ctx, activity)
	}
	if c.resolveStarted != nil {
		select {
		case c.resolveStarted <- struct{}{}:
		case <-ctx.Done():
			return agentloop.Result{}, ctx.Err()
		}
	}
	if c.resolveRelease != nil {
		select {
		case <-c.resolveRelease:
		case <-ctx.Done():
			return agentloop.Result{}, ctx.Err()
		}
	}
	if len(c.resolveErrors) > 0 {
		err := c.resolveErrors[0]
		c.resolveErrors = c.resolveErrors[1:]
		if err != nil {
			return agentloop.Result{}, err
		}
	}
	return c.resolved, nil
}
func (c *fakeConversation) ResolveQuestion(ctx context.Context, answer agentloop.QuestionAnswer) (agentloop.Result, error) {
	c.questionCalls++
	c.questionAnswer = answer
	if c.questionStarted != nil {
		select {
		case c.questionStarted <- struct{}{}:
		case <-ctx.Done():
			return agentloop.Result{}, ctx.Err()
		}
	}
	if c.questionRelease != nil {
		select {
		case <-c.questionRelease:
		case <-ctx.Done():
			return agentloop.Result{}, ctx.Err()
		}
	}
	return c.questionResolved, nil
}
func (c *fakeConversation) ResolveFileMutation(_ context.Context, callID string, resolution agentloop.FileMutationResolution) (agentloop.Result, error) {
	c.fileCallID = callID
	c.fileResolution = resolution
	return c.fileResolved, nil
}
func (c *fakeConversation) CancelPendingFileMutation(callID string) (agentloop.Result, error) {
	c.cancelledFileCall = callID
	return agentloop.Result{}, nil
}
func (c *fakeConversation) FileAuthorizationMode() agentloop.FileAuthorizationMode {
	if c.fileMode == "" {
		return agentloop.FileAuthorizationConfirm
	}
	return c.fileMode
}
func (c *fakeConversation) SetFileAuthorizationMode(mode agentloop.FileAuthorizationMode) error {
	c.fileMode = mode
	return nil
}
func (c *fakeConversation) ReasoningEffort() modelclient.ReasoningEffort {
	if c.reasoningEffort == "" {
		return modelclient.ReasoningEffortAuto
	}
	return c.reasoningEffort
}
func (c *fakeConversation) SetReasoningEffort(value modelclient.ReasoningEffort) error {
	if c.setReasoningErr != nil {
		return c.setReasoningErr
	}
	c.reasoningEffort = value
	return nil
}
func (c *fakeConversation) ContextStatus() agentloop.ContextStatus        { return c.contextStatus }
func (c *fakeConversation) ContextUpdates() <-chan agentloop.ContextEvent { return c.contextUpdates }
func (c *fakeConversation) WorkspaceStatus() agentloop.WorkspaceStatus {
	if c.workspaceStatus.Available || c.workspaceStatus.Code != "" {
		return c.workspaceStatus
	}
	return agentloop.WorkspaceStatus{Available: false, Code: "workspace_unavailable"}
}
func (c *fakeConversation) LearningStatus(context.Context) (agentloop.LearningStatus, error) {
	return c.learningStatus, c.learningErr
}

func runTurn(t *testing.T, value model, command tea.Cmd) model {
	t.Helper()
	for value.busy {
		if command == nil {
			t.Fatal("turn command ended while the model was still busy")
		}
		message := command()
		updated, next := value.Update(message)
		value = updated.(model)
		command = next
	}
	return value
}

func TestAgentUIIsChineseAndSendsInput(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{Text: "这是回答。", Events: []agentloop.Event{{Tool: "search_knowledge", Summary: "检索到 1 条知识片段"}}}}
	value := newModel(t.Context(), conversation, "test-model")
	view := value.View()
	for _, expected := range []string{"AI 学习助手", "模型 test-model", "Enter 发送", "长期偏好"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view missing %q: %s", expected, view)
		}
	}
	value.input.SetValue("解释图论")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if !value.busy || command == nil {
		t.Fatalf("busy=%t command=%v", value.busy, command)
	}
	value = runTurn(t, value, command)
	if conversation.sent != "解释图论" || value.busy || !strings.Contains(value.viewport.View(), "这是回答") {
		t.Fatalf("sent=%q busy=%t viewport=%s", conversation.sent, value.busy, value.viewport.View())
	}
}

func TestAgentUIPresentsBoundedSessionPersistenceHelp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		want  []string
	}{
		{
			name:  "saved",
			state: "saved",
			want:  []string{"本机加密保存", "自动标题", "恢复后的历史上下文", "当前 provider", "endpoint 变化须先确认", "YOLO", "旧文件授权", "pending 交互", "不会恢复"},
		},
		{
			name:  "unsaved",
			state: "unsaved",
			want:  []string{"仅在当前进程有效", "退出后不可恢复", "key backend", "绝不落明文", "当前 provider", "endpoint 变化须先确认", "YOLO", "pending 交互"},
		},
		{
			name:  "failed",
			state: "failed",
			want:  []string{"最近内容保存失败", "可能尚未持久化", "不会改用明文", "当前 provider", "endpoint 变化须先确认", "旧文件授权", "不会恢复"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			help := sessionPersistenceHelp(test.state)
			for _, topic := range test.want {
				if !strings.Contains(help, topic) {
					t.Fatalf("%s help missing topic %q: %q", test.name, topic, help)
				}
			}
			if strings.Contains(help, "https://") || strings.Contains(help, "/home/") || strings.Contains(help, "opaque-id") {
				t.Fatalf("%s help leaked endpoint, path, or internal ID: %q", test.name, help)
			}

			value := newModel(t.Context(), &fakeConversation{
				persistenceState:  test.state,
				persistenceDetail: "https://private.example/v1 /home/user/session opaque-id-123",
			}, "model")
			updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
			view := updated.(model).View()
			if !strings.Contains(view, test.want[0]) {
				t.Fatalf("%s first 46x18 view does not show persistence help: %s", test.name, view)
			}
			if strings.Contains(view, "private.example") || strings.Contains(view, "/home/user") || strings.Contains(view, "opaque-id-123") {
				t.Fatalf("%s first view leaked persistence detail: %s", test.name, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if width := lipgloss.Width(line); width > minimumWidth {
					t.Fatalf("%s 46x18 line width=%d: %q", test.name, width, line)
				}
			}
		})
	}
}

func TestAgentUIWorkspaceNoticeUsesSafeSessionStatus(t *testing.T) {
	available := newModel(t.Context(), &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"}}, "model")
	availableView := available.View()
	for _, expected := range []string{"工作区 project 已启用", ".git", ".comet", "发送给当前模型 provider", "工作区 project"} {
		if !strings.Contains(availableView, expected) {
			t.Fatalf("available workspace view missing %q: %s", expected, availableView)
		}
	}

	unavailable := newModel(t.Context(), &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Code: "workspace_unavailable"}}, "model")
	unavailableView := unavailable.View()
	if !strings.Contains(unavailableView, "工作区不可用（workspace_unavailable）") || strings.Contains(unavailableView, "/home/") {
		t.Fatalf("unavailable workspace view=%s", unavailableView)
	}
}

func TestAgentUIFileMutationSelectorKeepsPendingAcrossYOLOSwitch(t *testing.T) {
	conversation := &fakeConversation{
		workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"},
		fileResolved:    agentloop.Result{Text: "文件修改已完成。"},
	}
	value := newModel(t.Context(), conversation, "model")
	pending := &agentloop.PendingFileMutation{
		CallID: "write-call", Tool: "write", Operation: "write_create", Path: "notes.md",
		PreviewKind: "content", Preview: "hello\n", Truncated: true,
	}
	value.handleTurnResult(agentloop.Result{PendingFileMutation: pending})
	if value.selector == nil || value.selector.kind != selectorFileMutation || !strings.Contains(value.View(), "hello") || value.pendingFileMutation == nil {
		t.Fatalf("file mutation selector was not rendered: %s", value.View())
	}
	resized, _ := value.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	value = resized.(model)
	for _, line := range strings.Split(value.View(), "\n") {
		if lipgloss.Width(line) > 60 {
			t.Fatalf("file selector line width=%d: %q", lipgloss.Width(line), line)
		}
	}

	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyF4})
	value = updated.(model)
	if command != nil || value.selector == nil || value.selector.kind != selectorFileMode {
		t.Fatalf("F4 did not open file mode selector: selector=%+v command=%v", value.selector, command)
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyDown})
	value = updated.(model)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if conversation.fileMode != agentloop.FileAuthorizationYOLO || value.selector == nil || value.selector.kind != selectorFileMutation || value.pendingFileMutation == nil {
		t.Fatalf("mode=%q selector=%+v pending=%+v", conversation.fileMode, value.selector, value.pendingFileMutation)
	}
	if !strings.Contains(value.View(), "文件 YOLO") || !strings.Contains(value.View(), "仅当前 Session") {
		t.Fatalf("YOLO warning/status missing: %s", value.View())
	}

	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if !value.busy || command == nil {
		t.Fatalf("approve did not start resolution: busy=%t command=%v", value.busy, command)
	}
	value = runTurn(t, value, command)
	if conversation.fileCallID != "write-call" || conversation.fileResolution != agentloop.FileMutationApprove || value.pendingFileMutation != nil {
		t.Fatalf("call=%q resolution=%q pending=%+v", conversation.fileCallID, conversation.fileResolution, value.pendingFileMutation)
	}

	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyF4})
	value = updated.(model)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyUp})
	value = updated.(model)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if conversation.fileMode != agentloop.FileAuthorizationConfirm || !strings.Contains(value.View(), "文件 确认") {
		t.Fatalf("mode did not switch back to confirm: mode=%q view=%s", conversation.fileMode, value.View())
	}
}

func TestAgentUIEscCancelsPendingFileMutation(t *testing.T) {
	conversation := &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"}}
	value := newModel(t.Context(), conversation, "model")
	pending := &agentloop.PendingFileMutation{
		CallID: "edit-call", Tool: "edit", Operation: "edit", Path: "notes.md", PreviewKind: "diff", Preview: "-old\n+new\n",
	}
	value.handleTurnResult(agentloop.Result{PendingFileMutation: pending})
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEsc})
	value = updated.(model)
	if command != nil || conversation.cancelledFileCall != "edit-call" || value.pendingFileMutation != nil || value.selector != nil {
		t.Fatalf("command=%v cancelled=%q pending=%+v selector=%+v", command, conversation.cancelledFileCall, value.pendingFileMutation, value.selector)
	}
	value.toolsExpanded = true
	value.refreshTranscript(false)
	for _, expected := range []string{"路径：notes.md", "操作：edit", "代码：cancelled"} {
		if !strings.Contains(value.viewport.View(), expected) {
			t.Fatalf("cancelled mutation missing %q: %s", expected, value.viewport.View())
		}
	}
}

func TestAgentUIDeclinesPendingFileMutation(t *testing.T) {
	conversation := &fakeConversation{
		workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"},
		fileResolved:    agentloop.Result{Text: "已保留原文件。"},
	}
	value := newModel(t.Context(), conversation, "model")
	pending := &agentloop.PendingFileMutation{
		CallID: "edit-call", Tool: "edit", Operation: "edit", Path: "notes.md", PreviewKind: "diff", Preview: "-old\n+new\n", Truncated: true,
	}
	value.handleTurnResult(agentloop.Result{PendingFileMutation: pending})
	view := value.View()
	for _, expected := range []string{"notes.md", "操作：edit", "-old", "+new", "预览已按安全上限截断"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("edit confirmation missing %q: %s", expected, view)
		}
	}
	updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyDown})
	value = updated.(model)
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if !value.busy || command == nil {
		t.Fatalf("decline did not start resolution: busy=%t command=%v", value.busy, command)
	}
	value = runTurn(t, value, command)
	if conversation.fileCallID != "edit-call" || conversation.fileResolution != agentloop.FileMutationDecline || value.pendingFileMutation != nil || !strings.Contains(value.viewport.View(), "已保留原文件") {
		t.Fatalf("call=%q resolution=%q pending=%+v transcript=%s", conversation.fileCallID, conversation.fileResolution, value.pendingFileMutation, value.viewport.View())
	}
}

func TestAgentUIFileToolTimelineIsBoundedAndExpandable(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{
		Text: "文件检查已结束。",
		Events: []agentloop.Event{
			{ID: "write-call", Tool: "write", Summary: "已完成 write_create：notes.md", Status: agentloop.EventSucceeded},
			{ID: "search-call", Tool: "search", Summary: "在 src 中找到 100 处匹配", Status: agentloop.EventFailed, Detail: "match_limit"},
		},
	}}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("检查文件")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	collapsed := value.viewport.View()
	if !strings.Contains(collapsed, "工具调用 · 2 项") || !strings.Contains(collapsed, "写入文件") || !strings.Contains(collapsed, "搜索文件") || strings.Contains(collapsed, "match_limit") {
		t.Fatalf("collapsed file timeline=%s", collapsed)
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	value = updated.(model)
	expanded := value.viewport.View()
	for _, expected := range []string{"工具：write", "工具：search", "代码：match_limit", "notes.md"} {
		if !strings.Contains(expanded, expected) {
			t.Fatalf("expanded file timeline missing %q: %s", expected, expanded)
		}
	}
	for _, forbidden := range []string{"/home/", `{"path"`, "expected_hash", "provider reasoning"} {
		if strings.Contains(expanded, forbidden) {
			t.Fatalf("expanded file timeline leaked %q: %s", forbidden, expanded)
		}
	}
}

type learningStatusConversation struct {
	fakeConversation
	status agentloop.LearningStatus
	err    error
	calls  int
}

func (c *learningStatusConversation) LearningStatus(context.Context) (agentloop.LearningStatus, error) {
	c.calls++
	return c.status, c.err
}

func TestAgentUIWideTerminalUsesAvailableWidth(t *testing.T) {
	const terminalWidth = 200
	value := newModel(t.Context(), &fakeConversation{}, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: terminalWidth, Height: 30})
	value = updated.(model)
	view := value.View()
	if value.contentWidth != terminalWidth-horizontalPadding {
		t.Fatalf("content width=%d want=%d; wide terminal left blank=%q", value.contentWidth, terminalWidth-horizontalPadding, strings.Split(view, "\n")[0])
	}
	firstRow := strings.Split(view, "\n")[0]
	leadingWidth := lipgloss.Width(firstRow) - lipgloss.Width(strings.TrimLeft(firstRow, " "))
	if leadingWidth > 3 {
		t.Fatalf("content begins at column %d, want <=3: %q", leadingWidth, firstRow)
	}
	if value.sidebarWidth == 0 || value.viewport.Width < sidebarMinMainWidth || !strings.Contains(firstRow, "◇ edu-agent") {
		t.Fatalf("wide layout sidebar=%d main=%d first-row=%q", value.sidebarWidth, value.viewport.Width, firstRow)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > terminalWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), terminalWidth, line)
		}
	}
}

func TestAgentUIRemovesDedicatedTopBarAndRelocatesIdentity(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	value = updated.(model)
	view := value.View()
	firstRow := strings.Split(view, "\n")[0]
	composer := strings.Index(view, "╭─ 消息")
	identity := strings.LastIndex(view, "◇ edu-agent")
	if strings.Contains(firstRow, "◇ edu-agent") || composer < 0 || identity <= composer {
		t.Fatalf("product identity was not moved below transcript/composer: first=%q view=%s", firstRow, view)
	}
	expectedViewportHeight := value.height - lipgloss.Height(value.renderControl(value.viewport.Width)) - lipgloss.Height(value.renderFooter(value.viewport.Width))
	if value.viewport.Height != expectedViewportHeight {
		t.Fatalf("viewport height=%d want=%d after removing top bar", value.viewport.Height, expectedViewportHeight)
	}
}

func TestAgentUIRightSidebarShowsAuthoritativeLearningStatus(t *testing.T) {
	conversation := &learningStatusConversation{status: agentloop.LearningStatus{
		Active: true,
		View: api.SessionView{
			Session: api.TutoringSession{
				State: "RouteActive", Focus: api.FocusContext{RouteStepID: "step-2"},
			},
			EstimatedActiveTime: api.ActiveTimeEstimate{DurationSeconds: 1920, Estimated: true},
			WorkItem: &api.SessionWorkItem{
				GoalRevision: &api.GoalRevision{Text: "掌握图论基础\x1b[2J并完成练习"},
				RouteRevision: &api.RouteRevision{Steps: []api.RouteStep{
					{RouteStepID: "step-1", TeachingIntent: "理解顶点与边"},
					{RouteStepID: "step-2", TeachingIntent: "掌握图的遍历"},
					{RouteStepID: "step-3", TeachingIntent: "应用最短路径"},
				}},
				Activity: &api.Activity{Type: "practice", Difficulty: 2},
			},
		},
	}}
	value := newModel(t.Context(), conversation, "test-model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	value = updated.(model)
	command := loadLearningStatusCmd(value.ctx, value.session)
	if command == nil {
		t.Fatal("learning status command is nil")
	}
	updated, _ = value.Update(command())
	value = updated.(model)
	view := value.View()
	for _, expected := range []string{"◇ edu-agent", "AGENT", "当前学习", "掌握图论基础", "路线学习中", "2/3", "掌握图的遍历", "练习 · 难度2", "约 32 分钟", "Ctrl+R 刷新"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("sidebar missing %q: %s", expected, view)
		}
	}
	if strings.ContainsAny(view, "\x1b\r\a") || strings.Contains(view, "step-2") || value.sidebarWidth == 0 || value.viewport.Width < sidebarMinMainWidth {
		t.Fatalf("unsafe or invalid sidebar layout: sidebar=%d main=%d view=%q", value.sidebarWidth, value.viewport.Width, view)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 120 {
			t.Fatalf("line width=%d limit=120 line=%q", lipgloss.Width(line), line)
		}
	}

	updated, refresh := value.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	value = updated.(model)
	if refresh == nil || !value.learningLoading {
		t.Fatalf("Ctrl+R did not start refresh: loading=%t", value.learningLoading)
	}
	updated, _ = value.Update(refresh())
	value = updated.(model)
	if conversation.calls != 2 || !value.learningLoaded {
		t.Fatalf("refresh calls=%d loaded=%t", conversation.calls, value.learningLoaded)
	}

	conversation.err = errors.New("server unavailable")
	updated, refresh = value.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	value = updated.(model)
	updated, _ = value.Update(refresh())
	value = updated.(model)
	if !value.learningFailed || strings.Contains(value.View(), "掌握图论基础") || !strings.Contains(value.View(), "当前状态暂不可用") {
		t.Fatalf("failed refresh retained stale learning data: %s", value.View())
	}
}

func TestAgentUISidebarCollapsesBeforeMainTranscriptGetsTooNarrow(t *testing.T) {
	conversation := &learningStatusConversation{status: agentloop.LearningStatus{
		Active: true,
		View: api.SessionView{
			Session:             api.TutoringSession{State: "RouteActive", Focus: api.FocusContext{RouteStepID: "step-2"}},
			EstimatedActiveTime: api.ActiveTimeEstimate{DurationSeconds: 1920, Estimated: true},
			WorkItem: &api.SessionWorkItem{
				GoalRevision:  &api.GoalRevision{Text: "掌握图论基础并完成一组综合练习"},
				RouteRevision: &api.RouteRevision{Steps: []api.RouteStep{{RouteStepID: "step-1"}, {RouteStepID: "step-2"}, {RouteStepID: "step-3"}}},
				Activity:      &api.Activity{Type: "practice", Difficulty: 2},
			},
		},
	}}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(loadLearningStatusCmd(value.ctx, value.session)())
	value = updated.(model)
	updated, _ = value.Update(tea.WindowSizeMsg{Width: 91, Height: 24})
	value = updated.(model)
	if value.sidebarWidth != 0 || strings.Contains(value.View(), "当前学习") {
		t.Fatalf("sidebar did not collapse: width=%d view=%s", value.sidebarWidth, value.View())
	}
	updated, _ = value.Update(tea.WindowSizeMsg{Width: 92, Height: minimumHeight})
	value = updated.(model)
	view := value.View()
	if value.sidebarWidth < sidebarMinWidth || value.viewport.Width < sidebarMinMainWidth ||
		!strings.Contains(strings.Split(view, "\n")[0], "◇ edu-agent") ||
		!strings.Contains(view, "练习 · 难度2") || !strings.Contains(view, "约 32 分钟") {
		t.Fatalf("compact sidebar omitted core status: sidebar=%d main=%d view=%s", value.sidebarWidth, value.viewport.Width, view)
	}
	lines := strings.Split(view, "\n")
	if len(lines) > minimumHeight {
		t.Fatalf("sidebar minimum-height view has %d lines: %s", len(lines), view)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > 92 {
			t.Fatalf("sidebar minimum-height line width=%d: %q", lipgloss.Width(line), line)
		}
	}
}

func TestAgentUIPlacesMetadataBelowDynamicComposer(t *testing.T) {
	conversation := &contextAwareConversation{status: agentloop.ContextStatus{
		Estimated: true, WindowPercent: 54, RecentCompleteTurns: 6, MemoryItemCount: 18,
	}}
	value := newModel(t.Context(), conversation, "test-model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 96, Height: 30})
	value = updated.(model)
	value.input.SetValue("解释图论\n给练习")
	value.resize()
	view := value.View()

	header := strings.Split(view, "\n")[0]
	for _, forbidden := range []string{"模型 test-model", "上下文", "就绪"} {
		if strings.Contains(header, forbidden) {
			t.Fatalf("header still contains %q: %s", forbidden, header)
		}
	}
	composerEnd := strings.Index(view, "╰")
	metadata := strings.LastIndex(view, "约54%")
	if composerEnd < 0 || metadata <= composerEnd || !strings.Contains(view, "╭─ 消息") ||
		!strings.Contains(view, "› 解释图论") || !strings.Contains(view, "8/8000") {
		t.Fatalf("composer/footer hierarchy is incomplete: %s", view)
	}
}

func TestAgentUIComposerGrowsAndShrinksWithContent(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	baseRows, baseViewport := value.input.Height(), value.viewport.Height
	value.input.SetValue(strings.Repeat("多行内容\n", 10))
	value.resize()
	if value.input.Height() != composerMaxRows || value.viewport.Height >= baseViewport {
		t.Fatalf("expanded rows=%d viewport=%d baseRows=%d baseViewport=%d", value.input.Height(), value.viewport.Height, baseRows, baseViewport)
	}
	value.input.Reset()
	value.resize()
	if value.input.Height() != composerMinRows || value.viewport.Height != baseViewport {
		t.Fatalf("collapsed rows=%d viewport=%d baseViewport=%d", value.input.Height(), value.viewport.Height, baseViewport)
	}
}

func TestAgentUIPreferenceRequiresYOrN(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{
		Content: "先给结论", Reason: "用户明确要求长期保持回答顺序",
		Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
	}
	conversation := &fakeConversation{result: agentloop.Result{Pending: pending}, resolved: agentloop.Result{Text: "已处理。"}}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("记住这个偏好")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	if value.pending == nil || !strings.Contains(value.View(), "将保存以下长期偏好") || !strings.Contains(value.viewport.View(), "用户明确要求长期保持回答顺序") {
		t.Fatalf("pending view=%s viewport=%s", value.View(), value.viewport.View())
	}
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if command == nil || !value.busy {
		t.Fatalf("confirmation did not start")
	}
	if _, duplicate := value.Update(tea.KeyMsg{Type: tea.KeyEnter}); duplicate != nil {
		t.Fatal("busy confirmation accepted a duplicate Enter")
	}
	if _, decline := value.Update(tea.KeyMsg{Type: tea.KeyEsc}); decline != nil {
		t.Fatal("busy confirmation accepted Esc after submission")
	}
	value = runTurn(t, value, command)
	if conversation.resolveCalls != 1 || !conversation.approved || value.pending != nil {
		t.Fatalf("calls=%d approved=%t pending=%v", conversation.resolveCalls, conversation.approved, value.pending)
	}
}

type blockingConversation struct {
	fakeConversation
	started  chan struct{}
	canceled chan struct{}
}

func (c *blockingConversation) Send(ctx context.Context, _ string) (agentloop.Result, error) {
	close(c.started)
	<-ctx.Done()
	close(c.canceled)
	return agentloop.Result{}, ctx.Err()
}

func TestAgentUIAmbiguousPreferenceCanOnlyRetry(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{
		Content: "先给结论", Reason: "用户明确要求长期保持回答顺序",
		Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
	}
	conversation := &fakeConversation{
		result: agentloop.Result{Pending: pending}, resolved: agentloop.Result{Text: "已核对提交。"},
		resolveErrors: []error{agentloop.ErrPreferenceOutcomeUnknown},
	}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("记住这个偏好")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	view := value.View()
	if value.pending == nil || !value.pending.RetryOnly || !strings.Contains(view, "结果未知") || !strings.Contains(view, "重试核对原操作") || strings.Contains(view, "仅本次会话使用") {
		t.Fatalf("ambiguous preference state = pending=%+v view=%s", value.pending, view)
	}
	if _, decline := value.Update(tea.KeyMsg{Type: tea.KeyEsc}); decline != nil || conversation.resolveCalls != 1 {
		t.Fatalf("ambiguous preference accepted decline: command=%v calls=%d", decline, conversation.resolveCalls)
	}
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	if value.pending != nil || conversation.resolveCalls != 2 || !strings.Contains(value.View(), "已核对提交") {
		t.Fatalf("retry state pending=%+v calls=%d view=%s", value.pending, conversation.resolveCalls, value.View())
	}
}

func TestAgentUIKnownCompensationFailureRestoresAllPreferenceChoices(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{
		Content: "先给结论", Reason: "用户明确要求长期保持回答顺序",
		Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
	}
	conversation := &fakeConversation{
		result: agentloop.Result{Pending: pending},
		resolveErrors: []error{
			agentloop.ErrPreferenceOutcomeUnknown,
			errors.New("长期偏好未保存（admission_forbidden）"),
		},
	}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("记住这个偏好")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	if value.pending == nil || !value.pending.RetryOnly {
		t.Fatalf("first ambiguous failure did not enter retry-only state: %+v", value.pending)
	}
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	view := value.View()
	if value.pending == nil || value.pending.RetryOnly || value.selector == nil || value.selector.kind != selectorPreference || conversation.resolveCalls != 2 ||
		!strings.Contains(view, "仅本次会话使用") || !strings.Contains(view, "不保存") {
		t.Fatalf("known compensation result did not restore choices: pending=%+v calls=%d view=%s", value.pending, conversation.resolveCalls, view)
	}
}

func TestAgentUIExitCancelsInFlightConversation(t *testing.T) {
	conversation := &blockingConversation{started: make(chan struct{}), canceled: make(chan struct{})}
	value := newModel(context.Background(), conversation, "model")
	value.input.SetValue("阻塞请求")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if command == nil {
		t.Fatal("send command missing")
	}
	start, ok := command().(turnMsg)
	if !ok || start.stream == nil {
		t.Fatalf("turn stream did not start: %#v", start)
	}
	updated, wait := value.Update(start)
	value = updated.(model)
	if wait == nil {
		t.Fatal("turn stream wait command missing")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- wait() }()
	select {
	case <-conversation.started:
	case <-time.After(time.Second):
		t.Fatal("conversation did not start")
	}
	_, quit := value.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if quit == nil {
		t.Fatal("quit command missing")
	}
	select {
	case <-conversation.canceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight conversation was not canceled")
	}
	select {
	case message := <-result:
		if turn, ok := message.(turnMsg); !ok || !turn.done || turn.err == nil {
			t.Fatalf("canceled turn = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled conversation did not return")
	}
}

func TestAgentUISanitizesUntrustedTerminalControls(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{
		Text: "正常回答\x1b[2J\x1b]0;伪造标题\a结束",
		Pending: &agentloop.PreferenceConfirmation{
			Content: "保留\x1b[H偏好", Reason: "原因\r覆盖", Category: "personal_context",
			Sensitivity: "sensitive", Stability: "transient",
		},
	}}
	value := newModel(t.Context(), conversation, "model\x1b[2J")
	value.input.SetValue("测试")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	view := value.View()
	if strings.ContainsAny(view, "\x1b\r\a") || !strings.Contains(view, "�") || !strings.Contains(value.viewport.View(), "原因") {
		t.Fatalf("untrusted controls reached terminal: %q", view)
	}
}

func TestAgentUIChromeSanitizesComposerAndSingleLineMetadata(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model\n伪造状态\tname")
	value.status = "就绪\n伪造状态"
	value.input.SetValue("正常输入\x1b[2J\t文本\u202e")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	view := value.View()
	if strings.ContainsAny(view, "\x1b\r\a\t") || strings.ContainsRune(view, '\u202e') ||
		strings.ContainsAny(value.input.Value(), "\x1b\r\a\t") || strings.ContainsRune(value.input.Value(), '\u202e') {
		t.Fatalf("unsafe chrome/composer content reached terminal: %q", view)
	}
	lines := strings.Split(view, "\n")
	if len(lines) > minimumHeight {
		t.Fatalf("line count=%d limit=%d view=%q", len(lines), minimumHeight, view)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), minimumWidth, line)
		}
	}
}

func TestAgentUILongPreferenceKeepsConfirmationControlsVisible(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{
		Content: strings.Repeat("完整候选内容", 180), Reason: strings.Repeat("保存原因", 40),
		Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
	}
	conversation := &fakeConversation{result: agentloop.Result{Pending: pending}}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 80, Height: minimumHeight})
	value = updated.(model)
	value.input.SetValue("记住")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	view := value.View()
	if !strings.Contains(view, "保存为长期偏好") || !strings.Contains(view, "PgUp/PgDn") || value.viewport.TotalLineCount() <= value.viewport.Height {
		t.Fatalf("long confirmation is not inspectable: lines=%d height=%d view=%s", value.viewport.TotalLineCount(), value.viewport.Height, view)
	}
	before := value.viewport.YOffset
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})
	value = updated.(model)
	if value.viewport.YOffset >= before || conversation.resolveCalls != 0 {
		t.Fatalf("scroll failed or resolved unexpectedly: before=%d after=%d calls=%d", before, value.viewport.YOffset, conversation.resolveCalls)
	}
}

func TestAgentUIStreamsThinkingAndToolActivityIntoTranscript(t *testing.T) {
	toolDone := agentloop.Event{ID: "call-1", Tool: "search_knowledge", Summary: "检索到 2 条知识片段", Status: agentloop.EventSucceeded}
	conversation := &fakeConversation{
		activities: []agentloop.Activity{
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-1", Summary: "正在分析问题", Status: agentloop.EventRunning}},
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-1", Summary: "已确定下一步工具操作", Status: agentloop.EventSucceeded}},
			{Kind: agentloop.ActivityTool, Event: agentloop.Event{ID: "call-1", Tool: "search_knowledge", Summary: "正在检索服务端知识库", Status: agentloop.EventRunning}},
			{Kind: agentloop.ActivityTool, Event: toolDone},
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-2", Summary: "正在结合工具结果继续分析", Status: agentloop.EventRunning}},
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-2", Summary: "已完成回答组织", Status: agentloop.EventSucceeded}},
		},
		result: agentloop.Result{Text: "这是结合知识库后的回答。", Events: []agentloop.Event{toolDone}},
	}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 88, Height: 42})
	value = updated.(model)
	value.input.SetValue("解释图论")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	start := command().(turnMsg)
	updated, wait := value.Update(start)
	value = updated.(model)
	updated, next := value.Update(wait())
	value = updated.(model)
	if !value.busy || !strings.Contains(value.viewport.View(), "思考中") || !strings.Contains(value.viewport.View(), "正在分析问题") {
		t.Fatalf("live thinking state missing: %s", value.viewport.View())
	}
	value = runTurn(t, value, next)
	transcript := value.viewport.View()
	for _, expected := range []string{"已思考", "工具调用 · 1 项", "检索知识库", "检索到 2 条知识片段", "这是结合知识库后的回答"} {
		if !strings.Contains(transcript, expected) {
			t.Fatalf("transcript missing %q: %s", expected, transcript)
		}
	}
	if strings.Count(transcript, "检索到 2 条知识片段") != 1 || strings.Contains(transcript, `{"query"`) {
		t.Fatalf("tool lifecycle duplicated or leaked arguments: %s", transcript)
	}
}

func TestAgentUIToolTimelineShowsStatusAndDetails(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{
		Text: "已完成检查。",
		Events: []agentloop.Event{
			{Tool: "search_knowledge", Summary: "检索到 2 条知识片段", Status: agentloop.EventSucceeded},
			{Tool: "get_due_reviews", Summary: "复习任务不可用", Status: agentloop.EventFailed, Detail: "protocol_error"},
		},
	}}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("检查工具")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	collapsed := value.viewport.View()
	if !strings.Contains(collapsed, "工具调用 · 2 项") || !strings.Contains(collapsed, "检索知识库") || !strings.Contains(collapsed, "复习任务不可用") || strings.Contains(collapsed, "protocol_error") {
		t.Fatalf("collapsed tool timeline=%s", collapsed)
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	value = updated.(model)
	expanded := value.viewport.View()
	if !strings.Contains(expanded, "search_knowledge") || !strings.Contains(expanded, "protocol_error") || !strings.Contains(expanded, "状态：失败") {
		t.Fatalf("expanded tool timeline=%s", expanded)
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ctrl+0")})
	value = updated.(model)
	if value.toolsExpanded || strings.Contains(value.viewport.View(), "protocol_error") {
		t.Fatalf("Ctrl+0 did not collapse activity details: %s", value.viewport.View())
	}
}

func TestAgentUIHistoryScrollSupportsArrowPageAndMouseInputs(t *testing.T) {
	base := newModel(t.Context(), &fakeConversation{}, "model")
	base.entries = append(base.entries, transcriptEntry{kind: entryAssistant, text: strings.Repeat("历史内容\n", 120)})
	base.refreshTranscript(true)
	if !base.viewport.AtBottom() {
		t.Fatal("test fixture did not start at transcript bottom")
	}

	upInputs := []struct {
		name string
		msg  tea.Msg
	}{
		{name: "arrow up", msg: tea.KeyMsg{Type: tea.KeyUp}},
		{name: "page up", msg: tea.KeyMsg{Type: tea.KeyPgUp}},
		{name: "mouse wheel up", msg: tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}},
	}
	for _, input := range upInputs {
		t.Run(input.name, func(t *testing.T) {
			value := base
			value.viewport.GotoBottom()
			value.follow, value.hasNewContent = true, false
			before := value.viewport.YOffset
			updated, _ := value.Update(input.msg)
			value = updated.(model)
			if value.viewport.YOffset >= before || value.follow {
				t.Fatalf("input did not scroll history upward: before=%d after=%d follow=%t", before, value.viewport.YOffset, value.follow)
			}
		})
	}

	downInputs := []struct {
		name string
		msg  tea.Msg
	}{
		{name: "arrow down", msg: tea.KeyMsg{Type: tea.KeyDown}},
		{name: "page down", msg: tea.KeyMsg{Type: tea.KeyPgDown}},
		{name: "mouse wheel down", msg: tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}},
	}
	for _, input := range downInputs {
		t.Run(input.name, func(t *testing.T) {
			value := base
			value.viewport.GotoTop()
			value.follow, value.hasNewContent = false, true
			before := value.viewport.YOffset
			updated, _ := value.Update(input.msg)
			value = updated.(model)
			if value.viewport.YOffset <= before {
				t.Fatalf("input did not scroll history downward: before=%d after=%d", before, value.viewport.YOffset)
			}
		})
	}

	value := base
	value.viewport.GotoBottom()
	value.follow, value.hasNewContent = false, true
	updated, _ := value.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	value = updated.(model)
	if !value.follow || value.hasNewContent {
		t.Fatalf("scrolling at bottom did not restore follow mode: follow=%t new=%t", value.follow, value.hasNewContent)
	}
}

func TestAgentUIScrollPauseShowsNewContentIndicator(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	value.entries = append(value.entries, transcriptEntry{kind: entryAssistant, text: strings.Repeat("一行内容\n", 80)})
	value.refreshTranscript(true)
	updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})
	value = updated.(model)
	if value.follow {
		t.Fatal("scrolling up did not pause follow mode")
	}
	before := value.viewport.YOffset
	updated, _ = value.Update(turnMsg{kind: turnSend, result: agentloop.Result{Text: "这是新回复"}, done: true})
	value = updated.(model)
	if !value.hasNewContent || value.viewport.YOffset != before || !strings.Contains(value.View(), "有新消息") {
		t.Fatalf("follow=%t new=%t before=%d after=%d view=%s", value.follow, value.hasNewContent, before, value.viewport.YOffset, value.View())
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	value = updated.(model)
	if value.hasNewContent || !value.follow || !value.viewport.AtBottom() {
		t.Fatalf("Ctrl+G did not restore follow mode: follow=%t new=%t bottom=%t", value.follow, value.hasNewContent, value.viewport.AtBottom())
	}
}

func TestAgentUIPreservesCommittedAnswerWhenSessionPublicationFails(t *testing.T) {
	conversation := &fakeConversation{
		result:  agentloop.Result{Text: "模型已经提交的正文"},
		sendErr: errors.New("session_save_failed: checkpoint publication failed"),
	}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("不要重放这条输入")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	transcript := value.viewport.View()
	if !strings.Contains(transcript, "模型已经提交的正文") || !strings.Contains(transcript, "checkpoint publication failed") || value.input.Value() != "" {
		t.Fatalf("committed result was not preserved safely: input=%q transcript=%s", value.input.Value(), transcript)
	}
}

func TestAgentUIKeepsStructuredErrorCard(t *testing.T) {
	conversation := &fakeConversation{sendErr: errors.New("provider unavailable")}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("测试错误")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	transcript := value.viewport.View()
	if value.busy || value.status != "请求失败" || !strings.Contains(transcript, "请求失败") || !strings.Contains(transcript, "阶段：模型请求") || !strings.Contains(transcript, "provider unavailable") {
		t.Fatalf("error state busy=%t status=%q transcript=%s", value.busy, value.status, transcript)
	}
}

type contextAwareConversation struct {
	fakeConversation
	status  agentloop.ContextStatus
	updates chan agentloop.ContextEvent
}

func (c *contextAwareConversation) ContextStatus() agentloop.ContextStatus        { return c.status }
func (c *contextAwareConversation) ContextUpdates() <-chan agentloop.ContextEvent { return c.updates }

func TestAgentUISidebarShowsContextTokensAndCumulativeCacheHitRate(t *testing.T) {
	conversation := &contextAwareConversation{
		status: agentloop.ContextStatus{
			Estimated: true, WindowPercent: 38, CurrentTokens: 12340, ContextWindow: 32768,
			RecentCompleteTurns: 3, MemoryItemCount: 4,
		},
		updates: make(chan agentloop.ContextEvent, 2),
	}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	value = updated.(model)
	view := value.View()
	if !strings.Contains(view, "上下文 约12.3k/32.8k") || !strings.Contains(view, "缓存命中 —") {
		t.Fatalf("estimated sidebar metrics missing: %s", view)
	}

	conversation.updates <- agentloop.ContextEvent{
		Kind: agentloop.ContextEventStatus,
		Status: agentloop.ContextStatus{
			WindowPercent: 37, CurrentTokens: 12000, ContextWindow: 32768,
			CachePromptTokens: 12000, CacheReadTokens: 0, CacheHitRate: 0, CacheHitRateAvailable: true,
			RecentCompleteTurns: 3, MemoryItemCount: 4,
		},
	}
	message := waitContextCmd(value.ctx, conversation.updates)().(contextMsg)
	updated, _ = value.Update(message)
	value = updated.(model)
	view = value.View()
	if !strings.Contains(view, "上下文 12k/32.8k") || !strings.Contains(view, "缓存命中 0.0%") {
		t.Fatalf("explicit zero cache-hit metrics missing: %s", view)
	}

	conversation.updates <- agentloop.ContextEvent{
		Kind: agentloop.ContextEventStatus,
		Status: agentloop.ContextStatus{
			WindowPercent: 37, CurrentTokens: 12000, ContextWindow: 32768,
			CachePromptTokens: 20000, CacheReadTokens: 12000, CacheHitRate: 60, CacheHitRateAvailable: true,
			RecentCompleteTurns: 3, MemoryItemCount: 4,
		},
	}
	message = waitContextCmd(value.ctx, conversation.updates)().(contextMsg)
	updated, _ = value.Update(message)
	value = updated.(model)
	view = value.View()
	if !strings.Contains(view, "上下文 12k/32.8k") || !strings.Contains(view, "缓存命中 60.0%") {
		t.Fatalf("actual sidebar metrics missing: %s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 120 {
			t.Fatalf("line width=%d limit=120 line=%q", lipgloss.Width(line), line)
		}
	}
}

func TestAgentUIContextHeaderUpdatesAndStructuredCards(t *testing.T) {
	conversation := &contextAwareConversation{
		status:  agentloop.ContextStatus{Estimated: true, WindowPercent: 54, RecentCompleteTurns: 6, MemoryItemCount: 18, Mode: agentloop.ContextCompactionAuto},
		updates: make(chan agentloop.ContextEvent, 4),
	}
	value := newModel(t.Context(), conversation, "model")
	updatedModel, _ := value.Update(tea.WindowSizeMsg{Width: 96, Height: 48})
	value = updatedModel.(model)
	if view := value.View(); !strings.Contains(view, "上下文 约54%") || !strings.Contains(view, "最近 6 轮") || !strings.Contains(view, "记忆 18 条") {
		t.Fatalf("initial context status missing: %s", view)
	}
	conversation.updates <- agentloop.ContextEvent{
		Kind:   agentloop.ContextEventStatus,
		Status: agentloop.ContextStatus{Estimated: true, WindowPercent: 63, RecentCompleteTurns: 5, MemoryItemCount: 20},
	}
	message := waitContextCmd(value.ctx, conversation.updates)().(contextMsg)
	updated, next := value.Update(message)
	value = updated.(model)
	view := value.View()
	if next == nil || !strings.Contains(view, "上下文 约63%") || !strings.Contains(view, "最近 5 轮") || !strings.Contains(view, "记忆 20 条") || strings.Contains(value.viewport.View(), "上下文已整理") {
		t.Fatalf("routine status should update chrome only: %s", view)
	}

	for _, event := range []agentloop.ContextEvent{
		{Kind: agentloop.ContextEventCompacted, Code: "context_compacted", DroppedTurns: 9, RecentTurns: 6, ObservationCount: 14, ReflectionCount: 3},
		{Kind: agentloop.ContextEventDegraded, Code: agentloop.ContextCompactionDegraded, DroppedTurns: 4},
		{Kind: agentloop.ContextEventSourceUnavailable, Code: agentloop.ContextSourceUnavailable},
	} {
		event.Status = value.contextStatus
		updated, _ = value.Update(contextMsg{event: event, stream: conversation.updates})
		value = updated.(model)
	}
	transcript := value.viewport.View()
	for _, expected := range []string{"上下文已整理", "已整理较早 9 轮", "上下文整理降级", agentloop.ContextCompactionDegraded, "会话证据来源不可用", agentloop.ContextSourceUnavailable} {
		if !strings.Contains(transcript, expected) {
			t.Fatalf("context transcript missing %q: %s", expected, transcript)
		}
	}
	for _, forbidden := range []string{"SECRET", `{"memory_id"`, "Observer prompt", "raw arguments"} {
		if strings.Contains(transcript, forbidden) {
			t.Fatalf("context card leaked %q: %s", forbidden, transcript)
		}
	}
}

func TestAgentUIContextCardsRespectScrollPause(t *testing.T) {
	conversation := &contextAwareConversation{updates: make(chan agentloop.ContextEvent, 1)}
	value := newModel(t.Context(), conversation, "model")
	value.entries = append(value.entries, transcriptEntry{kind: entryAssistant, text: strings.Repeat("历史内容\n", 80)})
	value.refreshTranscript(true)
	updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})
	value = updated.(model)
	before := value.viewport.YOffset
	updated, _ = value.Update(contextMsg{event: agentloop.ContextEvent{
		Kind: agentloop.ContextEventCompacted, DroppedTurns: 5, RecentTurns: 3,
	}, stream: conversation.updates})
	value = updated.(model)
	if !value.hasNewContent || value.viewport.YOffset != before || !strings.Contains(value.View(), "有新消息") {
		t.Fatalf("context card ignored paused scroll: before=%d after=%d view=%s", before, value.viewport.YOffset, value.View())
	}
}

func TestAgentUITypedContextErrorsHaveDistinctGuidance(t *testing.T) {
	budget := errorCardText(&agentloop.ContextError{Code: agentloop.ContextBudgetInvalid, Err: errors.New("fixed budget")}, false)
	turn := errorCardText(&agentloop.ContextError{Code: agentloop.ContextTurnTooLarge, Err: errors.New("large turn")}, false)
	recent := errorCardText(&agentloop.ContextError{Code: agentloop.ContextRecentTurnsTooLarge, Err: errors.New("large recent turns")}, false)
	if !strings.Contains(budget, "恢复自动整理") || !strings.Contains(turn, "当前这一轮本身过大") ||
		!strings.Contains(recent, "最近完整轮次无法同时保留") || budget == turn || turn == recent {
		t.Fatalf("typed guidance budget=%q turn=%q recent=%q", budget, turn, recent)
	}
	legacy := errorCardText(errors.New("context overflow"), false)
	if !strings.Contains(legacy, "对话上下文") || !strings.Contains(legacy, "开启新会话") {
		t.Fatalf("legacy fallback=%q", legacy)
	}
	deadline := errorCardText(context.DeadlineExceeded, false)
	if !strings.Contains(deadline, "模型响应") || !strings.Contains(deadline, "无响应超时") || !strings.Contains(deadline, "model set --timeout") {
		t.Fatalf("deadline guidance=%q", deadline)
	}
}

func TestAgentUIRecallDisplayName(t *testing.T) {
	if got := toolDisplayName("recall_session_memory"); got != "回查会话证据" {
		t.Fatalf("display name=%q", got)
	}
}
func TestRenderMarkdownSupportsLearningAnswerStructure(t *testing.T) {
	rendered := renderMarkdown("## 结论\n\n- 第一项\n- 第二项\n\n> 关键提示\n\n```go\nfmt.Println(\"ok\")\n```", 60)
	for _, expected := range []string{"结论", "• 第一项", "• 第二项", "关键提示", "go", "fmt.Println"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered markdown missing %q: %s", expected, rendered)
		}
	}
}

func TestAgentUIMinimumTerminalWidthStaysBounded(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "a-very-long-model-name-that-must-be-truncated")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	value.busy = true
	value.status = "正在保存长期偏好"
	view := value.View()
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), minimumWidth, line)
		}
	}
}

func TestAgentUISmallTerminalStaysBounded(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 12, Height: 3})
	view := updated.(model).View()
	lines := strings.Split(view, "\n")
	if len(lines) > 3 {
		t.Fatalf("lines=%d view=%q", len(lines), view)
	}
	for _, line := range lines {
		if len([]rune(line)) > 12 {
			t.Fatalf("line too wide: %q", line)
		}
	}
}

func testQuestion(mode agentloop.QuestionMode) *agentloop.PendingQuestion {
	return &agentloop.PendingQuestion{
		ID: "next-step", Header: "下一步", Question: "你希望如何继续？", Mode: mode, AllowCustom: true,
		Options: []agentloop.QuestionOption{
			{ID: "first", Label: "第一项", Description: "继续第一项内容"},
			{ID: "second", Label: "第二项", Description: "继续第二项内容"},
		},
	}
}

func TestAgentUIQuestionSelectorSupportsSingleMultipleCustomAndCancel(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		conversation := &fakeConversation{result: agentloop.Result{PendingQuestion: testQuestion(agentloop.QuestionSingle)}, questionResolved: agentloop.Result{Text: "已选择"}}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("请问我")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyDown})
		value = updated.(model)
		updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		if conversation.questionCalls != 1 || len(conversation.questionAnswer.OptionIDs) != 1 || conversation.questionAnswer.OptionIDs[0] != "second" {
			t.Fatalf("answer=%+v calls=%d", conversation.questionAnswer, conversation.questionCalls)
		}
	})

	t.Run("multiple", func(t *testing.T) {
		conversation := &fakeConversation{result: agentloop.Result{PendingQuestion: testQuestion(agentloop.QuestionMultiple)}, questionResolved: agentloop.Result{Text: "已多选"}}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("请多选")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		updated, noSubmit := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = updated.(model)
		if noSubmit != nil || value.selector == nil || value.selector.submitted {
			t.Fatalf("empty multiple selection submitted unexpectedly: command=%v selector=%+v", noSubmit, value.selector)
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
		value = updated.(model)
		if value.selector.focus != 1 || value.selector.hasSelectedOptions() {
			t.Fatalf("numeric shortcut must focus without toggling: focus=%d options=%+v", value.selector.focus, value.selector.options)
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeySpace})
		value = updated.(model)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
		value = updated.(model)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeySpace})
		value = updated.(model)
		updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		if strings.Join(conversation.questionAnswer.OptionIDs, ",") != "first,second" {
			t.Fatalf("answer=%+v", conversation.questionAnswer)
		}
	})

	t.Run("custom multiline", func(t *testing.T) {
		conversation := &fakeConversation{result: agentloop.Result{PendingQuestion: testQuestion(agentloop.QuestionSingle)}, questionResolved: agentloop.Result{Text: "已记录自定义"}}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("我要自定义")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("0")})
		value = updated.(model)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第一行")})
		value = updated.(model)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
		value = updated.(model)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第二行")})
		value = updated.(model)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyUp})
		value = updated.(model)
		if value.selector.focus != len(value.selector.options) {
			t.Fatalf("custom editor Up escaped the editor: focus=%d", value.selector.focus)
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyDown})
		value = updated.(model)
		updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		if conversation.questionAnswer.Custom != "第一行\n第二行" || len(conversation.questionAnswer.OptionIDs) != 0 {
			t.Fatalf("answer=%+v", conversation.questionAnswer)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		conversation := &fakeConversation{result: agentloop.Result{PendingQuestion: testQuestion(agentloop.QuestionSingle)}, questionResolved: agentloop.Result{Text: "已取消"}}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("取消问题")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
		value = runTurn(t, updated.(model), command)
		if conversation.questionAnswer.Status != agentloop.QuestionCancelled {
			t.Fatalf("answer=%+v", conversation.questionAnswer)
		}
	})
}

func TestAgentUICancelledInteractionContinuationClearsConsumedSelector(t *testing.T) {
	t.Run("question", func(t *testing.T) {
		conversation := &fakeConversation{
			result:          agentloop.Result{PendingQuestion: testQuestion(agentloop.QuestionSingle)},
			questionStarted: make(chan struct{}), questionRelease: make(chan struct{}),
		}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("请问我")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)

		updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = updated.(model)
		message := command()
		updated, command = value.Update(message)
		value = updated.(model)
		select {
		case <-conversation.questionStarted:
		case <-time.After(time.Second):
			t.Fatal("question continuation did not start")
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
		value = updated.(model)
		message = command()
		updated, _ = value.Update(message)
		value = updated.(model)
		if value.busy || value.selector != nil || value.pendingQuestion != nil || !value.input.Focused() {
			t.Fatalf("cancelled question restored stale selector: busy=%t selector=%+v pending=%+v", value.busy, value.selector, value.pendingQuestion)
		}
	})

	t.Run("session-only preference", func(t *testing.T) {
		pending := &agentloop.PreferenceConfirmation{Content: "先给结论", Reason: "明确要求", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable"}
		conversation := &fakeConversation{
			result:         agentloop.Result{Pending: pending},
			resolveStarted: make(chan struct{}), resolveRelease: make(chan struct{}),
		}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("记住偏好")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyDown})
		value = updated.(model)

		updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = updated.(model)
		message := command()
		updated, command = value.Update(message)
		value = updated.(model)
		select {
		case <-conversation.resolveStarted:
		case <-time.After(time.Second):
			t.Fatal("preference continuation did not start")
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
		value = updated.(model)
		message = command()
		updated, _ = value.Update(message)
		value = updated.(model)
		if value.busy || value.selector != nil || value.pending != nil || !value.input.Focused() {
			t.Fatalf("cancelled preference restored stale selector: busy=%t selector=%+v pending=%+v", value.busy, value.selector, value.pending)
		}
	})
}

func TestAgentUIPreferenceSelectorUsesThreeExplicitResolutions(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{Content: "先给结论", Reason: "明确要求", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable"}
	tests := []struct {
		name string
		keys []tea.KeyMsg
		want agentloop.PreferenceResolution
	}{
		{name: "save", keys: []tea.KeyMsg{{Type: tea.KeyEnter}}, want: agentloop.PreferenceSave},
		{name: "session only", keys: []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyEnter}}, want: agentloop.PreferenceSessionOnly},
		{name: "decline with escape", keys: []tea.KeyMsg{{Type: tea.KeyEsc}}, want: agentloop.PreferenceDecline},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversation := &fakeConversation{result: agentloop.Result{Pending: pending}, resolved: agentloop.Result{Text: "已处理"}}
			value := newModel(t.Context(), conversation, "model")
			value.input.SetValue("处理偏好")
			updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
			value = runTurn(t, updated.(model), command)
			for index, key := range test.keys {
				updated, command = value.Update(key)
				value = updated.(model)
				if index+1 < len(test.keys) && command != nil {
					t.Fatalf("navigation unexpectedly started resolution")
				}
			}
			value = runTurn(t, value, command)
			if conversation.resolveCalls != 1 || conversation.resolution != test.want {
				t.Fatalf("resolution=%q calls=%d", conversation.resolution, conversation.resolveCalls)
			}
		})
	}
}

type blockingPreferenceConversation struct {
	fakeConversation
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	writes   int
}

func (c *blockingPreferenceConversation) ResolvePreference(ctx context.Context, resolution agentloop.PreferenceResolution) (agentloop.Result, error) {
	c.writes++
	c.resolution = resolution
	close(c.started)
	select {
	case <-c.release:
		return agentloop.Result{Text: "保存完成"}, nil
	case <-ctx.Done():
		close(c.canceled)
		return agentloop.Result{}, ctx.Err()
	}
}

func TestAgentUIPreferenceWriteIgnoresEscapeAndExecutesOnce(t *testing.T) {
	conversation := &blockingPreferenceConversation{
		fakeConversation: fakeConversation{result: agentloop.Result{Pending: &agentloop.PreferenceConfirmation{Content: "先给结论", Reason: "明确要求", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable"}}},
		started:          make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{}),
	}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("保存偏好")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	start := command().(turnMsg)
	updated, wait := value.Update(start)
	value = updated.(model)
	result := make(chan tea.Msg, 1)
	go func() { result <- wait() }()
	select {
	case <-conversation.started:
	case <-time.After(time.Second):
		t.Fatal("preference write did not start")
	}
	updated, stop := value.Update(tea.KeyMsg{Type: tea.KeyEsc})
	value = updated.(model)
	if stop != nil || value.stopping {
		t.Fatalf("non-cancellable write accepted Esc: stopping=%t", value.stopping)
	}
	select {
	case <-conversation.canceled:
		t.Fatal("preference write context was cancelled")
	case <-time.After(20 * time.Millisecond):
	}
	close(conversation.release)
	updated, _ = value.Update(<-result)
	value = updated.(model)
	if value.busy || conversation.writes != 1 || conversation.resolution != agentloop.PreferenceSave {
		t.Fatalf("busy=%t writes=%d resolution=%q", value.busy, conversation.writes, conversation.resolution)
	}
}

func TestAgentUIReasoningSelectorAndUnsupportedError(t *testing.T) {
	t.Run("idle selection", func(t *testing.T) {
		conversation := &fakeConversation{reasoningEffort: modelclient.ReasoningEffortAuto}
		value := newModel(t.Context(), conversation, "model")
		updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyF3})
		value = updated.(model)
		if value.selector == nil || value.selector.kind != selectorReasoning || !strings.Contains(value.View(), "none") || !strings.Contains(value.View(), "auto") {
			t.Fatalf("reasoning selector missing: %s", value.View())
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")})
		value = updated.(model)
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = updated.(model)
		if conversation.reasoningEffort != modelclient.ReasoningEffortMedium || !strings.Contains(value.View(), "推理 medium") {
			t.Fatalf("effort=%q view=%s", conversation.reasoningEffort, value.View())
		}
	})

	t.Run("provider rejection has stable code", func(t *testing.T) {
		conversation := &fakeConversation{sendErr: &modelclient.ClientError{Code: modelclient.ErrorCodeReasoningEffortUnsupported, Message: "所选推理强度不受支持"}}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("测试推理")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		if !strings.Contains(value.View(), string(modelclient.ErrorCodeReasoningEffortUnsupported)) || value.input.Value() != "测试推理" {
			t.Fatalf("error/input missing: input=%q view=%s", value.input.Value(), value.View())
		}
	})
}

func TestAgentUIReasoningEffortSwitchClearsPendingMarkerWhenNextRequestStarts(t *testing.T) {
	conversation := &fakeConversation{reasoningEffort: modelclient.ReasoningEffortLow}
	value := newModel(t.Context(), conversation, "model")
	value.busy = true
	value.activeTurnID = 1
	value.activeEffort = modelclient.ReasoningEffortLow
	value.handleActivity(1, agentloop.Activity{
		Kind: agentloop.ActivityThinking, Phase: agentloop.ActivityWaitingModel,
		ReasoningEffort: modelclient.ReasoningEffortLow,
		Event:           agentloop.Event{ID: "thinking-1", Summary: "正在分析问题", Status: agentloop.EventRunning},
	})
	value.applyReasoningEffort(modelclient.ReasoningEffortHigh)
	if footer := value.renderFooterStatus(120); !strings.Contains(footer, "下一请求 high") {
		t.Fatalf("pending effort not shown: %s", footer)
	}
	value.handleActivity(1, agentloop.Activity{
		Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool,
		Event: agentloop.Event{ID: "tool-1", Tool: "get_learning_progress", Summary: "学习进度已读取", Status: agentloop.EventSucceeded},
	})
	value.handleActivity(1, agentloop.Activity{
		Kind: agentloop.ActivityThinking, Phase: agentloop.ActivityWaitingModel,
		ReasoningEffort: modelclient.ReasoningEffortHigh,
		Event:           agentloop.Event{ID: "thinking-2", Summary: "正在结合工具结果继续分析", Status: agentloop.EventRunning},
	})
	if footer := value.renderFooterStatus(120); value.activeEffort != modelclient.ReasoningEffortHigh || strings.Contains(footer, "下一请求") {
		t.Fatalf("next request did not activate selected effort: active=%q footer=%s", value.activeEffort, footer)
	}
}

func TestAgentUIStreamingDeduplicatesFinalAndKeepsProtocolPartial(t *testing.T) {
	t.Run("final replaces draft", func(t *testing.T) {
		conversation := &fakeConversation{
			activities: []agentloop.Activity{
				{Kind: agentloop.ActivityTextDelta, Delta: "最终"},
				{Kind: agentloop.ActivityTextDelta, Delta: "回答"},
			},
			result: agentloop.Result{Text: "最终回答"},
		}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("流式")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		if count := strings.Count(value.viewport.View(), "最终回答"); count != 1 {
			t.Fatalf("final duplicated %d times: %s", count, value.viewport.View())
		}
	})

	t.Run("protocol failure preserves sanitized partial", func(t *testing.T) {
		conversation := &fakeConversation{
			activities: []agentloop.Activity{{Kind: agentloop.ActivityTextDelta, Delta: "部分回答\x1b[2J"}},
			sendErr:    &modelclient.ClientError{Code: modelclient.ErrorCodeStreamProtocol, Message: "流协议失败"},
		}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("流失败")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		view := value.View()
		if !strings.Contains(view, "部分回答") || !strings.Contains(view, "未完成") || !strings.Contains(view, string(modelclient.ErrorCodeStreamProtocol)) || strings.ContainsRune(view, '\x1b') {
			t.Fatalf("partial/error state=%s", view)
		}
	})
}

type cancellableActivityConversation struct {
	fakeConversation
	started    chan struct{}
	canceled   chan struct{}
	activities []agentloop.Activity
}

func (c *cancellableActivityConversation) Send(ctx context.Context, _ string) (agentloop.Result, error) {
	for _, activity := range c.activities {
		agentloop.PublishActivity(ctx, activity)
	}
	close(c.started)
	<-ctx.Done()
	close(c.canceled)
	return agentloop.Result{}, ctx.Err()
}

func TestAgentUIEscapeStopsTurnPreservesVisibleWorkAndRejectsLateEvents(t *testing.T) {
	conversation := &cancellableActivityConversation{
		started: make(chan struct{}), canceled: make(chan struct{}),
		activities: []agentloop.Activity{
			{Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool, Event: agentloop.Event{ID: "tool-1", Tool: "search_knowledge", Summary: "检索完成", Status: agentloop.EventSucceeded}},
			{Kind: agentloop.ActivityTextDelta, Delta: "部分答案"},
		},
	}
	value := newModel(context.Background(), conversation, "model")
	value.input.SetValue("需要取消")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	start := command().(turnMsg)
	oldTurnID := start.turnID
	updated, wait := value.Update(start)
	value = updated.(model)
	for range conversation.activities {
		updated, wait = value.Update(wait())
		value = updated.(model)
	}
	select {
	case <-conversation.started:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- wait() }()
	updated, firstStop := value.Update(tea.KeyMsg{Type: tea.KeyEsc})
	value = updated.(model)
	updated, secondStop := value.Update(tea.KeyMsg{Type: tea.KeyEsc})
	value = updated.(model)
	if firstStop != nil || secondStop != nil || !value.stopping {
		t.Fatalf("repeated Esc state: stopping=%t first=%v second=%v", value.stopping, firstStop, secondStop)
	}
	select {
	case <-conversation.canceled:
	case <-time.After(time.Second):
		t.Fatal("turn context was not cancelled")
	}
	updated, _ = value.Update(<-result)
	value = updated.(model)
	view := value.View()
	if value.busy || !strings.Contains(view, "需要取消") || !strings.Contains(view, "检索完成") || !strings.Contains(view, "部分答案") || !strings.Contains(view, "已停止") || strings.Contains(view, "请求失败") {
		t.Fatalf("cancelled state=%s", view)
	}
	before := len(value.entries)
	late := agentloop.Activity{Kind: agentloop.ActivityTextDelta, Delta: "不应出现"}
	updated, _ = value.Update(turnMsg{turnID: oldTurnID, kind: turnSend, activity: &late, stream: &turnStream{activities: make(chan agentloop.Activity), completion: make(chan turnMsg, 1)}})
	value = updated.(model)
	if len(value.entries) != before || strings.Contains(value.View(), "不应出现") {
		t.Fatalf("late event contaminated transcript: %s", value.View())
	}
}

type delayedCancellationConversation struct {
	fakeConversation
	started        chan struct{}
	cancelObserved chan struct{}
	release        chan struct{}
}

func (c *delayedCancellationConversation) Send(ctx context.Context, _ string) (agentloop.Result, error) {
	close(c.started)
	<-ctx.Done()
	close(c.cancelObserved)
	<-c.release
	return agentloop.Result{}, ctx.Err()
}

func TestAgentUIWaitsForCancelledWorkerBeforeRestoringComposer(t *testing.T) {
	conversation := &delayedCancellationConversation{
		started: make(chan struct{}), cancelObserved: make(chan struct{}), release: make(chan struct{}),
	}
	value := newModel(context.Background(), conversation, "model")
	value.input.SetValue("延迟停止")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	start := command().(turnMsg)
	updated, wait := value.Update(start)
	value = updated.(model)
	select {
	case <-conversation.started:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- wait() }()
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
	value = updated.(model)
	select {
	case <-conversation.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("worker did not observe cancellation")
	}
	select {
	case message := <-result:
		t.Fatalf("UI completed before worker acknowledged cancellation: %#v", message)
	case <-time.After(25 * time.Millisecond):
	}
	value.input.SetValue("不应发送")
	updated, duplicate := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if duplicate != nil || !value.busy || !value.stopping {
		t.Fatalf("composer reopened while worker was alive: busy=%t stopping=%t command=%v", value.busy, value.stopping, duplicate)
	}
	close(conversation.release)
	select {
	case message := <-result:
		updated, _ = value.Update(message)
		value = updated.(model)
	case <-time.After(time.Second):
		t.Fatal("cancelled worker did not finish")
	}
	if value.busy || value.stopping || !strings.Contains(value.View(), "已停止") {
		t.Fatalf("cancel completion state: %s", value.View())
	}
}

func TestAgentUIActivityDetailsShowPhaseElapsedAndSlowHint(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	value.busy = true
	value.activeCancelable = true
	value.activeTurnID = 1
	value.activeStarted = time.Now().Add(-slowTurnThreshold - time.Second)
	value.handleActivity(1, agentloop.Activity{
		Kind: agentloop.ActivityThinking, Phase: agentloop.ActivityWaitingModel,
		StartedAt: time.Now().Add(-3 * time.Second), UpdatedAt: time.Now(), TimeoutBudget: 90 * time.Second,
		Event: agentloop.Event{ID: "thinking", Summary: "等待模型", Status: agentloop.EventRunning},
	})
	value.toolsExpanded = true
	value.refreshTranscript(false)
	view := value.View()
	if !strings.Contains(view, "等待模型响应") || !strings.Contains(view, "用时：") || !strings.Contains(view, "已运行") || !strings.Contains(view, "无响应超时 1m30s") || !strings.Contains(view, "无响应超时：1m30s") || !strings.Contains(view, "Esc") {
		t.Fatalf("lifecycle details missing: %s", view)
	}
}

func TestAgentUIQuestionSelectorWrapsLongOptionLabelsAndDescriptions(t *testing.T) {
	question := testQuestion(agentloop.QuestionSingle)
	question.Header = strings.Repeat("题", 17) + "尾"
	question.Question = strings.Repeat("问", 35) + "尾"
	question.Options[0].Label = strings.Repeat("宽", 15) + "尾"
	question.Options[0].Description = strings.Repeat("详", 29) + "尾"
	conversation := &fakeConversation{result: agentloop.Result{PendingQuestion: question}}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	value.input.SetValue("显示长选项")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	view := value.View()
	if strings.Count(view, "题") != 17 || strings.Count(view, "问") != 35 || strings.Count(view, "宽") != 15 || strings.Count(view, "详") != 29 {
		t.Fatalf("maximum legal question content was truncated: %s", view)
	}
	lines := strings.Split(view, "\n")
	if len(lines) > minimumHeight {
		t.Fatalf("selector exceeded minimum height: lines=%d view=%s", len(lines), view)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), minimumWidth, line)
		}
	}
}

func TestAgentUIQuestionSelectorResizePreservesFocusSelectionAndDraft(t *testing.T) {
	question := testQuestion(agentloop.QuestionMultiple)
	question.Options = append(question.Options,
		agentloop.QuestionOption{ID: "third", Label: "第三项", Description: "继续第三项内容"},
		agentloop.QuestionOption{ID: "fourth", Label: "第四项", Description: "继续第四项内容"},
	)
	conversation := &fakeConversation{result: agentloop.Result{PendingQuestion: question}}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("调整尺寸")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	value = updated.(model)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeySpace})
	value = updated.(model)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("0")})
	value = updated.(model)
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("保留草稿")})
	value = updated.(model)

	for _, size := range []tea.WindowSizeMsg{{Width: 58, Height: 22}, {Width: minimumWidth, Height: minimumHeight}} {
		updated, _ = value.Update(size)
		value = updated.(model)
	}
	if value.selector == nil || value.selector.focus != len(question.Options) || !value.selector.options[1].Selected || value.selector.custom.Value() != "保留草稿" {
		t.Fatalf("resize lost selector state: selector=%+v", value.selector)
	}
	view := value.View()
	lines := strings.Split(view, "\n")
	if len(lines) > minimumHeight || !strings.Contains(view, "保留草稿") {
		t.Fatalf("resized selector exceeded bounds or hid draft: lines=%d view=%s", len(lines), view)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), minimumWidth, line)
		}
	}
}

func TestAgentUIQuestionSelectorFitsMinimumTerminalAndKeepsDetailsScrollable(t *testing.T) {
	question := testQuestion(agentloop.QuestionMultiple)
	question.Question = strings.Repeat("请选择下一步学习方向", 20)
	question.Options = append(question.Options,
		agentloop.QuestionOption{ID: "third", Label: "第三项", Description: strings.Repeat("很长的第三项说明", 30)},
		agentloop.QuestionOption{ID: "fourth", Label: "第四项", Description: strings.Repeat("很长的第四项说明", 30)},
	)
	conversation := &fakeConversation{result: agentloop.Result{PendingQuestion: question}}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	value.input.SetValue("显示问题")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	view := value.View()
	lines := strings.Split(view, "\n")
	if len(lines) > minimumHeight || !strings.Contains(view, "自定义") || !strings.Contains(view, "Enter") || value.viewport.TotalLineCount() <= value.viewport.Height {
		t.Fatalf("minimum selector layout lines=%d total=%d height=%d view=%s", len(lines), value.viewport.TotalLineCount(), value.viewport.Height, view)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), minimumWidth, line)
		}
	}
}
