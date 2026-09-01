package agentui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const (
	minimumWidth      = 46
	minimumHeight     = 18
	horizontalPadding = 6
	turnStreamBuffer  = 128
	slowTurnThreshold = 8 * time.Second
)

type Conversation interface {
	Send(context.Context, string) (agentloop.Result, error)
	ResolvePreference(context.Context, agentloop.PreferenceResolution) (agentloop.Result, error)
	ResolveQuestion(context.Context, agentloop.QuestionAnswer) (agentloop.Result, error)
	ResolveFileMutation(context.Context, string, agentloop.FileMutationResolution) (agentloop.Result, error)
	CancelPendingFileMutation(string) (agentloop.Result, error)
	FileAuthorizationMode() agentloop.FileAuthorizationMode
	SetFileAuthorizationMode(agentloop.FileAuthorizationMode) error
	ReasoningEffort() modelclient.ReasoningEffort
	SetReasoningEffort(modelclient.ReasoningEffort) error
	ContextStatus() agentloop.ContextStatus
	ContextUpdates() <-chan agentloop.ContextEvent
	WorkspaceStatus() agentloop.WorkspaceStatus
	LearningStatus(context.Context) (agentloop.LearningStatus, error)
}

type Runner struct {
	In        io.Reader
	Out       io.Writer
	Session   Conversation
	ModelName string
}

func (r Runner) Run(ctx context.Context) error {
	if r.Session == nil {
		return fmt.Errorf("agent session is not configured")
	}
	initial := newModel(ctx, r.Session, r.ModelName)
	defer initial.cancel()
	program := tea.NewProgram(initial, tea.WithAltScreen(), tea.WithInput(r.In), tea.WithOutput(r.Out), tea.WithContext(ctx))
	_, err := program.Run()
	return err
}

type turnKind string

const (
	turnSend         turnKind = "send"
	turnQuestion     turnKind = "question"
	turnPreference   turnKind = "preference"
	turnFileMutation turnKind = "file_mutation"
)

type turnStream struct {
	activities chan agentloop.Activity
	completion chan turnMsg
	wake       chan struct{}
	deltaMu    sync.Mutex
	pending    string
}

func (s *turnStream) publish(ctx context.Context, activity agentloop.Activity) {
	if activity.Kind == agentloop.ActivityTextDelta {
		if activity.Delta == "" || ctx.Err() != nil {
			return
		}
		s.deltaMu.Lock()
		s.pending += activity.Delta
		s.deltaMu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
		return
	}
	select {
	case s.activities <- activity:
	case <-ctx.Done():
	}
}

func (s *turnStream) popDelta() (agentloop.Activity, bool) {
	s.deltaMu.Lock()
	defer s.deltaMu.Unlock()
	if s.pending == "" {
		return agentloop.Activity{}, false
	}
	delta := s.pending
	s.pending = ""
	return agentloop.Activity{Kind: agentloop.ActivityTextDelta, Delta: delta}, true
}

type turnMsg struct {
	turnID   uint64
	kind     turnKind
	result   agentloop.Result
	err      error
	activity *agentloop.Activity
	stream   *turnStream
	done     bool
	tick     bool
}

type contextMsg struct {
	event  agentloop.ContextEvent
	stream <-chan agentloop.ContextEvent
	closed bool
}

type learningMsg struct {
	status agentloop.LearningStatus
	err    error
}

type model struct {
	ctx                    context.Context
	cancel                 context.CancelFunc
	session                Conversation
	modelName              string
	width                  int
	height                 int
	contentWidth           int
	sidebarWidth           int
	viewport               viewport.Model
	input                  textarea.Model
	entries                []transcriptEntry
	pending                *agentloop.PreferenceConfirmation
	pendingQuestion        *agentloop.PendingQuestion
	pendingFileMutation    *agentloop.PendingFileMutation
	pendingFileTurnID      uint64
	selector               *selectorModel
	busy                   bool
	stopping               bool
	status                 string
	follow                 bool
	hasNewContent          bool
	toolsExpanded          bool
	shownEventKeys         map[string]struct{}
	contextStatus          agentloop.ContextStatus
	contextUpdates         <-chan agentloop.ContextEvent
	workspaceStatus        agentloop.WorkspaceStatus
	learningStatus         agentloop.LearningStatus
	learningLoaded         bool
	learningLoading        bool
	learningRefreshPending bool
	learningFailed         bool
	learningProvider       bool

	turnSeq               uint64
	activeTurnID          uint64
	activeTurnCancel      context.CancelFunc
	activeCancelable      bool
	activeKind            turnKind
	activeInput           string
	activeQuestion        *agentloop.PendingQuestion
	activePreference      *agentloop.PreferenceConfirmation
	activeFileMutation    *agentloop.PendingFileMutation
	activeResolution      agentloop.PreferenceResolution
	activeFileResolution  agentloop.FileMutationResolution
	activeEffort          modelclient.ReasoningEffort
	activePhase           agentloop.ActivityPhase
	activeStarted         time.Time
	activeActivityStarted time.Time
	activeTimeoutBudget   time.Duration
	activeFileTool        string
	activeFileDetail      *agentloop.FileActivityDetail
}

