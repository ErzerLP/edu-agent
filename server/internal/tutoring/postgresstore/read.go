package postgresstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func withPrivacyRead[T any](ctx context.Context, store *Store, read func(DBTX) (T, error)) (T, error) {
	var zero T
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return zero, fmt.Errorf("begin tutoring privacy read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	value, err := read(tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, fmt.Errorf("commit tutoring privacy read: %w", err)
	}
	return value, nil
}
