package agentcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
	"github.com/mattn/go-runewidth"
	"golang.org/x/text/unicode/norm"
)

const (
	sessionTitleFailedCode    = "session_title_failed"
	sessionTitleFailedNotice  = "[session_title_failed] 自动标题生成失败；已保留旧标题或本地摘要"
	sessionTitleFailedMessage = "自动标题生成失败；已保留旧标题或本地摘要。"
)

var (
	ErrProviderConfirmationRequired  = errors.New("历史会话的模型提供商端点已变更，需要明确确认后才能发送历史文本")
	ErrWorkspaceConfirmationRequired = errors.New("历史会话属于另一个工作区，需要明确确认")
)

type Provider struct {
	Name     string
	Endpoint string
	Model    string
}

type WorkspaceBinding struct {
	Root             string
	Label            string
	PathHash         string
	RootIdentityHash string
	Available        bool
}

type Dependencies struct {
	Store            *agentsession.Store
	Model            agentloop.Model
	Server           agentloop.Server
	LoopOptions      agentloop.Options
	Provider         Provider
	WorkspaceRoot    string
	WorkspaceBinding *WorkspaceBinding
	Now              func() time.Time
}

type ResumeOptions struct {
	SessionID        string
	CurrentWorkspace string
	ConfirmWorkspace func(WorkspaceBinding) (bool, error)
	PrepareOnly      bool
}

type Status struct {
	Persistent                   bool
	DegradedReason               string
	ProviderConfirmationRequired bool
	TitleFailureCode             string
	Notices                      []string
}

type Controller struct {
	mu sync.Mutex

	loop       *agentloop.Session
	store      *agentsession.Store
	handle     *agentsession.Handle
	record     agentsession.SessionRecord
	transcript agentsession.TranscriptV1
	dirty      *agentsession.DirtyMarker

	model         agentloop.Model
	server        agentloop.Server
	provider      Provider
	limits        agentsession.Limits
	now           func() time.Time
	loopOptions   agentloop.Options
	workspaceRoot string

	persistent      bool
	degradedReason  string
	providerBlocked bool
	resumed         bool
	prepared        bool
	generation      uint64
	switching       bool
	operating       bool
	notices         []string
	pendingUser     string
	saveFailed      error
	fileJournalErr  error // Failed WAL/settlement blocks further effects and dirty consumption.
	saving          atomic.Bool
	titleCancel     context.CancelFunc
	titleJob        uint64
	contextCancel   func()
	closed          bool
	shutdownErr     error
}

func BindWorkspace(path string) (WorkspaceBinding, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return WorkspaceBinding{}, errors.New("workspace root is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return WorkspaceBinding{}, err
	}
	absolute = filepath.Clean(absolute)
	binding := WorkspaceBinding{Root: absolute, Label: filepath.Base(absolute)}
	pathDigest := sha256.Sum256([]byte(filepath.Clean(absolute)))
	binding.PathHash = "sha256:" + hex.EncodeToString(pathDigest[:])
	root, err := securefile.OpenRoot(absolute)
	if err != nil {
		return binding, err
	}
	defer root.Close()
	identity, err := root.Identity()
	if err != nil {
		return binding, err
	}
	identityDigest := sha256.Sum256([]byte(identity))
	binding.RootIdentityHash = "sha256:" + hex.EncodeToString(identityDigest[:])
	binding.Available = true
	return binding, nil
}

func WorkspaceScopeID(binding WorkspaceBinding) string {
	if binding.RootIdentityHash != "" {
		return binding.RootIdentityHash
	}
	return binding.PathHash
}

func ProviderEndpointIdentity(value Provider) string {
	return strings.ToLower(strings.TrimSpace(value.Name)) + "\x00" + normalizedEndpoint(value.Endpoint)
}

func Start(ctx context.Context, dependencies Dependencies, noSave bool) (*Controller, error) {
	controller, err := newController(dependencies)
	if err != nil {
		if dependencies.Store != nil {
			_ = dependencies.Store.Close()
		}
		return nil, err
	}
	if noSave || dependencies.Store == nil {
		controller.persistent = false
		if !noSave {
			controller.degradedReason = "[session_store_unavailable] 加密会话存储不可用；当前会话不会保存"
		}
		return controller, nil
	}
	checkpoint, err := controller.loop.ExportCheckpoint()
	if err != nil {
		controller.Close()
		return nil, err
	}
	checkpointData, err := agentloop.EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		controller.Close()
		return nil, err
	}
	transcriptData, err := agentsession.EncodeTranscript(controller.transcript, controller.limits)
	if err != nil {
		controller.Close()
		return nil, err
	}
	binding := WorkspaceBinding{}
	if dependencies.WorkspaceBinding != nil {
		binding = *dependencies.WorkspaceBinding
	} else {
		binding, _ = BindWorkspace(dependencies.WorkspaceRoot)
	}
	learner, memory, verified := privacyStamp(ctx, dependencies.Server)
	handle, record, err := dependencies.Store.Create(ctx, agentsession.CreateInput{
		Title: "新会话", WorkspaceID: WorkspaceScopeID(binding), WorkspaceRoot: binding.Root, WorkspaceLabel: binding.Label,
		WorkspacePathHash: binding.PathHash, WorkspaceRootIdentityHash: binding.RootIdentityHash,
		ProviderName: dependencies.Provider.Name, ProviderEndpoint: normalizedEndpoint(dependencies.Provider.Endpoint), ProviderModel: dependencies.Provider.Model,
		PrivacyLearnerGeneration: learner, PrivacyMemoryGeneration: memory, PrivacyVerified: verified,
		Checkpoint: checkpointData, Transcript: transcriptData,
	})
	if err != nil {
		_ = dependencies.Store.Close()
		controller.store = nil
		controller.persistent = false
		switch {
		case errors.Is(err, agentsession.ErrStoreFull):
			controller.degradedReason = "[session_store_full] 加密会话存储已达到硬上限；当前会话不会保存，也不会自动删除旧Session"
		case errors.Is(err, agentsession.ErrCheckpointSaveFailed), errors.Is(err, agentsession.ErrOutcomeUnknown):
			controller.degradedReason = "[session_save_failed] 新Session元数据未能安全发布；已关闭残留存储并改为不保存模式"
		default:
			controller.degradedReason = "[session_store_unavailable] 加密会话存储无法安全创建Session；当前会话不会保存"
		}
		return controller, nil
	}
	controller.handle = handle
	controller.record = record
	controller.persistent = true
	controller.startContextWorker()
	return controller, nil
}