func newModel(ctx context.Context, session Conversation, modelName string) model {
	sessionCtx, cancel := context.WithCancel(ctx)
	input := textarea.New()
	input.Placeholder = "输入学习问题；Agent 会按需读取知识、进度和长期偏好"
	input.Prompt = "› "
	input.ShowLineNumbers = false
	input.CharLimit = 8000
	input.MaxHeight = composerMaxRows
	input.FocusedStyle.Prompt = composerPromptStyle
	input.FocusedStyle.Placeholder = mutedStyle
	input.BlurredStyle.Prompt = mutedStyle
	input.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter", "shift+enter")
	input.Focus()
	view := viewport.New(80, 14)
	workspaceStatus := session.WorkspaceStatus()
	if !workspaceStatus.Available && workspaceStatus.Code == "" {
		workspaceStatus.Code = "workspace_unavailable"
	}
	entries := []transcriptEntry{{kind: entryNotice, text: "可以直接提问，也可以让我结合服务端知识库、学习进度和长期偏好帮助你学习。"}}
	if workspaceStatus.Available {
		entries = append(entries, transcriptEntry{kind: entryNotice, text: fmt.Sprintf("工作区 %s 已启用。默认对每次文件写入/编辑逐次确认；可按 F4 为当前 Session 切换 YOLO。工作区内所有文件（包括隐藏文件、.git、.comet 和可能包含秘密的文件）都可被读取并发送给当前模型 provider。", safeWorkspaceLabel(workspaceStatus.Label))})
		input.Placeholder = "输入问题；Agent 可按需读取或修改工作区文件"
	} else {
		entries = append(entries, transcriptEntry{kind: entryNotice, text: fmt.Sprintf("工作区不可用（%s）；文件工具未启用，普通对话仍可使用。", safeSingleLineTerminalText(workspaceStatus.Code))})
	}
	value := model{
		ctx: sessionCtx, cancel: cancel, session: session, modelName: safeSingleLineTerminalText(modelName), width: 80, height: 24,
		viewport: view, input: input, status: "就绪", follow: true, shownEventKeys: map[string]struct{}{},
		entries:       entries,
		contextStatus: session.ContextStatus(), contextUpdates: session.ContextUpdates(), workspaceStatus: workspaceStatus, learningProvider: true, learningLoading: true,
	}
	value.resize()
	value.refreshTranscript(false)
	return value
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, waitContextCmd(m.ctx, m.contextUpdates), loadLearningStatusCmd(m.ctx, m.session))
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		m.refreshTranscript(false)
		return m, nil
	case turnMsg:
		if msg.turnID != m.activeTurnID {
			return m, nil
		}
		if msg.tick {
			m.refreshTranscript(false)
			return m, waitTurnCmd(m.ctx, msg.turnID, msg.kind, msg.stream)
		}
		if msg.stream != nil && msg.activity == nil && !msg.done {
			return m, waitTurnCmd(m.ctx, msg.turnID, msg.kind, msg.stream)
		}
		if msg.activity != nil {
			if !m.stopping {
				m.handleActivity(msg.turnID, *msg.activity)
				m.refreshTranscript(true)
			}
			return m, waitTurnCmd(m.ctx, msg.turnID, msg.kind, msg.stream)
		}
		m.finishTurn(msg.result, msg.err)
		m.resize()
		m.refreshTranscript(true)
		learningCmd := m.startLearningRefresh()
		return m, tea.Batch(textarea.Blink, learningCmd)
	case learningMsg:
		m.learningLoading = false
		m.learningLoaded = msg.err == nil
		m.learningFailed = msg.err != nil
		if msg.err == nil {
			m.learningStatus = msg.status
		} else {
			m.learningStatus = agentloop.LearningStatus{}
		}
		if m.learningRefreshPending {
			m.learningRefreshPending = false
			return m, m.startLearningRefresh()
		}
		return m, nil
	case contextMsg:
		if msg.closed {
			return m, nil
		}
		m.contextStatus = msg.event.Status
		if msg.event.Kind == agentloop.ContextEventCompacted || msg.event.Kind == agentloop.ContextEventDegraded || msg.event.Kind == agentloop.ContextEventSourceUnavailable {
			m.entries = append(m.entries, transcriptEntry{kind: entryContext, contextEvent: msg.event})
			m.refreshTranscript(true)
		}
		return m, waitContextCmd(m.ctx, msg.stream)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" || key == "ctrl+q" {
		m.cancel()
		return m, tea.Quit
	}
	if m.terminalTooSmall() {
		return m, nil
	}
	if key == "ctrl+r" && m.sidebarWidth > 0 {
		return m, m.startLearningRefresh()
	}
	if m.handleNavigationKey(key) {
		return m, nil
	}
	if key == "f3" {
		return m.handleReasoningKey()
	}
	if key == "f4" {
		return m.handleFileModeKey()
	}
	if m.selector != nil {
		action, command := m.selector.handleKey(msg)
		switch action.kind {
		case selectorCancel:
			switch m.selector.kind {
			case selectorQuestion:
				return m.startQuestionResolution(agentloop.QuestionAnswer{QuestionID: m.selector.questionID, Status: agentloop.QuestionCancelled})
			case selectorFileMutation:
				return m.cancelFileMutation()
			case selectorFileMode:
				m.selector = nil
				m.restoreInteractionSelector()
				m.resize()
				return m, nil
			case selectorReasoning:
				m.selector = nil
				m.restoreInputFocus()
				m.resize()
				return m, nil
			}
		case selectorSubmit:
			switch m.selector.kind {
			case selectorQuestion:
				return m.startQuestionResolution(agentloop.QuestionAnswer{
					QuestionID: m.selector.questionID, Status: agentloop.QuestionAnswered,
					OptionIDs: action.optionIDs, Custom: action.custom,
				})
			case selectorPreference, selectorPreferenceRetry:
				return m.startPreferenceResolution(action.resolution)
			case selectorFileMutation:
				return m.startFileMutationResolution(action.fileResolution)
			case selectorFileMode:
				m.applyFileAuthorizationMode(action.fileMode)
				m.resize()
				m.refreshTranscript(false)
				return m, nil
			case selectorReasoning:
				m.applyReasoningEffort(action.effort)
				m.resize()
				m.refreshTranscript(false)
				return m, nil
			}
		}
		return m, command
	}
	if key == "esc" {
		if m.busy {
			if m.activeCancelable && m.activeTurnCancel != nil && !m.stopping {
				m.stopping = true
				m.status = "正在停止当前轮次"
				m.activePhase = agentloop.ActivityStopping
				m.activeTurnCancel()
			}
			return m, nil
		}
		if strings.TrimSpace(m.input.Value()) == "" {
			m.cancel()
			return m, tea.Quit
		}
		return m, nil
	}
	if m.busy {
		return m, nil
	}
	if key == "enter" {
		m.sanitizeComposer()
		input := strings.TrimSpace(m.input.Value())
		if input == "" {
			return m, nil
		}
		m.entries = append(m.entries, transcriptEntry{kind: entryUser, text: input})
		m.input.Reset()
		m.input.Blur()
		m.pending, m.pendingQuestion = nil, nil
		m.follow, m.hasNewContent = true, false
		m.shownEventKeys = map[string]struct{}{}
		m.refreshTranscript(true)
		return m.beginTurn(turnSend, true, input, nil, nil, "", func(turnCtx context.Context) (agentloop.Result, error) {
			return m.session.Send(turnCtx, input)
		})
	}
	previousHeight := m.input.Height()
	var command tea.Cmd
	m.input, command = m.input.Update(msg)
	m.resize()
	if m.input.Height() != previousHeight {
		m.refreshTranscript(false)
	}
	return m, command
}

