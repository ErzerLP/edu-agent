package agentloop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

const (
	maxHistoryToolOutputBytes       = 2 << 10
	maxRecallToolOutputBytes        = 8 << 10
	maxProjectionNormalizationBytes = 1 << 20
)

func projectToolResult(tool string, value any) ToolResultProjection {
	return ToolResultProjection{
		Live:            boundedProjectionJSON(tool, value, maxToolOutputBytes, "single_tool_limit"),
		History:         boundedProjectionJSON(tool, historyValue(tool, value), maxHistoryToolOutputBytes, "history_projection_limit"),
		Recall:          boundedProjectionJSON(tool, value, maxRecallToolOutputBytes, "recall_projection_limit"),
		ServerReference: serverReferenceForToolResult(tool, value),
	}
}

func projectWorkspaceToolResult(tool string, result workspace.Result) ToolResultProjection {
	value := workspaceModelVisibleValue(result)
	projection := projectToolResult(tool, value)
	projection.ServerReference = nil
	projection.WorkspaceReference = cloneWorkspaceReference(result.Reference)
	return projection
}

func workspaceModelVisibleValue(result workspace.Result) any {
	object := normalizedProjectionObject(result.Value)
	if object == nil {
		return result.Value
	}
	if _, failed := object["error"]; failed {
		if _, ok := object["message"]; !ok && result.Summary != "" {
			object["message"] = truncateUTF8(result.Summary, 240)
		}
	}
	if result.Reference != nil {
		if _, ok := object["path"]; !ok && result.Reference.Path != "" {
			object["path"] = result.Reference.Path
		}
		if result.Reference.InvalidateObserved && result.Reference.ContentHash != "" {
			if _, ok := object["expected_hash"]; !ok {
				object["expected_hash"] = result.Reference.ContentHash
			}
		}
	}
	if result.Publication != "" {
		if _, ok := object["publication_outcome"]; !ok {
			object["publication_outcome"] = string(result.Publication)
		}
	}
	return object
}

func serverReferenceForToolResult(tool string, value any) *ServerReference {
	reference := &ServerReference{Tool: tool, Entity: tool}
	object := normalizedProjectionObject(value)
	if object == nil {
		return reference
	}
	reference.Generation = projectionGeneration(object)
	switch tool {
	case "search_knowledge":
		reference.Entity = "knowledge_revision"
		reference.EntityID, _ = object["knowledge_revision_id"].(string)
		reference.Revision = reference.EntityID
	case "get_learning_progress":
		reference.Entity = "learning_session"
		if view, ok := object["session"].(map[string]any); ok {
			if session, nested := view["session"].(map[string]any); nested {
				reference.EntityID = firstProjectionString(session, "session_id", "id")
				reference.Version = projectionInt64(firstProjectionValue(session, "aggregate_version", "version"))
			} else {
				reference.EntityID = firstProjectionString(view, "session_id", "id")
				reference.Version = projectionInt64(firstProjectionValue(view, "aggregate_version", "version"))
			}
			if reference.Generation == 0 {
				reference.Generation = projectionGeneration(view)
			}
		}
	case "get_learning_route":
		reference.Entity = "route_revision"
		reference.EntityID = firstProjectionString(object, "route_revision_id", "goal_revision_id")
		reference.Revision = reference.EntityID
		reference.Version = projectionInt64(object["revision"])
	case "get_due_reviews":
		reference.Entity = "due_reviews"
		reference.EntityID = "current_user"
		reference.Revision = firstProjectionString(object, "due_before")
	case "list_long_term_preferences":
		reference.Entity = "long_term_preferences"
		reference.EntityID = "current_user"
		if generation, ok := object["read_generation"].(map[string]any); ok {
			reference.LearnerGeneration = projectionInt64(generation["learner_generation"])
			reference.MemoryGeneration = projectionInt64(generation["memory_generation"])
			if reference.MemoryGeneration > 0 {
				reference.Generation = reference.MemoryGeneration
			}
		}
		if items, ok := object["items"].([]any); ok {
			for _, item := range items {
				if current, ok := item.(map[string]any); ok {
					reference.Version = max(reference.Version, projectionInt64(current["revision"]))
				}
			}
		}
	case "remember_preference":
		reference.Entity = "memory_candidate"
		reference.EntityID = firstProjectionString(object, "candidate_id")
		reference.Version = projectionInt64(object["revision"])
	}
	return reference
}

