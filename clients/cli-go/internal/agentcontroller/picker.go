package agentcontroller

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/mattn/go-runewidth"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	ErrSwitchUnavailable     = errors.New("当前 Session 状态不允许切换")
	ErrCurrentSession        = errors.New("不能删除当前 Session")
	ErrUnknownOutcomePending = errors.New("当前 Session 存在待核对的未知副作用结果")
)

const (
	SwitchBlockActiveTurn        = "active_turn"
	SwitchBlockPendingQuestion   = "pending_question"
	SwitchBlockPendingPreference = "pending_preference"
	SwitchBlockPendingFile       = "pending_file_mutation"
	SwitchBlockUnknownOutcome    = "unknown_outcome"
	SwitchBlockSaving            = "saving"
	SwitchBlockSwitching         = "switching"
	SwitchBlockClosing           = "closing"
	SwitchBlockUnsaved           = "unsaved"
)

type SwitchGate struct {
	Allowed bool
	Code    string
	Reason  string
}

type SessionListRequest struct {
	All   bool
	Query string
	Limit int
}

type SessionListItem struct {
	Summary agentsession.Summary
	Current bool
}

type SwitchPlan struct {
	SessionID            string
	Title                string
	WorkspaceLabel       string
	ExpectedRevision     uint64
	NeedWorkspaceConfirm bool
	NeedProviderConfirm  bool
	WorkspaceUnavailable bool
	SameTarget           bool
	controllerGeneration uint64
	currentSessionID     string
}

type SwitchConfirmation struct {
	Workspace bool
	Provider  bool
}

type UnknownOutcome struct {
	ReceiptID string
	Kind      string
	Label     string
}

// Selector provides the same bounded list/search/rename/delete and switch-plan
// rules used by the in-process controller to the command-level picker.
type Selector struct {
	store            *agentsession.Store
	workspaceID      string
	provider         Provider
	currentID        string
	currentStorageID string
}

func NewSelector(store *agentsession.Store, workspaceID string, provider Provider) *Selector {
	return &Selector{store: store, workspaceID: workspaceID, provider: provider}
}

func (s *Selector) Generation() uint64 { return 1 }
func (s *Selector) SwitchGate() SwitchGate {
	if s == nil || s.store == nil {
		return switchBlocked(SwitchBlockUnsaved)
	}
	return SwitchGate{Allowed: true}
}
func (s *Selector) ListSessions(ctx context.Context, request SessionListRequest) ([]SessionListItem, error) {
	if s == nil || s.store == nil {
		return nil, agentsession.ErrKeyUnavailable
	}
	return listSessionItems(ctx, s.store, s.workspaceID, s.currentID, s.currentStorageID, request, s.store.Limits())
}
func (s *Selector) PlanSwitch(summary agentsession.Summary) SwitchPlan {
	return buildSwitchPlan(1, s.currentID, s.workspaceID, s.provider, summary)
}
func (s *Selector) RenameSession(ctx context.Context, sessionID, title string, expectedRevision uint64) (agentsession.Summary, error) {
	return renameStoredSession(ctx, s.store, sessionID, title, expectedRevision, s.store.Limits())
}
func (s *Selector) DeleteSession(ctx context.Context, target agentsession.DeleteTarget) error {
	if target.SessionID != "" && target.SessionID == s.currentID || target.StorageID != "" && target.StorageID == s.currentStorageID {
		return ErrCurrentSession
	}
	return s.store.Delete(ctx, target)
}

func (c *Controller) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *Controller) SwitchGate() SwitchGate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.switchGateLocked()
}

func (c *Controller) switchGateLocked() SwitchGate {
	switch {
	case c.closed:
		return switchBlocked(SwitchBlockClosing)
	case c.switching:
		return switchBlocked(SwitchBlockSwitching)
	case c.operating:
		return switchBlocked(SwitchBlockActiveTurn)
	case c.saving.Load():
		return switchBlocked(SwitchBlockSaving)
	case !c.persistent || c.store == nil || c.handle == nil:
		return switchBlocked(SwitchBlockUnsaved)
	case hasUnknownPreferenceReceipt(c.record.PreferenceReceipts) || hasUnknownFileReceipt(c.record.FileReceipts):
		return switchBlocked(SwitchBlockUnknownOutcome)
	}
	state := c.loop.SwitchState()
	switch {
	case state.Closed:
		return switchBlocked(SwitchBlockClosing)
	case state.ActiveTurn || state.Resolving:
		return switchBlocked(SwitchBlockActiveTurn)
	case state.PendingQuestion:
		return switchBlocked(SwitchBlockPendingQuestion)
	case state.PendingPreference:
		return switchBlocked(SwitchBlockPendingPreference)
	case state.PendingFileMutation:
		return switchBlocked(SwitchBlockPendingFile)
	default:
		return SwitchGate{Allowed: true}
	}
}

