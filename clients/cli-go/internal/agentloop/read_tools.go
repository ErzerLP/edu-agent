package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func (s *Session) executeReadTool(ctx context.Context, call modelclient.ToolCall) (any, string) {
	if c, ok := s.server.(*agentcontext.Client); ok {
		if err := c.Check(ctx); err != nil {
			return toolFailure(err, "learning_context_unavailable"), "上下文需要重新校验"
		}
	}
	if call.Function.Name == "learning_context" {
		var args struct {
			View   string `json:"view"`
			Cursor string `json:"cursor"`
			Query  string `json:"query"`
			Offset int    `json:"offset"`
		}
		if decodeArguments(call.Function.Arguments, &args) != nil || len(args.Cursor) > 4096 || len(args.Query) > 2000 || args.Offset < 0 {
			return toolError("invalid_arguments"), "学习查询参数无效"
		}
		name := map[string]string{"goals": "list_learning_goals", "goal": "get_bound_goal", "plans": "get_planning_drafts", "progress": "get_learning_progress", "reviews": "get_due_reviews", "route": "get_learning_route", "search": "search_knowledge"}[args.View]
		if name == "" {
			return toolError("invalid_arguments"), "未知学习视图"
		}
		params := map[string]any{}
		switch args.View {
		case "goals", "progress", "reviews":
			params["cursor"] = args.Cursor
		case "route":
			params["offset"] = args.Offset
		case "search":
			params["query"] = args.Query
		}
		raw, _ := json.Marshal(params)
		call.Function.Name, call.Function.Arguments = name, string(raw)
		value, summary := s.executeBaseReadTool(ctx, call)
		object := normalizedProjectionObject(value)
		if object != nil {
			object["learning_view"] = name
			return object, summary
		}
		return value, summary
	}
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
	case "list_learning_goals", "get_bound_goal", "get_planning_drafts":
		return s.readLearningContext(ctx, call)
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
			"scope_snapshot_id":     result.ScopeSnapshotID,
			"hits":                  hits,
			"degraded":              result.Degraded,
			"truncated":             result.Truncated,
		}, fmt.Sprintf("检索到 %d 条知识片段", len(hits))
	case "get_learning_progress":
		var args struct {
			Cursor string `json:"cursor"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || len(args.Cursor) > 4096 {
			return toolError("invalid_arguments"), "学习进度参数无效"
		}
		q := api.ProgressQuery{LearningSpaceID: s.options.LearningBinding.Normalize().SpaceID, GoalID: s.options.LearningBinding.GoalID, Limit: 20, Cursor: args.Cursor}
		reader, ok := s.server.(interface {
			Progress(context.Context, api.ProgressQuery) (api.ProgressPage, error)
		})
		if !ok {
			return toolError("progress_unavailable"), "目标进度不可用"
		}
		result, err := reader.Progress(ctx, q)
		if err != nil {
			return toolFailure(err, "progress_unavailable"), "目标进度不可用"
		}
		return result, fmt.Sprintf("已读取 %d/%d 个目标", len(result.Items), result.Total)
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
		q := api.ProgressQuery{LearningSpaceID: s.options.LearningBinding.Normalize().SpaceID, GoalID: s.options.LearningBinding.GoalID, Limit: 20, Cursor: args.Cursor}
		reader, ok := s.server.(interface {
			ScopedReviews(context.Context, api.ProgressQuery, *time.Time) (api.ReviewsPage, error)
		})
		if !ok {
			return toolError("reviews_unavailable"), "复习任务不可用"
		}
		result, err := reader.ScopedReviews(ctx, q, nil)
		if err != nil {
			return toolFailure(err, "reviews_unavailable"), "复习任务不可用"
		}
		value := map[string]any{
			"items":      result.Items,
			"returned":   len(result.Items),
			"due_before": result.DueBefore,
			"total":      result.Total,
			"metadata":   result.Metadata,
			"updated_at": result.UpdatedAt,
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
