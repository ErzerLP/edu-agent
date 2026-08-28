package postgresstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLOutboxConcurrentClaimHasSingleLeaseOwner(t *testing.T) {
	pool := outboxIntegrationPool(t)
	store := outboxdb.New(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := outboxIntegrationMessage(t, "concurrent-claim-owner", 1, now)
	if inserted, err := store.Enqueue(ctx, message); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
	}

	const claimers = 16
	start := make(chan struct{})
	results := make(chan []outbox.Message, claimers)
	errorsFound := make(chan error, claimers)
	var wait sync.WaitGroup
	wait.Add(claimers)
	for range claimers {
		go func() {
			defer wait.Done()
			<-start
			claimed, err := store.Claim(ctx, now, time.Minute, 1)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- claimed
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("concurrent claim: %v", err)
	}

	var claimedCount int
	leaseOwners := map[string]struct{}{}
	for claimed := range results {
		if len(claimed) > 1 {
			t.Fatalf("single-message claim returned %d rows", len(claimed))
		}
		if len(claimed) == 0 {
			continue
		}
		claimedCount++
		if claimed[0].ID != message.ID || claimed[0].LeaseToken == "" {
			t.Fatalf("unexpected claimed message: %+v", claimed[0])
		}
		leaseOwners[claimed[0].LeaseToken] = struct{}{}
	}
	if claimedCount != 1 || len(leaseOwners) != 1 {
		t.Fatalf("concurrent claim rows=%d unique_owners=%d", claimedCount, len(leaseOwners))
	}

	var status string
	var attempts int
	var hasLease bool
	if err := pool.QueryRow(ctx, `SELECT status,attempts,lease_token IS NOT NULL FROM outbox_messages WHERE id=$1`, message.ID).Scan(&status, &attempts, &hasLease); err != nil {
		t.Fatal(err)
	}
	if status != "processing" || attempts != 1 || !hasLease {
		t.Fatalf("claimed row status=%s attempts=%d has_lease=%v", status, attempts, hasLease)
	}
}

func TestPostgreSQLOutboxExpiredLeaseReclaimFencesPreviousOwner(t *testing.T) {
	pool := outboxIntegrationPool(t)
	store := outboxdb.New(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := outboxIntegrationMessage(t, "expired-lease-reclaim", 1, now)
	if inserted, err := store.Enqueue(ctx, message); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
	}
	first, err := store.Claim(ctx, now, time.Minute, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	beforeExpiry, err := store.Claim(ctx, now.Add(30*time.Second), time.Minute, 1)
	if err != nil || len(beforeExpiry) != 0 {
		t.Fatalf("claim before expiry=%+v err=%v", beforeExpiry, err)
	}
	reclaimed, err := store.Claim(ctx, now.Add(61*time.Second), time.Minute, 1)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("expired lease reclaim=%+v err=%v", reclaimed, err)
	}
	if reclaimed[0].ID != message.ID || reclaimed[0].LeaseToken == first[0].LeaseToken || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaimed=%+v first=%+v", reclaimed[0], first[0])
	}

	oldLeaseChecks := []struct {
		name string
		run  func() error
	}{
		{name: "ack", run: func() error {
			return store.MarkApplied(ctx, message.ID, first[0].LeaseToken, now.Add(62*time.Second))
		}},
		{name: "retry", run: func() error {
			return store.MarkFailed(ctx, message.ID, first[0].LeaseToken, "transient", now.Add(62*time.Second), now.Add(63*time.Second), false)
		}},
		{name: "defer", run: func() error {
			return store.MarkDeferred(ctx, message.ID, first[0].LeaseToken, "dependency_unavailable", now.Add(62*time.Second), now.Add(64*time.Second))
		}},
	}
	for _, check := range oldLeaseChecks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, outbox.ErrLeaseLost) {
				t.Fatalf("previous owner transition error=%v", err)
			}
		})
	}
	if err := store.MarkApplied(ctx, message.ID, reclaimed[0].LeaseToken, now.Add(65*time.Second)); err != nil {
		t.Fatal(err)
	}

	var status string
	var attempts int
	var leaseCleared bool
	if err := pool.QueryRow(ctx, `
		SELECT status,attempts,lease_token IS NULL AND lease_expires_at IS NULL
		FROM outbox_messages WHERE id=$1`, message.ID).Scan(&status, &attempts, &leaseCleared); err != nil {
		t.Fatal(err)
	}
	if status != "applied" || attempts != 2 || !leaseCleared {
		t.Fatalf("final row status=%s attempts=%d lease_cleared=%v", status, attempts, leaseCleared)
	}
}