func switchBlocked(code string) SwitchGate {
	reason := map[string]string{
		SwitchBlockActiveTurn:        "当前轮次仍在运行或停止中",
		SwitchBlockPendingQuestion:   "当前 Session 正在等待问题选择",
		SwitchBlockPendingPreference: "当前 Session 正在等待长期偏好确认",
		SwitchBlockPendingFile:       "当前 Session 正在等待文件修改授权",
		SwitchBlockUnknownOutcome:    "先核对未知偏好结果；未知文件结果需重新读取、预览并授权",
		SwitchBlockSaving:            "当前 Session 正在保存",
		SwitchBlockSwitching:         "Session 正在切换",
		SwitchBlockClosing:           "Session 正在关闭",
		SwitchBlockUnsaved:           "当前 Session 未启用历史存储",
	}[code]
	return SwitchGate{Code: code, Reason: reason}
}

func (c *Controller) ListSessions(ctx context.Context, request SessionListRequest) ([]SessionListItem, error) {
	c.mu.Lock()
	store, workspaceID, currentID, currentStorageID := c.store, c.record.WorkspaceID, c.record.SessionID, c.record.StorageID
	gate := c.switchGateLocked()
	c.mu.Unlock()
	if !gate.Allowed && gate.Code != SwitchBlockUnknownOutcome {
		return nil, ErrSwitchUnavailable
	}
	return listSessionItems(ctx, store, workspaceID, currentID, currentStorageID, request, c.limits)
}

func listSessionItems(ctx context.Context, store *agentsession.Store, workspaceID, currentID, currentStorageID string, request SessionListRequest, limits agentsession.Limits) ([]SessionListItem, error) {
	if store == nil {
		return nil, agentsession.ErrKeyUnavailable
	}
	query, err := normalizeSearchQuery(request.Query, limits)
	if err != nil {
		return nil, err
	}
	summaries, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]SessionListItem, 0, len(summaries))
	for _, summary := range summaries {
		if !request.All && (workspaceID == "" || summary.WorkspaceID != workspaceID) {
			continue
		}
		if query != "" && !summaryMatches(summary, query) {
			continue
		}
		items = append(items, SessionListItem{Summary: summary, Current: summary.SessionID != "" && summary.SessionID == currentID || summary.StorageID != "" && summary.StorageID == currentStorageID})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Summary.UpdatedAt.Equal(items[j].Summary.UpdatedAt) {
			if items[i].Summary.SessionID == items[j].Summary.SessionID {
				return items[i].Summary.StorageID < items[j].Summary.StorageID
			}
			return items[i].Summary.SessionID < items[j].Summary.SessionID
		}
		return items[i].Summary.UpdatedAt.After(items[j].Summary.UpdatedAt)
	})
	limit := limits.PickerResults
	if request.Limit > 0 && request.Limit < limit {
		limit = request.Limit
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (c *Controller) PlanSwitch(summary agentsession.Summary) SwitchPlan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return buildSwitchPlan(c.generation, c.record.SessionID, c.record.WorkspaceID, c.provider, summary)
}

func buildSwitchPlan(generation uint64, currentID, workspaceID string, provider Provider, summary agentsession.Summary) SwitchPlan {
	plan := SwitchPlan{
		SessionID: summary.SessionID, Title: summary.Title, WorkspaceLabel: summary.WorkspaceLabel,
		ExpectedRevision: summary.RecordRevision, SameTarget: summary.SessionID != "" && summary.SessionID == currentID,
		controllerGeneration: generation, currentSessionID: currentID,
	}
	plan.NeedWorkspaceConfirm = !plan.SameTarget && workspaceID != "" && summary.WorkspaceID != "" && summary.WorkspaceID != workspaceID
	plan.NeedProviderConfirm = !plan.SameTarget && ProviderEndpointIdentity(Provider{Name: summary.ProviderName, Endpoint: summary.ProviderEndpoint}) != ProviderEndpointIdentity(provider)
	plan.WorkspaceUnavailable = summary.Unavailable || summary.Corrupt
	return plan
}

