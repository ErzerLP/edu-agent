package postgresstore

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

func (s *Store) OfflineLearnerGeneration(ctx context.Context) (uint64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite})
	if err != nil {
		return 0, fmt.Errorf("begin offline bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit offline bootstrap: %w", err)
	}
	return uint64(generation), nil
}
