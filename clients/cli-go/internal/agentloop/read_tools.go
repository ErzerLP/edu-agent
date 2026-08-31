package agentloop

import (
	"context"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func (s *Session) executeReadTool(ctx context.Context, call modelclient.ToolCall) (any, string) {
	if call.Function.Name == "list_long_term_preferences" {
		var args struct {
			Cursor string `json:"cursor"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || len(args.Cursor) > 4096 {
			return toolError("invalid_arguments"), "长期偏好参数无效"
		}
		return s.listLongTermPreferences(ctx, args.Cursor, call.ID)
	}
	return s.executeBaseReadTool(ctx, call)
}

func (s *Session) executeBaseReadTool(ctx context.Context, call modelclient.ToolCall) (any, string) {
	switch call.Function.Name {
	case "search_knowledge":
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return toolError("invalid_arguments"), "知识检索参数无效"
		}
		result, err := s.server.RetrieveKnowledge(ctx, api.KnowledgeRetrievalRequest{
			Query: strings.TrimSpace(args.Query), QueryContextSchemaVersion: api.QueryContextSchemaVersion,
			Context: map[string]any{"surface": "client_agent"},
			Limits:  &api.KnowledgeQueryLimits{MaxDepth: 4, CandidatesPerLayer: 8, MaxHits: 8, TotalCandidates: 32},
		})
		if err != nil {
			return toolFailure(err, "knowledge_unavailable"), "知识库检索失败"
		}
		hits := make([]map[string]any, 0, len(result.Hits))
		for _, hit := range result.Hits {
			hits = append(hits, map[string]any{
				"path":             hit.Path,
				"node_revision_id": hit.NodeRevisionID,
				"text":             hit.CanonicalSlice,
				"provenance":       hit.Provenance,
			})
		}
		return map[string]any{
			"knowledge_revision_id": result.KnowledgeRevisionID,
			"hits":                  hits,
			"degraded":              result.Degraded,
			"truncated":             result.Truncated,
		}, fmt.Sprintf("检索到 %d 条知识片段", len(hits))
	case "get_learning_progress":
		if err := requireEmptyArguments(call.Function.Arguments); err != nil {
			return toolError("invalid_arguments"), "学习进度参数无效"
		}
		result, err := s.server.CurrentSession(ctx)
		if err != nil {
			if isAPINotFound(err) {
				return map[string]any{"active": false, "reason": "no_current_session"}, "当前没有进行中的学习会话"
			}
			return toolFailure(err, "current_session_unavailable"), "当前学习进度不可用"
		}
		return map[string]any{"active": true, "session": result}, "已读取当前学习状态"
	case "get_learning_route":
		var args struct {
			Offset int `json:"offset"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || args.Offset < 0 {
			return toolError("invalid_arguments"), "学习路线参数无效"
		}
		view, err := s.server.CurrentSession(ctx)
		if err != nil {
			if isAPINotFound(err) {
				return map[string]any{"active": false, "reason": "no_current_session"}, "当前没有进行中的学习会话"
			}
			return toolFailure(err, "learning_route_unavailable"), "学习路线不可用"
		}
		if view.WorkItem == nil || view.WorkItem.RouteRevision == nil {
			return map[string]any{"active": false, "reason": "route_not_ready", "session_state": view.Session.State}, "当前学习会话尚未生成路线"
		}
		route := view.WorkItem.RouteRevision
		start := min(args.Offset, len(route.Steps))
		end := min(start+12, len(route.Steps))
		steps := make([]map[string]any, 0, end-start)
		for _, step := range route.Steps[start:end] {
			steps = append(steps, map[string]any{
				"ordinal":              step.Ordinal,
				"node_revision_id":     step.NodeRevisionID,
				"teaching_intent":      step.TeachingIntent,
				"completion_condition": step.CompletionCondition,
			})
		}
		value := map[string]any{
			"active":            true,
			"route_revision_id": route.RouteRevisionID,
			"goal_revision_id":  route.GoalRevisionID,
			"revision":          route.Revision,
			"generation":        view.Metadata.Generation,
			"offset":            start,
			"returned":          len(steps),
			"total_steps":       len(route.Steps),
			"steps":             steps,
			"has_more":          end < len(route.Steps),
		}
		if end < len(route.Steps) {
			value["next_offset"] = end
		}
		return value, fmt.Sprintf("已读取当前路线的 %d/%d 个步骤", len(steps), len(route.Steps))
	case "get_due_reviews":
		var args struct {
			Cursor string `json:"cursor"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || len(args.Cursor) > 4096 {
			return toolError("invalid_arguments"), "复习任务参数无效"
		}
		due := s.options.Now().UTC()
		result, err := s.server.Reviews(ctx, args.Cursor, 20, &due)
		if err != nil {
			return toolFailure(err, "reviews_unavailable"), "复习任务不可用"
		}
		value := map[string]any{
			"items":      result.Items,
			"returned":   len(result.Items),
			"due_before": due,
			"generation": result.Metadata.Generation,
			"has_more":   result.NextCursor != "",
		}
		if result.NextCursor != "" {
			value["next_cursor"] = result.NextCursor
		}
		return value, fmt.Sprintf("已读取 %d 项到期复习", len(result.Items))
	case "recall_session_memory":
		var args struct {
			MemoryID string `json:"memory_id"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil ||
			!validOpaqueID(args.MemoryID, "obs_") && !validOpaqueID(args.MemoryID, "ref_") {
			return toolError("invalid_arguments"), "会话证据回查参数无效"
		}
		value := s.contextRuntime.recallMemory(args.MemoryID)
		if toolResultCode(value) == ContextSourceUnavailable {
			s.contextRuntime.PublishSourceUnavailable()
		}
		return value, recallSummary(value)
	default:
		return toolError("unknown tool"), "模型请求了未知工具"
	}
}