func (c *Controller) RenameSession(ctx context.Context, sessionID, title string, expectedRevision uint64) (agentsession.Summary, error) {
	c.mu.Lock()
	if c.closed || c.switching {
		c.mu.Unlock()
		return agentsession.Summary{}, ErrSwitchUnavailable
	}
	if sessionID != c.record.SessionID {
		store := c.store
		c.mu.Unlock()
		return renameStoredSession(ctx, store, sessionID, title, expectedRevision, store.Limits())
	}
	if expectedRevision == 0 || expectedRevision != c.record.RecordRevision {
		c.mu.Unlock()
		return agentsession.Summary{}, agentsession.ErrCheckpointConflict
	}
	normalized, manual, err := normalizeManualTitle(title, c.transcript, c.limits)
	if err != nil {
		c.mu.Unlock()
		return agentsession.Summary{}, err
	}
	previous := c.record
	if c.titleCancel != nil {
		c.titleCancel()
		c.titleCancel = nil
	}
	c.record.Title = normalized
	c.record.TitleRevision++
	if manual {
		c.record.TitleSource = "manual"
	} else {
		c.record.TitleSource = "auto"
		c.record.AutoTitleTurns = 0
		c.record.LastTitleAt = time.Time{}
	}
	if err := c.saveRecordLocked(ctx, false); err != nil {
		c.record = previous
		c.mu.Unlock()
		return agentsession.Summary{}, err
	}
	summary := summaryFromRecord(c.record)
	c.mu.Unlock()
	return summary, nil
}

func renameStoredSession(ctx context.Context, store *agentsession.Store, sessionID, title string, expectedRevision uint64, limits agentsession.Limits) (agentsession.Summary, error) {
	if store == nil {
		return agentsession.Summary{}, agentsession.ErrKeyUnavailable
	}
	handle, loaded, err := store.OpenSession(ctx, sessionID)
	if err != nil {
		return agentsession.Summary{}, err
	}
	defer handle.Close()
	if loaded.Record.RecordRevision != expectedRevision || expectedRevision == 0 {
		return agentsession.Summary{}, agentsession.ErrCheckpointConflict
	}
	transcript, err := agentsession.DecodeTranscript(loaded.Record.Transcript, limits)
	if err != nil {
		return agentsession.Summary{}, err
	}
	normalized, manual, err := normalizeManualTitle(title, transcript, limits)
	if err != nil {
		return agentsession.Summary{}, err
	}
	candidate := loaded.Record
	candidate.Title = normalized
	candidate.TitleRevision++
	if manual {
		candidate.TitleSource = "manual"
	} else {
		candidate.TitleSource = "auto"
		candidate.AutoTitleTurns = 0
		candidate.LastTitleAt = time.Time{}
	}
	saved, err := handle.Save(ctx, expectedRevision, candidate)
	if err != nil {
		return agentsession.Summary{}, err
	}
	return summaryFromRecord(saved), nil
}

func (c *Controller) DeleteSession(ctx context.Context, target agentsession.DeleteTarget) error {
	c.mu.Lock()
	if c.closed || c.switching {
		c.mu.Unlock()
		return ErrSwitchUnavailable
	}
	if target.SessionID != "" && target.SessionID == c.record.SessionID || target.StorageID != "" && target.StorageID == c.record.StorageID {
		c.mu.Unlock()
		return ErrCurrentSession
	}
	store := c.store
	c.mu.Unlock()
	if store == nil {
		return agentsession.ErrKeyUnavailable
	}
	return store.Delete(ctx, target)
}

func (c *Controller) SessionTitle() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.record.Title
}

func (c *Controller) SessionTranscript() agentsession.TranscriptV1 {
	c.mu.Lock()
	defer c.mu.Unlock()
	encoded, err := agentsession.EncodeTranscript(c.transcript, c.limits)
	if err != nil {
		return agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{}}
	}
	value, err := agentsession.DecodeTranscript(encoded, c.limits)
	if err != nil {
		return agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{}}
	}
	return value
}

