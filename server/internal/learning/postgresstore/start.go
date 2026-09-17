package postgresstore

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/jackc/pgx/v5"
)

// WithinTx 让原应用服务使用保存点；只有开学发布事务可以提交。
func (s *Store) WithinTx(tx pgx.Tx) *Store {
	bound := *s
	bound.pool = planningTx{tx}
	return &bound
}

func (s *Store) SessionKnowledgeContext(ctx context.Context, session, goalRevision, scope string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	id, err := s.tutoring.KnowledgeContextWith(ctx, tx, session)
	if err != nil || id == nil {
		return false, err
	}
	owner, ok := s.knowledge.(interface {
		ContextMatchesTx(context.Context, pgx.Tx, string, string, string) (bool, error)
	})
	if !ok {
		return false, &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid}
	}
	return owner.ContextMatchesTx(ctx, tx, *id, goalRevision, scope)
}

// BindStartContextTx 在开学事务内交给现场 owner 保存已验证引用。
func (s *Store) BindStartContextTx(ctx context.Context, tx pgx.Tx, session, goalRevision, id, scope string) error {
	owner, ok := s.knowledge.(interface {
		ContextMatchesTx(context.Context, pgx.Tx, string, string, string) (bool, error)
	})
	if !ok {
		return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid}
	}
	valid, err := owner.ContextMatchesTx(ctx, tx, id, goalRevision, scope)
	if err != nil {
		return err
	}
	if !valid {
		return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid}
	}
	return s.tutoring.BindKnowledgeContextWith(ctx, tx, session, goalRevision, id)
}

// ReadSessionContextTx 在学习区与目标仍可读时读取现场 owner 的关联。
func (s *Store) ReadSessionContextTx(ctx context.Context, tx pgx.Tx, session string) (*string, error) {
	if err := s.WithinTx(tx).ValidateSessionScope(ctx, session); err != nil {
		return nil, err
	}
	return s.tutoring.KnowledgeContextWith(ctx, tx, session)
}