func Resume(ctx context.Context, dependencies Dependencies, options ResumeOptions) (*Controller, error) {
	if dependencies.Store == nil {
		return nil, errors.New("加密会话存储不可用")
	}
	handle, loaded, err := dependencies.Store.OpenSession(ctx, options.SessionID)
	if err != nil {
		_ = dependencies.Store.Close()
		return nil, sessionStoreLoadError(err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = handle.Close()
			_ = dependencies.Store.Close()
		}
	}()

	storedWorkspace := WorkspaceBinding{
		Root: loaded.Record.WorkspaceRoot, Label: loaded.Record.WorkspaceLabel, PathHash: loaded.Record.WorkspacePathHash,
		RootIdentityHash: loaded.Record.WorkspaceRootIdentityHash,
	}
	loopOptions := dependencies.LoopOptions
	loopOptions.Workspace = nil
	loopOptions.WorkspaceStatus = workspace.Status{Code: workspace.CodeWorkspaceUnavailable}
	if storedWorkspace.Root != "" {
		current, _ := BindWorkspace(options.CurrentWorkspace)
		currentScope := WorkspaceScopeID(current)
		storedScope := loaded.Record.WorkspaceID
		if currentScope != "" && storedScope != "" && currentScope != storedScope {
			if options.ConfirmWorkspace == nil {
				return nil, ErrWorkspaceConfirmationRequired
			}
			confirmed, confirmErr := options.ConfirmWorkspace(storedWorkspace)
			if confirmErr != nil {
				return nil, confirmErr
			}
			if !confirmed {
				return nil, ErrWorkspaceConfirmationRequired
			}
		}
		actual, bindErr := BindWorkspace(storedWorkspace.Root)
		if bindErr == nil && (storedWorkspace.RootIdentityHash == "" || actual.RootIdentityHash == storedWorkspace.RootIdentityHash) {
			opened, openErr := workspace.Open(storedWorkspace.Root)
			if openErr == nil {
				loopOptions.Workspace = opened
				loopOptions.WorkspaceStatus = opened.Status()
				storedWorkspace.Available = true
			}
		}
	}
	dependencies.LoopOptions = loopOptions
	controller, err := newController(dependencies)
	if err != nil {
		return nil, err
	}
	controller.handle = handle
	controller.record = loaded.Record
	controller.resumed = true
	controller.prepared = options.PrepareOnly
	controller.record.LastOpenedAt = controller.now().UTC()
	controller.record.Lifecycle = "active"
	controller.persistent = true
	transcript, err := agentsession.DecodeTranscript(loaded.Record.Transcript, controller.limits)
	if err != nil {
		controller.abort()
		return nil, err
	}
	controller.transcript = transcript
	if hasTitleFailureStatus(nil, transcript) {
		controller.addTitleFailureStatusLocked()
	}

	learner, memory, verified := privacyStamp(ctx, dependencies.Server)
	checkpointData := loaded.Record.Checkpoint
	privacyNeedsSave := !controller.record.LastOpenedAt.Equal(loaded.Record.LastOpenedAt) || loaded.Record.Lifecycle != "active"
	privacyQuarantine := false
	privacyRestored := false
	samePrivacyGeneration := learner == loaded.Record.PrivacyLearnerGeneration && memory == loaded.Record.PrivacyMemoryGeneration
	switch {
	case verified && !loaded.Record.PrivacyVerified && len(loaded.Record.QuarantinedCheckpoint) > 0 && samePrivacyGeneration:
		checkpointData = loaded.Record.QuarantinedCheckpoint
		controller.record.Checkpoint = append([]byte(nil), checkpointData...)
		controller.record.QuarantinedCheckpoint = nil
		controller.record.PrivacyVerified = true
		privacyNeedsSave = true
		privacyRestored = true
	case verified && samePrivacyGeneration:
		controller.record.PrivacyVerified = true
		privacyNeedsSave = privacyNeedsSave || !loaded.Record.PrivacyVerified
	case verified:
		controller.record.PrivacyVerified = true
		controller.record.PrivacyLearnerGeneration = learner
		controller.record.PrivacyMemoryGeneration = memory
		controller.record.QuarantinedCheckpoint = nil
		privacyNeedsSave = true
		privacyQuarantine = true
	case !verified:
		if len(controller.record.QuarantinedCheckpoint) == 0 {
			controller.record.QuarantinedCheckpoint = append([]byte(nil), loaded.Record.Checkpoint...)
		}
		controller.record.PrivacyVerified = false
		privacyNeedsSave = true
		privacyQuarantine = true
	}

	checkpoint, err := agentloop.DecodeSessionCheckpoint(checkpointData)
	if err != nil {
		controller.abort()
		return nil, checkpointLoadError(err)
	}
	if err := controller.loop.RestoreCheckpoint(checkpoint); err != nil {
		controller.abort()
		return nil, checkpointLoadError(err)
	}
	if loaded.Interrupted != nil {
		for _, entry := range dirtyFileEntries(*loaded.Interrupted) {
			if receipt, ok := journalReceipt(entry); ok {
				if err := controller.loop.InvalidateFileEffect(receipt.Effect); err != nil {
					controller.abort()
					return nil, checkpointLoadError(err)
				}
			}
		}
	}
	if privacyQuarantine {
		controller.loop.QuarantineHistoricalServerEvidence()
		controller.appendNoticeLocked("privacy_revalidation", agentsession.NoticeOutcomeRequired, "历史服务端证据已隔离，等待隐私代际重新验证。")
		controller.appendStatusNoticeLocked("[session_privacy_revalidation_pending] 隐私代际无法复用；历史服务端正文已从模型上下文隔离")
	} else if privacyRestored {
		controller.appendNoticeLocked("privacy_revalidated", agentsession.NoticeOutcomeInformational, "历史服务端证据的隐私代际已重新验证。")
		controller.appendStatusNoticeLocked("历史服务端证据已通过隐私代际重新验证")
	}
	if loaded.Interrupted != nil {
		controller.dirty = loaded.Interrupted
		if err := controller.recordRecoveryUnknownLocked(*loaded.Interrupted); err != nil {
			controller.abort()
			return nil, err
		}
		controller.appendNoticeLocked("session_interrupted", agentsession.NoticeOutcomeInterrupted, "上次运行在稳定检查点之后中断；未重放任何模型或工具操作。")
		if !options.PrepareOnly {
			if err := controller.saveCheckpointLocked(ctx, true); err != nil {
				controller.abort()
				return nil, err
			}
			privacyNeedsSave = false
		}
		controller.appendStatusNoticeLocked("[session_interrupted] 已从最后一个稳定检查点恢复；上次未完成操作没有重放")
		for _, entry := range dirtyFileEntries(*loaded.Interrupted) {
			if receipt, ok := journalReceipt(entry); ok {
				controller.appendStatusNoticeLocked(fileJournalRecoveryLabel(receipt))
			}
		}
	}
	if privacyNeedsSave && !options.PrepareOnly {
		if err := controller.saveCheckpointLocked(ctx, false); err != nil {
			controller.abort()
			return nil, err
		}
	}
	if ProviderEndpointIdentity(Provider{Name: loaded.Record.ProviderName, Endpoint: loaded.Record.ProviderEndpoint}) != ProviderEndpointIdentity(dependencies.Provider) {
		controller.providerBlocked = true
		controller.appendStatusNoticeLocked("[session_provider_confirmation_required] 模型提供商端点已变更；确认前不会发送历史文本或生成标题")
	} else if strings.TrimSpace(loaded.Record.ProviderModel) != strings.TrimSpace(dependencies.Provider.Model) {
		controller.appendStatusNoticeLocked("当前模型名称与历史会话不同；恢复将使用当前模型")
	}
	if storedWorkspace.Root != "" && !storedWorkspace.Available {
		controller.appendStatusNoticeLocked("[session_workspace_unavailable] 历史工作区不可用或身份不匹配；本次恢复已禁用文件工具")
	}
	if hasUnknownPreferenceReceipt(controller.record.PreferenceReceipts) {
		controller.appendStatusNoticeLocked("[session_preference_outcome_unknown] 存在长期偏好未知结果；只能复用原操作ID执行retry-only核对")
	}
	if hasUnknownFileReceipt(controller.record.FileReceipts) {
		controller.appendStatusNoticeLocked("[session_file_outcome_unknown] 存在文件发布未知结果；修改前必须重新读取、预览并授权")
	}
	closeOnError = false
	if !options.PrepareOnly {
		controller.startContextWorker()
	}
	return controller, nil
}

func baseLoopOptions(options agentloop.Options) agentloop.Options {
	options.Workspace = nil
	options.Durability = nil
	return options
}

func (c *Controller) startContextWorker() {
	c.mu.Lock()
	if c.closed || !c.persistent || c.loop == nil || c.contextCancel != nil {
		c.mu.Unlock()
		return
	}
	stream, cancel := c.loop.SubscribeContextUpdates()
	generation := c.generation
	c.contextCancel = cancel
	c.mu.Unlock()
	go c.runContextWorker(generation, stream)
}

