package postgresstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLIdentityPairingConcurrentSingleUse(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	code, _, err := service.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 16
	start := make(chan struct{})
	results := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := range workers {
		go func() {
			defer wait.Done()
			<-start
			_, exchangeErr := service.ExchangePairingCode(ctx, code, fmt.Sprintf("Concurrent device %d", index))
			results <- exchangeErr
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	var succeeded, rejected int
	for result := range results {
		switch {
		case result == nil:
			succeeded++
		case errors.Is(result, identity.ErrInvalidPairingCode):
			rejected++
		default:
			t.Fatalf("concurrent exchange returned unexpected error: %v", result)
		}
	}
	if succeeded != 1 || rejected != workers-1 {
		t.Fatalf("concurrent exchanges succeeded=%d rejected=%d", succeeded, rejected)
	}

	lookupID := strings.SplitN(code, ".", 2)[0]
	var devices, tokens, consumed int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM devices),
		  (SELECT count(*) FROM device_tokens),
		  (SELECT count(*) FROM pairing_codes WHERE lookup_id=$1 AND consumed_at IS NOT NULL)`,
		lookupID).Scan(&devices, &tokens, &consumed); err != nil {
		t.Fatal(err)
	}
	if devices != 1 || tokens != 1 || consumed != 1 {
		t.Fatalf("concurrent exchange devices=%d tokens=%d consumed_codes=%d", devices, tokens, consumed)
	}
}

func TestPostgreSQLIdentityPairingReplayRejectedAfterCommit(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx := context.Background()

	code, _, err := service.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExchangePairingCode(ctx, code, "First device"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExchangePairingCode(ctx, code, "Replay device"); !errors.Is(err, identity.ErrInvalidPairingCode) {
		t.Fatalf("committed pairing replay error=%v", err)
	}

	var devices, tokens int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM devices),(SELECT count(*) FROM device_tokens)`).Scan(&devices, &tokens); err != nil {
		t.Fatal(err)
	}
	if devices != 1 || tokens != 1 {
		t.Fatalf("pairing replay created devices=%d tokens=%d", devices, tokens)
	}
}

func TestPostgreSQLIdentityPairingTransactionRollbackAtEveryWrite(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL identity integration suite not run")
	}
	tests := []struct {
		name          string
		createTrigger string
		dropTrigger   string
	}{
		{
			name:          "device insert",
			createTrigger: `CREATE TRIGGER zz_injected_identity_exchange_failure BEFORE INSERT ON devices FOR EACH ROW EXECUTE FUNCTION fail_identity_exchange_write()`,
			dropTrigger:   `DROP TRIGGER zz_injected_identity_exchange_failure ON devices`,
		},
		{
			name:          "token insert",
			createTrigger: `CREATE TRIGGER zz_injected_identity_exchange_failure BEFORE INSERT ON device_tokens FOR EACH ROW EXECUTE FUNCTION fail_identity_exchange_write()`,
			dropTrigger:   `DROP TRIGGER zz_injected_identity_exchange_failure ON device_tokens`,
		},
		{
			name:          "offline credential trigger insert",
			createTrigger: `CREATE TRIGGER zz_injected_identity_exchange_failure BEFORE INSERT ON offline_device_credentials FOR EACH ROW EXECUTE FUNCTION fail_identity_exchange_write()`,
			dropTrigger:   `DROP TRIGGER zz_injected_identity_exchange_failure ON offline_device_credentials`,
		},
		{
			name:          "offline sequence head trigger insert",
			createTrigger: `CREATE TRIGGER zz_injected_identity_exchange_failure BEFORE INSERT ON offline_device_sequence_heads FOR EACH ROW EXECUTE FUNCTION fail_identity_exchange_write()`,
			dropTrigger:   `DROP TRIGGER zz_injected_identity_exchange_failure ON offline_device_sequence_heads`,
		},
		{
			name:          "code consume",
			createTrigger: `CREATE TRIGGER zz_injected_identity_exchange_failure BEFORE UPDATE OF consumed_at ON pairing_codes FOR EACH ROW EXECUTE FUNCTION fail_identity_exchange_write()`,
			dropTrigger:   `DROP TRIGGER zz_injected_identity_exchange_failure ON pairing_codes`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := identityIntegrationPool(t)
			service := identityIntegrationService(t, pool)
			ctx := context.Background()
			code, _, err := service.CreatePairingCode(ctx)
			if err != nil {
				t.Fatal(err)
			}
			lookupID := strings.SplitN(code, ".", 2)[0]

			if _, err := pool.Exec(ctx, `
				CREATE FUNCTION fail_identity_exchange_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
				  RAISE EXCEPTION 'injected identity exchange write failure';
				END $$`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, test.createTrigger); err != nil {
				t.Fatal(err)
			}
			if _, err := service.ExchangePairingCode(ctx, code, "Rolled back device"); err == nil || errors.Is(err, identity.ErrInvalidPairingCode) {
				t.Fatalf("injected exchange failure error=%v", err)
			}

			var devices, tokens, credentials, sequences, attempts int
			var consumedAt *time.Time
			if err := pool.QueryRow(ctx, `
				SELECT
				  (SELECT count(*) FROM devices),
				  (SELECT count(*) FROM device_tokens),
				  (SELECT count(*) FROM offline_device_credentials),
				  (SELECT count(*) FROM offline_device_sequence_heads),
				  attempts,consumed_at
				FROM pairing_codes WHERE lookup_id=$1`, lookupID).Scan(
				&devices, &tokens, &credentials, &sequences, &attempts, &consumedAt,
			); err != nil {
				t.Fatal(err)
			}
			if devices != 0 || tokens != 0 || credentials != 0 || sequences != 0 || attempts != 0 || consumedAt != nil {
				t.Fatalf("failed exchange left devices=%d tokens=%d credentials=%d sequences=%d attempts=%d consumed_at=%v", devices, tokens, credentials, sequences, attempts, consumedAt)
			}

			if _, err := pool.Exec(ctx, test.dropTrigger); err != nil {
				t.Fatal(err)
			}
			if _, err := service.ExchangePairingCode(ctx, code, "Recovered device"); err != nil {
				t.Fatalf("same pairing code did not recover after rollback: %v", err)
			}
			if err := pool.QueryRow(ctx, `
				SELECT
				  (SELECT count(*) FROM devices),
				  (SELECT count(*) FROM device_tokens),
				  (SELECT count(*) FROM offline_device_credentials),
				  (SELECT count(*) FROM offline_device_sequence_heads),
				  attempts,consumed_at
				FROM pairing_codes WHERE lookup_id=$1`, lookupID).Scan(
				&devices, &tokens, &credentials, &sequences, &attempts, &consumedAt,
			); err != nil {
				t.Fatal(err)
			}
			if devices != 1 || tokens != 1 || credentials != 1 || sequences != 1 || attempts != 0 || consumedAt == nil {
				t.Fatalf("recovered exchange devices=%d tokens=%d credentials=%d sequences=%d attempts=%d consumed_at=%v", devices, tokens, credentials, sequences, attempts, consumedAt)
			}
		})
	}
}

