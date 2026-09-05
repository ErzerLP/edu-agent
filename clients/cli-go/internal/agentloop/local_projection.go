package agentloop

import (
	"encoding/base64"
	"encoding/json"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

// This is an execution-only value: raw bytes never enter the ledger or DTO.
// A projection may omit a body, but never advance its raw-byte cursor for it.
type localToolResult struct {
	Action, TaskID, Code string
	Snapshot             *localexec.Snapshot
	Stdin                *bool
	Input                *localexec.InputResult
	NotSaved             bool
	Pages                map[string]localexec.OutputPage
	Tasks                []localexec.Snapshot
	Offset               int64
	Total                int
}

func localSnapshotValue(snapshot localexec.Snapshot, minimal bool) map[string]any {
	value := map[string]any{
		"task_id": snapshot.TaskID, "state": snapshot.State,
		"controllable": snapshot.Controllable, "output_state": snapshot.OutputState,
	}
	if snapshot.Reason != "" {
		value["reason"] = snapshot.Reason
	}
	if snapshot.ExitCode != nil {
		value["exit_code"] = *snapshot.ExitCode
	}
	if snapshot.CleanupIncomplete {
		value["cleanup_incomplete"] = true
	}
	if !minimal {
		value["stdout_bytes"], value["stderr_bytes"] = snapshot.StdoutBytes, snapshot.StderrBytes
		value["stdout_retained"], value["stderr_retained"] = snapshot.StdoutRetained, snapshot.StderrRetained
	}
	return value
}

func localOutputEncoding(data []byte) (string, string) {
	safe := utf8.Valid(data)
	if safe {
		for _, r := range string(data) {
			if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) {
				safe = false
				break
			}
		}
	}
	if !safe {
		return base64.StdEncoding.EncodeToString(data), "base64"
	}
	return string(data), "utf8"
}

func localPageValue(page localexec.OutputPage, payloadLimit int, minimal bool) map[string]any {
	kept := min(len(page.Data), payloadLimit)
	data, encoding := localOutputEncoding(page.Data[:kept])
	next := page.Offset + int64(kept)
	// Preserve an explicit storage gap only when no available bytes were omitted.
	gap := len(page.Data) == 0 && page.NextOffset > page.Offset
	if gap {
		next = page.NextOffset
	}
	value := map[string]any{"offset": page.Offset, "next_offset": next, "more": next < page.Received}
	if !minimal {
		value["received"], value["retained"] = page.Received, page.Retained
		value["truncated"], value["incomplete"] = page.Truncated, page.Incomplete
	}
	if gap {
		value["gap"] = true
	}
	if page.Incomplete {
		value["incomplete"] = true
	}
	if page.Truncated {
		value["truncated"] = true
	}
	if kept < len(page.Data) {
		value["projection_omitted"] = true
	}
	if kept > 0 {
		value["data"], value["encoding"] = data, encoding
	}
	return value
}

func (result localToolResult) value(payloadLimit, itemLimit int, minimal, history bool) map[string]any {
	value := map[string]any{"action": result.Action, "availability": "memory_only"}
	if history {
		value["historical"] = true
	}
	if result.Snapshot != nil && result.Snapshot.TaskID != "" {
		for key, item := range localSnapshotValue(*result.Snapshot, minimal) {
			value[key] = item
		}
		if !history && !minimal && result.Snapshot.Shell != "" {
			shell, encoding := localOutputEncoding([]byte(result.Snapshot.Shell))
			value["shell"], value["shell_encoding"] = shell, encoding
		}
	} else if result.TaskID != "" {
		value["task_id"], value["state"], value["controllable"] = result.TaskID, "unknown", false
		value["availability"], value["replay"] = "unavailable", false
	}
	if result.Code != "" {
		value["error"] = result.Code
	}
	if result.NotSaved {
		value["saved"] = false
	}
	if result.Stdin != nil {
		value["stdin_mode"] = "eof"
		if *result.Stdin {
			value["stdin_mode"] = "pipe"
		}
	}
	if result.Input != nil {
		value["written"], value["input_outcome"] = result.Input.Written, result.Input.Outcome
	}
	for stream, page := range result.Pages {
		value[stream] = localPageValue(page, payloadLimit, minimal)
	}
	if result.Action == "list" && result.Code == "" {
		count := min(len(result.Tasks), itemLimit)
		tasks := make([]any, 0, count)
		for _, snapshot := range result.Tasks[:count] {
			tasks = append(tasks, localSnapshotValue(snapshot, minimal))
		}
		value["tasks"], value["offset"], value["next_offset"] = tasks, result.Offset, result.Offset+int64(count)
		value["total"], value["more"] = result.Total, result.Offset+int64(count) < int64(result.Total)
	}
	return value
}

