package postgresstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type failingBarrierKeyDestroyer struct{}

func (failingBarrierKeyDestroyer) DestroyGenerationKeysTx(context.Context, privacy.DBTX, privacy.BackupKeyDestroyRequest) (privacy.BackupKeyDestroyResult, error) {
	return privacy.BackupKeyDestroyResult{}, errors.New("injected backup key destruction failure")
}

func newAuthorizedBarrierStore(pool *pgxpool.Pool, manager *privacy.ReadPermitManager, options ...privacydb.Option) *privacydb.Store {
	tutoringStore := tutoringdb.New(pool)
	base := []privacydb.Option{
		privacydb.WithReadPermits(manager),
		privacydb.WithLocalOwner(identitydb.New(pool)),
		privacydb.WithLocalOwner(knowledgedb.New(pool)),
		privacydb.WithLocalOwner(learningdb.New(pool, tutoringStore)),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memorydb.New(pool)),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	}
	return privacydb.New(pool, append(base, options...)...)
}

func issueBarrierGrant(t *testing.T, pool *pgxpool.Pool, deviceID string) privacy.IssuedErasureGrant {
	t.Helper()
	service, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := service.Issue(context.Background(), deviceID, "authorized-barrier-test")
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func barrierRequest(deviceID, operationID string, at time.Time) privacy.ErasureRequest {
	return privacy.ErasureRequest{
		DeviceID: deviceID, OperationID: operationID, ActorDeviceID: deviceID,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: at.UTC(),
		ManagedBackupUnrecoverableAfter:  at.UTC().Add(24 * time.Hour),
		ExpectedCurrentLearnerGeneration: 1,
	}
}

func TestPostgreSQLAuthorizedBarrierRollsBackGrantOnFailure(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'authorized rollback',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	manager := privacy.NewReadPermitManager()
	issued := issueBarrierGrant(t, pool, deviceID)
	authorization := privacy.NewErasureGrantAuthorization(deviceID, issued.Token)
	request := barrierRequest(deviceID, uuid.NewString(), time.Now().UTC())
	failing := newAuthorizedBarrierStore(pool, manager, privacydb.WithBackupKeyDestroyer(failingBarrierKeyDestroyer{}))
	conflict := request
	conflict.ExpectedCurrentLearnerGeneration = 2
	if _, err := failing.CommitBarrierAuthorized(ctx, conflict, authorization); privacy.ErrorCode(err) != privacy.CodeIdempotencyConflict {
		t.Fatalf("expected-generation barrier conflict err=%v", err)
	}
	var consumed bool
	if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM privacy_erasure_grants WHERE device_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, deviceID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed {
		t.Fatal("barrier conflict consumed the grant")
	}
	if _, err := failing.CommitBarrierAuthorized(ctx, request, authorization); err == nil {
		t.Fatal("injected barrier failure unexpectedly committed")
	}
	if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM privacy_erasure_grants WHERE device_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, deviceID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed {
		t.Fatal("failed barrier consumed the grant")
	}
	working := newAuthorizedBarrierStore(pool, manager)
	if _, err := working.CommitBarrierAuthorized(ctx, request, authorization); err != nil {
		t.Fatalf("rolled-back grant was not reusable: %v", err)
	}
}

func TestPostgreSQLAuthorizedBarrierCrossClockReplayWithoutGrant(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'authorized replay',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	store := newAuthorizedBarrierStore(pool, privacy.NewReadPermitManager())
	issued := issueBarrierGrant(t, pool, deviceID)
	operationID := uuid.NewString()
	firstRequest := barrierRequest(deviceID, operationID, time.Now().UTC())
	first, err := store.CommitBarrierAuthorized(ctx, firstRequest, privacy.NewErasureGrantAuthorization(deviceID, issued.Token))
	if err != nil {
		t.Fatal(err)
	}
	retryRequest := barrierRequest(deviceID, operationID, firstRequest.RequestedAt.Add(8*time.Hour))
	replayed, err := store.CommitBarrierAuthorized(ctx, retryRequest, privacy.NewErasureGrantAuthorization(deviceID, ""))
	if err != nil || replayed.ErasureID != first.ErasureID || !replayed.RequestedAt.Equal(first.RequestedAt) {
		t.Fatalf("cross-clock replay=%+v first=%+v err=%v", replayed, first, err)
	}
}

func TestPostgreSQLPrivacyReceiptNotFound(t *testing.T) {
	store := newAuthorizedBarrierStore(privacyIntegrationPool(t), privacy.NewReadPermitManager())
	if _, err := store.Receipt(context.Background(), uuid.NewString()); privacy.ErrorCode(err) != privacy.CodeNotFound {
		t.Fatalf("unknown receipt err=%v", err)
	}
}

func TestPostgreSQLAuthorizedBarrierConcurrentGrantDifferentOperations(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'authorized race',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	store := newAuthorizedBarrierStore(pool, privacy.NewReadPermitManager())
	issued := issueBarrierGrant(t, pool, deviceID)
	authorization := privacy.NewErasureGrantAuthorization(deviceID, issued.Token)
	requests := []privacy.ErasureRequest{
		barrierRequest(deviceID, uuid.NewString(), time.Now().UTC()),
		barrierRequest(deviceID, uuid.NewString(), time.Now().UTC().Add(time.Second)),
	}
	type result struct {
		receipt privacy.ErasureReceipt
		err     error
	}
	results := make(chan result, len(requests))
	var wait sync.WaitGroup
	for _, request := range requests {
		wait.Add(1)
		go func(value privacy.ErasureRequest) {
			defer wait.Done()
			receipt, err := store.CommitBarrierAuthorized(context.Background(), value, authorization)
			results <- result{receipt: receipt, err: err}
		}(request)
	}
	wait.Wait()
	close(results)
	var committed int
	for result := range results {
		if result.err == nil {
			committed++
			continue
		}
		if privacy.ErrorCode(result.err) != privacy.CodeErasureInProgress && !errors.Is(result.err, privacy.ErrErasureGrantInvalid) {
			t.Fatalf("unexpected concurrent authorization error: %v", result.err)
		}
	}
	var erasures, consumed int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM privacy_erasures),(SELECT count(*) FROM privacy_erasure_grants WHERE device_id=$1 AND consumed_at IS NOT NULL)`, deviceID).Scan(&erasures, &consumed); err != nil {
		t.Fatal(err)
	}
	if committed != 1 || erasures != 1 || consumed != 1 {
		t.Fatalf("committed=%d erasures=%d consumed_grants=%d", committed, erasures, consumed)
	}
}
