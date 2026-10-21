package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/modeladmission"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Claim 使用持久租约与全局并发配额；过期的已发出请求只结算未知结果，不重新请求。
func (s *Service) claim(ctx context.Context) (*row, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return nil, err
	}
	if err = modeladmission.CheckTx(ctx, tx, s.settings.View().Limits.Concurrency, "server", ""); errors.Is(err, modeladmission.ErrBusy) {
		return nil, tx.Commit(ctx)
	} else if err != nil {
		return nil, err
	}
	if err = s.usage.RecoverTx(ctx, tx, generation); err != nil {
		return nil, err
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE privacy_generation=$1 AND expires_at>clock_timestamp() AND ((state->>'saved')::boolean OR process_id=$2) AND (state->>'status'='queued' OR (state->>'status' IN ('running','cancelling') AND lease_until<clock_timestamp())) ORDER BY (state->>'kind'='topic_index'),created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, generation, s.process))
	if errors.Is(err, ErrNotFound) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if err = s.decode(&item); err != nil {
		item.BodyAvailable = false
		item.body = Body{}
		item.Status = "failed"
		item.Reason = "temporary_unavailable"
		if item.Saved {
			item.Reason = "checkpoint_unavailable"
		}
	} else {
		if item.Kind == "assessment" && item.Status != "cancelling" {
			waiting, e := s.recoverAssessmentTx(ctx, tx, &item)
			if e != nil {
				return nil, e
			}
			if waiting {
				if err = s.save(ctx, tx, &item, "proposal_wait"); err != nil {
					return nil, err
				}
				return nil, tx.Commit(ctx)
			}
		}
		if !terminal(item.Status) && (item.callStarted || item.Status == "cancelling") {
			wasCancelling := item.Status == "cancelling"
			item.Status = "failed"
			if item.body.Output != "" {
				item.Status = "partial"
			}
			item.ResultUnknown = item.callStarted
			item.CostUnknown = item.CostUnknown || item.callStarted
			item.Reason = "lease_expired"
			if wasCancelling {
				item.Status = "cancelled"
				item.Reason = "user_stopped"
			}
		} else if !terminal(item.Status) {
			item.Status = "running"
			id := uuid.NewString()
			var until time.Time
			if err = tx.QueryRow(ctx, `SELECT clock_timestamp()+($1::bigint*interval '1 microsecond')`, s.Lease.Microseconds()).Scan(&until); err != nil {
				return nil, err
			}
			item.lease = &id
			item.leaseUntil = &until
		}
	}

	if terminal(item.Status) {
		item.lease = nil
		item.leaseUntil = nil
	}
	item.Stage = item.Status
	if err = s.save(ctx, tx, &item, "claimed"); err != nil {
		return nil, err
	}
	s.cache(item)
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	if terminal(item.Status) {
		return nil, nil
	}
	return &item, nil
}

// mutate 在每个 checkpoint、付费请求和续租前重新核对真实授权与目标版本。
func (s *Service) mutate(ctx context.Context, owned *row, kind string, change func(*row) error) error {
	return s.mutateTx(ctx, owned, kind, func(_ pgx.Tx, item *row) error { return change(item) })
}