func localResultJSON(value map[string]any) string {
	// Only controlled metadata, strings, integers and booleans are constructed.
	data, _ := json.Marshal(value)
	return string(data)
}

// One dedicated projection path covers byte limits and the per-turn token
// budget. No generic recursive string/array compactor can touch identities or
// pagination. The smallest honest locator may exceed a soft per-result share.
func (result localToolResult) project(maxBytes int, fits func(string) bool, history bool) string {
	accepts := func(value string) bool { return len(value) <= maxBytes && (fits == nil || fits(value)) }
	payload, items := 65536, len(result.Tasks)
	if history {
		payload = 0
	}
	for {
		candidate := localResultJSON(result.value(payload, items, false, history))
		if accepts(candidate) {
			return candidate
		}
		if items > 0 {
			items /= 2
			continue
		}
		if payload > 0 {
			payload /= 2
			continue
		}
		break
	}
	return localResultJSON(result.value(0, 0, true, history))
}

func (s *Session) appendLocalToolResult(callID string, result localToolResult) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	allowed := min(max(32, s.currentToolResultBudget/max(1, s.currentToolResultShares)-30), max(32, s.currentToolResultBudget-s.currentToolResultTokens-6))
	live := result.project(maxToolOutputBytes, func(text string) bool { return s.estimator.EstimateText(text) <= allowed }, false)
	history := result.project(maxHistoryToolOutputBytes, nil, true)
	// The ledger (including its ModelMessage and recall) gets metadata only,
	// not even the bounded live body. Raw output remains owned by localexec.
	sourceMessage := modelclient.Message{Role: "tool", ToolCallID: callID, Content: history}
	sourceID, err := s.contextRuntime.appendSource(sourceDraft{
		TurnID: s.currentTurnID, Kind: SourceTool, CreatedAt: s.options.Now().UTC(), ModelMessage: sourceMessage,
		RecallText: history, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	})
	// A source allocation failure must not erase an already performed operation.
	// Retain the protocol pair first; the caller will finish via factual fallback.
	s.toolHistory[callID] = history
	delete(s.toolReferences, callID)
	delete(s.workspaceReferences, callID)
	s.messages = append(s.messages, modelclient.Message{Role: "tool", ToolCallID: callID, Content: live})
	s.messageTurnIDs = append(s.messageTurnIDs, s.currentTurnID)
	s.currentToolResultTokens = min(s.currentToolResultBudget, s.currentToolResultTokens+s.estimator.EstimateText(live)+6)
	if sourceID != "" {
		if turn := s.turns[s.currentTurnID]; turn != nil {
			turn.SourceIDs = append(turn.SourceIDs, sourceID)
		}
	}
	return err
}

// Stable history contains no live output or raw arguments. Context sources are
// already metadata-only; this also covers all local calls left in a pending batch.
func (s *Session) normalizeLocalHistoryLocked(turnID string) {
	localCalls := make(map[string]struct{})
	for index := range s.messages {
		message := &s.messages[index]
		if s.messageTurnIDs[index] != turnID {
			continue
		}
		for callIndex := range message.ToolCalls {
			call := &message.ToolCalls[callIndex]
			if isLocalExecutionTool(call.Function.Name) {
				call.Function.Arguments = `{}`
				localCalls[call.ID] = struct{}{}
			}
		}
	}
	for index := range s.messages {
		message := &s.messages[index]
		if _, local := localCalls[message.ToolCallID]; message.Role == "tool" && local {
			message.Content = s.toolHistory[message.ToolCallID]
		}
	}
}