func (c *Controller) runContextWorker(generation uint64, stream <-chan agentloop.ContextEvent) {
	for event := range stream {
		if event.Kind != agentloop.ContextEventCompacted && event.Kind != agentloop.ContextEventDegraded && event.Kind != agentloop.ContextEventSourceUnavailable {
			continue
		}
		c.mu.Lock()
		if c.closed || c.generation != generation {
			c.mu.Unlock()
			return
		}
		c.appendDurableContextLocked(event)
		state := c.loop.SwitchState()
		if c.persistent && c.dirty == nil && !c.switching && !state.ActiveTurn && !state.Resolving && !state.PendingQuestion && !state.PendingPreference && !state.PendingFileMutation && !state.Closed {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.saveCheckpointLocked(ctx, false)
			cancel()
			if err != nil {
				c.saveFailed = checkpointPersistenceError(err)
			}
		}
		c.mu.Unlock()
	}
}

func (c *Controller) appendDurableContextLocked(event agentloop.ContextEvent) {
	typeName := agentsession.ContextEventCompacted
	message := fmt.Sprintf("较早展示已整理；保留最近 %d 轮。", event.RecentTurns)
	switch event.Kind {
	case agentloop.ContextEventDegraded:
		typeName = agentsession.ContextEventDegraded
		message = "上下文整理已降级；较早展示可能已收起。"
		if event.Code == "context_history_projected" {
			message = "[context_history_projected] 较早助手正文已使用带来源的有界节选；用户陈述和工具事实保持完整。"
		}
	case agentloop.ContextEventSourceUnavailable:
		typeName = agentsession.ContextEventSourceUnavailable
		message = "历史证据来源不可用。"
	}
	turn := c.record.CommittedUserTurns
	if turn == 0 {
		turn = 1
	}
	c.transcript.Entries = append(c.transcript.Entries, agentsession.TranscriptEntryV1{
		Sequence: c.nextSequenceLocked(), PresentationTurn: turn, Kind: agentsession.TranscriptKindContext, CreatedAt: c.now().UTC(),
		Context: &agentsession.StableContextEventV1{Type: typeName, Message: message},
	})
}

func newController(dependencies Dependencies) (*Controller, error) {
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	limits := agentsession.DefaultLimits()
	if dependencies.Store != nil {
		limits = dependencies.Store.Limits()
	}
	controller := &Controller{
		store: dependencies.Store, model: dependencies.Model, server: dependencies.Server, provider: dependencies.Provider, limits: limits, now: now, generation: 1,
		loopOptions: baseLoopOptions(dependencies.LoopOptions), workspaceRoot: dependencies.WorkspaceRoot,
		transcript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{}},
	}
	dependencies.LoopOptions.Durability = controller
	loop, err := agentloop.New(dependencies.Model, dependencies.Server, dependencies.LoopOptions)
	if err != nil {
		return nil, err
	}
	controller.loop = loop
	return controller, nil
}

func privacyStamp(ctx context.Context, server agentloop.Server) (int64, int64, bool) {
	if server == nil {
		return 0, 0, false
	}
	page, err := server.ExportMemory(ctx, "", 1)
	if err != nil {
		return 0, 0, false
	}
	return page.ReadGeneration.LearnerGeneration, page.ReadGeneration.MemoryGeneration, true
}

func normalizedEndpoint(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(strings.TrimSpace(value), "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if parsed.Scheme == "https" && port == "443" || parsed.Scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	titleFailureCode := ""
	if hasTitleFailureStatus(c.notices, c.transcript) {
		titleFailureCode = sessionTitleFailedCode
	}
	return Status{Persistent: c.persistent, DegradedReason: c.degradedReason, ProviderConfirmationRequired: c.providerBlocked, TitleFailureCode: titleFailureCode, Notices: append([]string(nil), c.notices...)}
}

func (c *Controller) addTitleFailureStatusLocked() {
	for _, notice := range c.notices {
		if strings.HasPrefix(strings.TrimSpace(notice), "["+sessionTitleFailedCode+"]") {
			return
		}
	}
	c.appendStatusNoticeLocked(sessionTitleFailedNotice)
}

func (c *Controller) appendStatusNoticeLocked(notice string) {
	c.notices = append(c.notices, notice)
	if len(c.notices) > c.limits.NoticeCount {
		c.notices = c.notices[len(c.notices)-c.limits.NoticeCount:]
	}
}

func hasTitleFailureStatus(notices []string, transcript agentsession.TranscriptV1) bool {
	for _, notice := range notices {
		if strings.HasPrefix(strings.TrimSpace(notice), "["+sessionTitleFailedCode+"]") {
			return true
		}
	}
	for _, entry := range transcript.Entries {
		if entry.Notice != nil && entry.Notice.Code == sessionTitleFailedCode {
			return true
		}
	}
	return false
}

func (c *Controller) publishTitleFailureLocked() {
	c.addTitleFailureStatusLocked()
	for _, entry := range c.transcript.Entries {
		if entry.Notice != nil && entry.Notice.Code == sessionTitleFailedCode {
			return
		}
	}
	c.appendNoticeLocked(sessionTitleFailedCode, agentsession.NoticeOutcomeFailed, sessionTitleFailedMessage)
}

func (c *Controller) SessionPersistenceStatus() (state, detail string) {
	if c.saving.Load() {
		return "saving", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.persistent {
		return "unsaved", c.degradedReason
	}
	if c.saveFailed != nil {
		return "failed", ""
	}
	return "saved", ""
}

func (c *Controller) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.record.SessionID
}

func (c *Controller) ConfirmProvider(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.providerBlocked {
		return nil
	}
	c.record.ProviderName = c.provider.Name
	c.record.ProviderEndpoint = normalizedEndpoint(c.provider.Endpoint)
	c.record.ProviderModel = c.provider.Model
	if c.persistent && !c.prepared {
		if err := c.saveCheckpointLocked(ctx, false); err != nil {
			return err
		}
	}
	c.providerBlocked = false
	return nil
}

func (c *Controller) BeginTurn(ctx context.Context, intent agentloop.DirtyIntent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.persistent {
		return nil
	}
	if c.saveFailed != nil {
		return c.saveFailed
	}
	if c.providerBlocked {
		return ErrProviderConfirmationRequired
	}
	if c.dirty != nil {
		return agentsession.ErrCheckpointConflict
	}
	marker, err := c.handle.MarkDirty(ctx, c.record.RecordRevision, intent.TurnSequence, intent.OperationClass, intent.MayHaveSideEffect)
	if err != nil {
		return err
	}
	c.dirty = &marker
	return nil
}

func (c *Controller) BeforePreferenceWrite(ctx context.Context, receipt agentloop.PreferenceWriteAhead) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.persistent {
		return nil
	}
	if err := c.ensureDirtyLocked(); err != nil {
		return err
	}
	if c.fileCallExistsLocked(receipt.ToolCallID) {
		return c.failFileJournalLocked(agentsession.ErrCheckpointConflict)
	}
	candidate := *c.dirty
	candidate.MayHaveSideEffect = true
	candidate.Preference = &agentsession.PreferenceWriteAhead{
		ToolCallID: receipt.ToolCallID, CreateOperationID: receipt.CreateOperationID, AdmitOperationID: receipt.AdmitOperationID,
		RejectOperationID: receipt.RejectOperationID,
		Payload: agentsession.PreferencePayload{
			Content: receipt.Payload.Content, Reason: receipt.Payload.Reason, Category: receipt.Payload.Category,
			Sensitivity: receipt.Payload.Sensitivity, Stability: receipt.Payload.Stability, ValidUntil: receipt.Payload.ValidUntil,
		},
		CandidateID: receipt.CandidateID, CandidateRevision: receipt.CandidateRevision, Stage: string(receipt.Stage), StableCode: receipt.StableCode,
		Outcome: string(receipt.Outcome),
	}
	updated, err := c.handle.UpdateDirty(ctx, candidate)
	if err != nil {
		if len(dirtyFileEntries(*c.dirty)) != 0 {
			return c.failFileJournalLocked(err)
		}
		return err
	}
	c.dirty = &updated
	return nil
}

