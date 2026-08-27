package postgresstore

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResponseCommitGate keeps persistent privacy owner gates locked until a
// complete buffered response has been emitted.
type ResponseCommitGate struct {
	pool *pgxpool.Pool
}

func NewResponseCommitGate(pool *pgxpool.Pool) *ResponseCommitGate {
	return &ResponseCommitGate{pool: pool}
}

func (g *ResponseCommitGate) WithReadGates(ctx context.Context, owners []privacy.OwnerKind, commit func() error) error {
	if g == nil || g.pool == nil || commit == nil {
		return &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_response_commit_gate"}
	}
	ordered, err := orderedResponseOwners(owners)
	if err != nil {
		return err
	}
	tx, err := g.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin privacy response commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, owner := range ordered {
		if _, err := privacy.LockOwnerRead(ctx, tx, owner); err != nil {
			return err
		}
	}
	if err := commit(); err != nil {
		return err
	}
	return nil
}

func orderedResponseOwners(owners []privacy.OwnerKind) ([]privacy.OwnerKind, error) {
	ownerSet := make(map[privacy.OwnerKind]struct{}, len(owners))
	for _, owner := range owners {
		if !owner.Valid() {
			return nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_response_commit_owner"}
		}
		ownerSet[owner] = struct{}{}
	}
	if len(ownerSet) == 0 {
		return nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "response_commit_owner_required"}
	}
	ordered := make([]privacy.OwnerKind, 0, len(ownerSet))
	for _, owner := range privacy.AllOwners {
		if _, ok := ownerSet[owner]; ok {
			ordered = append(ordered, owner)
		}
	}
	return ordered, nil
}
