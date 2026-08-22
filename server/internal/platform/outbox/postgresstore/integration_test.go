package postgresstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLOutboxIdempotencyRequiresIdenticalMessage(t *testing.T) {
	pool := outboxIntegrationPool(t)
	ctx := context.Background()
	store := postgresstore.New(pool)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "learning.test", AggregateID: "aggregate-1", IdempotencyKey: "outbox-full-message-key",
		Revision: 3, Generation: 2, Payload: json.RawMessage(`{"value":1}`),
		AuditMetadata: json.RawMessage(`{"actor":"device-1"}`), MaxAttempts: 4,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := store.Enqueue(ctx, message)
	if err != nil || !inserted {
		t.Fatalf("initial enqueue inserted=%v err=%v", inserted, err)
	}
	inserted, err = store.Enqueue(ctx, message)
	if err != nil || inserted {
		t.Fatalf("identical replay inserted=%v err=%v", inserted, err)
	}

	tests := []struct {
		name   string
		mutate func(*outbox.Message)
	}{
		{name: "payload", mutate: func(value *outbox.Message) { value.Payload = json.RawMessage(`{"value":2}`) }},
		{name: "revision", mutate: func(value *outbox.Message) { value.Revision++ }},
		{name: "generation", mutate: func(value *outbox.Message) { value.Generation++ }},
		{name: "audit metadata", mutate: func(value *outbox.Message) { value.AuditMetadata = json.RawMessage(`{"actor":"device-2"}`) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conflict := message
			test.mutate(&conflict)
			inserted, err := store.Enqueue(ctx, conflict)
			if inserted || !errors.Is(err, postgresstore.ErrIdempotencyConflict) {
				t.Fatalf("conflicting enqueue inserted=%v err=%v", inserted, err)
			}
		})
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages WHERE idempotency_key=$1`, message.IdempotencyKey).Scan(&count); err != nil || count != 1 {
		t.Fatalf("outbox conflict changed rows count=%d err=%v", count, err)
	}
}

func TestPostgreSQLOutboxProcessingCancelReclaimAndConcurrency(t *testing.T) {
	pool := outboxIntegrationPool(t)
	store := postgresstore.New(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "memory.delivery", AggregateID: "reclaim", IdempotencyKey: "outbox-processing-reclaim",
		Revision: 1, Generation: 1, Payload: json.RawMessage(`{"secret":"sensitive"}`), MaxAttempts: 5,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if inserted, err := store.Enqueue(ctx, message); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%v err=%v", inserted, err)
	}
	first, err := store.Claim(ctx, now, time.Second, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	reclaimed, err := store.Claim(ctx, now.Add(2*time.Second), time.Minute, 1)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].LeaseToken == first[0].LeaseToken {
		t.Fatalf("reclaim=%+v first=%+v err=%v", reclaimed, first, err)
	}
	tombstone := json.RawMessage(`{"redacted":true}`)
	request := outbox.CancelRequest{
		IdempotencyKey: message.IdempotencyKey, LeaseToken: first[0].LeaseToken,
		Disposition: outbox.DispositionExpired, TombstonePayload: tombstone, CanceledAt: now.Add(3 * time.Second),
	}
	if err := store.Cancel(ctx, request); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("old lease cancellation err=%v", err)
	}
	request.LeaseToken = reclaimed[0].LeaseToken
	if err := store.Cancel(ctx, request); err != nil {
		t.Fatalf("current lease cancellation: %v", err)
	}
	if err := store.MarkApplied(ctx, message.ID, reclaimed[0].LeaseToken, now.Add(4*time.Second)); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("old worker applied canceled row err=%v", err)
	}
	if err := store.MarkFailed(ctx, message.ID, reclaimed[0].LeaseToken, "transient", now.Add(4*time.Second), now.Add(5*time.Second), false); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("old worker failed canceled row err=%v", err)
	}
	if err := store.Cancel(ctx, request); err != nil {
		t.Fatalf("identical cancellation replay: %v", err)
	}
	conflict := request
	conflict.Disposition = outbox.DispositionDeleted
	if err := store.Cancel(ctx, conflict); !errors.Is(err, postgresstore.ErrIdempotencyConflict) {
		t.Fatalf("different disposition replay err=%v", err)
	}
	conflict = request
	conflict.TombstonePayload = json.RawMessage(`{"redacted":"different"}`)
	if err := store.Cancel(ctx, conflict); !errors.Is(err, postgresstore.ErrIdempotencyConflict) {
		t.Fatalf("different tombstone replay err=%v", err)
	}
	var status, disposition, payload string
	var tokenNull, expiryNull bool
	if err := pool.QueryRow(ctx, `
		SELECT status,terminal_disposition,payload::text,lease_token IS NULL,lease_expires_at IS NULL
		FROM outbox_messages WHERE id=$1`, message.ID).Scan(&status, &disposition, &payload, &tokenNull, &expiryNull); err != nil {
		t.Fatal(err)
	}
	if status != "canceled" || disposition != "expired" || payload != `{"redacted": true}` || !tokenNull || !expiryNull {
		t.Fatalf("canceled row status=%s disposition=%s payload=%s token_null=%v expiry_null=%v", status, disposition, payload, tokenNull, expiryNull)
	}

	for i := range 12 {
		iterationNow := now.Add(time.Duration(10+i) * time.Second)
		key := fmt.Sprintf("outbox-cancel-race-%02d", i)
		value, err := outbox.NewMessage(outbox.NewMessageInput{
			BusinessType: "memory.delivery", AggregateID: key, IdempotencyKey: key,
			Revision: 1, Generation: 1, Payload: json.RawMessage(`{"sensitive":true}`), MaxAttempts: 3,
		}, iterationNow)
		if err != nil {
			t.Fatal(err)
		}
		if inserted, err := store.Enqueue(ctx, value); err != nil || !inserted {
			t.Fatalf("race %d enqueue inserted=%v err=%v", i, inserted, err)
		}
		claims, err := store.Claim(ctx, iterationNow, time.Minute, 1)
		if err != nil || len(claims) != 1 || claims[0].ID != value.ID {
			t.Fatalf("race %d claim=%+v err=%v", i, claims, err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			results <- store.Cancel(ctx, outbox.CancelRequest{
				IdempotencyKey: key, LeaseToken: claims[0].LeaseToken,
				Disposition: outbox.DispositionExpired, TombstonePayload: tombstone, CanceledAt: iterationNow.Add(time.Second),
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			results <- store.MarkApplied(ctx, value.ID, claims[0].LeaseToken, iterationNow.Add(time.Second))
		}()
		close(start)
		wg.Wait()
		close(results)
		var succeeded int
		for result := range results {
			if result == nil {
				succeeded++
				continue
			}
			if !errors.Is(result, outbox.ErrLeaseLost) && !errors.Is(result, outbox.ErrInvalidTransition) {
				t.Fatalf("race %d unexpected result: %v", i, result)
			}
		}
		if succeeded != 1 {
			t.Fatalf("race %d successful transitions=%d", i, succeeded)
		}
		var finalStatus string
		if err := pool.QueryRow(ctx, `SELECT status FROM outbox_messages WHERE id=$1`, value.ID).Scan(&finalStatus); err != nil {
			t.Fatal(err)
		}
		if finalStatus != "canceled" && finalStatus != "applied" {
			t.Fatalf("race %d final status=%s", i, finalStatus)
		}
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("outbox cancellation race exceeded deadline: %v", err)
	}
}

func outboxIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL outbox integration suite not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("outbox_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
