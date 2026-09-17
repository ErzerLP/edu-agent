package postgresstore

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
)

// KnowledgeContextWith 只返回可空引用，旧会话的公开 DTO 保持不变。
func (s *Store) KnowledgeContextWith(ctx context.Context, db DBTX, id string) (*string, error) {
	if _, err := s.LockReadWith(ctx, db); err != nil {
		return nil, err
	}
	var value *string
	err := db.QueryRow(ctx, `SELECT knowledge_context_revision_id::text FROM tutoring_sessions WHERE id=$1`, id).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return value, err
}

// BindKnowledgeContextWith 只允许给尚未开课的新现场绑定已由 knowledge 校验的上下文。
func (s *Store) BindKnowledgeContextWith(ctx context.Context, db DBTX, id, goalRevision, contextID string) error {
	tag, err := db.Exec(ctx, `UPDATE tutoring_sessions SET knowledge_context_revision_id=$3 WHERE id=$1 AND goal_revision_id=$2 AND state=$4 AND knowledge_context_revision_id IS NULL`, id, goalRevision, contextID, tutoring.StateGoalReady)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