func (m model) handleFileModeKey() (tea.Model, tea.Cmd) {
	if !m.workspaceStatus.Available {
		return m, nil
	}
	if m.selector != nil && m.selector.kind == selectorFileMode {
		m.selector = nil
		m.restoreInteractionSelector()
		m.resize()
		return m, nil
	}
	if m.selector != nil && m.selector.kind != selectorFileMutation {
		return m, nil
	}
	m.selector = newFileModeSelector(m.session.FileAuthorizationMode())
	m.input.Blur()
	m.resize()
	return m, nil
}

func (m *model) applyFileAuthorizationMode(mode agentloop.FileAuthorizationMode) {
	if err := m.session.SetFileAuthorizationMode(mode); err != nil {
		m.entries = append(m.entries, transcriptEntry{kind: entryError, text: stableErrorCardText(err)})
		m.status = "文件授权模式更新失败"
		return
	}
	m.selector = nil
	if mode == agentloop.FileAuthorizationYOLO {
		m.entries = append(m.entries, transcriptEntry{kind: entryNotice, text: "文件模式已切换为 YOLO（仅当前 Session）。后续 write/edit 不再逐次确认；隐藏文件、.git、.comet 和秘密文件没有额外路径保护，内容可能发送给当前 provider。"})
		m.status = "文件模式已切换为 YOLO"
	} else {
		m.status = "文件模式已切换为逐次确认"
	}
	m.restoreInteractionSelector()
}

func (m *model) restoreInteractionSelector() {
	switch {
	case m.pendingFileMutation != nil:
		m.selector = newFileMutationSelector(m.pendingFileMutation)
	case m.pendingQuestion != nil:
		m.selector = newQuestionSelector(m.pendingQuestion)
	case m.pending != nil:
		if m.pending.RetryOnly {
			m.selector = newPreferenceRetrySelector()
		} else {
			m.selector = newPreferenceSelector(m.pending)
		}
	default:
		m.restoreInputFocus()
	}
}

func (m model) startFileMutationResolution(resolution agentloop.FileMutationResolution) (tea.Model, tea.Cmd) {
	pending := cloneFileMutation(m.pendingFileMutation)
	if pending == nil {
		return m, nil
	}
	m.pendingFileMutation = nil
	m.selector = nil
	m.input.Blur()
	m.activeFileMutation = pending
	m.activeFileResolution = resolution
	return m.beginTurn(turnFileMutation, true, "", nil, nil, "", func(turnCtx context.Context) (agentloop.Result, error) {
		return m.session.ResolveFileMutation(turnCtx, pending.CallID, resolution)
	})
}

func (m model) cancelFileMutation() (tea.Model, tea.Cmd) {
	pending := cloneFileMutation(m.pendingFileMutation)
	if pending == nil {
		return m, nil
	}
	result, err := m.session.CancelPendingFileMutation(pending.CallID)
	if err == nil {
		activity := agentloop.Activity{
			Kind: agentloop.ActivityTool, Phase: agentloop.ActivityStopped, StableCode: "cancelled",
			Event: agentloop.Event{ID: pending.CallID, Tool: pending.Tool, Summary: "文件修改授权已取消", Status: agentloop.EventFailed, Detail: "cancelled"},
			File: &agentloop.FileActivityDetail{
				Path: pending.Path, Operation: pending.Operation, PreviewKind: pending.PreviewKind,
				Preview: pending.Preview, PreviewTruncated: pending.Truncated,
			},
		}
		m.entries = upsertActivity(m.entries, m.pendingFileTurnID, activity)
		m.shownEventKeys[eventKey(activity.Event)] = struct{}{}
	}
	m.pendingFileMutation = nil
	m.selector = nil
	if err != nil {
		m.entries = append(m.entries, transcriptEntry{kind: entryError, text: errorCardText(err, false)})
		m.status = "文件修改停止失败"
	} else if strings.TrimSpace(result.Text) != "" {
		m.entries = append(m.entries, transcriptEntry{kind: entryAssistant, text: result.Text, turnID: m.pendingFileTurnID})
		m.status = "文件修改后续处理已停止"
	} else {
		m.status = "已停止当前文件修改轮次"
	}
	m.pendingFileTurnID = 0
	m.restoreInputFocus()
	m.resize()
	m.refreshTranscript(true)
	return m, nil
}

func (m model) handleReasoningKey() (tea.Model, tea.Cmd) {
	if m.selector != nil && m.selector.kind != selectorReasoning {
		return m, nil
	}
	if m.selector != nil {
		m.selector = nil
		m.restoreInputFocus()
		m.resize()
		return m, nil
	}
	m.selector = newReasoningSelector(m.session.ReasoningEffort())
	m.input.Blur()
	m.resize()
	return m, nil
}

