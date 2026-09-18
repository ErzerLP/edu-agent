package learningchange

import (
	"context"
	"errors"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

// RunOnce 只处理持久队列；断线、刷新或服务重启不需要重新生成和批准候选。
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	if !s.Available() {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id,space_id,goal_id,session_id,device_id,token_id FROM learning_changes WHERE status='queued_for_boundary' AND ciphertext IS NOT NULL ORDER BY updated_at,id LIMIT 32`)
	if err != nil {
		return 0, err
	}
	type item struct{ id, space, goal, session, device, token string }
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.space, &v.goal, &v.session, &v.device, &v.token); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range items {
		actor := identity.Credential{Device: identity.Device{ID: v.device}, TokenID: v.token}
		scoped, e := learningspace.WithScope(ctx, v.space)
		if e != nil {
			return n, e
		}
		changed, e := s.drain(scoped, actor, v.goal, v.session, v.id)
		if e != nil {
			return n, e
		}
		if changed {
			n++
		}
	}
	return n, nil
}
func (s *Service) drain(ctx context.Context, actor identity.Credential, goal, session, id string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	g, gateErr := gates(ctx, tx, actor, true)
	if gateErr != nil && !errors.Is(gateErr, ErrForbidden) {
		return false, gateErr
	}
	_, scopeErr := s.scope(ctx, tx, goal, session, true)
	if scopeErr != nil && !errors.Is(scopeErr, ErrInactive) {
		return false, scopeErr
	}
	if err = lock(ctx, tx, "learning-change:"+id); err != nil {
		return false, err
	}
	c, err := s.read(ctx, tx, goal, id, 0)
	if err != nil {
		return false, err
	}
	if c.Status != "queued_for_boundary" {
		return false, nil
	}
	if gateErr != nil || scopeErr != nil || c.Generation != g {
		c.Status, c.Reason = "cancelled", "权限、目标生命周期或隐私代次已变化，队列已停止。"
	} else {
		snap, e := s.snapshot(ctx, tx, goal, session, g)
		if e != nil {
			return false, e
		}
		stale := snap.Base.GoalRevisionID != c.Base.GoalRevisionID || snap.Base.RouteRevisionID != c.Base.RouteRevisionID || snap.Base.ContextID != c.Base.ContextID || snap.Base.ActivityID != "" && snap.Base.ActivityID != c.Base.ActivityID
		valid, e := s.knowledge.ReviewedStructureContextMatchesTx(ctx, tx, snap.AvailableContextID, c.Candidate.ContextID)
		if e != nil {
			return false, e
		}
		stale = stale || !valid
		if c.Base.ArtifactID != "" {
			r, e := s.content.GetTx(ctx, tx, c.Base.ArtifactID, g)
			if e != nil {
				return false, e
			}
			stale = stale || r.Version != c.Base.ArtifactVersion
		}
		if stale {
			c.Status, c.Reason = "stale", "排队期间目标、路线、内容或当前活动已变化；原候选保留。"
		} else if !safe(snap.Session.State) {
			// 移到队尾，多个长题不会阻塞其他会话的安全点。
			_, err = tx.Exec(ctx, `UPDATE learning_changes SET updated_at=clock_timestamp() WHERE id=$1`, id)
			if err != nil {
				return false, err
			}
			return false, tx.Commit(ctx)
		} else {
			// 保存点使正式 owner 的任何失败整体回滚，再记录可恢复的失效状态。
			child, e := tx.Begin(ctx)
			if e != nil {
				return false, e
			}
			e = s.apply(ctx, child, actor, &c, snap, false)
			if e != nil {
				child.Rollback(context.Background())
				if learning.ErrorCode(e) == "" && !errors.Is(e, ErrConflict) && !errors.Is(e, ErrInvalid) {
					return false, e
				}
				c.Status, c.Reason = "stale", "应用前复验失败，未提交路线或内容；请重新审阅当前版本。"
				c.Applied = nil
			} else if err = child.Commit(ctx); err != nil {
				return false, err
			}
		}
	}
	if err = s.save(ctx, tx, c, false); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FlushSession 在普通教学动作完成后接入队列，后台 worker 是断线恢复的补充。
func (s *Service) FlushSession(ctx context.Context, session string) error {
	if !s.Available() {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id,goal_id,device_id,token_id FROM learning_changes WHERE session_id=$1 AND space_id=$2 AND status='queued_for_boundary' ORDER BY created_at,id`, session, learningspace.Scope(ctx))
	if err != nil {
		return err
	}
	type item struct{ id, goal, device, token string }
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.goal, &v.device, &v.token); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range items {
		if _, err = s.drain(ctx, identity.Credential{Device: identity.Device{ID: v.device}, TokenID: v.token}, v.goal, session, v.id); err != nil {
			return err
		}
	}
	return nil
}