func (c *Controller) BeforeFilePublication(ctx context.Context, receipt agentloop.FileWriteAhead) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.persistent {
		return nil
	}
	if err := c.ensureDirtyLocked(); err != nil {
		return err
	}
	if c.fileCallExistsLocked(receipt.ToolCallID) || c.dirty.Preference != nil && c.dirty.Preference.ToolCallID == receipt.ToolCallID {
		return c.failFileJournalLocked(agentsession.ErrCheckpointConflict)
	}
	for _, previous := range c.record.PreferenceReceipts {
		if previous.ToolCallID == receipt.ToolCallID {
			return c.failFileJournalLocked(agentsession.ErrCheckpointConflict)
		}
	}
	if len(dirtyFileEntries(*c.dirty)) >= c.limits.ReceiptCount {
		return c.failFileJournalLocked(agentsession.ErrStoreFull)
	}
	candidate := *c.dirty
	candidate.MayHaveSideEffect = true
	candidate.FileJournal = dirtyFileEntries(candidate)
	candidate.File = &agentsession.FileWriteAhead{
		ToolCallID: receipt.ToolCallID, Effect: receipt.Effect,
		InvalidateObserved: true, StableCode: agentsession.FilePublicationUnknownCode, PublicationOutcome: agentsession.NoticeOutcomeUnknown,
	}
	return c.updateFileJournalLocked(ctx, candidate)
}

func (c *Controller) ensureDirtyLocked() error {
	if c.fileJournalErr != nil {
		return c.fileJournalErr
	}
	if c.dirty == nil {
		return agentsession.ErrCheckpointConflict
	}
	return nil
}

func (c *Controller) beginRuntimeOperation() (*agentloop.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, agentloop.ErrSessionClosed
	}
	if c.switching || c.operating || c.loop == nil {
		return nil, ErrSwitchUnavailable
	}
	c.operating = true
	return c.loop, nil
}

func (c *Controller) endRuntimeOperation() {
	c.mu.Lock()
	c.operating = false
	c.mu.Unlock()
}

func (c *Controller) Send(ctx context.Context, input string) (agentloop.Result, error) {
	c.mu.Lock()
	if c.providerBlocked {
		c.mu.Unlock()
		return agentloop.Result{}, ErrProviderConfirmationRequired
	}
	if c.closed {
		c.mu.Unlock()
		return agentloop.Result{}, agentloop.ErrSessionClosed
	}
	if c.switching || c.operating || c.loop == nil {
		c.mu.Unlock()
		return agentloop.Result{}, ErrSwitchUnavailable
	}
	c.operating = true
	loop := c.loop
	c.pendingUser = strings.TrimSpace(input)
	c.mu.Unlock()
	result, err := loop.Send(ctx, input)
	return c.finishOperation(ctx, result, err)
}

func (c *Controller) ResolvePreference(ctx context.Context, resolution agentloop.PreferenceResolution) (agentloop.Result, error) {
	loop, startErr := c.beginRuntimeOperation()
	if startErr != nil {
		return agentloop.Result{}, startErr
	}
	result, err := loop.ResolvePreference(ctx, resolution)
	var recoverySaveErr error
	if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) {
		c.mu.Lock()
		if c.dirty != nil {
			recoverySaveErr = c.recordRecoveryUnknownLocked(*c.dirty)
			if recoverySaveErr != nil {
				c.mu.Unlock()
				c.endRuntimeOperation()
				return result, errors.Join(err, recoverySaveErr)
			}
			c.appendPendingUserLocked()
			c.appendTypedNoticeLocked(agentsession.TranscriptKindPreferenceNotice, c.record.CommittedUserTurns+1, "preference_outcome_unknown", agentsession.NoticeOutcomeUnknown, "长期偏好写入结果未知；仅可使用原操作ID重试核对。")
			c.record.CommittedUserTurns++
			saveErr := c.saveExistingCheckpointLocked(ctx)
			if saveErr != nil {
				c.saveFailed = checkpointPersistenceError(saveErr)
				recoverySaveErr = c.saveFailed
				_ = c.degradeNewSessionAfterSaveFailureLocked(c.saveFailed)
			}
		}
		c.mu.Unlock()
	}
	if recoverySaveErr != nil {
		c.endRuntimeOperation()
		return result, errors.Join(err, recoverySaveErr)
	}
	return c.finishOperation(ctx, result, err)
}

func (c *Controller) ResolveQuestion(ctx context.Context, answer agentloop.QuestionAnswer) (agentloop.Result, error) {
	loop, err := c.beginRuntimeOperation()
	if err != nil {
		return agentloop.Result{}, err
	}
	result, err := loop.ResolveQuestion(ctx, answer)
	return c.finishOperation(ctx, result, err)
}

func (c *Controller) ResolveFileMutation(ctx context.Context, callID string, resolution agentloop.FileMutationResolution) (agentloop.Result, error) {
	loop, err := c.beginRuntimeOperation()
	if err != nil {
		return agentloop.Result{}, err
	}
	result, err := loop.ResolveFileMutation(ctx, callID, resolution)
	return c.finishOperation(ctx, result, err)
}

func (c *Controller) CancelPendingFileMutation(callID string) (agentloop.Result, error) {
	loop, err := c.beginRuntimeOperation()
	if err != nil {
		return agentloop.Result{}, err
	}
	result, err := loop.CancelPendingFileMutation(callID)
	return c.finishOperation(context.Background(), result, err)
}

func (c *Controller) finishOperation(ctx context.Context, result agentloop.Result, operationErr error) (agentloop.Result, error) {
	if result.Pending != nil || result.PendingQuestion != nil || result.PendingFileMutation != nil {
		c.endRuntimeOperation()
		return result, operationErr
	}
	c.mu.Lock()
	c.operating = false
	defer c.mu.Unlock()
	if !c.persistent || c.dirty == nil {
		return result, operationErr
	}
	if c.fileJournalErr != nil {
		return result, errors.Join(operationErr, c.fileJournalErr)
	}
	if errors.Is(operationErr, agentloop.ErrPreferenceOutcomeUnknown) && c.dirty == nil {
		return result, operationErr
	}
	if c.dirty.Preference != nil {
		outcome := c.dirty.Preference.Outcome
		if outcome == "" && operationErr == nil {
			outcome = agentsession.NoticeOutcomeCompleted
			if c.dirty.Preference.StableCode != "preference_saved" {
				outcome = agentsession.NoticeOutcomeRejected
			}
		}
		if outcome == agentsession.NoticeOutcomeCompleted || outcome == agentsession.NoticeOutcomeRejected {
			c.record.PreferenceReceipts = appendBoundedPreference(c.record.PreferenceReceipts, preferenceReceiptFromWriteAhead(*c.dirty.Preference, outcome), c.limits.ReceiptCount)
		}
	}
	c.appendOperationTranscriptLocked(result, operationErr)
	checkpoint, checkpointErr := c.loop.ExportCheckpoint()
	if checkpointErr != nil {
		persistenceErr := checkpointPersistenceError(checkpointErr)
		c.saveFailed = persistenceErr
		return result, persistenceErr
	}
	encoded, encodeErr := agentloop.EncodeSessionCheckpoint(checkpoint)
	if encodeErr != nil {
		persistenceErr := checkpointPersistenceError(encodeErr)
		c.saveFailed = persistenceErr
		return result, persistenceErr
	}
	c.record.Checkpoint = encoded
	if saveErr := c.saveRecordLocked(ctx, true); saveErr != nil {
		persistenceErr := checkpointPersistenceError(saveErr)
		c.saveFailed = persistenceErr
		_ = c.degradeNewSessionAfterSaveFailureLocked(persistenceErr)
		return result, persistenceErr
	}
	if operationErr == nil && result.Text != "" {
		c.scheduleTitleLocked(checkpoint)
	}
	return result, operationErr
}

