package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
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
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(210021)`); err != nil {
		return nil, err
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM learning_mentor_runs WHERE lease_until>clock_timestamp() AND state->>'status' IN ('running','cancelling')`).Scan(&active); err != nil {
		return nil, err
	}
	if active >= s.settings.View().Limits.Concurrency {
		return nil, nil
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE privacy_generation=$1 AND expires_at>clock_timestamp() AND ((state->>'saved')::boolean OR process_id=$2) AND (state->>'status'='queued' OR (state->>'status' IN ('running','cancelling') AND lease_until<clock_timestamp())) ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, generation, s.process))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
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
	} else if item.callStarted || item.Status == "cancelling" {
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
	} else {
		item.Status = "running"
		id := uuid.NewString()
		var until time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()+($1::bigint*interval '1 microsecond')`, s.Lease.Microseconds()).Scan(&until); err != nil {
			return nil, err
		}
		item.lease = &id
		item.leaseUntil = &until
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
	if owned.TeachingSessionID != "" {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:goal:"+owned.GoalID); err != nil {
			return err
		}
	}
	if _, err = goalGate(ctx, tx, owned.SpaceID, owned.GoalID, owned.GoalVersion); err != nil {
		return err
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
	if item.body.ContentEdit != nil {
		if err = s.contentReadable(ctx, tx, item); err != nil {
			return err
		}
	}
	if err = change(tx, &item); err != nil {
		return err
	}
	if kind == "" {
		_, err = tx.Exec(ctx, `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()+($2::bigint*interval '1 microsecond') WHERE id=$1`, item.RunID, s.Lease.Microseconds())
	} else {
		if terminal(item.Status) || item.Status == "waiting_input" || item.Status == "waiting_approval" || item.Status == "paused_budget" {
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
	item.Status = "failed"
	item.Reason = "model_failed"
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
		item.Reason = "context_changed"
	}
	if wasCancelling {
		item.Status = "cancelled"
		item.Reason = "user_stopped"
	}
	item.ResultUnknown = item.ResultUnknown || item.callStarted
	item.CostUnknown = item.CostUnknown || item.callStarted
	item.Stage = item.Status
	item.lease = nil
	item.leaseUntil = nil
	if err = s.save(ctx, tx, &item, "finished"); err != nil {
		return err
	}
	s.cache(item)
	return tx.Commit(ctx)
}

func (s *Service) RunOnce(ctx context.Context) (int, error) {
	item, err := s.claim(ctx)
	if err != nil || item == nil {
		return 0, err
	}
	model, limits, fingerprint, err := s.settings.MentorClient()
	if err != nil || model == nil || fingerprint != item.Configuration {
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
	host.model, _, _, err = s.settings.MentorClient(func(next http.RoundTripper) http.RoundTripper { return budgetTransport{host: host, next: next} })
	if host.body.ContentEdit != nil {
		err = host.editContent(execution)
	} else if host.body.Research != nil {
		err = host.runResearch(execution)
	} else if len(host.body.Pending) > 0 {
		_, err = host.Execute(execution, host.body.Pending)
	}
	if err == nil && host.body.Interaction == nil && host.body.Research == nil && host.body.ContentEdit == nil {
		_, err = (agentcore.Runner[struct{}]{Model: callModel{host}, Context: host, History: host, Tools: host, Events: host}).Run(execution)
	}
	if execution.Err() != nil {
		err = context.Cause(execution)
	}
	close(done)
	cancel()
	<-renewed
	if errors.Is(err, ErrBudget) {
		return 1, nil
	}
	if err != nil {
		settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return 1, s.finish(settle, *item, err)
	}
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