type outboxCommittedEffectConsumer struct {
	pool  *pgxpool.Pool
	calls int
}

func (*outboxCommittedEffectConsumer) CanApply(context.Context, outbox.Message) (outbox.ApplyDecision, error) {
	return outbox.ApplyDecision{Apply: true}, nil
}

func (c *outboxCommittedEffectConsumer) Apply(ctx context.Context, message outbox.Message) error {
	c.calls++
	_, err := c.pool.Exec(ctx, `
		INSERT INTO outbox_test_effects(idempotency_key,applied_at)
		VALUES($1,clock_timestamp())
		ON CONFLICT (idempotency_key) DO NOTHING`, message.IdempotencyKey)
	return err
}

func TestPostgreSQLOutboxWorkerReclaimConvergesCommittedIdempotentSideEffect(t *testing.T) {
	pool := outboxIntegrationPool(t)
	store := outboxdb.New(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := outboxIntegrationMessage(t, "committed-effect-worker-reclaim", 1, now)
	if inserted, err := store.Enqueue(ctx, message); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE outbox_test_effects(idempotency_key TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	consumer := &outboxCommittedEffectConsumer{pool: pool}
	newWorker := func(workerNow time.Time) *outbox.Worker {
		worker, err := outbox.NewWorker(store, map[string]outbox.Consumer{"operations.test": consumer}, outbox.WorkerOptions{
			BatchSize: 1, Lease: time.Second, BaseBackoff: time.Second, MaxBackoff: time.Minute,
			Now: func() time.Time { return workerNow }, Jitter: func(time.Duration) time.Duration { return 0 },
		})
		if err != nil {
			t.Fatal(err)
		}
		return worker
	}

	installOutboxTransitionFault(t, pool)
	if processed, err := newWorker(now).RunOnce(ctx); err == nil || processed != 0 || !strings.Contains(err.Error(), "injected outbox transition failure") {
		t.Fatalf("first worker finalize failure processed=%d err=%v", processed, err)
	}
	var effects, attempts int
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM outbox_test_effects WHERE idempotency_key=$1),status,attempts
		FROM outbox_messages WHERE id=$2`, message.IdempotencyKey, message.ID).Scan(&effects, &status, &attempts); err != nil {
		t.Fatal(err)
	}
	if effects != 1 || status != "processing" || attempts != 1 || consumer.calls != 1 {
		t.Fatalf("post-commit finalize fault effects=%d status=%s attempts=%d consumer_calls=%d", effects, status, attempts, consumer.calls)
	}

	removeOutboxTransitionFault(t, pool)
	if processed, err := newWorker(now.Add(2 * time.Second)).RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("restarted worker reclaim processed=%d err=%v", processed, err)
	}
	var terminalRows int
	var dispositionCleared bool
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM outbox_test_effects WHERE idempotency_key=$1),
		  status,attempts,terminal_disposition IS NULL,
		  count(*) FILTER (WHERE status IN ('applied','dead','canceled'))
		FROM outbox_messages WHERE id=$2
		GROUP BY status,attempts,terminal_disposition`, message.IdempotencyKey, message.ID).Scan(
		&effects, &status, &attempts, &dispositionCleared, &terminalRows,
	); err != nil {
		t.Fatal(err)
	}
	if effects != 1 || status != "applied" || attempts != 2 || !dispositionCleared || terminalRows != 1 || consumer.calls != 2 {
		t.Fatalf("worker reclaim effects=%d status=%s attempts=%d disposition_cleared=%v terminal_rows=%d consumer_calls=%d", effects, status, attempts, dispositionCleared, terminalRows, consumer.calls)
	}
	if processed, err := newWorker(now.Add(3 * time.Second)).RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("terminal worker replay processed=%d err=%v", processed, err)
	}
}