func TestPostgreSQLIdentityRevokeTransactionRollsBackTokenFailure(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx := context.Background()
	code, _, err := service.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := service.ExchangePairingCode(ctx, code, "Revocation rollback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_identity_token_revoke() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  RAISE EXCEPTION 'injected token revocation failure';
		END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER zz_injected_identity_token_revoke
		BEFORE UPDATE OF revoked_at ON device_tokens
		FOR EACH ROW EXECUTE FUNCTION fail_identity_token_revoke()`); err != nil {
		t.Fatal(err)
	}

	if err := service.RevokeDevice(ctx, issued.Device.ID); err == nil {
		t.Fatal("injected token revocation unexpectedly succeeded")
	}
	var deviceRevoked, tokenRevoked bool
	if err := pool.QueryRow(ctx, `
		SELECT d.revoked_at IS NOT NULL,dt.revoked_at IS NOT NULL
		FROM devices d JOIN device_tokens dt ON dt.device_id=d.id WHERE d.id=$1`, issued.Device.ID).Scan(
		&deviceRevoked, &tokenRevoked,
	); err != nil {
		t.Fatal(err)
	}
	if deviceRevoked || tokenRevoked {
		t.Fatalf("failed revoke left device_revoked=%v token_revoked=%v", deviceRevoked, tokenRevoked)
	}
	if _, err := service.Authenticate(ctx, issued.Token, "devices:read"); err != nil {
		t.Fatalf("rolled-back revoke invalidated credential: %v", err)
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER zz_injected_identity_token_revoke ON device_tokens`); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeDevice(ctx, issued.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, issued.Token, "devices:read"); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("committed revoke authentication error=%v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT d.revoked_at IS NOT NULL,dt.revoked_at IS NOT NULL
		FROM devices d JOIN device_tokens dt ON dt.device_id=d.id WHERE d.id=$1`, issued.Device.ID).Scan(
		&deviceRevoked, &tokenRevoked,
	); err != nil {
		t.Fatal(err)
	}
	if !deviceRevoked || !tokenRevoked {
		t.Fatalf("committed revoke left device_revoked=%v token_revoked=%v", deviceRevoked, tokenRevoked)
	}
}

func TestPostgreSQLIdentityRevokeAuthenticationRaceFencesAfterCommit(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for iteration := range 16 {
		code, _, err := service.CreatePairingCode(ctx)
		if err != nil {
			t.Fatal(err)
		}
		issued, err := service.ExchangePairingCode(ctx, code, fmt.Sprintf("Race device %d", iteration))
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		authResult := make(chan error, 1)
		revokeResult := make(chan error, 1)
		go func() {
			<-start
			_, authenticateErr := service.Authenticate(ctx, issued.Token, "devices:read")
			authResult <- authenticateErr
		}()
		go func() {
			<-start
			revokeResult <- service.RevokeDevice(ctx, issued.Device.ID)
		}()
		close(start)

		if err := <-revokeResult; err != nil {
			t.Fatalf("race %d revoke: %v", iteration, err)
		}
		if err := <-authResult; err != nil && !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("race %d authentication: %v", iteration, err)
		}
		if _, err := service.Authenticate(ctx, issued.Token, "devices:read"); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("race %d post-revoke authentication error=%v", iteration, err)
		}

		var deviceRevoked, tokenRevoked bool
		if err := pool.QueryRow(ctx, `
			SELECT d.revoked_at IS NOT NULL,dt.revoked_at IS NOT NULL
			FROM devices d JOIN device_tokens dt ON dt.device_id=d.id WHERE d.id=$1`, issued.Device.ID).Scan(
			&deviceRevoked, &tokenRevoked,
		); err != nil {
			t.Fatal(err)
		}
		if !deviceRevoked || !tokenRevoked {
			t.Fatalf("race %d device_revoked=%v token_revoked=%v", iteration, deviceRevoked, tokenRevoked)
		}
	}
}

func identityIntegrationService(t *testing.T, pool *pgxpool.Pool) *identity.Service {
	t.Helper()
	service, err := identity.NewService(postgresstore.New(pool), identity.Options{
		PairingCodeTTL:         10 * time.Minute,
		PairingCodeMaxAttempts: 3,
		LastUsedTouchInterval:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func identityIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL identity integration suite not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)

	schema := "identity_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`) })

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