func (m *model) applyReasoningEffort(effort modelclient.ReasoningEffort) {
	if err := m.session.SetReasoningEffort(effort); err != nil {
		m.entries = append(m.entries, transcriptEntry{kind: entryError, text: stableErrorCardText(err)})
		m.status = "推理强度更新失败"
		return
	}
	m.selector = nil
	if m.busy {
		m.status = fmt.Sprintf("下一模型请求将使用 %s；当前请求保持 %s", effort, m.activeEffort)
	} else {
		m.status = "推理强度已设为 " + string(effort)
	}
	m.restoreInputFocus()
}

func (m model) startQuestionResolution(answer agentloop.QuestionAnswer) (tea.Model, tea.Cmd) {
	question := cloneQuestion(m.pendingQuestion)
	m.pendingQuestion = nil
	m.selector = nil
	m.input.Blur()
	return m.beginTurn(turnQuestion, true, "", question, nil, "", func(turnCtx context.Context) (agentloop.Result, error) {
		return m.session.ResolveQuestion(turnCtx, answer)
	})
}

func (m model) startPreferenceResolution(resolution agentloop.PreferenceResolution) (tea.Model, tea.Cmd) {
	preference := clonePreference(m.pending)
	m.pending = nil
	m.selector = nil
	m.input.Blur()
	cancelable := resolution != agentloop.PreferenceSave && resolution != agentloop.PreferenceRetry
	return m.beginTurn(turnPreference, cancelable, "", nil, preference, resolution, func(turnCtx context.Context) (agentloop.Result, error) {
		return m.session.ResolvePreference(turnCtx, resolution)
	})
}

func (m model) beginTurn(kind turnKind, cancelable bool, input string, question *agentloop.PendingQuestion, preference *agentloop.PreferenceConfirmation, resolution agentloop.PreferenceResolution, run func(context.Context) (agentloop.Result, error)) (tea.Model, tea.Cmd) {
	m.turnSeq++
	m.activeTurnID = m.turnSeq
	m.activeKind = kind
	m.activeInput = input
	m.activeQuestion = question
	m.activePreference = preference
	m.activeResolution = resolution
	m.activeCancelable = cancelable
	m.activeEffort = m.session.ReasoningEffort()
	if m.activeEffort == "" {
		m.activeEffort = modelclient.ReasoningEffortAuto
	}
	m.activePhase = agentloop.ActivityPreparingContext
	m.activeStarted = time.Now()
	m.busy, m.stopping, m.status = true, false, phaseLabel(agentloop.ActivityPreparingContext)
	turnCtx := m.ctx
	if cancelable {
		turnCtx, m.activeTurnCancel = context.WithCancel(m.ctx)
	} else {
		m.activeTurnCancel = nil
	}
	m.resize()
	return m, startTurnCmd(turnCtx, m.activeTurnID, kind, run)
}

func (m *model) finishTurn(result agentloop.Result, err error) {
	if m.activeTurnCancel != nil {
		m.activeTurnCancel()
	}
	turnID, kind := m.activeTurnID, m.activeKind
	wasStopping := m.stopping
	activeInput := m.activeInput
	activePreference := clonePreference(m.activePreference)
	activeFileMutation := cloneFileMutation(m.activeFileMutation)
	activeResolution := m.activeResolution

	m.busy, m.stopping, m.activeCancelable = false, false, false
	m.activeTurnCancel = nil
	m.activePhase = ""
	m.activeStarted = time.Time{}
	m.activeActivityStarted = time.Time{}
	m.activeTimeoutBudget = 0

	if err != nil {
		if errors.Is(err, context.Canceled) && wasStopping {
			m.markAssistantDraft(turnID, "stopped")
			m.status = "已停止当前轮次"
			m.clearActiveTurn()
			m.restoreInputFocus()
			return
		}
		m.markAssistantDraft(turnID, "failed")
		m.handleTurnError(err)
		if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) {
			if m.pending == nil {
				m.pending = activePreference
			}
			if m.pending != nil {
				m.pending.RetryOnly = true
			}
			m.selector = newPreferenceRetrySelector()
		} else {
			if kind == turnPreference && (activeResolution == agentloop.PreferenceSave || activeResolution == agentloop.PreferenceRetry) {
				if activePreference != nil {
					activePreference.RetryOnly = false
				}
				m.restorePending(kind, nil, activePreference)
			}
			if kind == turnSend {
				m.input.SetValue(activeInput)
			}
			if kind == turnFileMutation && activeFileMutation != nil {
				m.pendingFileMutation = activeFileMutation
				m.selector = newFileMutationSelector(activeFileMutation)
			}
		}
		m.clearActiveTurn()
		m.restoreInputFocus()
		return
	}

	m.finalizeAssistantDraft(turnID, result.Text)
	m.handleTurnResult(result)
	m.clearActiveTurn()
	m.restoreInputFocus()
}

func (m *model) restorePending(kind turnKind, question *agentloop.PendingQuestion, preference *agentloop.PreferenceConfirmation) {
	switch kind {
	case turnQuestion:
		m.pendingQuestion = question
		if question != nil {
			m.selector = newQuestionSelector(question)
		}
	case turnPreference:
		m.pending = preference
		if preference != nil {
			if preference.RetryOnly {
				m.selector = newPreferenceRetrySelector()
			} else {
				m.selector = newPreferenceSelector(preference)
			}
		}
	case turnFileMutation:
		m.pendingFileMutation = m.activeFileMutation
		if m.pendingFileMutation != nil {
			m.selector = newFileMutationSelector(m.pendingFileMutation)
		}
	}
}

func (m *model) clearActiveTurn() {
	m.activeTurnID = 0
	m.activeKind = ""
	m.activeInput = ""
	m.activeQuestion = nil
	m.activePreference = nil
	m.activeFileMutation = nil
	m.activeResolution = ""
	m.activeFileResolution = ""
	m.activeFileTool = ""
	m.activeFileDetail = nil
}