func (s *Service) mutateTx(ctx context.Context, owned *row, kind string, change func(pgx.Tx, *row) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return err
	}
	if generation != owned.Generation {
		return ErrInactive
	}
	if err = actorGate(ctx, tx, owned.device, owned.token, true); err != nil {
		return err
	}
	if owned.Kind == "topic_index" {
		var deleted bool
		var scope string
		if err = tx.QueryRow(ctx, `SELECT deleted,COALESCE(topic_index_scope,'') FROM learning_tutor_conversations WHERE id=$1 FOR SHARE`, owned.ConversationID).Scan(&deleted, &scope); err != nil {
			return err
		}
		if deleted || scope != owned.body.TopicIndex.Scope {
			return ErrInactive
		}
	}
	if owned.TeachingSessionID != "" {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:goal:"+owned.GoalID); err != nil {
			return err
		}
	}
	if owned.Kind != "assessment" {
		if _, err = goalGate(ctx, tx, owned.SpaceID, owned.GoalID, owned.GoalVersion); err != nil {
			return err
		}
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 FOR UPDATE`, owned.RunID))
	if err != nil {
		return err
	}
	if item.lease == nil || owned.lease == nil || *item.lease != *owned.lease || !item.leaseValid || item.Status != "running" || !item.BodyAvailable || time.Now().After(item.ExpiresAt) {
		return ErrLease
	}
	if err = s.decode(&item); err != nil {
		return err
	}
	if item.Kind == "assessment" {
		if err = s.assessmentContextTx(ctx, tx, item); err != nil {
			return err
		}
	}
	if _, err = s.sourceMaterialTx(ctx, tx, item.SpaceID, item.token, item.body); err != nil {
		return err
	}
	if item.Kind == "topic_index" {
		h := executionHost{service: s, owned: item, body: item.body}
		if err = h.topicGate(ctx, tx); err != nil {
			return err
		}
		if len(item.body.MemorySources) > 0 {
			if err = h.validateMemorySources(ctx, tx); err != nil {
				return err
			}
		}
	}
	if item.SourceResearchJobID != "" {
		if err = s.sourceResearchGateTx(ctx, tx, item, owned.body.Web.Epoch); err != nil {
			return err
		}
	}
	if item.body.ContentEdit != nil {
		if err = s.contentReadable(ctx, tx, item); err != nil {
			return err
		}
	}
	if item.body.StudioPlan != nil {
		if err = s.studioPlanGateTx(ctx, tx, item); err != nil {
			return err
		}
	}
	if item.body.StudioContent != nil {
		if err = s.studioContentGateTx(ctx, tx, item); err != nil {
			return err
		}
	}
	if err = change(tx, &item); err != nil {
		return err
	}
	if item.body.SourceManifest != nil {
		sources, e := s.sourceMaterialTx(ctx, tx, item.SpaceID, item.token, item.body)
		if e != nil {
			return e
		}
		if e = verifyStoredCitations(item.body, sources); e != nil {
			return e
		}
	}
	if kind == "" {
		_, err = tx.Exec(ctx, `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()+($2::bigint*interval '1 microsecond') WHERE id=$1`, item.RunID, s.Lease.Microseconds())
		if err == nil && item.Kind == "topic_index" {
			_, err = tx.Exec(ctx, `UPDATE learning_topic_index_heads SET lease_until=clock_timestamp()+($2::bigint*interval '1 microsecond') WHERE run_id=$1`, item.RunID, s.Lease.Microseconds())
		}
	} else {
		if terminal(item.Status) || item.Status == "waiting_input" || item.Status == "waiting_approval" || item.Status == "paused_tool_steps" {
			item.lease = nil
			item.leaseUntil = nil
		}
		err = s.save(ctx, tx, &item, kind)
	}
	if err != nil {
		return err
	}
	if kind != "" {
		s.cache(item)
	}
	return tx.Commit(ctx)
}

// 失败结算只保留最后一个有效 checkpoint，不提交失效上下文中的迟到正文。
func (s *Service) finish(ctx context.Context, owned row, cause error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return err
	}
	if generation != owned.Generation {
		s.drop(owned.RunID)
		return nil
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 FOR UPDATE`, owned.RunID))
	if err != nil {
		return err
	}
	if item.lease == nil || owned.lease == nil || *item.lease != *owned.lease || !item.leaseValid || terminal(item.Status) {
		return nil
	}
	if err = s.decode(&item); err != nil {
		item.BodyAvailable = false
		item.body = Body{}
	}
	wasCancelling := item.Status == "cancelling"
	if item.SourceResearchJobID != "" && errors.Is(cause, errCitation) {
		item.callStarted = false
	}
	item.Status = "failed"
	item.Reason = "model_failed"
	if errors.Is(cause, ErrStorage) {
		item.Reason = "run_storage_unavailable"
	}
	if errors.Is(cause, errCitation) {
		item.Reason = "invalid_source_citation"
	}
	if errors.Is(cause, ErrContextConfiguration) {
		item.Reason = "context_configuration_invalid"
	}
	if errors.Is(cause, ErrLimit) {
		item.Reason = "history_context_limit"
	}
	if errors.Is(cause, learningcontent.ErrConflict) {
		item.Reason = "selection_expired"
	}
	if errors.Is(cause, learningcontent.ErrInvalid) {
		item.Reason = "invalid_content_candidate"
	}
	if item.body.Research != nil {
		item.Reason = "research_failed"
		var searchErr *searchFailure
		if errors.As(cause, &searchErr) {
			item.Reason = searchErr.Error()
		}
		if errors.Is(cause, errCitation) {
			item.Reason = "invalid_source_citation"
		}
		if len(item.body.Research.Sources) > 0 {
			item.Status = "partial"
		}
	}
	if item.body.Output != "" {
		item.Status = "partial"
	}
	if errors.Is(cause, ErrInactive) || errors.Is(cause, ErrForbidden) || errors.Is(cause, ErrConflict) || errors.Is(cause, ErrLease) {
		item.Status = "cancelled"
		if item.Kind == "assessment" {
			item.Status = "failed"
		}
		item.Reason = "context_changed"
	}
	if item.body.Studio != nil || item.body.StudioPlan != nil || item.body.StudioContent != nil {
		item.Reason = studioFailureCategory(cause)
	}
	if wasCancelling {
		item.Status = "cancelled"
		item.Reason = "user_stopped"
	}
	if item.Kind == "assessment" && item.body.Assessment != nil && !wasCancelling {
		recovery, err := s.starter.Learning.InspectAssessmentTx(ctx, tx, *item.body.Assessment)
		if err != nil {
			return err
		}
		if recovery.ProposalStatus == "ready" && recovery.OperationStatus == "" {
			item.callStarted = false
			item.ResultUnknown = false
			if item.Reason == "model_failed" {
				item.Reason = "assessment_commit_failed"
			}
		}
	}
	item.ResultUnknown = item.ResultUnknown || item.callStarted
	item.CostUnknown = item.CostUnknown || item.callStarted
	if item.SourceResearchJobID != "" && item.body.Web != nil {
		item.Reason = studioFailureCategory(cause)
		notice := "本阶段未完成，已保存的候选和报告部分仍可查看与导入。"
		if errors.Is(cause, ErrLimit) || errors.Is(cause, ErrContextConfiguration) {
			notice = "本阶段超过模型上下文或存储资源限制，未继续外发；已保存结果保留。"
		}
		if llm.Category(cause) == llm.ErrorOutputBudgetExceeded {
			notice = "本阶段达到单次输出上限，未把截断内容计作成功；已完成结果保留。"
		}
		var searchErr *searchFailure
		if errors.As(cause, &searchErr) {
			item.Reason = searchErr.Error()
			notice = "搜索服务请求失败，请检查搜索连接或提供商状态；已保存结果保留，未自动重试。"
		}
		item.body.Web.Progress.Notice = notice
	}
	item.Stage = item.Status
	item.lease = nil
	item.leaseUntil = nil
	if err = s.save(ctx, tx, &item, "finished"); err != nil {
		return err
	}
	s.cache(item)
	return tx.Commit(ctx)
}

