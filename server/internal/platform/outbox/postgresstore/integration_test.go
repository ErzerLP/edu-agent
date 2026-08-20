package postgresstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