func (m *model) handleActivity(turnID uint64, activity agentloop.Activity) {
	if !activity.StartedAt.IsZero() {
		m.activeActivityStarted = activity.StartedAt
	}
	m.activeTimeoutBudget = activity.TimeoutBudget
	if (activity.Phase == agentloop.ActivityWaitingModel || activity.Phase == agentloop.ActivityReceivingStream) && activity.ReasoningEffort != "" {
		m.activeEffort = activity.ReasoningEffort
	}
	if activity.Kind == agentloop.ActivityTextDelta {
		m.activeFileTool, m.activeFileDetail = "", nil
		m.entries = upsertAssistantDelta(m.entries, turnID, activity.Delta)
		m.activePhase = agentloop.ActivityReceivingStream
		m.status = phaseLabel(agentloop.ActivityReceivingStream)
		return
	}
	if activity.Kind == agentloop.ActivityTool && normalizedEventStatus(activity.Event.Status) == agentloop.EventRunning {
		m.activeFileTool = activity.Event.Tool
		m.activeFileDetail = cloneFileActivityDetail(activity.File)
	} else {
		m.activeFileTool, m.activeFileDetail = "", nil
	}
	m.activePhase = activity.Phase
	m.entries = upsertActivity(m.entries, turnID, activity)
	if activity.Kind == agentloop.ActivityTool && activity.Event.Status != agentloop.EventRunning {
		m.shownEventKeys[eventKey(activity.Event)] = struct{}{}
	}
	if label := phaseLabel(activity.Phase); label != "" {
		m.status = label
	}
	if activity.Event.Status == agentloop.EventConfirmationRequired {
		m.status = phaseLabel(agentloop.ActivityWaitingUser)
	}
	if activity.Event.Status == agentloop.EventFailed || activity.Event.Status == agentloop.EventInvalid || activity.Event.Status == agentloop.EventOutcomeUnknown {
		m.status = "Agent 遇到异常"
	}
}

func (m *model) handleTurnError(err error) {
	if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) {
		m.entries = updatePreferenceToolStatus(m.entries, agentloop.EventOutcomeUnknown, "长期偏好保存结果未知", "outcome_unknown")
		m.status = "保存结果待核对"
	} else {
		if m.activeKind == turnPreference {
			m.entries = updatePreferenceToolStatus(m.entries, agentloop.EventFailed, "长期偏好未保存", stableErrorCode(err))
		}
		m.status = "请求失败"
	}
	m.entries = append(m.entries, transcriptEntry{kind: entryError, text: errorCardText(err, m.activeKind == turnPreference)})
}

func (m *model) handleTurnResult(result agentloop.Result) {
	newEvents := make([]agentloop.Event, 0, len(result.Events))
	for _, event := range result.Events {
		key := eventKey(event)
		if _, shown := m.shownEventKeys[key]; shown {
			continue
		}
		m.shownEventKeys[key] = struct{}{}
		newEvents = append(newEvents, event)
	}
	m.entries = appendToolEvents(m.entries, m.activeTurnID, newEvents)
	m.pending, m.pendingQuestion, m.pendingFileMutation, m.selector = nil, nil, nil, nil
	if result.PendingFileMutation != nil {
		m.pendingFileMutation = cloneFileMutation(result.PendingFileMutation)
		m.pendingFileTurnID = m.activeTurnID
		m.entries = append(m.entries, transcriptEntry{kind: entryFileConfirm, text: fileMutationConfirmationText(m.pendingFileMutation), fileMutation: m.pendingFileMutation})
		m.selector = newFileMutationSelector(m.pendingFileMutation)
		m.status = "等待文件修改授权"
		return
	}
	if result.PendingQuestion != nil {
		m.pendingQuestion = cloneQuestion(result.PendingQuestion)
		m.entries = append(m.entries, transcriptEntry{kind: entryQuestion, text: questionTranscriptText(m.pendingQuestion), question: m.pendingQuestion})
		m.selector = newQuestionSelector(m.pendingQuestion)
		m.status = "等待你的选择"
		return
	}
	if result.Pending != nil {
		m.pending = clonePreference(result.Pending)
		m.entries = append(m.entries, transcriptEntry{kind: entryConfirm, text: preferenceConfirmationText(m.pending), pending: m.pending})
		if m.pending.RetryOnly {
			m.selector = newPreferenceRetrySelector()
		} else {
			m.selector = newPreferenceSelector(m.pending)
		}
		m.status = "等待长期偏好确认"
		return
	}
	m.status = "就绪"
}

func (m *model) finalizeAssistantDraft(turnID uint64, text string) {
	m.entries = finalizeAssistant(m.entries, turnID, text)
}

func (m *model) markAssistantDraft(turnID uint64, state string) {
	m.entries = markAssistant(m.entries, turnID, state)
}