func serverReferenceNewer(current, previous *ServerReference) bool {
	if current == nil || previous == nil || current.Identity() == "" || current.Identity() != previous.Identity() {
		return false
	}
	if current.LearnerGeneration > 0 || current.MemoryGeneration > 0 || previous.LearnerGeneration > 0 || previous.MemoryGeneration > 0 {
		return current.LearnerGeneration > previous.LearnerGeneration || current.MemoryGeneration > previous.MemoryGeneration
	}
	return current.Generation > previous.Generation
}

func serverReferenceStale(current, previous *ServerReference) bool {
	if current == nil || previous == nil || current.Identity() == "" || current.Identity() != previous.Identity() {
		return false
	}
	if current.LearnerGeneration > 0 || current.MemoryGeneration > 0 || previous.LearnerGeneration > 0 || previous.MemoryGeneration > 0 {
		return current.LearnerGeneration < previous.LearnerGeneration || current.MemoryGeneration < previous.MemoryGeneration
	}
	return current.Generation < previous.Generation
}

func toolResultInvalidatesIdentity(tool string, value any) bool {
	object := normalizedProjectionObject(value)
	if object == nil {
		return false
	}
	if code, _ := object["code"].(string); code == "privacy_clear_in_progress" {
		return true
	}
	if tool != "list_long_term_preferences" {
		return false
	}
	if invalidated, _ := object["privacy_invalidated"].(bool); invalidated {
		return true
	}
	if codes, ok := object["reason_codes"].([]any); ok {
		for _, raw := range codes {
			code, _ := raw.(string)
			if code == "content_redacted" || code == "privacy_clear_in_progress" {
				return true
			}
		}
	}
	if items, ok := object["items"].([]any); ok {
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if status, _ := item["content_status"].(string); status == "redacted" {
				return true
			}
		}
	}
	return false
}

func normalizedProjectionObject(value any) map[string]any {
	var data bytes.Buffer
	writer := &projectionLimitWriter{buffer: &data, remaining: maxProjectionNormalizationBytes}
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(value); err != nil {
		return nil
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data.Bytes()))
	decoder.UseNumber()
	if decoder.Decode(&object) != nil {
		return nil
	}
	return object
}

type projectionLimitWriter struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *projectionLimitWriter) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		return 0, errors.New("projection normalization limit exceeded")
	}
	written, err := w.buffer.Write(data)
	w.remaining -= written
	return written, err
}

func firstProjectionValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value
		}
	}
	return nil
}

func projectionGeneration(object map[string]any) int64 {
	if generation := projectionInt64(object["generation"]); generation > 0 {
		return generation
	}
	for _, key := range []string{"metadata", "read_generation"} {
		if nested, ok := object[key].(map[string]any); ok {
			for _, generationKey := range []string{"generation", "learner_generation", "memory_generation"} {
				if generation := projectionInt64(nested[generationKey]); generation > 0 {
					return generation
				}
			}
		}
	}
	return 0
}

func firstProjectionString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func projectionInt64(value any) int64 {
	switch current := value.(type) {
	case int:
		if current >= 0 {
			return int64(current)
		}
	case int8:
		if current >= 0 {
			return int64(current)
		}
	case int16:
		if current >= 0 {
			return int64(current)
		}
	case int32:
		if current >= 0 {
			return int64(current)
		}
	case int64:
		if current >= 0 {
			return current
		}
	case uint:
		if uint64(current) <= math.MaxInt64 {
			return int64(current)
		}
	case uint8:
		return int64(current)
	case uint16:
		return int64(current)
	case uint32:
		return int64(current)
	case uint64:
		if current <= math.MaxInt64 {
			return int64(current)
		}
	case float32:
		return projectionFloat64(float64(current))
	case float64:
		return projectionFloat64(current)
	case json.Number:
		return projectionDecimalString(string(current))
	case string:
		return projectionDecimalString(current)
	}
	return 0
}