func TestPostgreSQLOutboxClaimFailureDoesNotConsumeMessage(t *testing.T) {
	pool := outboxIntegrationPool(t)
	store := outboxdb.New(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := outboxIntegrationMessage(t, "claim-failure-rollback", 1, now)
	if inserted, err := store.Enqueue(ctx, message); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_outbox_claim_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  RAISE EXCEPTION 'injected outbox claim failure';
		END $$;
		CREATE TRIGGER zz_injected_outbox_claim_failure
		BEFORE UPDATE ON outbox_messages
		FOR EACH ROW
		WHEN (OLD.status='pending' AND NEW.status='processing')
		EXECUTE FUNCTION fail_outbox_claim_write()`); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.Claim(ctx, now, time.Minute, 1); err == nil || len(claimed) != 0 {
		t.Fatalf("injected claim rows=%+v err=%v", claimed, err)
	}

	var status string
	var attempts int
	var leaseCleared bool
	if err := pool.QueryRow(ctx, `
		SELECT status,attempts,lease_token IS NULL AND lease_expires_at IS NULL
		FROM outbox_messages WHERE id=$1`, message.ID).Scan(&status, &attempts, &leaseCleared); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 || !leaseCleared {
		t.Fatalf("failed claim status=%s attempts=%d lease_cleared=%v", status, attempts, leaseCleared)
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER zz_injected_outbox_claim_failure ON outbox_messages`); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 || claimed[0].ID != message.ID || claimed[0].Attempts != 1 {
		t.Fatalf("claim after fault removal=%+v err=%v", claimed, err)
	}
}

func TestPostgreSQLOutboxTransitionWriteFaultsRollbackAtomically(t *testing.T) {
	tests := []struct {
		name            string
		transition      func(context.Context, *outboxdb.Store, outbox.Message, time.Time) error
		wantStatus      string
		wantAttempts    int
		wantCategory    string
		wantDisposition string
	}{
		{
			name: "applied",
			transition: func(ctx context.Context, store *outboxdb.Store, message outbox.Message, now time.Time) error {
				return store.MarkApplied(ctx, message.ID, message.LeaseToken, now.Add(time.Second))
			},
			wantStatus:   "applied",
			wantAttempts: 1,
		},
		{
			name: "retry",
			transition: func(ctx context.Context, store *outboxdb.Store, message outbox.Message, now time.Time) error {
				return store.MarkFailed(ctx, message.ID, message.LeaseToken, "transient", now.Add(time.Second), now.Add(2*time.Second), false)
			},
			wantStatus:   "pending",
			wantAttempts: 1,
			wantCategory: "transient",
		},
		{
			name: "defer",
			transition: func(ctx context.Context, store *outboxdb.Store, message outbox.Message, now time.Time) error {
				return store.MarkDeferred(ctx, message.ID, message.LeaseToken, "dependency_unavailable", now.Add(time.Second), now.Add(2*time.Second))
			},
			wantStatus:   "pending",
			wantAttempts: 0,
			wantCategory: "dependency_unavailable",
		},
		{
			name: "cancel",
			transition: func(ctx context.Context, store *outboxdb.Store, message outbox.Message, now time.Time) error {
				return store.Cancel(ctx, outbox.CancelRequest{
					IdempotencyKey: message.IdempotencyKey, LeaseToken: message.LeaseToken,
					Disposition: outbox.DispositionExpired, CanceledAt: now.Add(time.Second),
				})
			},
			wantStatus:      "canceled",
			wantAttempts:    1,
			wantDisposition: string(outbox.DispositionExpired),
		},
		{
			name: "dead",
			transition: func(ctx context.Context, store *outboxdb.Store, message outbox.Message, now time.Time) error {
				return store.MarkFailed(ctx, message.ID, message.LeaseToken, "permanent", now.Add(time.Second), now.Add(2*time.Second), true)
			},
			wantStatus:   "dead",
			wantAttempts: 1,
			wantCategory: "permanent",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := outboxIntegrationPool(t)
			store := outboxdb.New(pool)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			message := outboxIntegrationMessage(t, "transition-fault-"+test.name, 1, now)
			if inserted, err := store.Enqueue(ctx, message); err != nil || !inserted {
				t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
			}
			claimed, err := store.Claim(ctx, now, time.Minute, 1)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			message = claimed[0]

			installOutboxTransitionFault(t, pool)
			if err := test.transition(ctx, store, message, now); err == nil {
				t.Fatal("injected transition write unexpectedly succeeded")
			}
			assertOutboxTransitionState(t, pool, message.ID, "processing", 1, message.LeaseToken, "", "")

			removeOutboxTransitionFault(t, pool)
			if err := test.transition(ctx, store, message, now); err != nil {
				t.Fatalf("transition after fault removal: %v", err)
			}
			assertOutboxTransitionState(t, pool, message.ID, test.wantStatus, test.wantAttempts, "", test.wantCategory, test.wantDisposition)
		})
	}
}

