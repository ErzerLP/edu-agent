package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLDisabledNocturneErasureConvergesWithoutHistoryAndAllowsSecond(t *testing.T) {
	pool := appIntegrationPool(t)
	ctx := context.Background()
	stores := newApplicationStores(pool)
	bridge, err := composeMemoryBridge(pool, stores, bridgeTestConfig(t, false), memoryBridgeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'disabled erasure actor',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}

	for generation := int64(1); generation <= 2; generation++ {
		now := time.Now().UTC()
		barrier, err := bridge.privacyStore.CommitBarrier(ctx, privacy.ErasureRequest{
			DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID,
			ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
			ManagedBackupUnrecoverableAfter:  now.Add(24 * time.Hour),
			ExpectedCurrentLearnerGeneration: generation,
		})
		if err != nil {
			t.Fatalf("generation %d barrier: %v", generation, err)
		}
		verified, err := resumePrivacyErasure(ctx, bridge.privacyService, barrier)
		if err != nil {
			t.Fatalf("generation %d resume: %v", generation, err)
		}
		if verified.Status != privacy.StatusVerified || verified.LearnerGeneration != generation+1 {
			t.Fatalf("generation %d receipt=%+v", generation, verified)
		}
		for _, step := range verified.Steps {
			switch step.Store {
			case privacy.StoreNocturnePaths, privacy.StoreNocturneOrphanHistory,
				privacy.StoreNocturneSnapshotChangeset, privacy.StoreManagedBackup:
				if step.Status != privacy.StepNotApplicable {
					t.Fatalf("generation %d store %s status=%s", generation, step.Store, step.Status)
				}
			}
		}
	}
}

type appUnknownRemoteEraser struct{ calls int }

func (e *appUnknownRemoteEraser) Erase(context.Context, privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
	e.calls++
	return privacy.RemoteEraseResult{
		Status: privacy.StepUnknown, StableReason: "nocturne_unavailable",
		EvidenceDigest: strings.Repeat("ab", 32), CompletedAt: time.Now().UTC(),
	}, nil
}

func TestPostgreSQLPrivacyResumeAfterRestartRunsLocalScrubWithoutNocturne(t *testing.T) {
	for name, preflightErr := range map[string]error{
		"unavailable":       errors.New("sidecar unavailable"),
		"contract_mismatch": appPreflightError{category: "contract_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			pool := appIntegrationPool(t)
			ctx := context.Background()
			stores := newApplicationStores(pool)
			initial, err := composeMemoryBridge(pool, stores, bridgeTestConfig(t, false), memoryBridgeDependencies{})
			if err != nil {
				t.Fatal(err)
			}
			deviceID := uuid.NewString()
			if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'restart private label',clock_timestamp())`, deviceID); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			barrier, err := initial.privacyStore.CommitBarrier(ctx, privacy.ErasureRequest{
				DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID,
				ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
				ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
			})
			if err != nil || barrier.Status != privacy.StatusBarrierCommitted {
				t.Fatalf("barrier=%+v err=%v", barrier, err)
			}

			// Recompose against the same database to model a process restart before local scrub.
			restartedStores := newApplicationStores(pool)
			restarted, err := composeMemoryBridge(pool, restartedStores, bridgeTestConfig(t, false), memoryBridgeDependencies{})
			if err != nil {
				t.Fatal(err)
			}
			probe := &appPreflightProbe{err: preflightErr}
			gate, err := newNocturnePreflightGate(probe, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyNocturneStartupPreflightForRuntime(ctx, pool, gate); err != nil {
				t.Fatalf("active erasure restart was blocked by preflight: %v", err)
			}
			eraser := &appUnknownRemoteEraser{}
			restarted.privacyService.preflight = gate
			restarted.privacyService.eraser = eraser
			processed, err := resumeActivePrivacyErasure(ctx, pool, restarted.privacyService)
			if err != nil || processed != 1 {
				t.Fatalf("privacy resume processed=%d err=%v", processed, err)
			}
			receipt, err := restarted.privacyStore.Receipt(ctx, barrier.ErasureID)
			if err != nil || receipt.Status != privacy.StatusPartial {
				t.Fatalf("privacy resume receipt=%+v err=%v", receipt, err)
			}
			var label string
			if err := pool.QueryRow(ctx, `SELECT display_name FROM devices WHERE id=$1`, deviceID).Scan(&label); err != nil || label != "[redacted]" {
				t.Fatalf("local scrub did not complete label=%q err=%v", label, err)
			}
			if name == "unavailable" && eraser.calls != 1 {
				t.Fatalf("transient unavailable did not call remote eraser: %d", eraser.calls)
			}
			if name == "contract_mismatch" && eraser.calls != 0 {
				t.Fatalf("contract mismatch reached remote mutation: %d", eraser.calls)
			}
			for _, step := range receipt.Steps {
				switch step.Store {
				case privacy.StoreIdentityMetadata, privacy.StoreKnowledgeContent, privacy.StoreKnowledgeIndex,
					privacy.StoreKnowledgeArtifacts, privacy.StoreLearningEventPayload, privacy.StoreLearningTypedPayload,
					privacy.StoreTutoringPayload, privacy.StoreInboxOutbox, privacy.StoreProjectionGenerations,
					privacy.StoreMemoryCandidateDelivery, privacy.StoreProcessCache:
					if step.Status != privacy.StepSucceeded {
						t.Fatalf("local step %s status=%s", step.Store, step.Status)
					}
				}
			}
		})
	}
}

func TestPostgreSQLDisabledNocturneManagedBackupHistoryRemainsUnknown(t *testing.T) {
	pool := appIntegrationPool(t)
	ctx := context.Background()
	keyID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_generation_keys(id,learner_generation,wrapped_key,key_digest,created_at)
		VALUES($1,1,decode(repeat('aa',48),'hex'),decode(repeat('bb',32),'hex'),clock_timestamp())`, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_managed_backup_inventory(
			id,relative_path,created_at,size_bytes,artifact_hash,learner_generation,wrapped_key_id,pruned_at)
		VALUES($1,'historical-disabled.backup.enc',clock_timestamp(),128,
		       decode(repeat('cc',32),'hex'),1,$2,clock_timestamp())`, uuid.NewString(), keyID); err != nil {
		t.Fatal(err)
	}
	stores := newApplicationStores(pool)
	bridge, err := composeMemoryBridge(pool, stores, bridgeTestConfig(t, false), memoryBridgeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'disabled history actor',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := bridge.privacyStore.CommitBarrier(ctx, privacy.ErasureRequest{
		DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
		ManagedBackupUnrecoverableAfter:  now.Add(24 * time.Hour),
		ExpectedCurrentLearnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := resumePrivacyErasure(ctx, bridge.privacyService, barrier)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Status != privacy.StatusPartial {
		t.Fatalf("receipt=%+v", partial)
	}
	for _, step := range partial.Steps {
		switch step.Store {
		case privacy.StoreNocturnePaths, privacy.StoreNocturneOrphanHistory, privacy.StoreNocturneSnapshotChangeset:
			if step.Status != privacy.StepUnknown {
				t.Fatalf("store %s status=%s", step.Store, step.Status)
			}
		case privacy.StoreManagedBackup:
			if step.Status != privacy.StepPending {
				t.Fatalf("managed backup status=%s", step.Status)
			}
		}
	}
}

func appIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "app_runtime_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, pool); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
	})
	return pool
}
