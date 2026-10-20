// Package modeladmission serializes all server model work in the existing pool.
package modeladmission

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrBusy = errors.New("gateway_busy")
var ErrInactive = errors.New("model_lease_inactive")

const Duration = 30 * time.Second
const Renewal = 10 * time.Second

type Lease struct {
	ID         string
	Generation int64
	Until      time.Time
}

// CheckTx is also used by mentor claim, whose existing run row is its lease.
// Callers keep this transaction open until their lease/run has been registered.
func CheckTx(ctx context.Context, tx pgx.Tx, capacity int, source, device string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(210021)`); err != nil {
		return err
	}
	var total, cli, perDevice int
	err := tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM learning_mentor_runs WHERE lease_until>clock_timestamp() AND state->>'status' IN ('running','cancelling'))+count(*),
 count(*) FILTER(WHERE source='cli'),count(*) FILTER(WHERE source='cli' AND actor_device_id::text=$1)
 FROM learning_model_execution_leases WHERE lease_until>clock_timestamp()`, device).Scan(&total, &cli, &perDevice)
	if err != nil {
		return err
	}
	if total >= capacity || source == "cli" && (cli >= capacity-1 || perDevice >= 1) {
		return ErrBusy
	}
	return nil
}
func AcquireTx(ctx context.Context, tx pgx.Tx, generation int64, capacity int, source, device, request string) (Lease, error) {
	lease := Lease{ID: uuid.NewString(), Generation: generation}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&current); err != nil {
		return lease, err
	}
	if current != generation {
		return lease, ErrInactive
	}
	if err := CheckTx(ctx, tx, capacity, source, device); err != nil {
		return lease, err
	}
	err := tx.QueryRow(ctx, `INSERT INTO learning_model_execution_leases(id,privacy_generation,owner,source,actor_device_id,request_id,lease_until) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,'')::uuid,clock_timestamp()+interval '30 seconds') RETURNING lease_until`, lease.ID, generation, uuid.NewString(), source, device, request).Scan(&lease.Until)
	return lease, err
}
func RenewTx(ctx context.Context, tx pgx.Tx, l Lease) (time.Time, error) {
	var until time.Time
	err := tx.QueryRow(ctx, `UPDATE learning_model_execution_leases SET lease_until=clock_timestamp()+interval '30 seconds' WHERE id=$1 AND privacy_generation=$2 AND lease_until>clock_timestamp() RETURNING lease_until`, l.ID, l.Generation).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInactive
	}
	return until, err
}
func Release(ctx context.Context, pool *pgxpool.Pool, l Lease) error {
	_, err := pool.Exec(ctx, `DELETE FROM learning_model_execution_leases WHERE id=$1 AND privacy_generation=$2`, l.ID, l.Generation)
	return err
}

// Maintain cancels before the current lease expires if renewal is unavailable.
// stop must be called after the upstream body closes, before releasing capacity.
func Maintain(ctx context.Context, initial time.Time, renew func(context.Context) (time.Time, error), cancel context.CancelCauseFunc) func() {
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		until := initial
		timer := time.NewTimer(Renewal)
		defer timer.Stop()
		for {
			select {
			case <-run.Done():
				return
			case <-timer.C:
				bounded, end := context.WithDeadline(run, until.Add(-time.Second))
				next, err := renew(bounded)
				end()
				if err != nil {
					cancel(err)
					return
				}
				until = next
				timer.Reset(Renewal)
			}
		}
	}()
	return func() { stop(); <-done }
}
