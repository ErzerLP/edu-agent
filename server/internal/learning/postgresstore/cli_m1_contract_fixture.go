//go:build cli_m1_contract

package postgresstore

import (
	"context"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/jackc/pgx/v5"
)

// RefreshActiveProjectionForContract rematerializes the active generation after
// a contract fixture intentionally changes canonical event history.
func (s *Store) RefreshActiveProjectionForContract(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin contract projection refresh: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var generation int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&generation); err != nil {
		return fmt.Errorf("lock contract learning write gate: %w", err)
	}
	highWater, err := eventHighWater(ctx, tx, true)
	if err != nil {
		return err
	}
	var generationID string
	if err := tx.QueryRow(ctx, `SELECT active_generation_id FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&generationID); err != nil {
		return fmt.Errorf("lock contract projection head: %w", err)
	}
	events, err := loadEvents(ctx, tx, 0, highWater)
	if err != nil {
		return err
	}
	projection, err := learning.Replay(events, s.registry, generationID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := replaceProjection(ctx, tx, generationID, projection, highWater, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit contract projection refresh: %w", err)
	}
	return nil
}