func (m *model) restoreInputFocus() {
	if m.selector == nil && !m.busy {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

func (m *model) handleNavigationKey(key string) bool {
	switch key {
	case "ctrl+0", "ctrl+o":
		m.toolsExpanded = !m.toolsExpanded
		m.refreshTranscript(false)
		return true
	case "pgup":
		m.viewport.PageUp()
		m.updateFollowAfterScroll()
		return true
	case "ctrl+up":
		m.viewport.ScrollUp(3)
		m.updateFollowAfterScroll()
		return true
	case "home", "ctrl+home":
		m.viewport.GotoTop()
		m.follow = false
		return true
	case "pgdown":
		m.viewport.PageDown()
		m.updateFollowAfterScroll()
		return true
	case "ctrl+down":
		m.viewport.ScrollDown(3)
		m.updateFollowAfterScroll()
		return true
	case "end", "ctrl+g":
		m.viewport.GotoBottom()
		m.follow, m.hasNewContent = true, false
		return true
	default:
		return false
	}
}

func (m *model) updateFollowAfterScroll() {
	m.follow = m.viewport.AtBottom()
	if m.follow {
		m.hasNewContent = false
	}
}

func startTurnCmd(ctx context.Context, turnID uint64, kind turnKind, run func(context.Context) (agentloop.Result, error)) tea.Cmd {
	return func() tea.Msg {
		stream := &turnStream{
			activities: make(chan agentloop.Activity, turnStreamBuffer),
			completion: make(chan turnMsg, 1),
			wake:       make(chan struct{}, 1),
		}
		go func() {
			turnCtx := agentloop.WithActivityReporter(ctx, func(activity agentloop.Activity) {
				stream.publish(ctx, activity)
			})
			result, err := run(turnCtx)
			stream.completion <- turnMsg{turnID: turnID, kind: kind, result: result, err: err, stream: stream, done: true}
		}()
		return turnMsg{turnID: turnID, kind: kind, stream: stream}
	}
}

func waitTurnCmd(ctx context.Context, turnID uint64, kind turnKind, stream *turnStream) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		for {
			select {
			case activity := <-stream.activities:
				value := activity
				return turnMsg{turnID: turnID, kind: kind, activity: &value, stream: stream}
			default:
			}
			if activity, ok := stream.popDelta(); ok {
				return turnMsg{turnID: turnID, kind: kind, activity: &activity, stream: stream}
			}
			select {
			case message := <-stream.completion:
				select {
				case activity := <-stream.activities:
					stream.completion <- message
					value := activity
					return turnMsg{turnID: turnID, kind: kind, activity: &value, stream: stream}
				default:
				}
				if activity, ok := stream.popDelta(); ok {
					stream.completion <- message
					return turnMsg{turnID: turnID, kind: kind, activity: &activity, stream: stream}
				}
				return message
			case activity := <-stream.activities:
				value := activity
				return turnMsg{turnID: turnID, kind: kind, activity: &value, stream: stream}
			case <-stream.wake:
				continue
			case <-timer.C:
				return turnMsg{turnID: turnID, kind: kind, stream: stream, tick: true}
			case <-ctx.Done():
				return turnMsg{turnID: turnID, kind: kind, err: ctx.Err(), done: true}
			}
		}
	}
}

func waitContextCmd(ctx context.Context, stream <-chan agentloop.ContextEvent) tea.Cmd {
	if stream == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case event, ok := <-stream:
			if !ok {
				return contextMsg{closed: true}
			}
			return contextMsg{event: event, stream: stream}
		case <-ctx.Done():
			return contextMsg{closed: true}
		}
	}
}

func (m *model) resize() {
	contentWidth := max(20, m.width-horizontalPadding)
	m.contentWidth = contentWidth
	mainWidth, sidebarWidth := sidebarLayoutWidths(contentWidth)
	m.sidebarWidth = sidebarWidth
	m.sanitizeComposer()
	m.input.SetWidth(composerInnerWidth(mainWidth))
	m.input.SetHeight(m.composerInputRows(mainWidth))
	if m.selector != nil {
		m.selector.setWidth(max(16, mainWidth-4))
	}
	m.viewport.Width = mainWidth
	controlHeight := lipgloss.Height(m.renderControl(mainWidth))
	footerHeight := lipgloss.Height(m.renderFooter(mainWidth))
	m.viewport.Height = m.height - controlHeight - footerHeight - 2
	minimumViewport := 5
	if m.selector != nil {
		minimumViewport = 2
	}
	if m.viewport.Height < minimumViewport {
		m.viewport.Height = minimumViewport
	}
}

func (m *model) sanitizeComposer() {
	value := safeComposerText(m.input.Value())
	if value != m.input.Value() {
		m.input.SetValue(value)
	}
}

func (m *model) refreshTranscript(newContent bool) {
	width := max(20, m.viewport.Width)
	previousOffset := m.viewport.YOffset
	parts := make([]string, 0, len(m.entries))
	for _, entry := range m.entries {
		parts = append(parts, renderTranscriptEntry(entry, width, m.toolsExpanded))
	}
	m.viewport.SetContent(strings.Join(parts, "\n\n"))
	if m.follow {
		m.viewport.GotoBottom()
		m.follow, m.hasNewContent = true, false
		return
	}
	m.viewport.SetYOffset(previousOffset)
	if newContent {
		m.hasNewContent = true
	}
}

func (m model) View() string {
	if m.terminalTooSmall() {
		return smallTerminalView(m.width, m.height)
	}
	mainWidth := max(20, m.viewport.Width)
	contentWidth := max(mainWidth, m.contentWidth)
	main := lipgloss.JoinVertical(lipgloss.Left,
		m.viewport.View(),
		m.renderControl(mainWidth),
		m.renderFooter(mainWidth),
	)
	if m.sidebarWidth > 0 {
		main = lipgloss.JoinHorizontal(lipgloss.Top,
			main,
			strings.Repeat(" ", sidebarGap),
			m.renderSidebar(m.sidebarWidth, lipgloss.Height(main)),
		)
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(contentWidth),
		dividerStyle.Render(strings.Repeat("─", contentWidth)),
		main,
	)
	return lipgloss.NewStyle().Width(m.width).Align(lipgloss.Center).Render(body)
}

func (m model) renderStatus() string {
	if m.isSlowTurn() && m.activeCancelable {
		return m.renderStatusText(m.slowTurnDetail())
	}
	return m.renderStatusText(m.status)
}

func (m model) renderStatusText(status string) string {
	text := "● " + safeSingleLineTerminalText(status)
	switch {
	case m.busy:
		return statusBusyStyle.Render(text)
	case m.status == "请求失败" || m.status == "推理强度更新失败" || m.status == "保存结果待核对" || m.status == "Agent 遇到异常":
		return statusErrorStyle.Render(text)
	default:
		return statusReadyStyle.Render(text)
	}
}