// 结算成功会吞掉执行错误；仅记录固定分类与运行标识，不记录原始错误或正文。
func logRunFailure(ctx context.Context, item row, cause error) {
	category := "model_failed"
	switch code := modelclient.StableErrorCode(cause); code {
	case modelclient.ErrorCodeInvalidReasoningEffort, modelclient.ErrorCodeReasoningEffortUnsupported,
		modelclient.ErrorCodeResponseProtocol, modelclient.ErrorCodeResponseTruncated,
		modelclient.ErrorCodeContentFiltered, modelclient.ErrorCodeStreamProtocol,
		modelclient.ErrorCodeStreamResponseTooLarge:
		category = string(code)
	default:
		var searchErr *searchFailure
		var networkErr net.Error
		switch {
		case errors.Is(cause, context.DeadlineExceeded):
			category = "timeout"
		case errors.Is(cause, context.Canceled):
			category = "cancelled"
		case errors.As(cause, &networkErr) && networkErr.Timeout():
			category = "timeout"
		case errors.As(cause, &networkErr):
			category = "unavailable"
		case errors.Is(cause, ErrInactive), errors.Is(cause, ErrForbidden), errors.Is(cause, ErrConflict), errors.Is(cause, ErrLease):
			category = "context_changed"
		case errors.Is(cause, ErrLimit):
			category = "history_context_limit"
		case errors.Is(cause, ErrStorage):
			category = "run_storage_unavailable"
		case errors.Is(cause, errCitation):
			category = "invalid_source_citation"
		case errors.Is(cause, research.ErrPolicy):
			category = "research_policy_rejected"
		case errors.Is(cause, learningcontent.ErrInvalid):
			category = "invalid_content_candidate"
		case errors.Is(cause, learningcontent.ErrConflict):
			category = "selection_expired"
		case errors.Is(cause, learningcontent.ErrUnavailable):
			category = "content_unavailable"
		case errors.As(cause, &searchErr):
			category = "research_search_failed"
		}
	}
	var modelErr *llm.Error
	if errors.As(cause, &modelErr) {
		switch code := modelErr.Category; code {
		case llm.ErrorUnauthorized, llm.ErrorRateLimited, llm.ErrorUpstream,
			llm.ErrorTimeout, llm.ErrorUnavailable, llm.ErrorInvalidResponse,
			llm.ErrorSchemaMismatch, llm.ErrorIncompatible, llm.ErrorInvalidRequest,
			llm.ErrorOutputBudgetExceeded, llm.ErrorResponseNotCompleted:
			category = string(code)
		}
	}
	if item.Kind == "studio" {
		category = studioFailureCategory(cause)
	}
	if errors.Is(cause, errCitation) {
		slog.WarnContext(ctx, "mentor_run_failed", "run_id", item.RunID, "kind", item.Kind, "stage", item.Stage, "error_category", category, "rejection_reason", citationReason(cause), "rejection_field", citationField(cause))
		return
	}
	if field, constraint := llm.SchemaRejection(cause); field != "" {
		slog.WarnContext(ctx, "mentor_run_failed", "run_id", item.RunID, "kind", item.Kind, "stage", item.Stage, "error_category", category, "rejection_reason", string(llm.ErrorSchemaMismatch), "rejection_field", field, "rejection_constraint", constraint)
		return
	}
	slog.WarnContext(ctx, "mentor_run_failed", "run_id", item.RunID, "kind", item.Kind, "stage", item.Stage, "error_category", category)
}

