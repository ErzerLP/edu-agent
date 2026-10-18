package mentorrun

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpg "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Record unavailability is an explicit product state. Only transient query
// cancellation/lock contention degrades to source-only planning; authorization,
// malformed records and storage/schema errors remain errors.
func studioLearningStateTx(ctx context.Context, tx pgx.Tx, direction string) (learning.StudioLearningState, error) {
	nested, err := tx.Begin(ctx)
	if err != nil {
		return learning.StudioLearningState{}, err
	}
	defer nested.Rollback(context.Background())
	var timeout string
	if err = nested.QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout); err != nil {
		return learning.StudioLearningState{}, err
	}
	if _, err = nested.Exec(ctx, `SET LOCAL statement_timeout='2s'`); err != nil {
		return learning.StudioLearningState{}, err
	}
	state, err := learningpg.ReadStudioLearningStateTx(ctx, nested, direction)
	if err != nil {
		var pgErr *pgconn.PgError
		if ctx.Err() != nil || !errors.As(err, &pgErr) || (pgErr.Code != "57014" && pgErr.Code != "55P03") {
			return state, err
		}
		if rollback := nested.Rollback(ctx); rollback != nil {
			return state, rollback
		}
		return learning.StudioLearningState{DirectionRevision: direction, Coverage: "unavailable", SuggestedFocus: []string{}}, nil
	}
	if _, err = nested.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, timeout); err != nil {
		return state, err
	}
	return state, nested.Commit(ctx)
}