func (m model) compactStatus() string {
	switch {
	case m.stopping:
		return "停止中"
	case m.isSlowTurn() && m.activeCancelable:
		started := m.activeActivityStarted
		if started.IsZero() {
			started = m.activeStarted
		}
		if m.activeFileTool == "search" && m.activeFileDetail != nil && m.activeFileDetail.HasScanned {
			return fmt.Sprintf("已等待 %s · 扫描 %d 文件/%d 字节", visibleDuration(time.Since(started)), m.activeFileDetail.ScannedFiles, m.activeFileDetail.ScannedBytes)
		}
		if m.activeFileTool == "read" && m.activeFileDetail != nil {
			return fmt.Sprintf("已等待 %s · 读取 %s/%d 字节", visibleDuration(time.Since(started)), safeSingleLineTerminalText(m.activeFileDetail.Path), m.activeFileDetail.Bytes)
		}
		if m.activeTimeoutBudget > 0 {
			return fmt.Sprintf("已等待 %s / 超时预算 %s", visibleDuration(time.Since(started)), visibleDuration(m.activeTimeoutBudget))
		}
		return "耗时较长"
	case m.busy:
		return "处理中"
	case m.status == "请求失败" || m.status == "推理强度更新失败":
		return "失败"
	case m.status == "保存结果待核对":
		return "待核对"
	case m.selector != nil:
		return "待选择"
	default:
		return "就绪"
	}
}

func (m model) isSlowTurn() bool {
	return m.busy && !m.activeStarted.IsZero() && time.Since(m.activeStarted) >= slowTurnThreshold
}

func (m model) slowTurnDetail() string {
	started := m.activeActivityStarted
	if started.IsZero() {
		started = m.activeStarted
	}
	elapsed := visibleDuration(time.Since(started))
	if m.activeFileTool == "search" && m.activeFileDetail != nil && m.activeFileDetail.HasScanned {
		detail := fmt.Sprintf("已等待 %s，已扫描 %d 个文件 / %d 字节", elapsed, m.activeFileDetail.ScannedFiles, m.activeFileDetail.ScannedBytes)
		if m.activeFileDetail.HasMatches {
			detail += fmt.Sprintf("，匹配 %d", m.activeFileDetail.Matches)
		}
		if m.activeTimeoutBudget > 0 {
			detail += fmt.Sprintf("，超时预算 %s", visibleDuration(m.activeTimeoutBudget))
		}
		return detail + "，可按 Esc 停止"
	}
	if m.activeFileTool == "read" && m.activeFileDetail != nil {
		detail := fmt.Sprintf("已等待 %s，正在读取 %s", elapsed, safeSingleLineTerminalText(m.activeFileDetail.Path))
		if m.activeFileDetail.HasRange {
			detail += fmt.Sprintf("，从第 %d 行开始", m.activeFileDetail.StartLine)
		}
		if m.activeFileDetail.HasBytes {
			detail += fmt.Sprintf("，已处理 %d 字节", m.activeFileDetail.Bytes)
		}
		if m.activeTimeoutBudget > 0 {
			detail += fmt.Sprintf("，超时预算 %s", visibleDuration(m.activeTimeoutBudget))
		}
		return detail + "，可按 Esc 停止"
	}
	if m.activeTimeoutBudget > 0 {
		return fmt.Sprintf("已等待 %s / 超时预算 %s，可按 Esc 停止", elapsed, visibleDuration(m.activeTimeoutBudget))
	}
	return fmt.Sprintf("已等待 %s，耗时较长，可按 Esc 停止", elapsed)
}

func visibleDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	if value < time.Second {
		return value.Round(100 * time.Millisecond).String()
	}
	return value.Round(time.Second).String()
}

func errorCardText(err error, preference bool) string {
	stage, suggestion := "Agent 请求", "可以调整问题后重试；错误卡片会保留在对话中。"
	code := stableErrorCode(err)
	if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) {
		stage = "长期偏好提交"
		suggestion = "结果未知，不能改选；请使用原操作 ID 重试核对。"
	} else if preference {
		stage = "长期偏好提交"
		suggestion = "服务端未接受本次提交；请检查错误后重试原选择。"
	} else if code == string(modelclient.ErrorCodeReasoningEffortUnsupported) {
		stage = "模型请求"
		suggestion = "当前提供商不支持所选推理强度；按 F3 选择 auto 或受支持档位后重试。"
	} else if code == string(modelclient.ErrorCodeResponseTruncated) {
		stage = "模型输出"
		suggestion = "回答达到长度上限；已保留安全的可见部分但未写入会话历史，请缩小问题或分步请求。"
	} else if code == string(modelclient.ErrorCodeContentFiltered) {
		stage = "模型内容策略"
		suggestion = "模型在完成前触发内容策略；部分输出未写入会话历史，可调整问题后重试。"
	} else if code == string(modelclient.ErrorCodeResponseProtocol) {
		stage = "模型响应协议"
		suggestion = "兼容端点返回了不完整或未知的完成状态；请检查模型服务兼容性。"
	} else if code == string(modelclient.ErrorCodeStreamProtocol) || code == string(modelclient.ErrorCodeStreamResponseTooLarge) {
		stage = "模型流式响应"
		suggestion = "已保留安全的部分输出；可稍后重试，未完成内容不会写入下一轮上下文。"
	} else {
		var contextErr *agentloop.ContextError
		if errors.As(err, &contextErr) {
			stage = "对话上下文"
			switch contextErr.Code {
			case agentloop.ContextBudgetInvalid:
				suggestion = "固定系统规则、工具定义或完整历史超过安全预算；可恢复自动整理或开启新会话。"
			case agentloop.ContextTurnTooLarge:
				suggestion = "当前这一轮本身过大；请缩短输入或减少单轮工具结果后重试。"
			case agentloop.ContextRecentTurnsTooLarge:
				suggestion = "当前轮次与最近完整轮次无法同时保留；请开启新会话，或避免连续返回超大工具结果。"
			default:
				suggestion = "上下文整理未完成；可缩短问题或开启新会话后重试。"
			}
		} else {
			message := strings.ToLower(err.Error())
			switch {
			case strings.Contains(message, "模型"), strings.Contains(message, "provider"), strings.Contains(message, "chat completion"):
				stage = "模型请求"
				suggestion = "检查模型配置或网络后重试。"
			case strings.Contains(message, "上下文"), strings.Contains(message, "context"):
				stage = "对话上下文"
				suggestion = "缩短问题或开启新会话后重试。"
			case strings.Contains(message, "工具"):
				stage = "Agent 工具"
				suggestion = "检查服务端状态后重试；已完成的工具结果仍保留。"
			}
		}
	}
	return fmt.Sprintf("阶段：%s\n代码：%s\n原因：%s\n建议：%s", stage, code, safeTerminalText(err.Error()), suggestion)
}

