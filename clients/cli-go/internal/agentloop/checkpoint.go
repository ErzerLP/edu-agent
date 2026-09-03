package agentloop

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const (
	SessionCheckpointSchemaVersion = 1
	MaxSessionCheckpointBytes      = 8 << 20
	maxCheckpointTurns             = 4096
	maxCheckpointMessages          = 16384
	maxCheckpointSources           = 32768
	maxCheckpointObservations      = 8192
	maxCheckpointReflections       = 4096
)

var (
	ErrCheckpointUnstable           = errors.New("Agent会话当前不在稳定检查点")
	ErrCheckpointCorrupt            = errors.New("Agent会话检查点已损坏")
	ErrCheckpointVersionUnsupported = errors.New("Agent会话检查点版本不受支持")
)

type SessionCheckpoint struct {
	SchemaVersion       int                         `json:"schema_version"`
	ReasoningEffort     modelclient.ReasoningEffort `json:"reasoning_effort"`
	Messages            []modelclient.Message       `json:"messages"`
	MessageTurnIDs      []string                    `json:"message_turn_ids"`
	Turns               []CheckpointTurn            `json:"turns"`
	CurrentTurnID       string                      `json:"current_turn_id,omitempty"`
	TurnSequence        int                         `json:"turn_sequence"`
	ToolHistory         []CheckpointStringEntry     `json:"tool_history"`
	ToolReferences      []CheckpointServerEntry     `json:"tool_references"`
	WorkspaceReferences []CheckpointWorkspaceEntry  `json:"workspace_references"`
	Context             CheckpointContext           `json:"context"`
}

type CheckpointTurn struct {
	ID                string   `json:"id"`
	Completed         bool     `json:"completed"`
	Protected         bool     `json:"protected"`
	OutcomeUnknown    bool     `json:"outcome_unknown"`
	SourceIDs         []string `json:"source_ids"`
	FileEffectCallID  string   `json:"file_effect_call_id,omitempty"`
	FileEffectUnknown bool     `json:"file_effect_unknown"`
	QuestionsAsked    int      `json:"questions_asked"`
	QuestionIDs       []string `json:"question_ids"`
}

type CheckpointStringEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type CheckpointServerEntry struct {
	Key   string          `json:"key"`
	Value ServerReference `json:"value"`
}

type CheckpointWorkspaceEntry struct {
	Key   string             `json:"key"`
	Value WorkspaceReference `json:"value"`
}

type CheckpointContext struct {
	Mode                   string                  `json:"mode"`
	Sources                []CheckpointSource      `json:"sources"`
	Observations           []CheckpointObservation `json:"observations"`
	Reflections            []CheckpointReflection  `json:"reflections"`
	Supersessions          []Supersession          `json:"supersessions"`
	Tombstones             []ObservationTombstone  `json:"tombstones"`
	CoverageWatermark      string                  `json:"coverage_watermark,omitempty"`
	CoverageIndex          int                     `json:"coverage_index"`
	SuccessfulObserverRuns int                     `json:"successful_observer_runs"`
	LastReflectedRun       int                     `json:"last_reflected_run"`
	AllocatedIDs           []string                `json:"allocated_ids"`
}

type CheckpointSource struct {
	ID                 string              `json:"id"`
	TurnID             string              `json:"turn_id"`
	Kind               SourceKind          `json:"kind"`
	CreatedAt          time.Time           `json:"created_at"`
	ModelMessage       modelclient.Message `json:"model_message"`
	HasModelMessage    bool                `json:"has_model_message"`
	RecallText         string              `json:"recall_text"`
	ContentHash        string              `json:"content_hash"`
	SourceAvailable    bool                `json:"source_available"`
	Retention          RetentionClass      `json:"retention"`
	Authority          AuthorityClass      `json:"authority"`
	Freshness          FreshnessClass      `json:"freshness"`
	ServerReference    *ServerReference    `json:"server_reference,omitempty"`
	WorkspaceReference *WorkspaceReference `json:"workspace_reference,omitempty"`
}

type CheckpointObservation struct {
	ID             string          `json:"id"`
	Content        string          `json:"content"`
	CreatedAt      time.Time       `json:"created_at"`
	Relevance      Relevance       `json:"relevance"`
	Kind           ObservationKind `json:"kind"`
	SourceEntryIDs []string        `json:"source_entry_ids"`
	Authority      AuthorityClass  `json:"authority"`
	Freshness      FreshnessClass  `json:"freshness"`
}

type CheckpointReflection struct {
	ID        string         `json:"id"`
	Content   string         `json:"content"`
	Kind      ReflectionKind `json:"kind"`
	Support   []CoverageEdge `json:"support"`
	Authority AuthorityClass `json:"authority"`
	Freshness FreshnessClass `json:"freshness"`
	CreatedAt time.Time      `json:"created_at"`
}