func (c *Controller) degradeNewSessionAfterSaveFailureLocked(cause error) error {
	if c.resumed || !c.persistent || cause == nil {
		return nil
	}
	// A failed save must not turn an effect-bearing persistent session into a
	// non-persistent writer. Keep the dirty evidence and the save-failure gate.
	if c.dirty != nil && len(dirtyFileEntries(*c.dirty)) != 0 {
		return nil
	}
	var closeErr error
	if c.handle != nil {
		closeErr = errors.Join(closeErr, c.handle.Close())
	}
	if c.store != nil {
		closeErr = errors.Join(closeErr, c.store.Close())
	}
	if closeErr != nil {
		return closeErr
	}
	c.persistent = false
	c.handle = nil
	c.store = nil
	c.dirty = nil
	if errors.Is(cause, agentsession.ErrStoreFull) {
		c.degradedReason = "session_store_full: Session历史空间不足，当前Session已停止保存且不可恢复"
	} else {
		c.degradedReason = "session_save_failed: Session保存失败，当前Session已停止保存且不可恢复"
	}
	return nil
}

func sessionStoreLoadError(err error) error {
	switch {
	case errors.Is(err, agentsession.ErrVersionUnsupported):
		return agentsession.ErrVersionUnsupported
	case errors.Is(err, agentsession.ErrCorrupt):
		return agentsession.ErrCorrupt
	default:
		return err
	}
}

func checkpointLoadError(err error) error {
	if errors.Is(err, agentloop.ErrCheckpointVersionUnsupported) {
		return agentsession.ErrVersionUnsupported
	}
	return agentsession.ErrCorrupt
}

func checkpointPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, agentsession.ErrCheckpointSaveFailed) || errors.Is(err, agentsession.ErrOutcomeUnknown) ||
		errors.Is(err, agentsession.ErrCheckpointConflict) || errors.Is(err, agentsession.ErrStoreFull) ||
		errors.Is(err, agentsession.ErrPrivacyInvalidated) || errors.Is(err, agentsession.ErrKeyUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %v", agentsession.ErrCheckpointSaveFailed, err)
}

func (c *Controller) saveCheckpointLocked(ctx context.Context, consumeDirty bool) error {
	checkpoint, err := c.loop.ExportCheckpoint()
	if err != nil {
		return err
	}
	encoded, err := agentloop.EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	c.record.Checkpoint = encoded
	return c.saveRecordLocked(ctx, consumeDirty)
}

func (c *Controller) saveExistingCheckpointLocked(ctx context.Context) error {
	return c.saveRecordLocked(ctx, true)
}

func (c *Controller) saveRecordLocked(ctx context.Context, consumeDirty bool) error {
	if consumeDirty && c.dirty != nil {
		if c.fileJournalErr != nil {
			return c.fileJournalErr
		}
		if err := c.mergeFileJournalLocked(*c.dirty); err != nil {
			return err
		}
	}
	c.saving.Store(true)
	defer c.saving.Store(false)
	transcript, err := agentsession.EncodeTranscript(c.transcript, c.limits)
	if err != nil {
		return err
	}
	candidate := c.record
	candidate.Transcript = transcript
	candidate.LastConsumedDirtyID = ""
	if consumeDirty && c.dirty != nil {
		candidate.LastConsumedDirtyID = c.dirty.DirtyID
	}
	saved, err := c.handle.Save(ctx, c.record.RecordRevision, candidate)
	if err != nil {
		return err
	}
	c.record = saved
	c.saveFailed = nil
	if consumeDirty {
		c.dirty = nil
	}
	return nil
}

func (c *Controller) appendOperationTranscriptLocked(result agentloop.Result, operationErr error) {
	turn := c.record.CommittedUserTurns + 1
	if c.record.CommittedUserTurns == 0 && c.record.TitleSource != "manual" && c.pendingUser != "" {
		fallback := fallbackTitle(c.pendingUser)
		if fallback != c.record.Title {
			c.record.Title = fallback
			c.record.TitleRevision++
		}
	}
	c.appendPendingUserLocked()
	if tools := durableToolActivities(result.Events); len(tools) > 0 {
		c.transcript.Entries = append(c.transcript.Entries, agentsession.TranscriptEntryV1{
			Sequence: c.nextSequenceLocked(), PresentationTurn: turn, Kind: agentsession.TranscriptKindTool, CreatedAt: c.now().UTC(), Tools: tools,
		})
	}
	state := agentsession.AssistantStateFinal
	modelCommitted := true
	text := result.Text
	if operationErr != nil {
		modelCommitted = false
		if errors.Is(operationErr, context.Canceled) || errors.Is(operationErr, context.DeadlineExceeded) {
			state = agentsession.AssistantStateStopped
			text = "当前回答已停止。"
		} else {
			state = agentsession.AssistantStateFailed
			text = "当前回答失败；错误详情未写入历史会话。"
		}
	}
	if text != "" {
		c.transcript.Entries = append(c.transcript.Entries, agentsession.TranscriptEntryV1{
			Sequence: c.nextSequenceLocked(), PresentationTurn: turn, Kind: agentsession.TranscriptKindAssistant, CreatedAt: c.now().UTC(),
			Text: text, AssistantState: state, ModelCommitted: modelCommitted,
		})
	}
	if operationErr != nil {
		code, retryable := durableOperationError(operationErr)
		c.transcript.Entries = append(c.transcript.Entries, agentsession.TranscriptEntryV1{
			Sequence: c.nextSequenceLocked(), PresentationTurn: turn, Kind: agentsession.TranscriptKindError, CreatedAt: c.now().UTC(),
			Error: &agentsession.StableErrorV1{Code: code, Retryable: retryable}, PresentationOnly: true,
		})
	}
	if c.dirty != nil && c.dirty.Preference != nil && operationErr == nil {
		c.appendTypedNoticeLocked(agentsession.TranscriptKindPreferenceNotice, turn, "preference_saved", agentsession.NoticeOutcomeCompleted, "长期偏好写入已完成。")
	}
	if c.dirty != nil {
		for _, entry := range dirtyFileEntries(*c.dirty) {
			if receipt, ok := journalReceipt(entry); ok {
				message := "文件发布已完成。"
				if receipt.Outcome == agentsession.NoticeOutcomeUnknown {
					message = "文件发布结果未知；后续修改前必须重新读取和授权。"
				}
				c.appendTypedNoticeLocked(agentsession.TranscriptKindFileNotice, turn, "file_publication", receipt.Outcome, message)
			}
		}
	}
	c.record.CommittedUserTurns++
}

func (c *Controller) appendPendingUserLocked() {
	if c.pendingUser == "" {
		return
	}
	userText := c.pendingUser
	turn := c.record.CommittedUserTurns + 1
	c.transcript.Entries = append(c.transcript.Entries, agentsession.TranscriptEntryV1{
		Sequence: c.nextSequenceLocked(), PresentationTurn: turn, Kind: agentsession.TranscriptKindUser,
		CreatedAt: c.now().UTC(), Text: userText,
	})
	summary := boundedSearchSummary(userText, c.limits)
	if c.record.FirstUserSummary == "" {
		c.record.FirstUserSummary = summary
	}
	c.record.RecentUserSummary = summary
	c.pendingUser = ""
}

func durableOperationError(err error) (string, bool) {
	if errors.Is(err, context.Canceled) {
		return "context_cancelled", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded", true
	}
	if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) {
		return "preference_outcome_unknown", true
	}
	if code := modelclient.StableErrorCode(err); code != "" {
		return string(code), true
	}
	var contextErr *agentloop.ContextError
	if errors.As(err, &contextErr) {
		return string(contextErr.Code), true
	}
	return "agent_request_failed", true
}

func durableToolActivities(events []agentloop.Event) []agentsession.TerminalToolActivityV1 {
	result := make([]agentsession.TerminalToolActivityV1, 0, len(events))
	for _, event := range events {
		state := ""
		summary := ""
		switch event.Status {
		case agentloop.EventSucceeded:
			state, summary = agentsession.ToolStateCompleted, "工具调用已完成"
		case agentloop.EventFailed, agentloop.EventInvalid:
			state, summary = agentsession.ToolStateFailed, "工具调用未完成"
		case agentloop.EventOutcomeUnknown:
			state, summary = agentsession.ToolStateUnknown, "工具调用结果未知"
		default:
			continue
		}
		if strings.TrimSpace(event.Tool) == "" {
			continue
		}
		result = append(result, agentsession.TerminalToolActivityV1{Name: event.Tool, State: state, Summary: summary})
	}
	return result
}

