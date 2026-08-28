package postgresstore_test

import (
	"context"
	"strings"
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

type scriptedManagedBackupVerifier struct {
	result   privacy.ManagedBackupVerificationResult
	before   func()
	calls    int
	requests []privacy.ManagedBackupVerificationRequest
}

func (v *scriptedManagedBackupVerifier) VerifyManagedBackups(_ context.Context, request privacy.ManagedBackupVerificationRequest) (privacy.ManagedBackupVerificationResult, error) {
	v.calls++
	v.requests = append(v.requests, request)
	if v.before != nil {
		v.before()
	}
	return v.result, nil
}

func TestRunManagedBackupVerificationFinalizesVerifiedAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	store, remote := prepareRemotePurgedErasure(t, pool)
	verifier := &scriptedManagedBackupVerifier{result: privacy.ManagedBackupVerificationResult{
		Status: privacy.StepNotApplicable, StableReason: "no_pre_barrier_managed_backup_artifacts",
		EvidenceDigest: strings.Repeat("33", 32), CompletedAt: time.Now().UTC(),
	}}
	verified, err := store.RunManagedBackupVerification(ctx, remote.ErasureID, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != privacy.StatusVerified || verifier.calls != 1 || len(verifier.requests) != 1 ||
		verifier.requests[0].ErasureID != remote.ErasureID || verifier.requests[0].LearnerGeneration != remote.LearnerGeneration {
		t.Fatalf("verified=%+v verifier=%+v", verified, verifier)
	}
	var managed privacy.StepReceipt
	var external privacy.StepReceipt
	for _, step := range verified.Steps {
		switch step.Store {
		case privacy.StoreManagedBackup:
			managed = step
		case privacy.StoreExternalProvider:
			external = step
		}
	}
	if managed.Status != privacy.StepNotApplicable || managed.Version != 2 || len(managed.EvidenceDigest) != 64 ||
		external.Status != privacy.StepUnsupported || external.Version != 1 {
		t.Fatalf("managed=%+v external=%+v", managed, external)
	}
	var stableReason string
	var receiptCount int
	if err := pool.QueryRow(ctx, `SELECT stable_reason FROM privacy_erasure_heads WHERE erasure_id=$1`, remote.ErasureID).Scan(&stableReason); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM privacy_erasure_step_receipts WHERE erasure_id=$1 AND store_kind='managed_backup'`, remote.ErasureID).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if stableReason != "active_stores_erased_backup_unrecoverable" || receiptCount != 2 {
		t.Fatalf("reason=%q receipt_count=%d", stableReason, receiptCount)
	}
	beforeVersion := verified.SummaryVersion
	replayed, err := store.RunManagedBackupVerification(ctx, remote.ErasureID, verifier)
	if err != nil || replayed.Status != privacy.StatusVerified || replayed.SummaryVersion != beforeVersion || verifier.calls != 1 {
		t.Fatalf("replayed=%+v calls=%d err=%v", replayed, verifier.calls, err)
	}
	var replayedReceiptCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM privacy_erasure_step_receipts WHERE erasure_id=$1 AND store_kind='managed_backup'`, remote.ErasureID).Scan(&replayedReceiptCount); err != nil {
		t.Fatal(err)
	}
	if replayedReceiptCount != receiptCount {
		t.Fatalf("verified replay appended receipt count=%d want=%d", replayedReceiptCount, receiptCount)
	}
}

func TestRunManagedBackupVerificationRejectsStaleReceiptCASAndResumesUnknown(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	store, remote := prepareRemotePurgedErasure(t, pool)
	verifier := &scriptedManagedBackupVerifier{result: privacy.ManagedBackupVerificationResult{
		Status: privacy.StepSucceeded, StableReason: "pre_barrier_managed_backups_unrecoverable_by_destroyed_keys",
		EvidenceDigest: strings.Repeat("44", 32), CompletedAt: time.Now().UTC(),
	}}
	verifier.before = func() { appendCompetingManagedBackupUnknown(t, pool, remote.ErasureID) }
	latest, err := store.RunManagedBackupVerification(ctx, remote.ErasureID, verifier)
	if privacy.ErrorCode(err) != privacy.CodeReceiptNotCurrent {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	var managed privacy.StepReceipt
	for _, step := range latest.Steps {
		if step.Store == privacy.StoreManagedBackup {
			managed = step
		}
	}
	if managed.Status != privacy.StepUnknown || managed.Version != 2 {
		t.Fatalf("managed after CAS=%+v", managed)
	}
	verifier.before = nil
	resumed, err := store.RunManagedBackupVerification(ctx, remote.ErasureID, verifier)
	if err != nil || resumed.Status != privacy.StatusVerified || verifier.calls != 2 {
		t.Fatalf("resumed=%+v calls=%d err=%v", resumed, verifier.calls, err)
	}
	for _, step := range resumed.Steps {
		if step.Store == privacy.StoreManagedBackup && (step.Status != privacy.StepSucceeded || step.Version != 3) {
			t.Fatalf("resumed managed receipt=%+v", step)
		}
	}
}

func prepareRemotePurgedErasure(t *testing.T, pool *pgxpool.Pool) (*privacydb.Store, privacy.ErasureReceipt) {
	t.Helper()
	ctx := context.Background()
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'verification actor',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	tutoringStore := tutoringdb.New(pool)
	store := privacydb.New(pool,
		privacydb.WithReadPermits(privacy.NewReadPermitManager()),
		privacydb.WithLocalOwner(identitydb.New(pool)),
		privacydb.WithLocalOwner(knowledgedb.New(pool)),
		privacydb.WithLocalOwner(learningdb.New(pool, tutoringStore)),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memorydb.New(pool)),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	)
	now := time.Now().UTC()
	barrier, err := store.CommitBarrier(ctx, privacy.ErasureRequest{
		DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
		ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	eraser := &scriptedRemoteEraser{results: []privacy.RemoteEraseResult{{
		Status: privacy.StepSucceeded, StableReason: "nocturne_absence_verified",
		EvidenceDigest: strings.Repeat("22", 32), CompletedAt: time.Now().UTC(),
	}}}
	remote, err := store.RunNocturneErase(ctx, barrier.ErasureID, eraser)
	if err != nil || remote.Status != privacy.StatusRemotePurged {
		t.Fatalf("remote=%+v err=%v", remote, err)
	}
	return store, remote
}

func appendCompetingManagedBackupUnknown(t *testing.T, pool *pgxpool.Pool, erasureID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var receiptID string
	var version int64
	var scope []byte
	if err := tx.QueryRow(ctx, `
		SELECT r.id,r.version,r.scope_digest
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1 AND h.store_kind='managed_backup'
		FOR UPDATE OF h`, erasureID).Scan(&receiptID, &version, &scope); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	newReceiptID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,
			stable_reason,verification_method,evidence_digest)
		VALUES($1,$2,'managed_backup',$3,$4,$5,$5,'unknown',
			'competing_inventory_unavailable','test_competing_receipt',decode(repeat('55',32),'hex'))`,
		newReceiptID, erasureID, version+1, scope, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE privacy_erasure_receipt_heads
		SET current_receipt_id=$3,current_version=$4,updated_at=$5
		WHERE erasure_id=$1 AND store_kind='managed_backup' AND current_receipt_id=$2`,
		erasureID, receiptID, newReceiptID, version+1, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