// ExportCheckpoint captures only a stable continuation point. Pending
// questions, file previews, authorization decisions, in-flight requests and
// pending preference operations are intentionally excluded.
func (s *Session) ExportCheckpoint() (SessionCheckpoint, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return SessionCheckpoint{}, ErrSessionClosed
	}
	if s.activeTurnID != "" || s.pendingKind != pendingNone || s.pendingResolving ||
		s.pendingOperationID != "" || s.pendingDecisionOperationID != "" || s.pendingRejectOperationID != "" || s.pendingCandidateID != "" {
		return SessionCheckpoint{}, ErrCheckpointUnstable
	}
	checkpoint := SessionCheckpoint{
		SchemaVersion:   SessionCheckpointSchemaVersion,
		ReasoningEffort: s.reasoningEffort,
		CurrentTurnID:   s.currentTurnID,
		TurnSequence:    s.turnSequence,
		Messages:        make([]modelclient.Message, 0, len(s.messages)),
		MessageTurnIDs:  make([]string, 0, len(s.messageTurnIDs)),
	}
	for index, message := range s.messages {
		if message.Role == "system" {
			continue
		}
		checkpoint.Messages = append(checkpoint.Messages, checkpointMessage(message))
		checkpoint.MessageTurnIDs = append(checkpoint.MessageTurnIDs, s.messageTurnIDs[index])
	}
	checkpoint.Turns = make([]CheckpointTurn, 0, len(s.turnOrder))
	for _, turnID := range s.turnOrder {
		turn := s.turns[turnID]
		if turn == nil || !turn.Completed {
			return SessionCheckpoint{}, ErrCheckpointUnstable
		}
		checkpoint.Turns = append(checkpoint.Turns, CheckpointTurn{
			ID: turn.ID, Completed: turn.Completed, Protected: turn.Protected, OutcomeUnknown: turn.OutcomeUnknown,
			SourceIDs: append([]string(nil), turn.SourceIDs...), FileEffectCallID: turn.FileEffectCallID,
			FileEffectUnknown: turn.FileEffectUnknown, QuestionsAsked: turn.QuestionsAsked,
			QuestionIDs: sortedStringSet(turn.QuestionIDs),
		})
	}
	checkpoint.ToolHistory = exportStringMap(s.toolHistory)
	checkpoint.ToolReferences = exportServerMap(s.toolReferences)
	checkpoint.WorkspaceReferences = exportWorkspaceMap(s.workspaceReferences)

	runtime := s.contextRuntime
	runtime.mu.Lock()
	checkpoint.Context = exportCheckpointContextLocked(runtime)
	runtime.mu.Unlock()
	if err := validateSessionCheckpoint(checkpoint); err != nil {
		return SessionCheckpoint{}, err
	}
	return checkpoint, nil
}

// RestoreCheckpoint installs a validated checkpoint into a fresh Session.
// Runtime-only interaction state is reset and file authorization always
// returns to per-operation confirmation.
func (s *Session) RestoreCheckpoint(checkpoint SessionCheckpoint) error {
	prepared, err := prepareSessionCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	if len(s.turnOrder) != 0 || s.activeTurnID != "" || s.currentTurnID != "" || s.pendingKind != pendingNone || !freshSystemMessages(s.messages, s.messageTurnIDs) {
		return errors.New("Agent检查点只能恢复到新会话")
	}
	runtime := s.contextRuntime
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.ledger.SourceOrder) != 0 || runtime.mode != checkpoint.Context.Mode {
		return errors.New("Agent检查点与当前上下文模式不兼容")
	}

	systemMessages := make([]modelclient.Message, len(s.messages))
	for index, message := range s.messages {
		systemMessages[index] = cloneModelMessage(message)
	}
	systemTurnIDs := append([]string(nil), s.messageTurnIDs...)
	s.messages = append(systemMessages, prepared.messages...)
	s.messageTurnIDs = append(systemTurnIDs, prepared.messageTurnIDs...)
	s.turns = prepared.turns
	s.turnOrder = prepared.turnOrder
	s.currentTurnID = checkpoint.CurrentTurnID
	s.turnSequence = checkpoint.TurnSequence
	s.activeTurnID = ""
	s.toolHistory = prepared.toolHistory
	s.toolReferences = prepared.toolReferences
	s.workspaceReferences = prepared.workspaceReferences
	s.reasoningEffort = checkpoint.ReasoningEffort
	s.fileAuthorizationMode = FileAuthorizationConfirm
	s.remaining = 0
	s.toolCallsRemaining = 0
	s.currentToolResultTokens = 0
	s.clearPendingLocked()

	runtime.ledger = prepared.ledger
	runtime.usedIDs = prepared.usedIDs
	runtime.hotTurns = prepared.hotTurns
	runtime.preferencePending = false
	runtime.consolidationRunning = false
	runtime.lastReflectedRun = checkpoint.Context.LastReflectedRun
	runtime.degradedTurns = make(map[string]struct{})
	runtime.recomputeDerivedCheckpointStateLocked()
	return nil
}