func TestPostgreSQLOutboxPrivacyRedactionRevokesActiveLease(t *testing.T) {
	pool := outboxIntegrationPool(t)
	store := outboxdb.New(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := outboxIntegrationMessage(t, "privacy-active-lease", 1, now)
	message.Payload = json.RawMessage(`{"learner":"sensitive"}`)
	message.AuditMetadata = json.RawMessage(`{"trace":"sensitive"}`)
	if inserted, err := store.Enqueue(ctx, message); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
	}
	claimed, err := store.Claim(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}

	request := establishOutboxRedactionBarrier(t, pool, now)
	installOutboxTransitionFault(t, pool)
	if err := store.RedactTx(ctx, request); err == nil {
		t.Fatal("injected privacy cancel write unexpectedly succeeded")
	}
	assertOutboxTransitionState(t, pool, message.ID, "processing", 1, claimed[0].LeaseToken, "", "")
	var unchangedPayload, unchangedAudit string
	if err := pool.QueryRow(ctx, `SELECT payload::text,audit_metadata::text FROM outbox_messages WHERE id=$1`, message.ID).Scan(&unchangedPayload, &unchangedAudit); err != nil {
		t.Fatal(err)
	}
	if unchangedPayload != `{"learner": "sensitive"}` || unchangedAudit != `{"trace": "sensitive"}` {
		t.Fatalf("failed privacy cancel changed payload=%s audit=%s", unchangedPayload, unchangedAudit)
	}
	removeOutboxTransitionFault(t, pool)
	if err := store.RedactTx(ctx, request); err != nil {
		t.Fatal(err)
	}
	if residual, err := store.VerifyRedacted(ctx, request); err != nil || residual != 0 {
		t.Fatalf("verify redacted residual=%d err=%v", residual, err)
	}

	oldLeaseChecks := []struct {
		name string
		run  func() error
	}{
		{name: "ack", run: func() error {
			return store.MarkApplied(ctx, message.ID, claimed[0].LeaseToken, now.Add(time.Second))
		}},
		{name: "retry", run: func() error {
			return store.MarkFailed(ctx, message.ID, claimed[0].LeaseToken, "transient", now.Add(time.Second), now.Add(2*time.Second), false)
		}},
		{name: "defer", run: func() error {
			return store.MarkDeferred(ctx, message.ID, claimed[0].LeaseToken, "dependency_unavailable", now.Add(time.Second), now.Add(2*time.Second))
		}},
	}
	for _, check := range oldLeaseChecks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, outbox.ErrLeaseLost) {
				t.Fatalf("redacted lease transition error=%v", err)
			}
		})
	}
	if after, err := store.Claim(ctx, now.Add(2*time.Minute), time.Minute, 1); err != nil || len(after) != 0 {
		t.Fatalf("redacted message was reclaimable rows=%+v err=%v", after, err)
	}

	var status, disposition, payload, audit string
	var leaseCleared bool
	if err := pool.QueryRow(ctx, `
		SELECT status,terminal_disposition,payload::text,audit_metadata::text,
		       lease_token IS NULL AND lease_expires_at IS NULL
		FROM outbox_messages WHERE id=$1`, message.ID).Scan(
		&status, &disposition, &payload, &audit, &leaseCleared,
	); err != nil {
		t.Fatal(err)
	}
	if status != "canceled" || disposition != "privacy_erasure" || payload != `{"redacted": true}` || audit != `{}` || !leaseCleared {
		t.Fatalf("redacted row status=%s disposition=%s payload=%s audit=%s lease_cleared=%v", status, disposition, payload, audit, leaseCleared)
	}
}