func studioFailureCategory(cause error) string {
	if llm.Category(cause) == llm.ErrorContextWindowExceeded {
		return "context_configuration_invalid"
	}
	if sourceUnavailable(cause) {
		return "source_unavailable"
	}
	if errors.Is(cause, errCitation) {
		return "invalid_source_citation"
	}
	var e *llm.Error
	if errors.As(cause, &e) {
		switch e.Category {
		case llm.ErrorUnauthorized, llm.ErrorRateLimited, llm.ErrorUpstream, llm.ErrorTimeout, llm.ErrorUnavailable, llm.ErrorInvalidResponse, llm.ErrorSchemaMismatch, llm.ErrorIncompatible, llm.ErrorInvalidRequest, llm.ErrorOutputBudgetExceeded, llm.ErrorResponseNotCompleted:
			return string(e.Category)
		}
	}
	if errors.Is(cause, learningcontent.ErrConflict) {
		return "studio_version_conflict"
	}
	if errors.Is(cause, learningcontent.ErrInvalid) {
		return "invalid_source_citation"
	}
	if errors.Is(cause, ErrContextConfiguration) {
		return "context_configuration_invalid"
	}
	if errors.Is(cause, ErrLimit) {
		return "history_context_limit"
	}
	if errors.Is(cause, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(cause, ErrInactive) || errors.Is(cause, ErrForbidden) || errors.Is(cause, ErrLease) {
		return "context_changed"
	}
	return "model_failed"
}

func (s *Service) RunOnce(ctx context.Context) (int, error) {
	if err := s.scheduleConfirmedStudio(ctx); err != nil {
		return 0, err
	}
	if err := s.enqueueAssessment(ctx); err != nil {
		return 0, err
	}
	s.schedulePendingTopics(ctx)
	item, err := s.claim(ctx)
	if err != nil || item == nil {
		return 0, err
	}
	model, limits, fingerprint, err := s.settings.MentorClient()
	if err != nil || model == nil || fingerprint != item.Configuration && !s.sourceResearchConfiguration(*item) {
		return 1, s.finish(ctx, *item, ErrInactive)
	}
	execution, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(context.Canceled) }
	done := make(chan struct{})
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(s.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-execution.Done():
				return
			case <-ticker.C:
				if err := s.mutate(execution, item, "", func(*row) error { return nil }); err != nil {
					cancelCause(err)
					return
				}
			}
		}
	}()
	host := &executionHost{service: s, owned: *item, body: item.body, limits: limits, model: model}
	host.model, _, _, err = s.settings.MentorUsageClient(func(connection settings.Connection, next http.RoundTripper) http.RoundTripper {
		return budgetTransport{host: host, next: next, connection: connection}
	})
	webAnswered := false
	if host.body.SourceResearch != nil {
		execution, release := host.webContext(execution)
		err = host.runSourceResearch(execution)
		release()
		webAnswered = true
	} else if host.body.Web != nil {
		err = host.prepareWebSearch(execution)
		if err == nil && len(host.body.Web.Documents) > 0 {
			webAnswered = true
			err = host.generateWebAnswer(execution)
		}
	}
	if err != nil || webAnswered {
		// The isolated public stage either paused/failed, or published its verified answer.
	} else if host.body.TopicIndex != nil {
		err = host.generateTopics(execution)
	} else if host.body.StudioPlan != nil {
		err = host.generateStudioPlan(execution)
	} else if host.body.StudioContent != nil {
		err = host.generateStudioContent(execution)
	} else if host.body.Assessment != nil {
		err = host.assess(execution)
	} else if host.body.Studio != nil {
		err = host.generateStudio(execution)
	} else if host.body.Study != nil {
		err = host.generateStudy(execution)
	} else if host.body.ContentEdit != nil {
		err = host.editContent(execution)
	} else if host.body.Research != nil {
		err = host.runResearch(execution)
	} else if len(host.body.Pending) > 0 {
		_, err = host.Execute(execution, host.body.Pending)
	}
	if err == nil && !webAnswered && host.body.StudioContent == nil && host.body.StudioPlan == nil && host.body.TopicIndex == nil && host.body.Assessment == nil && host.body.Study == nil && host.body.Interaction == nil && host.body.Research == nil && host.body.ContentEdit == nil && host.body.Studio == nil {
		_, err = (agentcore.Runner[struct{}]{Model: callModel{host}, Context: host, History: host, Tools: host, Events: host}).Run(execution)
	}
	if execution.Err() != nil {
		err = context.Cause(execution)
	}
	close(done)
	cancel()
	<-renewed
	if errors.Is(err, errToolStepsPaused) {
		return 1, nil
	}
	if err != nil {
		logRunFailure(ctx, host.owned, err)
		settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return 1, s.finish(settle, *item, err)
	}
	s.scheduleTopics(ctx, *item)
	return 1, nil
}