func EncodeSessionCheckpoint(checkpoint SessionCheckpoint) ([]byte, error) {
	if err := validateSessionCheckpoint(checkpoint); err != nil {
		return nil, err
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSessionCheckpointBytes {
		return nil, errors.New("Agent检查点超过大小上限")
	}
	return data, nil
}

func DecodeSessionCheckpoint(data []byte) (SessionCheckpoint, error) {
	var checkpoint SessionCheckpoint
	if len(data) == 0 || len(data) > MaxSessionCheckpointBytes || !utf8.Valid(data) {
		return checkpoint, ErrCheckpointCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return checkpoint, ErrCheckpointCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return checkpoint, ErrCheckpointCorrupt
	}
	if err := checkpointSchemaError(checkpoint.SchemaVersion); err != nil {
		return SessionCheckpoint{}, err
	}
	if err := validateSessionCheckpoint(checkpoint); err != nil {
		return SessionCheckpoint{}, ErrCheckpointCorrupt
	}
	return checkpoint, nil
}

func checkpointSchemaError(version int) error {
	switch {
	case version == SessionCheckpointSchemaVersion:
		return nil
	case version > 0:
		return ErrCheckpointVersionUnsupported
	default:
		return ErrCheckpointCorrupt
	}
}

type preparedSessionCheckpoint struct {
	messages            []modelclient.Message
	messageTurnIDs      []string
	turns               map[string]*sessionTurn
	turnOrder           []string
	toolHistory         map[string]string
	toolReferences      map[string]*ServerReference
	workspaceReferences map[string]*WorkspaceReference
	ledger              SessionLedger
	usedIDs             map[string]struct{}
	hotTurns            map[string]struct{}
}

func prepareSessionCheckpoint(checkpoint SessionCheckpoint) (preparedSessionCheckpoint, error) {
	if err := validateSessionCheckpoint(checkpoint); err != nil {
		return preparedSessionCheckpoint{}, err
	}
	prepared := preparedSessionCheckpoint{
		messages:            make([]modelclient.Message, len(checkpoint.Messages)),
		messageTurnIDs:      append([]string(nil), checkpoint.MessageTurnIDs...),
		turns:               make(map[string]*sessionTurn, len(checkpoint.Turns)),
		turnOrder:           make([]string, 0, len(checkpoint.Turns)),
		toolHistory:         make(map[string]string, len(checkpoint.ToolHistory)),
		toolReferences:      make(map[string]*ServerReference, len(checkpoint.ToolReferences)),
		workspaceReferences: make(map[string]*WorkspaceReference, len(checkpoint.WorkspaceReferences)),
		ledger:              newSessionLedger(), usedIDs: make(map[string]struct{}, len(checkpoint.Context.AllocatedIDs)), hotTurns: make(map[string]struct{}),
	}
	for _, id := range checkpoint.Context.AllocatedIDs {
		prepared.usedIDs[id] = struct{}{}
	}
	for index, message := range checkpoint.Messages {
		prepared.messages[index] = cloneModelMessage(message)
	}
	for _, current := range checkpoint.Turns {
		turn := &sessionTurn{
			ID: current.ID, Completed: current.Completed, Protected: current.Protected,
			OutcomeUnknown: current.OutcomeUnknown, SourceIDs: append([]string(nil), current.SourceIDs...),
			FileEffectCallID: current.FileEffectCallID, FileEffectUnknown: current.FileEffectUnknown,
			QuestionsAsked: current.QuestionsAsked, QuestionIDs: make(map[string]struct{}, len(current.QuestionIDs)),
		}
		for _, questionID := range current.QuestionIDs {
			turn.QuestionIDs[questionID] = struct{}{}
		}
		prepared.turns[current.ID] = turn
		prepared.turnOrder = append(prepared.turnOrder, current.ID)
	}
	for _, current := range checkpoint.ToolHistory {
		prepared.toolHistory[current.Key] = current.Value
	}
	for _, current := range checkpoint.ToolReferences {
		value := current.Value
		prepared.toolReferences[current.Key] = &value
	}
	for _, current := range checkpoint.WorkspaceReferences {
		value := current.Value
		prepared.workspaceReferences[current.Key] = &value
	}

	for index, current := range checkpoint.Context.Sources {
		entry := SourceEntry{
			ID: current.ID, TurnID: current.TurnID, Kind: current.Kind, CreatedAt: current.CreatedAt,
			ModelMessage: cloneModelMessage(current.ModelMessage), HasModelMessage: current.HasModelMessage,
			RecallText: current.RecallText, ContentHash: current.ContentHash, SourceAvailable: current.SourceAvailable,
			Retention: current.Retention, Authority: current.Authority,
			Freshness: current.Freshness, ServerReference: cloneServerReference(current.ServerReference),
			WorkspaceReference: cloneWorkspaceReference(current.WorkspaceReference),
		}
		prepared.ledger.SourceIndex[entry.ID] = index
		prepared.ledger.SourceOrder = append(prepared.ledger.SourceOrder, entry.ID)
		prepared.ledger.Sources[entry.ID] = entry
		if entry.Retention == RetentionHot {
			prepared.hotTurns[entry.TurnID] = struct{}{}
		}
	}
	for _, current := range checkpoint.Context.Observations {
		entry := Observation{
			ID: current.ID, Content: current.Content, CreatedAt: current.CreatedAt, Relevance: current.Relevance,
			Kind: current.Kind, SourceEntryIDs: append([]string(nil), current.SourceEntryIDs...),
			Authority: current.Authority, Freshness: current.Freshness,
		}
		prepared.ledger.ObservationOrder = append(prepared.ledger.ObservationOrder, entry.ID)
		prepared.ledger.Observations[entry.ID] = entry
	}
	for _, current := range checkpoint.Context.Reflections {
		entry := Reflection{
			ID: current.ID, Content: current.Content, Kind: current.Kind,
			Support: append([]CoverageEdge(nil), current.Support...), Authority: current.Authority,
			Freshness: current.Freshness, CreatedAt: current.CreatedAt,
		}
		prepared.ledger.ReflectionOrder = append(prepared.ledger.ReflectionOrder, entry.ID)
		prepared.ledger.Reflections[entry.ID] = entry
	}
	prepared.ledger.Supersessions = append([]Supersession(nil), checkpoint.Context.Supersessions...)
	for _, current := range checkpoint.Context.Tombstones {
		prepared.ledger.Tombstones[current.ObservationID] = current
	}
	prepared.ledger.CoverageWatermark = checkpoint.Context.CoverageWatermark
	prepared.ledger.CoverageIndex = checkpoint.Context.CoverageIndex
	prepared.ledger.SuccessfulObserverRuns = checkpoint.Context.SuccessfulObserverRuns
	return prepared, nil
}

func validateSessionCheckpoint(checkpoint SessionCheckpoint) error {
	if err := checkpointSchemaError(checkpoint.SchemaVersion); err != nil {
		return err
	}
	if !validReasoningEffort(checkpoint.ReasoningEffort) || checkpoint.TurnSequence < 0 {
		return errors.New("Agent检查点版本或推理强度无效")
	}
	if len(checkpoint.Messages) != len(checkpoint.MessageTurnIDs) || len(checkpoint.Messages) > maxCheckpointMessages || len(checkpoint.Turns) > maxCheckpointTurns {
		return errors.New("Agent检查点消息或轮次结构无效")
	}
	turns := make(map[string]struct{}, len(checkpoint.Turns))
	maxTurnSequence := 0
	for _, turn := range checkpoint.Turns {
		if turn.ID == "" || len(turn.ID) > 128 || !turn.Completed || turn.QuestionsAsked < 0 {
			return ErrCheckpointUnstable
		}
		turnNumber, err := strconv.Atoi(strings.TrimPrefix(turn.ID, "turn-"))
		if err != nil || turnNumber < 1 || turn.ID != fmt.Sprintf("turn-%d", turnNumber) {
			return errors.New("Agent检查点轮次ID无效")
		}
		if turnNumber > maxTurnSequence {
			maxTurnSequence = turnNumber
		}
		if _, exists := turns[turn.ID]; exists {
			return errors.New("Agent检查点轮次重复")
		}
		turns[turn.ID] = struct{}{}
		questionIDs := make(map[string]struct{}, len(turn.QuestionIDs))
		for _, questionID := range turn.QuestionIDs {
			if !validQuestionID(questionID) {
				return errors.New("Agent检查点问题ID无效")
			}
			if _, exists := questionIDs[questionID]; exists {
				return errors.New("Agent检查点问题ID重复")
			}
			questionIDs[questionID] = struct{}{}
		}
		if turn.FileEffectUnknown && turn.FileEffectCallID == "" {
			return errors.New("Agent检查点文件结果状态无效")
		}
	}
	if checkpoint.TurnSequence < maxTurnSequence {
		return errors.New("Agent检查点轮次序列无效")
	}
	if checkpoint.CurrentTurnID != "" {
		if _, exists := turns[checkpoint.CurrentTurnID]; !exists {
			return errors.New("Agent检查点当前轮次无效")
		}
	}
	for index, message := range checkpoint.Messages {
		if err := validateCheckpointMessage(message); err != nil {
			return err
		}
		if err := validateModelMessage(message); err != nil {
			return fmt.Errorf("Agent检查点消息无效: %w", err)
		}
		turnID := checkpoint.MessageTurnIDs[index]
		if turnID == "" {
			return errors.New("Agent检查点消息缺少轮次")
		}
		if _, exists := turns[turnID]; !exists {
			return errors.New("Agent检查点消息轮次无效")
		}
	}
	if err := validateCheckpointToolProtocol(checkpoint); err != nil {
		return err
	}
	if err := validateCheckpointMappings(checkpoint); err != nil {
		return err
	}
	if err := validateCheckpointContext(checkpoint.Context); err != nil {
		return err
	}
	contextSources := make(map[string]struct{}, len(checkpoint.Context.Sources))
	for _, source := range checkpoint.Context.Sources {
		contextSources[source.ID] = struct{}{}
	}
	for _, turn := range checkpoint.Turns {
		seenSources := make(map[string]struct{}, len(turn.SourceIDs))
		for _, sourceID := range turn.SourceIDs {
			if _, exists := contextSources[sourceID]; !exists {
				return errors.New("Agent检查点轮次来源无效")
			}
			if _, exists := seenSources[sourceID]; exists {
				return errors.New("Agent检查点轮次来源重复")
			}
			seenSources[sourceID] = struct{}{}
		}
	}
	data, err := json.Marshal(checkpoint)
	if err != nil || len(data) > MaxSessionCheckpointBytes {
		return errors.New("Agent检查点超过大小上限")
	}
	return nil
}

func validateCheckpointToolProtocol(checkpoint SessionCheckpoint) error {
	calls := make(map[string]string)
	results := make(map[string]struct{})
	unresolved := make(map[string]string)
	for index, message := range checkpoint.Messages {
		turnID := checkpoint.MessageTurnIDs[index]
		switch message.Role {
		case "assistant":
			if len(unresolved) != 0 {
				return errors.New("Agent检查点工具调用组不完整")
			}
			for _, call := range message.ToolCalls {
				if call.Function.Arguments != `{}` {
					return errors.New("Agent检查点包含原始工具参数")
				}
				if _, duplicate := calls[call.ID]; duplicate {
					return errors.New("Agent检查点工具调用ID重复")
				}
				calls[call.ID] = turnID
				unresolved[call.ID] = turnID
			}
		case "tool":
			callTurn, exists := unresolved[message.ToolCallID]
			if !exists || callTurn != turnID {
				return errors.New("Agent检查点工具结果没有匹配调用")
			}
			delete(unresolved, message.ToolCallID)
			results[message.ToolCallID] = struct{}{}
		case "user":
			if len(unresolved) != 0 {
				return errors.New("Agent检查点工具调用组不完整")
			}
		default:
			return errors.New("Agent检查点禁止持久化系统消息")
		}
	}
	if len(unresolved) != 0 {
		return errors.New("Agent检查点工具调用组不完整")
	}
	if len(checkpoint.ToolHistory) != len(results) {
		return errors.New("Agent检查点工具历史不完整")
	}
	for _, current := range checkpoint.ToolHistory {
		if _, exists := results[current.Key]; !exists {
			return errors.New("Agent检查点工具历史引用无效")
		}
	}
	for _, current := range checkpoint.ToolReferences {
		if _, exists := results[current.Key]; !exists {
			return errors.New("Agent检查点服务引用调用无效")
		}
	}
	for _, current := range checkpoint.WorkspaceReferences {
		if _, exists := results[current.Key]; !exists {
			return errors.New("Agent检查点工作区引用调用无效")
		}
	}
	for _, turn := range checkpoint.Turns {
		if turn.FileEffectCallID != "" {
			if callTurn, exists := calls[turn.FileEffectCallID]; !exists || callTurn != turn.ID {
				return errors.New("Agent检查点文件结果调用无效")
			}
		}
	}
	return nil
}

func validateCheckpointMappings(checkpoint SessionCheckpoint) error {
	seen := make(map[string]struct{})
	for _, current := range checkpoint.ToolHistory {
		if !validCheckpointMapKey(current.Key) || !safeCheckpointText(current.Value, maxToolOutputBytes, false) {
			return errors.New("Agent检查点工具历史无效")
		}
		if _, exists := seen[current.Key]; exists {
			return errors.New("Agent检查点工具历史重复")
		}
		seen[current.Key] = struct{}{}
	}
	seen = make(map[string]struct{})
	for _, current := range checkpoint.ToolReferences {
		if !validCheckpointMapKey(current.Key) || !validCheckpointServerReference(current.Value) {
			return errors.New("Agent检查点服务引用无效")
		}
		if _, exists := seen[current.Key]; exists {
			return errors.New("Agent检查点服务引用重复")
		}
		seen[current.Key] = struct{}{}
	}
	seen = make(map[string]struct{})
	for _, current := range checkpoint.WorkspaceReferences {
		if !validCheckpointMapKey(current.Key) || !validCheckpointWorkspaceReference(current.Value) {
			return errors.New("Agent检查点工作区引用无效")
		}
		if _, exists := seen[current.Key]; exists {
			return errors.New("Agent检查点工作区引用重复")
		}
		seen[current.Key] = struct{}{}
	}
	return nil
}

func validateCheckpointContext(value CheckpointContext) error {
	if value.Mode != ContextCompactionAuto && value.Mode != ContextCompactionRecentOnly && value.Mode != ContextCompactionOff {
		return errors.New("Agent检查点上下文模式无效")
	}
	if len(value.Sources) > maxCheckpointSources || len(value.Observations) > maxCheckpointObservations || len(value.Reflections) > maxCheckpointReflections {
		return errors.New("Agent检查点上下文条目超过上限")
	}
	if value.CoverageIndex < -1 || value.CoverageIndex >= len(value.Sources) || value.SuccessfulObserverRuns < 0 || value.LastReflectedRun < 0 || value.LastReflectedRun > value.SuccessfulObserverRuns {
		return errors.New("Agent检查点上下文游标无效")
	}
	if len(value.AllocatedIDs) > 100000 {
		return errors.New("Agent检查点已分配ID过多")
	}
	allocated := make(map[string]struct{}, len(value.AllocatedIDs))
	previousID := ""
	for _, id := range value.AllocatedIDs {
		if !validAnyOpaqueID(id) || previousID != "" && id <= previousID {
			return errors.New("Agent检查点已分配ID无效")
		}
		allocated[id] = struct{}{}
		previousID = id
	}
	sources := make(map[string]struct{}, len(value.Sources))
	for _, source := range value.Sources {
		if !validOpaqueID(source.ID, "src_") || !validCheckpointTurnID(source.TurnID) || source.CreatedAt.IsZero() || !safeCheckpointText(source.RecallText, maxContextSourceRecallBytes, false) ||
			!validCheckpointSourceKind(source.Kind) || !validCheckpointRetention(source.Retention) || !validCheckpointAuthority(source.Authority) || !validCheckpointFreshness(source.Freshness) || !validCheckpointHash(source.ContentHash) {
			return errors.New("Agent检查点上下文来源无效")
		}
		if source.SourceAvailable != (source.RecallText != "" && source.Freshness != FreshnessInvalidated) {
			return errors.New("Agent检查点上下文来源可用性无效")
		}
		if !source.HasModelMessage && !emptyCheckpointMessage(source.ModelMessage) {
			return errors.New("Agent检查点上下文来源消息状态无效")
		}
		// Warm evidence intentionally outlives raw sessionTurn records after
		// compaction, so a source TurnID need not still exist in turns.
		if _, exists := allocated[source.ID]; !exists {
			return errors.New("Agent检查点上下文来源ID未分配")
		}
		if _, exists := sources[source.ID]; exists {
			return errors.New("Agent检查点上下文来源重复")
		}
		sources[source.ID] = struct{}{}
		if source.HasModelMessage {
			if !validCheckpointSourceMessage(source) {
				return errors.New("Agent检查点上下文来源消息角色无效")
			}
			if err := validateModelMessage(source.ModelMessage); err != nil {
				return errors.New("Agent检查点上下文来源消息无效")
			}
		}
	}
	if value.CoverageIndex == -1 && value.CoverageWatermark != "" {
		return errors.New("Agent检查点覆盖水位无效")
	}
	if value.CoverageIndex >= 0 && value.CoverageWatermark != value.Sources[value.CoverageIndex].ID {
		return errors.New("Agent检查点覆盖水位无效")
	}
	observations := make(map[string]struct{}, len(value.Observations))
	for _, observation := range value.Observations {
		if !validOpaqueID(observation.ID, "obs_") || observation.CreatedAt.IsZero() || !safeCheckpointText(observation.Content, maxContextSourceRecallBytes, true) ||
			!validCheckpointRelevance(observation.Relevance) || !validCheckpointObservationKind(observation.Kind) || !validCheckpointAuthority(observation.Authority) || !validCheckpointFreshness(observation.Freshness) {
			return errors.New("Agent检查点观察无效")
		}
		if _, exists := allocated[observation.ID]; !exists {
			return errors.New("Agent检查点观察ID未分配")
		}
		if _, exists := observations[observation.ID]; exists {
			return errors.New("Agent检查点观察重复")
		}
		observations[observation.ID] = struct{}{}
		for _, sourceID := range observation.SourceEntryIDs {
			if _, exists := sources[sourceID]; !exists {
				return errors.New("Agent检查点观察来源无效")
			}
		}
	}
	reflections := make(map[string]struct{}, len(value.Reflections))
	for _, reflection := range value.Reflections {
		if !validOpaqueID(reflection.ID, "ref_") || reflection.CreatedAt.IsZero() || !safeCheckpointText(reflection.Content, maxContextSourceRecallBytes, true) ||
			!validCheckpointReflectionKind(reflection.Kind) || !validCheckpointAuthority(reflection.Authority) || !validCheckpointFreshness(reflection.Freshness) {
			return errors.New("Agent检查点反思无效")
		}
		if _, exists := allocated[reflection.ID]; !exists {
			return errors.New("Agent检查点反思ID未分配")
		}
		if _, exists := reflections[reflection.ID]; exists {
			return errors.New("Agent检查点反思重复")
		}
		reflections[reflection.ID] = struct{}{}
		for _, edge := range reflection.Support {
			if _, exists := observations[edge.ObservationID]; !exists || edge.Fidelity != CoveragePartial && edge.Fidelity != CoverageExact {
				return errors.New("Agent检查点覆盖边无效")
			}
		}
	}
	for _, current := range value.Supersessions {
		if _, exists := observations[current.OlderObservationID]; !exists || !safeCheckpointText(current.Reason, 256, true) {
			return errors.New("Agent检查点替代关系无效")
		}
		if _, exists := observations[current.NewerObservationID]; !exists {
			return errors.New("Agent检查点替代关系无效")
		}
	}
	seenTombstones := make(map[string]struct{})
	for _, current := range value.Tombstones {
		if _, exists := observations[current.ObservationID]; !exists || current.DroppedAt.IsZero() || !validCheckpointDropReason(current.Reason) {
			return errors.New("Agent检查点墓碑无效")
		}
		if _, exists := seenTombstones[current.ObservationID]; exists {
			return errors.New("Agent检查点墓碑重复")
		}
		seenTombstones[current.ObservationID] = struct{}{}
	}
	return nil
}

func exportCheckpointContextLocked(runtime *ContextRuntime) CheckpointContext {
	result := CheckpointContext{
		Mode: runtime.mode, CoverageWatermark: runtime.ledger.CoverageWatermark,
		CoverageIndex: runtime.ledger.CoverageIndex, SuccessfulObserverRuns: runtime.ledger.SuccessfulObserverRuns,
		LastReflectedRun: runtime.lastReflectedRun, AllocatedIDs: sortedStringSet(runtime.usedIDs),
	}
	result.Sources = make([]CheckpointSource, 0, len(runtime.ledger.SourceOrder))
	for _, id := range runtime.ledger.SourceOrder {
		current := runtime.ledger.Sources[id]
		result.Sources = append(result.Sources, CheckpointSource{
			ID: current.ID, TurnID: current.TurnID, Kind: current.Kind, CreatedAt: current.CreatedAt,
			ModelMessage: cloneModelMessage(current.ModelMessage), HasModelMessage: current.HasModelMessage,
			RecallText: current.RecallText, ContentHash: current.ContentHash, SourceAvailable: current.SourceAvailable,
			Retention: current.Retention, Authority: current.Authority,
			Freshness: current.Freshness, ServerReference: cloneServerReference(current.ServerReference),
			WorkspaceReference: cloneWorkspaceReference(current.WorkspaceReference),
		})
	}
	result.Observations = make([]CheckpointObservation, 0, len(runtime.ledger.ObservationOrder))
	for _, id := range runtime.ledger.ObservationOrder {
		current := runtime.ledger.Observations[id]
		result.Observations = append(result.Observations, CheckpointObservation{
			ID: current.ID, Content: current.Content, CreatedAt: current.CreatedAt, Relevance: current.Relevance,
			Kind: current.Kind, SourceEntryIDs: append([]string(nil), current.SourceEntryIDs...),
			Authority: current.Authority, Freshness: current.Freshness,
		})
	}
	result.Reflections = make([]CheckpointReflection, 0, len(runtime.ledger.ReflectionOrder))
	for _, id := range runtime.ledger.ReflectionOrder {
		current := runtime.ledger.Reflections[id]
		result.Reflections = append(result.Reflections, CheckpointReflection{
			ID: current.ID, Content: current.Content, Kind: current.Kind, Support: append([]CoverageEdge(nil), current.Support...),
			Authority: current.Authority, Freshness: current.Freshness, CreatedAt: current.CreatedAt,
		})
	}
	result.Supersessions = append([]Supersession(nil), runtime.ledger.Supersessions...)
	keys := make([]string, 0, len(runtime.ledger.Tombstones))
	for key := range runtime.ledger.Tombstones {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result.Tombstones = append(result.Tombstones, runtime.ledger.Tombstones[key])
	}
	return result
}

func exportStringMap(values map[string]string) []CheckpointStringEntry {
	keys := sortedCheckpointKeys(values)
	result := make([]CheckpointStringEntry, 0, len(keys))
	for _, key := range keys {
		result = append(result, CheckpointStringEntry{Key: key, Value: values[key]})
	}
	return result
}
func exportServerMap(values map[string]*ServerReference) []CheckpointServerEntry {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]CheckpointServerEntry, 0, len(keys))
	for _, key := range keys {
		if values[key] != nil {
			result = append(result, CheckpointServerEntry{Key: key, Value: *values[key]})
		}
	}
	return result
}
func exportWorkspaceMap(values map[string]*WorkspaceReference) []CheckpointWorkspaceEntry {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]CheckpointWorkspaceEntry, 0, len(keys))
	for _, key := range keys {
		if values[key] != nil {
			result = append(result, CheckpointWorkspaceEntry{Key: key, Value: *values[key]})
		}
	}
	return result
}
func sortedCheckpointKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func validateCheckpointMessage(message modelclient.Message) error {
	if !safeCheckpointText(message.Content, maxAssistantTextBytes, false) ||
		(message.ToolCallID != "" && !safeCheckpointString(message.ToolCallID, 512, true)) {
		return errors.New("Agent检查点消息文本无效")
	}
	for _, call := range message.ToolCalls {
		if !safeCheckpointString(call.ID, 512, true) || !safeCheckpointString(call.Type, 64, true) ||
			!safeCheckpointString(call.Function.Name, 256, true) || !safeCheckpointText(call.Function.Arguments, maxToolCallArgumentsBytes, true) {
			return errors.New("Agent检查点工具调用文本无效")
		}
	}
	switch message.Role {
	case "user":
		if message.ToolCallID != "" || len(message.ToolCalls) != 0 {
			return errors.New("Agent检查点消息角色结构无效")
		}
	case "assistant":
		if message.ToolCallID != "" {
			return errors.New("Agent检查点助手消息结构无效")
		}
	case "tool":
		if message.ToolCallID == "" || len(message.ToolCalls) != 0 {
			return errors.New("Agent检查点工具消息结构无效")
		}
	default:
		return errors.New("Agent检查点消息角色无效")
	}
	return nil
}
func emptyCheckpointMessage(message modelclient.Message) bool {
	return message.Role == "" && message.Content == "" && message.ToolCallID == "" && len(message.ToolCalls) == 0
}
func validCheckpointSourceMessage(source CheckpointSource) bool {
	message := source.ModelMessage
	switch source.Kind {
	case SourceUser:
		return message.Role == "user" && message.ToolCallID == "" && len(message.ToolCalls) == 0
	case SourceAssistant:
		return message.Role == "assistant" && message.ToolCallID == "" && len(message.ToolCalls) == 0
	case SourceTool:
		return message.Role == "tool" && message.ToolCallID != "" && len(message.ToolCalls) == 0
	default:
		return false
	}
}

