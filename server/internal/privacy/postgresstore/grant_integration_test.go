package postgresstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/google/uuid"
)

func TestPrivacyErasureGrantSingleUseDeviceBindingAndReplacement(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID, otherDeviceID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices(id,display_name,created_at) VALUES
		($1,'grant device',clock_timestamp()),($2,'other device',clock_timestamp())`, deviceID, otherDeviceID); err != nil {
		t.Fatal(err)
	}
	service, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Issue(ctx, deviceID, "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := base64.RawURLEncoding.DecodeString(first.Token)
	if err != nil || len(secret) != 32 {
		t.Fatalf("issued token is not 256 bits: len=%d err=%v", len(secret), err)
	}
	expectedHash := sha256.Sum256(secret)
	var storedHash string
	if err := pool.QueryRow(ctx, `SELECT encode(token_hash,'hex') FROM privacy_erasure_grants WHERE device_id=$1`, deviceID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != hex.EncodeToString(expectedHash[:]) || storedHash == first.Token {
		t.Fatal("grant store did not persist only the SHA-256 token hash")
	}
	if err := service.Consume(ctx, otherDeviceID, first.Token); !errors.Is(err, privacy.ErrErasureGrantInvalid) {
		t.Fatalf("wrong-device grant did not use generic error: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE devices SET revoked_at=clock_timestamp() WHERE id=$1`, otherDeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Issue(ctx, otherDeviceID, "integration-test"); !errors.Is(err, privacy.ErrErasureGrantDeviceUnavailable) {
		t.Fatalf("revoked device received a grant: %v", err)
	}

	second, err := service.Issue(ctx, deviceID, "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	var replaced int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM privacy_erasure_grants WHERE device_id=$1 AND consumed_at IS NOT NULL`, deviceID).Scan(&replaced); err != nil || replaced != 1 {
		t.Fatalf("active grant replacement count=%d err=%v", replaced, err)
	}
	if err := service.Consume(ctx, deviceID, first.Token); !errors.Is(err, privacy.ErrErasureGrantInvalid) {
		t.Fatalf("replaced grant was accepted: %v", err)
	}
	if err := service.Consume(ctx, deviceID, second.Token); err != nil {
		t.Fatalf("current grant was rejected: %v", err)
	}
	if err := service.Consume(ctx, deviceID, second.Token); !errors.Is(err, privacy.ErrErasureGrantInvalid) {
		t.Fatalf("grant replay did not use generic error: %v", err)
	}
	var attempts int
	var consumed bool
	if err := pool.QueryRow(ctx, `
		SELECT attempts,consumed_at IS NOT NULL
		FROM privacy_erasure_grants WHERE device_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, deviceID).
		Scan(&attempts, &consumed); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || !consumed {
		t.Fatalf("single-use grant audit attempts=%d consumed=%v", attempts, consumed)
	}
	if _, err := pool.Exec(ctx, `UPDATE privacy_erasure_grants SET consumed_at=NULL WHERE device_id=$1 AND consumed_at IS NOT NULL`, deviceID); err == nil {
		t.Fatal("database allowed a consumed grant to become reusable")
	}
}

func TestPrivacyErasureGrantExpiryAndAttemptBudget(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'grant expiry',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	expiringService, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(pool), privacy.ErasureGrantOptions{TTL: time.Millisecond, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := expiringService.Issue(ctx, deviceID, "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(0.01)`); err != nil {
		t.Fatal(err)
	}
	if err := expiringService.Consume(ctx, deviceID, expired.Token); !errors.Is(err, privacy.ErrErasureGrantInvalid) {
		t.Fatalf("expired grant did not use generic error: %v", err)
	}

	budgetService, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(pool), privacy.ErasureGrantOptions{TTL: time.Minute, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := budgetService.Issue(ctx, deviceID, "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	wrong := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x7f}, 32))
	for range 2 {
		if err := budgetService.Consume(ctx, deviceID, wrong); !errors.Is(err, privacy.ErrErasureGrantInvalid) {
			t.Fatalf("wrong grant did not use generic error: %v", err)
		}
	}
	if err := budgetService.Consume(ctx, deviceID, issued.Token); !errors.Is(err, privacy.ErrErasureGrantInvalid) {
		t.Fatalf("valid grant survived exhausted attempt budget: %v", err)
	}
	var attempts int
	var consumed bool
	if err := pool.QueryRow(ctx, `
		SELECT attempts,consumed_at IS NOT NULL
		FROM privacy_erasure_grants WHERE device_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, deviceID).
		Scan(&attempts, &consumed); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || consumed {
		t.Fatalf("attempt budget state attempts=%d consumed=%v", attempts, consumed)
	}
}

func TestPrivacyErasureGrantConcurrentConsume(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'grant concurrency',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	service, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := service.Issue(ctx, deviceID, "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if service.Consume(ctx, deviceID, issued.Token) == nil {
				successes.Add(1)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent grant successes=%d want=1", successes.Load())
	}
}