// Sweep 执行到期清理并释放本进程临时缓存；全局清除由 learning owner 同事务清理。
func (s *Service) Sweep(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		s.mu.Lock()
		clear(s.temporary)
		s.mu.Unlock()
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_processes(id,live_until) VALUES($1,clock_timestamp()+interval '30 seconds') ON CONFLICT(id) DO UPDATE SET live_until=excluded.live_until`, s.process); err != nil {
		return 0, err
	}
	if err = s.usage.RecoverTx(ctx, tx, generation); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE privacy_generation=$1 AND (state->>'body_available')::boolean AND (expires_at<=clock_timestamp() OR ((state->>'saved')::boolean=FALSE AND process_id<>$2 AND NOT EXISTS(SELECT 1 FROM learning_mentor_processes p WHERE p.id=process_id AND p.live_until>clock_timestamp()) AND (lease_until IS NULL OR lease_until<clock_timestamp()))) FOR UPDATE SKIP LOCKED`, generation, s.process)
	if err != nil {
		return 0, err
	}
	var expired []row
	for rows.Next() {
		item, e := scan(rows)
		if e != nil {
			rows.Close()
			return 0, e
		}
		expired = append(expired, item)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, item := range expired {
		item.BodyAvailable = false
		item.body = Body{}
		item.Reason = "expired"
		if time.Now().Before(item.ExpiresAt) {
			item.Reason = "temporary_unavailable"
		}
		if !terminal(item.Status) {
			item.Status = "failed"
		}
		item.ResultUnknown = item.ResultUnknown || item.callStarted
		item.lease = nil
		item.leaseUntil = nil
		if _, err = tx.Exec(ctx, `DELETE FROM learning_mentor_events WHERE run_id=$1`, item.RunID); err != nil {
			return 0, err
		}
		if err = s.save(ctx, tx, &item, "expired"); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	// 代次变化清除旧内存，不与同时创建的新运行竞争查询可见性。
	s.mu.Lock()
	if s.temporaryGeneration != generation {
		clear(s.temporary)
		s.temporaryGeneration = generation
	}
	for _, item := range expired {
		delete(s.temporary, item.RunID)
	}
	s.mu.Unlock()
	return len(expired), nil
}

// checkpointJSONSize 用于确保所有模型协议载荷共享正文上限。
func checkpointJSONSize(body Body) int {
	if !sourceFilesValid(body) {
		return MaxBody + 1
	}
	body.SourceFiles = nil
	raw, _ := json.Marshal(body)
	return len(raw)
}