func validCheckpointServerReference(value ServerReference) bool {
	return safeCheckpointString(value.Tool, 256, true) && safeCheckpointString(value.Entity, 256, true) &&
		safeCheckpointString(value.EntityID, 2048, false) && safeCheckpointString(value.Revision, 2048, false) &&
		value.Version >= 0 && value.Generation >= 0 && value.LearnerGeneration >= 0 && value.MemoryGeneration >= 0
}
func validCheckpointWorkspaceReference(value WorkspaceReference) bool {
	return safeCheckpointString(value.Path, 8192, true) && safeCheckpointString(value.Kind, 256, true) &&
		(value.ContentHash == "" || strings.HasPrefix(value.ContentHash, "sha256:") && len(value.ContentHash) == len("sha256:")+64)
}
func validCheckpointSourceKind(value SourceKind) bool {
	return value == SourceUser || value == SourceAssistant || value == SourceTool
}
func validCheckpointRetention(value RetentionClass) bool {
	return value == RetentionHot || value == RetentionWarm || value == RetentionMetadata
}
func validCheckpointAuthority(value AuthorityClass) bool {
	return value == AuthoritySessionStatement || value == AuthorityServerSnapshot || value == AuthorityServerReference || value == AuthorityWorkspaceSnapshot
}
func validCheckpointFreshness(value FreshnessClass) bool {
	return value == FreshnessSessionCurrent || value == FreshnessHistorical || value == FreshnessInvalidated || value == FreshnessWorkspaceObserved || value == FreshnessWorkspaceSuperseded
}
func validCheckpointRelevance(value Relevance) bool {
	return value == RelevanceLow || value == RelevanceMedium || value == RelevanceHigh || value == RelevanceCritical
}
func validCheckpointObservationKind(value ObservationKind) bool {
	switch value {
	case ObservationUserIntent, ObservationUserConstraint, ObservationCorrection, ObservationDecision, ObservationCompletion,
		ObservationOpenQuestion, ObservationToolSnapshot, ObservationFailure, ObservationPreferenceFlow:
		return true
	default:
		return false
	}
}
func validCheckpointReflectionKind(value ReflectionKind) bool {
	switch value {
	case ReflectionUserIntent, ReflectionUserConstraint, ReflectionCorrection, ReflectionDecision, ReflectionCompletion,
		ReflectionOpenBlocker, ReflectionServerState, ReflectionWorkspaceState, ReflectionPreferenceFlow:
		return true
	default:
		return false
	}
}
func validCheckpointDropReason(value DropReason) bool {
	switch value {
	case DropSuperseded, DropExactCoverage, DropDuplicate, DropLowValue, DropNewerSnapshot:
		return true
	default:
		return false
	}
}
func validCheckpointHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}
func validCheckpointTurnID(value string) bool {
	turnNumber, err := strconv.Atoi(strings.TrimPrefix(value, "turn-"))
	return err == nil && turnNumber > 0 && value == fmt.Sprintf("turn-%d", turnNumber)
}