func (c *Controller) appendTypedNoticeLocked(kind string, turn uint64, code, outcome, message string) {
	c.transcript.Entries = append(c.transcript.Entries, agentsession.TranscriptEntryV1{
		Sequence: c.nextSequenceLocked(), PresentationTurn: turn, Kind: kind, CreatedAt: c.now().UTC(),
		Notice: &agentsession.TypedNoticeV1{Code: code, Outcome: outcome, Message: message},
	})
}

func (c *Controller) appendNoticeLocked(code, outcome, message string) {
	c.transcript.Entries = append(c.transcript.Entries, agentsession.TranscriptEntryV1{
		Sequence: c.nextSequenceLocked(), Kind: agentsession.TranscriptKindSessionNotice, CreatedAt: c.now().UTC(),
		Notice: &agentsession.TypedNoticeV1{Code: code, Outcome: outcome, Message: message},
	})
}

func (c *Controller) nextSequenceLocked() uint64 {
	if len(c.transcript.Entries) == 0 {
		return 1
	}
	return c.transcript.Entries[len(c.transcript.Entries)-1].Sequence + 1
}

func (c *Controller) recordRecoveryUnknownLocked(marker agentsession.DirtyMarker) error {
	if marker.Preference != nil {
		outcome := marker.Preference.Outcome
		if outcome == "" {
			outcome = agentsession.NoticeOutcomeUnknown
		}
		c.record.PreferenceReceipts = appendBoundedPreference(c.record.PreferenceReceipts, preferenceReceiptFromWriteAhead(*marker.Preference, outcome), c.limits.ReceiptCount)
	}
	return c.mergeFileJournalLocked(marker)
}

func hasUnknownPreferenceReceipt(values []agentsession.PreferenceReceipt) bool {
	for _, value := range values {
		if value.Outcome == agentsession.NoticeOutcomeUnknown {
			return true
		}
	}
	return false
}

func hasUnknownFileReceipt(values []agentsession.FileReceipt) bool {
	for _, value := range values {
		if value.Outcome == agentsession.NoticeOutcomeUnknown {
			return true
		}
	}
	return false
}

func fileReceiptFromCheckpoint(writeAhead agentsession.FileWriteAhead, checkpoint agentloop.SessionCheckpoint, events []agentloop.Event) (agentsession.FileReceipt, bool, error) {
	unknown := false
	foundEffect := false
	for _, turn := range checkpoint.Turns {
		if turn.FileEffectCallID != writeAhead.ToolCallID {
			continue
		}
		foundEffect = true
		unknown = turn.FileEffectUnknown || turn.OutcomeUnknown
		break
	}
	if !foundEffect {
		return agentsession.FileReceipt{}, false, nil
	}
	var reference *agentloop.WorkspaceReference
	for _, current := range checkpoint.WorkspaceReferences {
		if current.Key == writeAhead.ToolCallID {
			value := current.Value
			reference = &value
			break
		}
	}
	expectedKind := writeAhead.Effect.ReferenceKind()
	if reference != nil && expectedKind == "file" && reference.Kind == "file_effect" {
		expectedKind = "file_effect"
	}
	if reference == nil || reference.Path != writeAhead.Effect.ReferencePath() || reference.Kind != expectedKind {
		return agentsession.FileReceipt{}, false, errors.New("文件发布回执缺少匹配的稳定工作区引用")
	}
	matchedEvent := false
	for _, event := range events {
		if event.ID != writeAhead.ToolCallID {
			continue
		}
		matchedEvent = (!unknown && event.Status == agentloop.EventSucceeded) || (unknown && event.Status == agentloop.EventOutcomeUnknown)
		break
	}
	if !matchedEvent {
		return agentsession.FileReceipt{}, false, errors.New("文件发布回执缺少匹配的稳定事件")
	}
	effect := writeAhead.Effect
	if observed, found := checkpointFileEffect(checkpoint, writeAhead.ToolCallID); found {
		if observed.Validate() != nil || !effect.SamePlan(observed) {
			return agentsession.FileReceipt{}, false, errors.New("文件副作用结果与预写计划不一致")
		}
		effect = observed
	} else if effect.Operation == workspace.ToolMkdir || effect.Operation == workspace.ToolCopy || effect.Operation == workspace.ToolMove {
		return agentsession.FileReceipt{}, false, errors.New("文件回执缺少完整副作用事实")
	}
	if effect.Operation == workspace.ToolCopy && !unknown && (effect.Target.Version == "" || effect.Target.Version != reference.ContentHash) {
		return agentsession.FileReceipt{}, false, errors.New("复制回执缺少匹配的实际目标哈希")
	}
	if effect.Operation == workspace.ToolMove && (reference.ContentHash != "" || !reference.InvalidateObserved) {
		return agentsession.FileReceipt{}, false, errors.New("移动回执不能伪造目标版本或省略双端失效")
	}
	if !unknown && effect.Operation != workspace.ToolArchive && effect.Operation != workspace.ToolMkdir && effect.Operation != workspace.ToolMove {
		effect.Target.Version = reference.ContentHash
	}
	receipt := agentsession.FileReceipt{
		ToolCallID: writeAhead.ToolCallID, Effect: effect, InvalidateObserved: reference.InvalidateObserved,
		StableCode: agentsession.FilePublicationCompletedCode, Outcome: agentsession.NoticeOutcomeCompleted,
	}
	if unknown {
		receipt.Effect.Target.Version = ""
		receipt.InvalidateObserved = true
		receipt.StableCode = agentsession.FilePublicationUnknownCode
		receipt.Outcome = agentsession.NoticeOutcomeUnknown
	}
	return receipt, true, nil
}

func preferenceReceiptFromWriteAhead(value agentsession.PreferenceWriteAhead, outcome string) agentsession.PreferenceReceipt {
	return agentsession.PreferenceReceipt{
		ToolCallID: value.ToolCallID, CreateOperationID: value.CreateOperationID, AdmitOperationID: value.AdmitOperationID,
		RejectOperationID: value.RejectOperationID, Payload: value.Payload, CandidateID: value.CandidateID,
		CandidateRevision: value.CandidateRevision, Stage: value.Stage, StableCode: value.StableCode, Outcome: outcome,
	}
}

func appendBoundedPreference(values []agentsession.PreferenceReceipt, value agentsession.PreferenceReceipt, limit int) []agentsession.PreferenceReceipt {
	for index := range values {
		if values[index].CreateOperationID == value.CreateOperationID {
			values[index] = value
			return values
		}
	}
	values = append(values, value)
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}

func appendBoundedFile(values []agentsession.FileReceipt, value agentsession.FileReceipt, limit int) []agentsession.FileReceipt {
	for index, existing := range values {
		if existing.ToolCallID == value.ToolCallID {
			values[index] = value
			return values
		}
	}
	values = append(values, value)
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}

func (c *Controller) scheduleTitleLocked(checkpoint agentloop.SessionCheckpoint) {
	if c.closed || c.providerBlocked || c.model == nil || c.dirty != nil || c.record.TitleSource == "manual" || c.record.CommittedUserTurns < 1 {
		return
	}
	if c.record.AutoTitleTurns != 0 {
		if c.record.CommittedUserTurns < c.record.AutoTitleTurns || c.record.CommittedUserTurns-c.record.AutoTitleTurns < c.limits.AutoTitleTurnInterval ||
			c.record.LastTitleAt.IsZero() || c.now().Sub(c.record.LastTitleAt) < c.limits.AutoTitleMinInterval {
			return
		}
	}
	if c.titleCancel != nil {
		return
	}
	input := titleInput(checkpoint, c.record.Title, c.limits)
	if input == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.limits.AutoTitleRequestTimeout)
	c.titleCancel = cancel
	c.titleJob++
	job := c.titleJob
	turns := c.record.CommittedUserTurns
	baseRevision := c.record.RecordRevision
	generation := c.generation
	// Record the attempt boundary immediately so failures and late results cannot
	// turn every following conversation turn into an unbounded retry loop.
	c.record.AutoTitleTurns = turns
	c.record.LastTitleAt = c.now().UTC()
	go c.generateTitle(ctx, job, generation, turns, baseRevision, input)
}

