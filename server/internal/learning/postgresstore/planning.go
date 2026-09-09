package postgresstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type planningDB interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// 子应用服务使用保存点，只有最外层确认事务可以提交业务事实和回执。
type planningTx struct{ pgx.Tx }

func (t planningTx) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return t.Tx.Begin(ctx)
}

func (s *Store) ReadPlanning(ctx context.Context, goal, id string) (learning.PlanningDraft, error) {
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.PlanningDraft, error) { return readPlanning(ctx, db, goal, id) })
}
func readPlanning(ctx context.Context, db learningLoaderDB, goal, id string) (learning.PlanningDraft, error) {
	var d learning.PlanningDraft
	var raw []byte
	if !learningspace.ValidID(goal) || !learningspace.ValidID(id) {
		return d, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	err := db.QueryRow(ctx, `SELECT payload FROM learning_plans WHERE id=$1 AND goal_id=$2 AND space_id=$3`, id, goal, learningspace.Scope(ctx)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && raw == nil {
		return d, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return d, err
	}
	err = json.Unmarshal(raw, &d)
	return d, err
}
func (s *Store) ListPlanning(ctx context.Context, goal string) ([]learning.PlanningDraft, error) {
	if !learningspace.ValidID(goal) {
		return nil, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) ([]learning.PlanningDraft, error) {
		rows, err := db.Query(ctx, `SELECT payload FROM learning_plans WHERE goal_id=$1 AND space_id=$2 AND payload IS NOT NULL ORDER BY id`, goal, learningspace.Scope(ctx))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := []learning.PlanningDraft{}
		for rows.Next() {
			var raw []byte
			var d learning.PlanningDraft
			if err = rows.Scan(&raw); err != nil {
				return nil, err
			}
			if err = json.Unmarshal(raw, &d); err != nil {
				return nil, err
			}
			result = append(result, d)
		}
		return result, rows.Err()
	})
}
func (s *Store) ChangePlanning(ctx context.Context, device, goal, id string, c learning.PlanningCommand, change func(context.Context, learning.ApplicationStore, learning.ProposalRepository, *learning.PlanningDraft) error) (learning.PlanningDraft, error) {
	var d learning.PlanningDraft
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return d, err
	}
	defer tx.Rollback(context.Background())
	if err = lockGoalSpace(ctx, tx); err != nil {
		return d, err
	}
	hash, err := learning.HashJSON(struct {
		Goal, ID, Space string
		Command         learning.PlanningCommand
	}{goal, id, learningspace.Scope(ctx), c})
	if err != nil {
		return d, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "planning-operation:"+device+":"+c.OperationID); err != nil {
		return d, err
	}
	var stored string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,payload FROM learning_plan_operations WHERE device_id=$1 AND operation_id=$2`, device, c.OperationID).Scan(&stored, &raw)
	if err == nil {
		if stored != hash {
			return d, &learning.Error{Code: learning.CodeIdempotencyConflict}
		}
		if raw == nil {
			return d, &learning.Error{Code: learning.CodeNotFound}
		}
		if err = json.Unmarshal(raw, &d); err != nil {
			return d, err
		}
		return d, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}
	// 与正式目标写入共用锁；规划生成期间不能悄悄改变目标依据。
	for _, key := range []string{"learning-aggregate:goal:" + goal, "planning-draft:" + id} {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return d, err
		}
	}
	d, err = readPlanning(ctx, tx, goal, id)
	if c.Action == "create" {
		if err == nil {
			return d, &learning.Error{Code: learning.CodeVersionConflict}
		}
		if learning.ErrorCode(err) != learning.CodeNotFound {
			return d, err
		}
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_plans WHERE id=$1)`, id).Scan(&exists); err != nil {
			return d, err
		}
		if exists {
			return d, &learning.Error{Code: learning.CodeIdempotencyConflict}
		}
	} else {
		if err != nil {
			return d, err
		}
		if d.Version != c.ExpectedVersion {
			return d, &learning.Error{Code: learning.CodeVersionConflict, ExpectedVersion: c.ExpectedVersion, CurrentVersion: d.Version}
		}
	}
	if d.SessionID != "" {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:session:"+d.SessionID); err != nil {
			return d, err
		}
	}
	bound := *s
	bound.pool = planningTx{tx}
	scope := d.Content.Details.ScopeSnapshotID
	if c.Action == "create" {
		g, e := scanGoal(tx.QueryRow(ctx, "SELECT "+goalColumns+" FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2 ORDER BY revision DESC LIMIT 1", goal, learningspace.Scope(ctx)))
		if e != nil {
			return d, e
		}
		scope = g.GoalManagement().Details.ScopeSnapshotID
	}
	if c.Content != nil {
		scope = c.Content.Details.ScopeSnapshotID
	}
	if scope != "" {
		owner, ok := s.knowledge.(interface {
			LockPlanningScopeWith(context.Context, pgx.Tx, string) error
		})
		if !ok {
			return d, &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid}
		}
		if err = owner.LockPlanningScopeWith(ctx, tx, scope); err != nil {
			return d, err
		}
	}
	if err = change(ctx, &bound, &bound, &d); err != nil {
		return d, err
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return d, err
	}
	if c.Action == "create" {
		_, err = tx.Exec(ctx, `INSERT INTO learning_plans(id,goal_id,space_id,payload) VALUES($1,$2,$3,$4)`, id, goal, learningspace.Scope(ctx), raw)
	} else {
		_, err = tx.Exec(ctx, `UPDATE learning_plans SET payload=$2 WHERE id=$1`, id, raw)
	}
	if err != nil {
		return d, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_plan_operations(device_id,operation_id,request_hash,payload) VALUES($1,$2,$3,$4)`, device, c.OperationID, hash, raw); err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}