func safeCheckpointString(value string, limit int, required bool) bool {
	if !safeCheckpointText(value, limit, required) {
		return false
	}
	return !strings.ContainsAny(value, "\n\t")
}

func safeCheckpointText(value string, limit int, required bool) bool {
	if required && value == "" {
		return false
	}
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if current == '\n' || current == '\t' {
			continue
		}
		if unicode.IsControl(current) || isBidiControl(current) {
			return false
		}
	}
	return true
}

func isBidiControl(value rune) bool {
	return value == '\u061c' || value == '\u200e' || value == '\u200f' ||
		value >= '\u202a' && value <= '\u202e' || value >= '\u2066' && value <= '\u2069'
}

func checkpointMessage(message modelclient.Message) modelclient.Message {
	message = cloneModelMessage(message)
	if message.Role == "assistant" {
		for index := range message.ToolCalls {
			message.ToolCalls[index].Function.Arguments = `{}`
		}
	}
	return message
}

func validAnyOpaqueID(value string) bool {
	return validOpaqueID(value, "src_") || validOpaqueID(value, "obs_") || validOpaqueID(value, "ref_")
}

func (r *ContextRuntime) recomputeDerivedCheckpointStateLocked() {
	r.ledger.SourceIndex = make(map[string]int, len(r.ledger.SourceOrder))
	r.hotTurns = make(map[string]struct{})
	for index, id := range r.ledger.SourceOrder {
		entry := r.ledger.Sources[id]
		entry.TokenEstimate = r.estimator.EstimateText(entry.RecallText)
		r.ledger.Sources[id] = entry
		r.ledger.SourceIndex[id] = index
		if entry.Retention == RetentionHot {
			r.hotTurns[entry.TurnID] = struct{}{}
		}
	}
	for _, id := range r.ledger.ObservationOrder {
		entry := r.ledger.Observations[id]
		entry.TokenEstimate = r.estimator.EstimateText(entry.Content)
		r.ledger.Observations[id] = entry
	}
	for _, id := range r.ledger.ReflectionOrder {
		entry := r.ledger.Reflections[id]
		entry.TokenEstimate = r.estimator.EstimateText(entry.Content)
		r.ledger.Reflections[id] = entry
	}
	r.observerFailures = 0
	r.observerBlockedUntil = 0
	r.reflectorBlockedUntil = 0
	r.softPressure = false
	r.status = ContextStatus{Estimated: true, ContextWindow: r.contextWindow, Mode: r.mode, Phase: "idle"}
	r.refreshMemoryCountsLocked()
}

func sortedStringSet(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func freshSystemMessages(messages []modelclient.Message, turnIDs []string) bool {
	if len(messages) == 0 || len(messages) != len(turnIDs) {
		return false
	}
	for index, message := range messages {
		if message.Role != "system" || turnIDs[index] != "" {
			return false
		}
	}
	return true
}
func validCheckpointMapKey(value string) bool {
	return strings.TrimSpace(value) == value && value != "" && len(value) <= 256 && utf8.ValidString(value)
}