func (c *Controller) generateTitle(ctx context.Context, job, generation, turns, baseRevision uint64, input string) {
	response, err := c.model.Complete(ctx, modelclient.Request{
		Messages: []modelclient.Message{
			{Role: "system", Content: "为这段学习对话生成简洁自然的中文会话标题。只输出单行JSON：{\"title\":\"...\"}。不得包含换行、工具信息、路径、错误或推理。"},
			{Role: "user", Content: input},
		},
		Tools: nil, MaxTokens: c.limits.AutoTitleMaxTokens, ReasoningEffort: modelclient.ReasoningEffortNone,
	})
	var title string
	var parseErr error
	if err == nil {
		if len(response.Message.ToolCalls) != 0 {
			parseErr = errors.New("title response contained tools")
		} else {
			title, parseErr = parseTitle(response.Message.Content, c.limits)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.titleJob == job && c.titleCancel != nil {
		c.titleCancel()
		c.titleCancel = nil
	}
	if c.titleJob != job || c.closed || !c.persistent || c.generation != generation || c.providerBlocked || c.dirty != nil || c.record.TitleSource == "manual" ||
		c.record.CommittedUserTurns != turns || c.record.RecordRevision != baseRevision {
		return
	}
	if err != nil || parseErr != nil {
		c.publishAndPersistTitleFailureLocked()
		return
	}
	candidate := c.record
	candidate.Title = title
	candidate.TitleSource = "auto"
	candidate.AutoTitleTurns = turns
	candidate.TitleRevision++
	candidate.LastConsumedDirtyID = ""
	cleanTranscript := withoutTitleFailureTranscript(c.transcript)
	encodedTranscript, encodeErr := agentsession.EncodeTranscript(cleanTranscript, c.limits)
	if encodeErr != nil {
		c.publishAndPersistTitleFailureLocked()
		return
	}
	candidate.Transcript = encodedTranscript
	candidate.TranscriptCount = uint64(len(cleanTranscript.Entries))
	if c.handle == nil {
		c.publishAndPersistTitleFailureLocked()
		return
	}
	saveCtx, saveCancel := context.WithTimeout(context.Background(), c.limits.AutoTitleSaveTimeout)
	saved, saveErr := c.handle.Save(saveCtx, c.record.RecordRevision, candidate)
	saveCancel()
	if saveErr != nil {
		c.publishTitleFailureLocked()
		return
	}
	c.record = saved
	c.transcript = cleanTranscript
	c.removeTitleFailureStatusLocked()
}

func (c *Controller) publishAndPersistTitleFailureLocked() {
	c.publishTitleFailureLocked()
	if c.handle == nil {
		return
	}
	candidate := c.record
	transcript, err := agentsession.EncodeTranscript(c.transcript, c.limits)
	if err != nil {
		return
	}
	candidate.Transcript = transcript
	candidate.TranscriptCount = uint64(len(c.transcript.Entries))
	saveCtx, cancel := context.WithTimeout(context.Background(), c.limits.AutoTitleSaveTimeout)
	saved, err := c.handle.Save(saveCtx, c.record.RecordRevision, candidate)
	cancel()
	if err == nil {
		c.record = saved
	}
}

func withoutTitleFailureTranscript(value agentsession.TranscriptV1) agentsession.TranscriptV1 {
	result := value
	result.Entries = make([]agentsession.TranscriptEntryV1, 0, len(value.Entries))
	for _, entry := range value.Entries {
		if entry.Notice != nil && entry.Notice.Code == sessionTitleFailedCode {
			continue
		}
		result.Entries = append(result.Entries, entry)
	}
	return result
}

func (c *Controller) removeTitleFailureStatusLocked() {
	filtered := c.notices[:0]
	for _, notice := range c.notices {
		if strings.HasPrefix(strings.TrimSpace(notice), "["+sessionTitleFailedCode+"]") {
			continue
		}
		filtered = append(filtered, notice)
	}
	c.notices = filtered
}

func titleInput(checkpoint agentloop.SessionCheckpoint, currentTitle string, limits agentsession.Limits) string {
	type titleTurn struct {
		id              string
		completed       bool
		user            string
		userUnsafe      bool
		assistant       string
		assistantUnsafe bool
	}
	ordered := make([]titleTurn, 0, len(checkpoint.Turns))
	turnIndex := make(map[string]int, len(checkpoint.Turns))
	for _, turn := range checkpoint.Turns {
		turnIndex[turn.ID] = len(ordered)
		ordered = append(ordered, titleTurn{
			id: turn.ID, completed: turn.Completed,
			assistantUnsafe: !turn.Completed || turn.Protected || turn.OutcomeUnknown || turn.FileEffectCallID != "" || turn.FileEffectUnknown,
		})
	}
	for _, source := range checkpoint.Context.Sources {
		position, exists := turnIndex[source.TurnID]
		if !exists {
			continue
		}
		if source.Kind == agentloop.SourceTool || source.ServerReference != nil || source.WorkspaceReference != nil ||
			source.Authority == agentloop.AuthorityServerSnapshot || source.Authority == agentloop.AuthorityServerReference || source.Authority == agentloop.AuthorityWorkspaceSnapshot ||
			source.Freshness == agentloop.FreshnessHistorical || source.Freshness == agentloop.FreshnessInvalidated || source.Freshness == agentloop.FreshnessWorkspaceObserved || source.Freshness == agentloop.FreshnessWorkspaceSuperseded {
			ordered[position].assistantUnsafe = true
		}
		if source.Kind == agentloop.SourceAssistant && source.RecallText != "" && source.RecallText != source.ModelMessage.Content {
			ordered[position].assistantUnsafe = true
		}
	}
	for index, message := range checkpoint.Messages {
		if index >= len(checkpoint.MessageTurnIDs) {
			break
		}
		position, exists := turnIndex[checkpoint.MessageTurnIDs[index]]
		if !exists {
			continue
		}
		if message.Role == "tool" || len(message.ToolCalls) != 0 {
			ordered[position].assistantUnsafe = true
		}
	}
	for index, message := range checkpoint.Messages {
		if index >= len(checkpoint.MessageTurnIDs) {
			break
		}
		position, exists := turnIndex[checkpoint.MessageTurnIDs[index]]
		if !exists || !ordered[position].completed {
			continue
		}
		text := strings.TrimSpace(message.Content)
		if text == "" {
			continue
		}
		switch message.Role {
		case "user":
			if len(message.ToolCalls) != 0 {
				continue
			}
			if sensitiveTitleInput(text) {
				ordered[position].userUnsafe = true
				continue
			}
			if ordered[position].user == "" {
				ordered[position].user = text
			}
		case "assistant":
			if sensitiveTitleInput(text) {
				continue
			}
			if len(message.ToolCalls) == 0 && !ordered[position].assistantUnsafe && !ordered[position].userUnsafe {
				ordered[position].assistant = text
			}
		}
	}
	candidates := make([]titleTurn, 0, len(ordered))
	for _, turn := range ordered {
		if turn.completed && turn.user != "" && !turn.userUnsafe {
			candidates = append(candidates, turn)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	selected := []titleTurn{candidates[0]}
	start := max(1, len(candidates)-3)
	selected = append(selected, candidates[start:]...)
	var builder strings.Builder
	appendPart := func(label, text string) bool {
		text = truncateUTF8Bytes(strings.TrimSpace(text), limits.AutoTitlePartBytes)
		if text == "" {
			return true
		}
		part := label + text
		separator := ""
		if builder.Len() > 0 {
			separator = "\n"
		}
		remaining := limits.AutoTitleInputBytes - builder.Len() - len(separator)
		if remaining <= len(label) {
			return false
		}
		part = truncateUTF8Bytes(part, remaining)
		builder.WriteString(separator)
		builder.WriteString(part)
		return len(part) == len(label)+len(text)
	}
	if title := strings.TrimSpace(currentTitle); title != "" && title != "新会话" && !sensitiveTitleInput(title) {
		if !appendPart("当前标题：", title) {
			return builder.String()
		}
	}
	for _, turn := range selected {
		if !appendPart("用户：", turn.user) || !appendPart("助手：", turn.assistant) {
			break
		}
	}
	return builder.String()
}

func truncateUTF8Bytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func sensitiveTitleInput(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "authorization:") || strings.Contains(lower, "bearer ") || strings.Contains(lower, "api_key") || strings.Contains(lower, "api key") || strings.Contains(lower, "sk-") {
		return true
	}
	for _, field := range strings.Fields(value) {
		if len(field) >= 32 && opaqueTitle(field) {
			return true
		}
	}
	return false
}

func parseTitle(data string, limits agentsession.Limits) (string, error) {
	if len(data) == 0 || len(data) > limits.AutoTitleResponseBytes || strings.ContainsAny(data, "\r\n") || !utf8.ValidString(data) {
		return "", errors.New("invalid title response")
	}
	var value struct {
		Title string `json:"title"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("trailing title response")
	}
	value.Title = norm.NFC.String(strings.TrimSpace(value.Title))
	if value.Title == "" || len(value.Title) > limits.ManualTitleBytes || utf8.RuneCountInString(value.Title) > limits.ManualTitleRunes || runewidth.StringWidth(value.Title) > limits.ManualTitleColumns || opaqueTitle(value.Title) {
		return "", errors.New("invalid title")
	}
	for _, current := range value.Title {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return "", errors.New("unsafe title")
		}
	}
	return value.Title, nil
}

func fallbackTitle(input string) string {
	input = norm.NFC.String(strings.Join(strings.Fields(strings.TrimSpace(input)), " "))
	var result strings.Builder
	for _, current := range input {
		if result.Len()+utf8.RuneLen(current) > 120 || runewidth.StringWidth(result.String()+string(current)) > 40 {
			break
		}
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			continue
		}
		result.WriteRune(current)
	}
	if strings.TrimSpace(result.String()) == "" {
		return "新会话"
	}
	return result.String()
}

func opaqueTitle(value string) bool {
	compact := strings.NewReplacer("-", "", "_", "", ".", "").Replace(value)
	if len(compact) >= 24 {
		hexOnly := true
		for _, current := range strings.ToLower(compact) {
			if current < '0' || current > '9' && current < 'a' || current > 'f' {
				hexOnly = false
				break
			}
		}
		if hexOnly {
			return true
		}
	}
	lower := strings.ToLower(value)
	return strings.Contains(lower, "api_key") || strings.Contains(lower, "bearer ") || strings.Contains(lower, "sk-")
}

func (c *Controller) abort() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.persistent = false
	if c.titleCancel != nil {
		c.titleCancel()
		c.titleCancel = nil
	}
	if c.contextCancel != nil {
		c.contextCancel()
		c.contextCancel = nil
	}
	loop, handle, store := c.loop, c.handle, c.store
	c.handle, c.store = nil, nil
	c.mu.Unlock()
	if loop != nil {
		loop.Close()
	}
	if handle != nil {
		_ = handle.Close()
	}
	if store != nil {
		_ = store.Close()
	}
}

func (c *Controller) ReasoningEffort() modelclient.ReasoningEffort {
	c.mu.Lock()
	loop := c.loop
	c.mu.Unlock()
	if loop == nil {
		return modelclient.ReasoningEffortAuto
	}
	return loop.ReasoningEffort()
}
func (c *Controller) SetReasoningEffort(value modelclient.ReasoningEffort) error {
	c.mu.Lock()
	loop := c.loop
	blocked := c.closed || c.switching
	c.mu.Unlock()
	if blocked || loop == nil {
		return ErrSwitchUnavailable
	}
	return loop.SetReasoningEffort(value)
}
func (c *Controller) FileAuthorizationMode() agentloop.FileAuthorizationMode {
	c.mu.Lock()
	loop := c.loop
	c.mu.Unlock()
	if loop == nil {
		return agentloop.FileAuthorizationConfirm
	}
	return loop.FileAuthorizationMode()
}
func (c *Controller) SetFileAuthorizationMode(value agentloop.FileAuthorizationMode) error {
	c.mu.Lock()
	loop := c.loop
	blocked := c.closed || c.switching
	c.mu.Unlock()
	if blocked || loop == nil {
		return ErrSwitchUnavailable
	}
	return loop.SetFileAuthorizationMode(value)
}
func (c *Controller) ContextStatus() agentloop.ContextStatus {
	c.mu.Lock()
	loop := c.loop
	c.mu.Unlock()
	return loop.ContextStatus()
}
func (c *Controller) ContextUpdates() <-chan agentloop.ContextEvent {
	c.mu.Lock()
	loop := c.loop
	c.mu.Unlock()
	return loop.ContextUpdates()
}
func (c *Controller) SubscribeContextUpdates() (<-chan agentloop.ContextEvent, func()) {
	c.mu.Lock()
	loop := c.loop
	c.mu.Unlock()
	return loop.SubscribeContextUpdates()
}
func (c *Controller) WorkspaceStatus() agentloop.WorkspaceStatus {
	c.mu.Lock()
	loop := c.loop
	c.mu.Unlock()
	if loop == nil {
		return agentloop.WorkspaceStatus{Code: "workspace_unavailable"}
	}
	return loop.WorkspaceStatus()
}
func (c *Controller) LearningStatus(ctx context.Context) (agentloop.LearningStatus, error) {
	c.mu.Lock()
	loop := c.loop
	c.mu.Unlock()
	if loop == nil {
		return agentloop.LearningStatus{}, ErrSwitchUnavailable
	}
	return loop.LearningStatus(ctx)
}

func (c *Controller) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		err := c.shutdownErr
		c.mu.Unlock()
		return err
	}
	c.closed = true
	if c.titleCancel != nil {
		c.titleCancel()
		c.titleCancel = nil
	}
	if c.contextCancel != nil {
		c.contextCancel()
		c.contextCancel = nil
	}
	empty := c.persistent && c.dirty == nil && c.record.CommittedUserTurns == 0 && len(c.record.PreferenceReceipts) == 0 && len(c.record.FileReceipts) == 0 && c.record.TitleSource != "manual"
	var shutdownErr error
	if c.persistent && !empty {
		previousLifecycle := c.record.Lifecycle
		c.record.Lifecycle = "closed"
		if err := c.saveCheckpointLocked(ctx, c.dirty != nil); err != nil {
			c.record.Lifecycle = previousLifecycle
			shutdownErr = checkpointPersistenceError(err)
			c.saveFailed = shutdownErr
		}
	}
	loop, handle, store := c.loop, c.handle, c.store
	sessionID, storageID, revision := c.record.SessionID, c.record.StorageID, c.record.RecordRevision
	c.handle, c.store = nil, nil
	c.mu.Unlock()

	if loop != nil {
		loop.Close()
	}
	if handle != nil {
		if err := handle.Close(); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	if empty && store != nil {
		if err := store.Delete(ctx, agentsession.DeleteTarget{SessionID: sessionID, StorageID: storageID, ExpectedRecordRevision: revision}); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	if store != nil {
		if err := store.Close(); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	c.mu.Lock()
	c.shutdownErr = shutdownErr
	c.mu.Unlock()
	return shutdownErr
}

func (c *Controller) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Shutdown(ctx)
}

func (c *Controller) String() string {
	status := c.Status()
	sessionID := c.SessionID()
	return fmt.Sprintf("session=%s persistent=%t", sessionID, status.Persistent)
}