func stableErrorCardText(err error) string {
	return fmt.Sprintf("阶段：推理强度设置\n代码：%s\n原因：%s", stableErrorCode(err), safeTerminalText(err.Error()))
}

func stableErrorCode(err error) string {
	if code := modelclient.StableErrorCode(err); code != "" {
		return string(code)
	}
	if errors.Is(err, context.Canceled) {
		return "context_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) {
		return "outcome_unknown"
	}
	var contextErr *agentloop.ContextError
	if errors.As(err, &contextErr) {
		return string(contextErr.Code)
	}
	return "agent_request_failed"
}

func questionTranscriptText(question *agentloop.PendingQuestion) string {
	if question == nil {
		return ""
	}
	lines := []string{question.Header, question.Question}
	for index, option := range question.Options {
		lines = append(lines, fmt.Sprintf("%d. %s — %s", index+1, option.Label, option.Description))
	}
	lines = append(lines, "自定义回答：可输入最多 2000 个字符的多行文本。")
	return strings.Join(lines, "\n")
}

func fileMutationConfirmationText(pending *agentloop.PendingFileMutation) string {
	if pending == nil {
		return ""
	}
	text := fmt.Sprintf("操作：%s\n路径：%s\n预览类型：%s\n%s", pending.Operation, pending.Path, pending.PreviewKind, pending.Preview)
	if pending.Truncated {
		text += "\n预览已按安全上限截断。"
	}
	text += "\nEsc 将停止当前轮次，不等价于拒绝。"
	return text
}

func preferenceConfirmationText(pending *agentloop.PreferenceConfirmation) string {
	text := fmt.Sprintf(
		"将保存以下长期偏好：\n内容：%s\n原因：%s\n分类：%s\n敏感性：%s\n稳定性：%s",
		pending.Content,
		pending.Reason,
		preferenceValueLabel(pending.Category),
		preferenceValueLabel(pending.Sensitivity),
		preferenceValueLabel(pending.Stability),
	)
	if pending.RetryOnly {
		text += "\n提交结果未知：不能改选，只能使用原操作 ID 重试核对。"
	}
	return text
}

func preferenceValueLabel(value string) string {
	labels := map[string]string{
		"interaction_preference": "交互偏好",
		"time_constraint":        "时间约束",
		"personal_context":       "个人学习背景",
		"non_sensitive":          "非敏感",
		"sensitive":              "敏感",
		"stable":                 "长期稳定",
		"transient":              "阶段性",
	}
	if label, ok := labels[value]; ok {
		return label + " (" + value + ")"
	}
	return value
}

func cloneQuestion(value *agentloop.PendingQuestion) *agentloop.PendingQuestion {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Options = append([]agentloop.QuestionOption(nil), value.Options...)
	return &clone
}

func cloneFileActivityDetail(value *agentloop.FileActivityDetail) *agentloop.FileActivityDetail {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneFileMutation(value *agentloop.PendingFileMutation) *agentloop.PendingFileMutation {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func clonePreference(value *agentloop.PreferenceConfirmation) *agentloop.PreferenceConfirmation {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func phaseLabel(phase agentloop.ActivityPhase) string {
	switch phase {
	case agentloop.ActivityPreparingContext:
		return "准备上下文"
	case agentloop.ActivityWaitingModel:
		return "等待模型响应"
	case agentloop.ActivityReceivingStream:
		return "正在接收模型输出"
	case agentloop.ActivityValidatingResponse:
		return "校验模型响应"
	case agentloop.ActivityAssemblingTools:
		return "组装工具调用"
	case agentloop.ActivityExecutingTool:
		return "执行工具"
	case agentloop.ActivityWaitingUser:
		return "等待你的选择"
	case agentloop.ActivityContinuingAfterTool:
		return "结合工具结果继续生成"
	case agentloop.ActivityStopping:
		return "正在停止当前轮次"
	case agentloop.ActivityStopped:
		return "已停止当前轮次"
	default:
		return "Agent 正在处理"
	}
}

func safeSingleLineTerminalText(value string) string {
	return strings.Map(func(current rune) rune {
		if current == '\n' || current == '\t' {
			return ' '
		}
		if unicode.IsControl(current) || current >= '\u202a' && current <= '\u202e' || current >= '\u2066' && current <= '\u2069' {
			return '�'
		}
		return current
	}, value)
}

func safeComposerText(value string) string {
	return strings.Map(func(current rune) rune {
		if current == '\n' {
			return current
		}
		if current == '\t' {
			return ' '
		}
		if unicode.IsControl(current) || current >= '\u202a' && current <= '\u202e' || current >= '\u2066' && current <= '\u2069' {
			return '�'
		}
		return current
	}, value)
}

func safeTerminalText(value string) string {
	return strings.Map(func(current rune) rune {
		if current == '\n' || current == '\t' {
			return current
		}
		if unicode.IsControl(current) || current >= '\u202a' && current <= '\u202e' || current >= '\u2066' && current <= '\u2069' {
			return '�'
		}
		return current
	}, value)
}

func (m model) terminalTooSmall() bool {
	return m.width < minimumWidth || m.height < minimumHeight
}

func smallTerminalView(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := []string{"edu-agent", "", "终端尺寸过小", "请调整窗口后继续"}
	if len(lines) > height {
		lines = lines[:height]
	}
	for index, line := range lines {
		for lipgloss.Width(line) > width && len([]rune(line)) > 0 {
			runes := []rune(line)
			line = string(runes[:len(runes)-1])
		}
		lines[index] = line
	}
	return strings.Join(lines, "\n")
}