func (c *Controller) UnknownOutcomes() []UnknownOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]UnknownOutcome, 0)
	for _, receipt := range c.record.PreferenceReceipts {
		if receipt.Outcome == agentsession.NoticeOutcomeUnknown {
			result = append(result, UnknownOutcome{ReceiptID: receipt.CreateOperationID, Kind: "preference", Label: "长期偏好结果待核对"})
		}
	}
	for _, receipt := range c.record.FileReceipts {
		if receipt.Outcome == agentsession.NoticeOutcomeUnknown {
			label := "文件结果未知：需重新读取、预览并授权"
			if receipt.Operation == "archive" {
				label = "归档结果未知：检查源 " + receipt.Path + " 与归档目标 " + receipt.ArchivePath + "；不会自动重试或清理"
			}
			result = append(result, UnknownOutcome{ReceiptID: receipt.Operation, Kind: "file", Label: label})
		}
	}
	return result
}

func normalizeSearchQuery(value string, limits agentsession.Limits) (string, error) {
	value = norm.NFC.String(strings.TrimSpace(value))
	if utf8.RuneCountInString(value) > limits.PickerQueryRunes {
		return "", agentsession.ErrInvalid
	}
	for _, current := range value {
		if unicode.IsControl(current) || current == '\u202e' || current == '\u202d' || current == '\u2066' || current == '\u2067' || current == '\u2068' || current == '\u2069' {
			return "", agentsession.ErrInvalid
		}
	}
	return cases.Fold().String(value), nil
}

func summaryMatches(summary agentsession.Summary, query string) bool {
	fields := []string{summary.Title, summary.SessionID, summary.StorageID, summary.FirstUserSummary, summary.RecentUserSummary, summary.WorkspaceLabel}
	fold := cases.Fold()
	for _, field := range fields {
		candidate := fold.String(norm.NFC.String(field))
		if strings.Contains(candidate, query) || fuzzySubsequence(candidate, query) {
			return true
		}
	}
	return false
}

func fuzzySubsequence(candidate, query string) bool {
	if query == "" {
		return true
	}
	remaining := []rune(query)
	for _, current := range candidate {
		if len(remaining) > 0 && current == remaining[0] {
			remaining = remaining[1:]
		}
	}
	return len(remaining) == 0
}

func normalizeManualTitle(value string, transcript agentsession.TranscriptV1, limits agentsession.Limits) (string, bool, error) {
	value = norm.NFC.String(strings.TrimSpace(value))
	if value == "" {
		fallback := "新 Session"
		for _, entry := range transcript.Entries {
			if entry.Kind == agentsession.TranscriptKindUser {
				fallback = boundedSearchSummary(entry.Text, limits)
				break
			}
		}
		return fallback, false, nil
	}
	if len(value) > limits.ManualTitleBytes || utf8.RuneCountInString(value) > limits.ManualTitleRunes || runewidth.StringWidth(value) > limits.ManualTitleColumns || strings.ContainsAny(value, "\r\n\t") {
		return "", false, agentsession.ErrInvalid
	}
	for _, current := range value {
		if unicode.IsControl(current) || current == '\u202e' || current == '\u202d' || current == '\u2066' || current == '\u2067' || current == '\u2068' || current == '\u2069' {
			return "", false, agentsession.ErrInvalid
		}
	}
	return value, true, nil
}

func boundedSearchSummary(value string, limits agentsession.Limits) string {
	value = norm.NFC.String(strings.Join(strings.Fields(value), " "))
	runes := []rune(value)
	if len(runes) > limits.SearchSummaryRunes {
		runes = runes[:limits.SearchSummaryRunes]
	}
	value = string(runes)
	for len(value) > limits.SearchSummaryBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func summaryFromRecord(record agentsession.SessionRecord) agentsession.Summary {
	return agentsession.Summary{
		SessionID: record.SessionID, StorageID: record.StorageID, RecordRevision: record.RecordRevision, CheckpointRevision: record.CheckpointRevision,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, LastOpenedAt: record.LastOpenedAt,
		Title: record.Title, TitleSource: record.TitleSource, FirstUserSummary: record.FirstUserSummary, RecentUserSummary: record.RecentUserSummary,
		TitleRevision: record.TitleRevision, CommittedUserTurns: record.CommittedUserTurns, TranscriptCount: record.TranscriptCount,
		ServerProfileFingerprint: record.ServerProfileFingerprint, WorkspaceID: record.WorkspaceID, WorkspaceLabel: record.WorkspaceLabel,
		ProviderName: record.ProviderName, ProviderEndpoint: record.ProviderEndpoint, ProviderModel: record.ProviderModel, Lifecycle: record.Lifecycle,
	}
}