func projectionFloat64(value float64) int64 {
	if value < 0 || value > math.MaxInt64 || math.Trunc(value) != value {
		return 0
	}
	return int64(value)
}

func projectionDecimalString(value string) int64 {
	if value == "" {
		return 0
	}
	for _, current := range value {
		if current < '0' || current > '9' {
			return 0
		}
	}
	result, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return result
}

func historyValue(tool string, value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return compactProjectionValue(value, 0, 6, 256)
	}
	if _, failed := object["error"]; failed {
		return preserveOutcomeFields(tool, object, false, "")
	}
	switch tool {
	case "search_knowledge":
		result := preserveFields(object, "knowledge_revision_id", "degraded", "truncated")
		if hits, ok := object["hits"].([]map[string]any); ok {
			compactHits := make([]map[string]any, 0, min(len(hits), 8))
			for _, hit := range hits[:min(len(hits), 8)] {
				entry := preserveFields(hit, "path", "node_revision_id")
				if text, ok := hit["text"].(string); ok {
					entry["excerpt"] = truncateUTF8(text, 256)
				}
				compactHits = append(compactHits, entry)
			}
			result["hits"] = compactHits
		}
		return result
	case "remember_preference":
		return preserveOutcomeFields(tool, object, false, "")
	default:
		return compactProjectionValue(object, 0, 5, 256)
	}
}

func boundedProjectionJSON(tool string, value any, limit int, reason string) string {
	if data, err := json.Marshal(value); err == nil && len(data) <= limit {
		return string(data)
	}
	for _, candidate := range []any{
		compactProjectionValue(value, 0, 5, 256),
		compactProjectionValue(value, 0, 3, 96),
	} {
		if data, err := json.Marshal(candidate); err == nil && len(data) <= limit {
			return string(data)
		}
	}
	object, _ := value.(map[string]any)
	fallback := preserveOutcomeFields(tool, object, true, reason)
	data, err := json.Marshal(fallback)
	if err != nil || len(data) > limit {
		return `{"truncated":true,"degraded":true,"reason":"tool_result_encoding_failed"}`
	}
	return string(data)
}

func compactProjectionValue(value any, depth, maxDepth, maxString int) any {
	if depth >= maxDepth {
		return map[string]any{"truncated": true, "degraded": true, "reason": "projection_depth_limit"}
	}
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, min(len(current), 24)+2)
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for index, key := range keys {
			if index >= 24 {
				result["truncated"] = true
				result["degraded"] = true
				break
			}
			result[key] = compactProjectionValue(current[key], depth+1, maxDepth, maxString)
		}
		return result
	case []any:
		limit := min(len(current), 8)
		result := make([]any, 0, limit)
		for _, item := range current[:limit] {
			result = append(result, compactProjectionValue(item, depth+1, maxDepth, maxString))
		}
		if len(current) > limit {
			return map[string]any{"items": result, "returned": limit, "total": len(current), "truncated": true, "degraded": true}
		}
		return result
	case string:
		if len(current) > maxString {
			return truncateUTF8(current, maxString) + "…"
		}
		return current
	default:
		// API DTOs and typed slices are normalized through JSON so projection
		// never relies on unsafe textual truncation of arbitrary JSON.
		data, err := json.Marshal(current)
		if err != nil {
			return fmt.Sprint(current)
		}
		var normalized any
		if json.Unmarshal(data, &normalized) == nil {
			switch normalized.(type) {
			case map[string]any, []any:
				return compactProjectionValue(normalized, depth, maxDepth, maxString)
			default:
				return normalized
			}
		}
		return current
	}
}

func preserveOutcomeFields(tool string, object map[string]any, truncated bool, reason string) map[string]any {
	result := map[string]any{"tool": tool}
	for _, key := range []string{"error", "code", "status", "submitted", "saved", "reason", "candidate_id", "replayed", "active", "degraded", "truncated"} {
		if value, ok := object[key]; ok {
			result[key] = value
		}
	}
	if truncated {
		result["truncated"] = true
		result["degraded"] = true
		result["projection_reason"] = reason
	}
	return result
}

