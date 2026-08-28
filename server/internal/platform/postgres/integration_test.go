package postgres_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitypostgres "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLMigrationPairingAndOutboxClaims(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL integration test not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("platform_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Check(ctx, pool); err != nil {
		t.Fatal(err)
	}

	identityService, err := identity.NewService(identitypostgres.New(pool), identity.Options{
		PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := identityService.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if _, err := identityService.ExchangePairingCode(ctx, code, fmt.Sprintf("device-%d", index)); err == nil {
				successes.Add(1)
			}
		}(i)
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("pairing code had %d successful consumers", successes.Load())
	}

	failureCode, _, err := identityService.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(failureCode, ".")
	wrongSecret, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	wrongSecret[0] ^= 1
	wrongCode := parts[0] + "." + base64.RawURLEncoding.EncodeToString(wrongSecret)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = identityService.ExchangePairingCode(ctx, wrongCode, "wrong-secret")
		}()
	}
	wait.Wait()
	var recordedAttempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM pairing_codes WHERE lookup_id=$1`, parts[0]).Scan(&recordedAttempts); err != nil {
		t.Fatal(err)
	}
	if recordedAttempts != 5 {
		t.Fatalf("concurrent pairing failures recorded %d attempts, want 5", recordedAttempts)
	}

	store := outboxpostgres.New(pool)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		message, err := outbox.NewMessage(outbox.NewMessageInput{
			BusinessType: "test.apply", AggregateID: fmt.Sprintf("aggregate-%d", i),
			IdempotencyKey: fmt.Sprintf("key-%d", i), Revision: 1, Generation: 1,
			Payload: []byte(`{"value":1}`), MaxAttempts: 3,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		inserted, err := store.Enqueue(ctx, message)
		if err != nil || !inserted {
			t.Fatalf("enqueue: inserted=%v err=%v", inserted, err)
		}
		if i == 0 {
			inserted, err = store.Enqueue(ctx, message)
			if err != nil || inserted {
				t.Fatalf("idempotency key was not unique: inserted=%v err=%v", inserted, err)
			}
		}
	}
	claimedMessages := make(chan outbox.Message, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			messages, err := store.Claim(ctx, now, time.Minute, 1)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			for _, message := range messages {
				claimedMessages <- message
			}
		}()
	}
	wait.Wait()
	close(claimedMessages)
	seen := map[string]outbox.Message{}
	for message := range claimedMessages {
		if _, exists := seen[message.ID]; exists {
			t.Fatalf("message %s was claimed concurrently", message.ID)
		}
		if message.LeaseToken == "" {
			t.Fatalf("message %s was claimed without a lease token", message.ID)
		}
		seen[message.ID] = message
	}
	if len(seen) != 2 {
		t.Fatalf("expected two separately claimed messages, got %d", len(seen))
	}

	reclaimed, err := store.Claim(ctx, now.Add(2*time.Minute), time.Minute, 1)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaim expired lease: messages=%d err=%v", len(reclaimed), err)
	}
	previous := seen[reclaimed[0].ID]
	if previous.LeaseToken == reclaimed[0].LeaseToken {
		t.Fatal("reclaimed message reused its previous lease token")
	}
	if err := store.MarkApplied(ctx, previous.ID, previous.LeaseToken, now.Add(2*time.Minute)); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("stale worker retained lease ownership: %v", err)
	}
	if err := store.MarkApplied(ctx, reclaimed[0].ID, reclaimed[0].LeaseToken, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("current lease owner could not complete message: %v", err)
	}
}
