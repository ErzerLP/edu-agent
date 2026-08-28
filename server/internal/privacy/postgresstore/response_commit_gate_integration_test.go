package postgresstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestResponseCommitGateLinearizesCrossProcessPrivacyClose(t *testing.T) {
	pool := privacyIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager := privacy.NewReadPermitManager(privacy.WithResponseCommitGate(privacydb.NewResponseCommitGate(pool)))
	owners := []privacy.OwnerKind{privacy.OwnerLearning, privacy.OwnerKnowledge}
	permit, err := manager.Acquire(ctx, owners...)
	if err != nil {
		t.Fatal(err)
	}

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		err := permit.CommitResponse(func() {
			close(writeStarted)
			<-releaseWrite
		})
		permit.Release()
		commitDone <- err
	}()
	select {
	case <-writeStarted:
	case <-ctx.Done():
		t.Fatal("response commit did not acquire persistent gates")
	}

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- closePersistentResponseOwners(ctx, pool, owners)
	}()
	<-closeStarted
	select {
	case err := <-closeDone:
		t.Fatalf("cross-process close overtook response flush: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseWrite)
	if err := <-commitDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}

	// The process-local manager remains open. The persistent gate must still
	// reject a response that begins after another process committed the close.
	closedPermit, err := manager.Acquire(ctx, owners...)
	if err != nil {
		t.Fatalf("local manager unexpectedly observed remote close: %v", err)
	}
	wrote := false
	err = closedPermit.CommitResponse(func() { wrote = true })
	closedPermit.Release()
	if privacy.ErrorCode(err) != privacy.CodeContentRedacted || wrote {
		t.Fatalf("remote close did not suppress response: err=%v wrote=%v", err, wrote)
	}
}

func TestResponseCommitGateCancellationUnblocksLocalDrainBehindPersistentClose(t *testing.T) {
	pool := privacyIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closingTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closingTx.Rollback(context.Background()) }()
	if _, err := closingTx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('privacy-owner:'||'knowledge',0))`); err != nil {
		t.Fatal(err)
	}

	manager := privacy.NewReadPermitManager(privacy.WithResponseCommitGate(privacydb.NewResponseCommitGate(pool)))
	permit, err := manager.Acquire(ctx, privacy.OwnerKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	wrote := false
	commitDone := make(chan error, 1)
	go func() {
		err := permit.CommitResponse(func() { wrote = true })
		permit.Release()
		commitDone <- err
	}()
	waitForPersistentResponseGateWait(t, ctx, pool)

	drainCtx, cancelDrain := context.WithTimeout(ctx, 2*time.Second)
	defer cancelDrain()
	if err := manager.CloseAndDrain(drainCtx, 2, privacy.OwnerKnowledge); err != nil {
		t.Fatalf("local drain deadlocked behind persistent close: %v", err)
	}
	if err := <-commitDone; privacy.ErrorCode(err) != privacy.CodeContentRedacted || wrote {
		t.Fatalf("persistent gate wait was not canceled safely: err=%v wrote=%v", err, wrote)
	}
}

func waitForPersistentResponseGateWait(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM pg_stat_activity
				WHERE pid<>pg_backend_pid()
				  AND wait_event_type='Lock'
				  AND wait_event='advisory'
				  AND query LIKE '%privacy_lock_owner_gate%'
			)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("response commit did not wait on the persistent owner gate")
		case <-ticker.C:
		}
	}
}

func closePersistentResponseOwners(ctx context.Context, pool *pgxpool.Pool, owners []privacy.OwnerKind) error {
	ownerSet := make(map[privacy.OwnerKind]struct{}, len(owners))
	for _, owner := range owners {
		ownerSet[owner] = struct{}{}
	}
	ordered := make([]privacy.OwnerKind, 0, len(ownerSet))
	for _, owner := range privacy.AllOwners {
		if _, ok := ownerSet[owner]; ok {
			ordered = append(ordered, owner)
		}
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	erasureID, deviceID := uuid.NewString(), uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'response gate close',clock_timestamp())`, deviceID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after
		) VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),'learner_request',$2,clock_timestamp(),2,clock_timestamp()+interval '1 hour')`, erasureID, deviceID, uuid.NewString()); err != nil {
		return err
	}
	for _, owner := range ordered {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('privacy-owner:'||$1,0))`, string(owner)); err != nil {
			return err
		}
	}
	for _, owner := range ordered {
		if _, err := tx.Exec(ctx, `
			UPDATE privacy_owner_generation_gates
			SET learner_generation=learner_generation+1,
				read_open=FALSE,
				write_open=FALSE,
				active_erasure_id=$2,
				updated_at=clock_timestamp()
			WHERE owner_kind=$1`, string(owner), erasureID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