func preserveFields(object map[string]any, keys ...string) map[string]any {
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := object[key]; ok {
			result[key] = value
		}
	}
	return result
}

func currentTurnBudgetProjection(tool string, value any) string {
	object, _ := value.(map[string]any)
	result := preserveOutcomeFields(tool, object, true, "current_turn_tool_result_budget")
	if _, ok := result["error"]; !ok && len(result) <= 4 {
		result["summary"] = "工具已执行，但详细结果因当前轮次累计预算而省略"
	}
	data, err := json.Marshal(result)
	if err != nil {
		return `{"truncated":true,"degraded":true,"reason":"current_turn_tool_result_budget"}`
	}
	return strings.TrimSpace(string(data))
}

func workspaceBudgetProjectionCandidates(tool string, value any) []string {
	object := normalizedProjectionObject(value)
	if object == nil {
		return []string{currentTurnBudgetProjection(tool, value)}
	}
	payloadLimits := []int{384, 192, 96, 48, 16, 4}
	candidates := make([]string, 0, len(payloadLimits))
	for _, payloadLimit := range payloadLimits {
		projected := workspaceBudgetProjection(tool, object, payloadLimit)
		data, err := json.Marshal(projected)
		if err == nil {
			candidates = append(candidates, string(data))
		}
	}
	return candidates
}

func workspaceBudgetProjection(tool string, object map[string]any, payloadLimit int) map[string]any {
	result := map[string]any{"tool": tool}
	for _, key := range []string{
		"path", "operation", "error", "code", "message", "suggestion", "publication_outcome",
		"content_hash", "expected_hash", "complete", "truncated", "truncation_reason", "returned",
		"returned_lines", "next_offset", "next_byte_offset", "first_changed_line", "preview_kind",
		"preview_truncated", "scanned_files", "scanned_bytes",
	} {
		if current, ok := object[key]; ok {
			if text, isText := current.(string); isText && (key == "message" || key == "suggestion") {
				current = truncateUTF8(text, max(24, payloadLimit))
			}
			result[key] = current
		}
	}

	payloadKept := false
	switch tool {
	case workspace.ToolList:
		if entries, ok := object["entries"].([]any); ok && len(entries) > 0 {
			result["entries"] = []any{compactProjectionValue(entries[0], 0, 3, max(12, payloadLimit))}
			payloadKept = true
		}
	case workspace.ToolRead:
		if content, ok := object["content"].(string); ok && content != "" {
			result["content"] = truncateUTF8(content, payloadLimit)
			payloadKept = true
		}
	case workspace.ToolSearch:
		if matches, ok := object["matches"].([]any); ok && len(matches) > 0 {
			result["matches"] = []any{compactProjectionValue(matches[0], 0, 3, max(12, payloadLimit))}
			payloadKept = true
		}
	case workspace.ToolWrite, workspace.ToolEdit:
		for _, key := range []string{"preview", "diff"} {
			if preview, ok := object[key].(string); ok && preview != "" {
				result[key] = truncateUTF8(preview, payloadLimit)
				payloadKept = true
				break
			}
		}
	}
	if _, failed := object["error"]; !failed && !payloadKept {
		result["payload_summary"] = "结果为空；请依据状态字段继续"
	}
	if tool == workspace.ToolList || tool == workspace.ToolRead || tool == workspace.ToolSearch {
		if complete, _ := object["complete"].(bool); complete {
			result["source_complete"] = true
		}
		result["complete"] = false
		result["truncation_reason"] = "current_turn_budget"
		if _, ok := result["suggestion"]; !ok {
			switch tool {
			case workspace.ToolList:
				result["suggestion"] = "按偏移继续"
			case workspace.ToolRead:
				result["suggestion"] = "按偏移并携带哈希继续"
			case workspace.ToolSearch:
				result["suggestion"] = "缩小范围后重试"
			}
		}
	} else if payloadKept {
		result["preview_truncated"] = true
	}
	return result
}