func installOutboxTransitionFault(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_outbox_transition_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  RAISE EXCEPTION 'injected outbox transition failure';
		END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER zz_injected_outbox_transition_failure
		BEFORE UPDATE ON outbox_messages
		FOR EACH ROW
		WHEN (OLD.status='processing')
		EXECUTE FUNCTION fail_outbox_transition_write()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS zz_injected_outbox_transition_failure ON outbox_messages`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS fail_outbox_transition_write()`)
	})
}

func removeOutboxTransitionFault(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TRIGGER zz_injected_outbox_transition_failure ON outbox_messages`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION fail_outbox_transition_write()`); err != nil {
		t.Fatal(err)
	}
}

func assertOutboxTransitionState(
	t *testing.T,
	pool *pgxpool.Pool,
	messageID, wantStatus string,
	wantAttempts int,
	wantLeaseToken, wantCategory, wantDisposition string,
) {
	t.Helper()
	var status, leaseToken, category, disposition string
	var attempts int
	if err := pool.QueryRow(context.Background(), `
		SELECT status,attempts,COALESCE(lease_token::text,''),COALESCE(last_error_category,''),COALESCE(terminal_disposition,'')
		FROM outbox_messages WHERE id=$1`, messageID).Scan(&status, &attempts, &leaseToken, &category, &disposition); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || attempts != wantAttempts || leaseToken != wantLeaseToken || category != wantCategory || disposition != wantDisposition {
		t.Fatalf("outbox state status=%s attempts=%d lease=%q category=%q disposition=%q", status, attempts, leaseToken, category, disposition)
	}
}

func outboxIntegrationMessage(t *testing.T, key string, generation int64, now time.Time) outbox.Message {
	t.Helper()
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType:   "operations.test",
		AggregateID:    key,
		IdempotencyKey: key,
		Revision:       1,
		Generation:     generation,
		Payload:        json.RawMessage(`{"value":1}`),
		AuditMetadata:  json.RawMessage(`{"actor":"test"}`),
		MaxAttempts:    5,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func establishOutboxRedactionBarrier(t *testing.T, pool *pgxpool.Pool, now time.Time) privacy.LocalRedactionRequest {
	t.Helper()
	ctx := context.Background()
	deviceID := uuid.NewString()
	erasureID := uuid.NewString()
	operationID := uuid.NewString()
	receiptID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'privacy actor',$2)`, deviceID, now); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasures(
		  id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
		  target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('11',32),'hex'),'learner_request',$2,$4,2,$5)`,
		erasureID, deviceID, operationID, now, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at)
		VALUES($1,'barrier_committed',1,'barrier committed',$2)`, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_redaction_barriers(
		  erasure_id,learner_generation,redacted_through_event_seq,policy_version,reason_code,event_id,committed_at)
		VALUES($1,2,0,$2,'learner_request',$3,$4)`, erasureID, privacy.PolicyVersion, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(
		  id,erasure_id,store_kind,version,scope_digest,started_at,status,stable_reason,verification_method)
		VALUES($1,$2,'inbox_outbox',1,decode(repeat('22',32),'hex'),$3,'pending','pending','owner verification')`,
		receiptID, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasure_receipt_heads(
		  erasure_id,store_kind,current_receipt_id,current_version,updated_at)
		VALUES($1,'inbox_outbox',$2,1,$3)`, erasureID, receiptID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=2,read_open=FALSE,write_open=FALSE,active_erasure_id=$1,updated_at=$2
		WHERE owner_kind='outbox'`, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return privacy.LocalRedactionRequest{
		ErasureID:         erasureID,
		Store:             privacy.StoreInboxOutbox,
		ReceiptID:         receiptID,
		LearnerGeneration: 2,
	}
}
