package app

import (
	"context"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	platformpostgres "github.com/edu-agent/edu-agent/server/internal/platform/postgres"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypostgres "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
)

const privacyGrantIssuer = "edu-agentd/privacy-grant"

// CreatePrivacyErasureGrant is a local management entry point. It only opens the
// database, verifies the schema, issues one device-bound grant, and closes it.
func CreatePrivacyErasureGrant(ctx context.Context, cfg config.Config, deviceID string) (string, time.Time, error) {
	pool, err := platformpostgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return "", time.Time{}, err
	}
	defer pool.Close()
	if cfg.MigrateOnStart {
		if err := migrations.Run(ctx, pool); err != nil {
			return "", time.Time{}, fmt.Errorf("run database migrations: %w", err)
		}
	} else if err := migrations.Check(ctx, pool); err != nil {
		return "", time.Time{}, fmt.Errorf("check database migrations: %w", err)
	}
	service, err := privacy.NewErasureGrantService(privacypostgres.NewGrantStore(pool), privacy.ErasureGrantOptions{
		TTL: cfg.Privacy.ErasureGrantTTL, MaxAttempts: cfg.Privacy.ErasureGrantMaxAttempts,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	issued, err := service.Issue(ctx, deviceID, privacyGrantIssuer)
	if err != nil {
		return "", time.Time{}, err
	}
	return issued.Token, issued.ExpiresAt, nil
}
