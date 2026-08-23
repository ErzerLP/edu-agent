package postgresstore_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const integrationDevice = "10000000-0000-4000-8000-000000000001"

func TestPostgreSQLPrivacyRedactionMaterializesAllSentAttemptsAndFencesUnsent(t *testing.T) {
	pool, store, ctx, _ := memoryHarness(t)
	states := []memory.AttemptState{
		memory.AttemptSent,
		memory.AttemptUnknown,
		memory.AttemptReconciling,
		memory.AttemptConfirmed,
		memory.AttemptFenced,
	}
	for index, state := range states {
		plan := automaticPlan(dbClock(t, pool))
		plan.Candidate.ValidUntil = plan.Candidate.CreatedAt.Add(time.Hour)
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
		attempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
			AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
			From: memory.AttemptPrepared, To: memory.AttemptSent,
			BootEpoch: fmt.Sprintf("privacy-sent-%d", index), At: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if state != memory.AttemptSent {
			var updateErr error
			if state == memory.AttemptConfirmed || state == memory.AttemptFenced {
				_, updateErr = pool.Exec(ctx, `
					UPDATE memory_delivery_attempt_heads
					SET state=$2,lease_token=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
					WHERE attempt_id=$1`, attempt.ID, state)
			} else {
				_, updateErr = pool.Exec(ctx, `
					UPDATE memory_delivery_attempt_heads
					SET state=$2,updated_at=clock_timestamp()
					WHERE attempt_id=$1`, attempt.ID, state)
			}
			if updateErr != nil {
				t.Fatal(updateErr)
			}
			if _, err := pool.Exec(ctx, `UPDATE memory_delivery_heads SET attempt_state=$2 WHERE delivery_id=$1`, plan.DeliveryID, state); err != nil {
				t.Fatal(err)
			}
		}
	}
	unsent := automaticPlan(dbClock(t, pool))
	unsent.Candidate.ValidUntil = unsent.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, unsent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimAttempt(ctx, unsent.DeliveryID, time.Time{}, time.Minute); err != nil {
		t.Fatal(err)
	}

	erasureID, generation, localReceipt := installMemoryPrivacyBarrier(t, pool)
	request := privacy.LocalRedactionRequest{
		ErasureID: erasureID, Store: privacy.StoreMemoryCandidateDelivery,
		ReceiptID: localReceipt, LearnerGeneration: generation,
	}
	if err := store.RedactTx(ctx, request); err != nil {
		t.Fatal(err)
	}
	var receiptsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory_delivery_receipts`).Scan(&receiptsBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.RedactTx(ctx, request); err != nil {
		t.Fatalf("idempotent privacy resume: %v", err)
	}

	var reconciliations, preserved, unsentReconciliations, activeAttempts, payloads, receiptsAfter int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_expiry_reconciliations),
		  (SELECT count(*)
		   FROM memory_expiry_reconciliations r
		   JOIN memory_deliveries d ON d.id=r.delivery_id
		   JOIN memory_delivery_attempts a
		     ON a.delivery_id=r.delivery_id AND a.attempt_token=r.attempt_token
		   JOIN memory_delivery_attempt_heads ah ON ah.attempt_id=a.id
		   WHERE r.reason IS NULL AND r.external_uri=d.external_uri
		     AND r.content_hash=d.payload_hash AND r.sent_boot_epoch=ah.boot_epoch
		     AND r.learner_generation=d.learner_generation
		     AND r.record_generation=d.record_generation),
		  (SELECT count(*) FROM memory_expiry_reconciliations WHERE delivery_id=$1),
		  (SELECT count(*)
		   FROM memory_delivery_attempt_heads ah
		   JOIN memory_delivery_attempts a ON a.id=ah.attempt_id
		   JOIN memory_deliveries d ON d.id=a.delivery_id
		   WHERE d.learner_generation<$2
		     AND ah.state IN ('prepared','sent','unknown','reconciling','confirmed')),
		  (SELECT count(*) FROM memory_delivery_payloads),
		  (SELECT count(*) FROM memory_delivery_receipts)`, unsent.DeliveryID, generation).
		Scan(&reconciliations, &preserved, &unsentReconciliations, &activeAttempts, &payloads, &receiptsAfter); err != nil {
		t.Fatal(err)
	}
	if reconciliations != len(states) || preserved != len(states) || unsentReconciliations != 0 || activeAttempts != 0 || payloads != 0 || receiptsAfter != receiptsBefore {
		t.Fatalf("privacy cleanup reconciliations=%d preserved=%d unsent=%d active=%d payloads=%d receipts=%d/%d",
			reconciliations, preserved, unsentReconciliations, activeAttempts, payloads, receiptsBefore, receiptsAfter)
	}
	var remoteHeads, localHeads, pendingRecords, deletedRecords int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE EXISTS (
		    SELECT 1 FROM memory_expiry_reconciliations r WHERE r.delivery_id=d.id
		  ) AND dh.status='fenced' AND dh.terminal_disposition='privacy_erasure'),
		  count(*) FILTER (WHERE NOT EXISTS (
		    SELECT 1 FROM memory_expiry_reconciliations r WHERE r.delivery_id=d.id
		  ) AND dh.status='deleted' AND dh.terminal_disposition='privacy_erasure'),
		  count(*) FILTER (WHERE EXISTS (
		    SELECT 1 FROM memory_expiry_reconciliations r WHERE r.delivery_id=d.id
		  ) AND rh.status='delete_pending'),
		  count(*) FILTER (WHERE NOT EXISTS (
		    SELECT 1 FROM memory_expiry_reconciliations r WHERE r.delivery_id=d.id
		  ) AND rh.status='deleted' AND rh.deleted_at IS NOT NULL)
		FROM memory_deliveries d
		JOIN memory_delivery_heads dh ON dh.delivery_id=d.id
		JOIN memory_record_heads rh ON rh.current_delivery_id=d.id
		WHERE d.learner_generation<$1`, generation).
		Scan(&remoteHeads, &localHeads, &pendingRecords, &deletedRecords); err != nil {
		t.Fatal(err)
	}
	if remoteHeads != len(states) || pendingRecords != len(states) || localHeads != 1 || deletedRecords != 1 {
		t.Fatalf("privacy heads remote=%d local=%d pending_records=%d deleted_records=%d", remoteHeads, localHeads, pendingRecords, deletedRecords)
	}
	if residual, err := store.VerifyRedacted(ctx, request); err != nil || residual != 0 {
		t.Fatalf("privacy verification residual=%d err=%v", residual, err)
	}
}

func TestPostgreSQLPrivacyInitializesTerminalReconciliationScopesAsSucceeded(t *testing.T) {
	pool, store, ctx, _ := memoryHarness(t)
	plan := automaticPlan(dbClock(t, pool))
	plan.Candidate.ValidUntil = plan.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "privacy-already-verified",
		At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_expiry_reconciliations(
			id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,
			sent_boot_epoch,learner_generation,record_generation,status,created_at,updated_at)
		SELECT $2,d.id,d.logical_memory_id,d.external_uri,d.payload_hash,$3,
		       'privacy-already-verified',d.learner_generation,d.record_generation,
		       'verified',clock_timestamp(),clock_timestamp()
		FROM memory_deliveries d WHERE d.id=$1`, plan.DeliveryID, uuid.NewString(), attempt.AttemptToken); err != nil {
		t.Fatal(err)
	}
	erasureID, generation, localReceipt := installMemoryPrivacyBarrier(t, pool)
	request := privacy.LocalRedactionRequest{
		ErasureID: erasureID, Store: privacy.StoreMemoryCandidateDelivery,
		ReceiptID: localReceipt, LearnerGeneration: generation,
	}
	if err := store.RedactTx(ctx, request); err != nil {
		t.Fatal(err)
	}
	var scopeCount, succeededCount, attemptCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE s.status='succeeded'),COALESCE(sum(s.attempt_count),0)
		FROM memory_erasure_delivery_scopes s
		JOIN memory_erasure_deliveries d ON d.id=s.erasure_delivery_id
		WHERE d.erasure_id=$1`, erasureID).Scan(&scopeCount, &succeededCount, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if scopeCount != 2 || succeededCount != 2 || attemptCount != 0 {
		t.Fatalf("terminal erasure scopes count=%d succeeded=%d attempts=%d", scopeCount, succeededCount, attemptCount)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_erasure_heads SET status='verified',updated_at=clock_timestamp() WHERE erasure_id=$1`, erasureID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=clock_timestamp()
		WHERE owner_kind='memory' AND active_erasure_id=$1`, erasureID); err != nil {
		t.Fatal(err)
	}
	secondErasureID, secondGeneration, secondReceipt := installMemoryPrivacyBarrier(t, pool)
	secondRequest := privacy.LocalRedactionRequest{
		ErasureID: secondErasureID, Store: privacy.StoreMemoryCandidateDelivery,
		ReceiptID: secondReceipt, LearnerGeneration: secondGeneration,
	}
	if err := store.RedactTx(ctx, secondRequest); err != nil {
		t.Fatal(err)
	}
	var sourceBindings, secondSucceeded int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_erasure_delivery_sources WHERE reconciliation_id=(SELECT id FROM memory_expiry_reconciliations WHERE delivery_id=$1)),
		  (SELECT count(*) FROM memory_erasure_delivery_scopes s
		   JOIN memory_erasure_deliveries d ON d.id=s.erasure_delivery_id
		   WHERE d.erasure_id=$2 AND s.status='succeeded')`, plan.DeliveryID, secondErasureID).
		Scan(&sourceBindings, &secondSucceeded); err != nil {
		t.Fatal(err)
	}
	if sourceBindings != 2 || secondSucceeded != 2 {
		t.Fatalf("second erasure source bindings=%d succeeded_scopes=%d", sourceBindings, secondSucceeded)
	}
}

func TestPostgreSQLMaintenanceSummaryScopesConflictAndPrivacyConvergence(t *testing.T) {
	pool, store, ctx, _ := memoryHarness(t)
	for range 2 {
		plan := automaticPlan(dbClock(t, pool))
		plan.Candidate.ValidUntil = plan.Candidate.CreatedAt.Add(time.Hour)
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
		attempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
			AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
			From: memory.AttemptPrepared, To: memory.AttemptSent,
			BootEpoch: "maintenance-privacy", At: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	erasureID, generation, localReceipt := installMemoryPrivacyBarrier(t, pool)
	request := privacy.LocalRedactionRequest{
		ErasureID: erasureID, Store: privacy.StoreMemoryCandidateDelivery,
		ReceiptID: localReceipt, LearnerGeneration: generation,
	}
	if err := store.RedactTx(ctx, request); err != nil {
		t.Fatal(err)
	}
	pathsReceipt := insertMaintenancePrivacyReceipt(t, pool, erasureID, "nocturne_paths")
	orphanReceipt := insertMaintenancePrivacyReceipt(t, pool, erasureID, "nocturne_orphan_history")
	pathsAuth := memory.MaintenanceAuthorization{ErasureID: erasureID, ReceiptID: pathsReceipt, TargetLearnerGeneration: generation}
	orphanAuth := memory.MaintenanceAuthorization{ErasureID: erasureID, ReceiptID: orphanReceipt, TargetLearnerGeneration: generation}

	summary, err := store.MaintenanceReconciliationSummary(ctx, pathsAuth)
	if err != nil || summary.Pending != 2 || summary.Conflicts != 0 {
		t.Fatalf("initial summary=%+v err=%v", summary, err)
	}
	conflict, err := store.ClaimMaintenanceExpiryReconciliation(ctx, pathsAuth, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeMaintenanceExpiryReconciliation(ctx, pathsAuth, memory.ReconciliationFinalization{
		ReconciliationID: conflict.ID, LeaseToken: conflict.LeaseToken, From: conflict.Status,
		Result: memory.ReconciliationConflictResult, ReceiptID: uuid.NewString(),
		Reason: "must_not_persist", EvidenceDigest: memory.SHA256String("conflict"), At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err = store.MaintenanceReconciliationSummary(ctx, pathsAuth)
	if err != nil || summary.Pending != 1 || summary.Conflicts != 1 {
		t.Fatalf("conflict summary=%+v err=%v", summary, err)
	}
	otherSummary, err := store.MaintenanceReconciliationSummary(ctx, orphanAuth)
	if err != nil || otherSummary.Pending != 2 || otherSummary.Conflicts != 0 {
		t.Fatalf("delivery-scoped summary=%+v err=%v", otherSummary, err)
	}

	succeeded, err := store.ClaimMaintenanceExpiryReconciliation(ctx, pathsAuth, time.Time{}, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(0.03)`); err != nil {
		t.Fatal(err)
	}
	rotatedPathsReceipt := rotateMaintenancePrivacyReceipt(t, pool, erasureID, "nocturne_paths", 2)
	rotatedPathsAuth := memory.MaintenanceAuthorization{ErasureID: erasureID, ReceiptID: rotatedPathsReceipt, TargetLearnerGeneration: generation}
	summary, err = store.MaintenanceReconciliationSummary(ctx, rotatedPathsAuth)
	if err != nil || summary.Pending != 1 || summary.Conflicts != 1 {
		t.Fatalf("rotated receipt summary=%+v err=%v", summary, err)
	}
	resumed, err := store.ClaimMaintenanceExpiryReconciliation(ctx, rotatedPathsAuth, time.Time{}, time.Minute)
	if err != nil || resumed.ID != succeeded.ID {
		t.Fatalf("rotated receipt claim=%+v want=%s err=%v", resumed, succeeded.ID, err)
	}
	if _, err := store.FinalizeMaintenanceExpiryReconciliation(ctx, rotatedPathsAuth, memory.ReconciliationFinalization{
		ReconciliationID: resumed.ID, LeaseToken: resumed.LeaseToken, From: resumed.Status,
		Result: memory.ReconciliationAbsenceResult, ReceiptID: uuid.NewString(),
		Reason: "must_not_persist", EvidenceDigest: memory.SHA256String("absence"), At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err = store.MaintenanceReconciliationSummary(ctx, rotatedPathsAuth)
	if err != nil || summary.Pending != 0 || summary.Conflicts != 1 {
		t.Fatalf("final summary=%+v err=%v", summary, err)
	}
	var deliveryStatus, disposition, recordStatus, reconciliationReason, receiptStatus string
	var deletedAt *time.Time
	var writeOpen bool
	var oldAppliedOrQueued int
	if err := pool.QueryRow(ctx, `
		SELECT dh.status,dh.terminal_disposition,rh.status,rh.deleted_at,
		       COALESCE(r.reason,''),receipt.status,
		       (SELECT write_open FROM privacy_owner_generation_gates WHERE owner_kind='memory'),
		       (SELECT count(*)
		        FROM memory_record_heads old
		        JOIN memory_deliveries d ON d.id=old.current_delivery_id
		        WHERE d.learner_generation<$2 AND old.status IN ('applied','queued'))
		FROM memory_expiry_reconciliations r
		JOIN memory_delivery_heads dh ON dh.delivery_id=r.delivery_id
		JOIN memory_record_heads rh ON rh.current_delivery_id=r.delivery_id
		JOIN memory_delivery_receipts receipt ON receipt.id=dh.current_receipt_id
		WHERE r.id=$1`, succeeded.ID, generation).
		Scan(&deliveryStatus, &disposition, &recordStatus, &deletedAt, &reconciliationReason,
			&receiptStatus, &writeOpen, &oldAppliedOrQueued); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "deleted" || disposition != "privacy_erasure" || recordStatus != "deleted" || deletedAt == nil ||
		reconciliationReason != "" || receiptStatus != "succeeded" || writeOpen || oldAppliedOrQueued != 0 {
		t.Fatalf("privacy convergence delivery=%s/%s record=%s deleted=%v reason=%q receipt=%s gate=%t old_active=%d",
			deliveryStatus, disposition, recordStatus, deletedAt, reconciliationReason, receiptStatus, writeOpen, oldAppliedOrQueued)
	}
	var conflictDeliveryStatus, conflictRecordStatus, conflictReceiptStatus, conflictReason string
	if err := pool.QueryRow(ctx, `
		SELECT dh.status,rh.status,receipt.status,COALESCE(r.reason,'')
		FROM memory_expiry_reconciliations r
		JOIN memory_delivery_heads dh ON dh.delivery_id=r.delivery_id
		JOIN memory_record_heads rh ON rh.current_delivery_id=r.delivery_id
		JOIN memory_delivery_receipts receipt ON receipt.id=dh.current_receipt_id
		WHERE r.id=$1`, conflict.ID).
		Scan(&conflictDeliveryStatus, &conflictRecordStatus, &conflictReceiptStatus, &conflictReason); err != nil {
		t.Fatal(err)
	}
	if conflictDeliveryStatus != "fenced" || conflictRecordStatus != "delete_pending" || conflictReceiptStatus != "partial" || conflictReason != "" {
		t.Fatalf("conflict state delivery=%s record=%s receipt=%s reason=%q", conflictDeliveryStatus, conflictRecordStatus, conflictReceiptStatus, conflictReason)
	}
	if _, err := store.MaintenanceReconciliationSummary(ctx, memory.MaintenanceAuthorization{
		ErasureID: uuid.NewString(), ReceiptID: pathsReceipt, TargetLearnerGeneration: generation,
	}); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("unrelated erasure summary err=%v", err)
	}
}

func TestPostgreSQLMaintenanceLatestRevisionPurgeConvergesHistoricalConflict(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	initial := automaticPlan(now)
	initial.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, initial); err != nil {
		t.Fatal(err)
	}
	finalizeAppliedDelivery(t, store, ctx, initial.DeliveryID, "revision-initial")
	correction := automaticCorrectionPlan(dbClock(t, pool), initial.LogicalMemoryID, 1, 1)
	if _, err := store.CreateCandidate(ctx, correction); err != nil {
		t.Fatal(err)
	}
	finalizeAppliedDelivery(t, store, ctx, correction.DeliveryID, "revision-correction")

	auth := startMaintenancePrivacyCleanup(t, pool, store)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var permit string
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,'memory',$3)::text`, auth.ErasureID, auth.TargetLearnerGeneration, auth.ReceiptID).Scan(&permit); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_expiry_reconciliations
		SET status='conflict',reason='historical_hash_conflict',updated_at=clock_timestamp()
		WHERE delivery_id=$1 AND status='pending'`, initial.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	latest, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, time.Minute)
	if err != nil || latest.DeliveryID != correction.DeliveryID || latest.RecordGeneration != 2 || latest.ContentHash != correction.Candidate.ContentHash {
		t.Fatalf("latest revision claim=%+v err=%v", latest, err)
	}
	latest, err = store.TransitionMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationTransition{
		ReconciliationID: latest.ID, LeaseToken: latest.LeaseToken, From: memory.ReconciliationReconciling,
		To: memory.ReconciliationDeletePending, At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationFinalization{
		ReconciliationID: latest.ID, LeaseToken: latest.LeaseToken, From: memory.ReconciliationDeletePending,
		Result: memory.ReconciliationDeleteResult, ReceiptID: uuid.NewString(), Reason: "remote_logical_delete_verified",
		EvidenceDigest: latest.ContentHash, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.MaintenanceReconciliationSummary(ctx, auth)
	if err != nil || summary.Pending != 0 || summary.Conflicts != 0 {
		t.Fatalf("converged summary=%+v err=%v", summary, err)
	}
	var verified, absenceVerified, deletedDeliveries int
	var recordStatus string
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_expiry_reconciliations WHERE logical_memory_id=$1 AND status='verified'),
		  (SELECT count(*) FROM memory_expiry_reconciliations WHERE logical_memory_id=$1 AND status='absence_verified'),
		  (SELECT count(*) FROM memory_delivery_heads h JOIN memory_deliveries d ON d.id=h.delivery_id WHERE d.logical_memory_id=$1 AND h.status='deleted'),
		  (SELECT status FROM memory_record_heads WHERE logical_memory_id=$1),
		  (SELECT deleted_at FROM memory_record_heads WHERE logical_memory_id=$1)`, initial.LogicalMemoryID).
		Scan(&verified, &absenceVerified, &deletedDeliveries, &recordStatus, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if verified != 1 || absenceVerified != 1 || deletedDeliveries != 2 || recordStatus != "deleted" || deletedAt == nil {
		t.Fatalf("verified=%d absence=%d deliveries=%d record=%s deleted_at=%v", verified, absenceVerified, deletedDeliveries, recordStatus, deletedAt)
	}
	openReceipt := completeMaintenancePrivacyReceipt(t, pool, auth)
	if err := store.OpenGenerationTx(ctx, pool, privacy.GenerationTransition{
		ErasureID: auth.ErasureID, FromGeneration: auth.TargetLearnerGeneration,
		TargetGeneration: auth.TargetLearnerGeneration, ReceiptID: openReceipt, At: dbClock(t, pool),
	}); err != nil {
		t.Fatal(err)
	}
	var readOpen, writeOpen bool
	if err := pool.QueryRow(ctx, `SELECT read_open,write_open FROM privacy_owner_generation_gates WHERE owner_kind='memory'`).Scan(&readOpen, &writeOpen); err != nil {
		t.Fatal(err)
	}
	if !readOpen || !writeOpen {
		t.Fatalf("memory gate did not reopen read=%v write=%v", readOpen, writeOpen)
	}
}

func TestPostgreSQLMaintenanceLatestAttemptPurgeConvergesSentAttempts(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: first.ID, AttemptToken: first.AttemptToken, LeaseToken: first.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "attempt-boot-1", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_delivery_attempt_heads SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE attempt_id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	reconciling, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AuthorizeAttemptRetry(ctx, memory.AttemptRetryAuthorization{
		AttemptID: reconciling.ID, AttemptToken: reconciling.AttemptToken, LeaseToken: reconciling.LeaseToken,
		From: memory.AttemptReconciling, ObservedBootEpoch: "attempt-boot-2", AbsenceObservations: 2,
		EvidenceDigest: memory.SHA256String("attempt-restart"), At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: second.ID, AttemptToken: second.AttemptToken, LeaseToken: second.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "attempt-boot-2", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := startMaintenancePrivacyCleanup(t, pool, store)
	latest, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, time.Minute)
	if err != nil || latest.AttemptToken != second.AttemptToken {
		t.Fatalf("latest attempt claim=%+v second=%+v err=%v", latest, second, err)
	}
	latest, err = store.TransitionMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationTransition{
		ReconciliationID: latest.ID, LeaseToken: latest.LeaseToken, From: memory.ReconciliationReconciling,
		To: memory.ReconciliationDeletePending, At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationFinalization{
		ReconciliationID: latest.ID, LeaseToken: latest.LeaseToken, From: memory.ReconciliationDeletePending,
		Result: memory.ReconciliationDeleteResult, ReceiptID: uuid.NewString(), Reason: "remote_logical_delete_verified",
		EvidenceDigest: latest.ContentHash, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.MaintenanceReconciliationSummary(ctx, auth)
	if err != nil || summary.Pending != 0 || summary.Conflicts != 0 {
		t.Fatalf("attempt convergence summary=%+v err=%v", summary, err)
	}
	var reconciliations, terminal, deletedDeliveries int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE status IN ('verified','absence_verified')),
		       (SELECT count(*) FROM memory_delivery_heads WHERE delivery_id=$1 AND status='deleted')
		FROM memory_expiry_reconciliations WHERE delivery_id=$1`, plan.DeliveryID).
		Scan(&reconciliations, &terminal, &deletedDeliveries); err != nil {
		t.Fatal(err)
	}
	if reconciliations != 2 || terminal != 2 || deletedDeliveries != 1 {
		t.Fatalf("reconciliations=%d terminal=%d deleted=%d", reconciliations, terminal, deletedDeliveries)
	}
}

func TestPostgreSQLErasureDeliveryIsSingleAndResumeKeepsOnePermit(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: first.ID, AttemptToken: first.AttemptToken, LeaseToken: first.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "erasure-permit-1", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE attempt_id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	reconciling, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AuthorizeAttemptRetry(ctx, memory.AttemptRetryAuthorization{
		AttemptID: reconciling.ID, AttemptToken: reconciling.AttemptToken, LeaseToken: reconciling.LeaseToken,
		From: memory.AttemptReconciling, ObservedBootEpoch: "erasure-permit-2", AbsenceObservations: 2,
		EvidenceDigest: memory.SHA256String("erasure-permit-restart"), At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: second.ID, AttemptToken: second.AttemptToken, LeaseToken: second.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "erasure-permit-2", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := startMaintenancePrivacyCleanup(t, pool, store)
	var erasureDeliveryID string
	var deliveries, sources, scopes int
	if err := pool.QueryRow(ctx, `
		SELECT min(erasure_delivery.id::text),count(DISTINCT erasure_delivery.id),
		       count(DISTINCT source.reconciliation_id),count(DISTINCT scope.store_kind)
		FROM memory_erasure_deliveries erasure_delivery
		JOIN memory_erasure_delivery_sources source ON source.erasure_delivery_id=erasure_delivery.id
		JOIN memory_erasure_delivery_scopes scope ON scope.erasure_delivery_id=erasure_delivery.id
		WHERE erasure_delivery.erasure_id=$1 AND erasure_delivery.logical_memory_id=$2`,
		auth.ErasureID, plan.LogicalMemoryID).Scan(&erasureDeliveryID, &deliveries, &sources, &scopes); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || sources != 2 || scopes != 2 {
		t.Fatalf("erasure delivery=%s deliveries=%d sources=%d scopes=%d", erasureDeliveryID, deliveries, sources, scopes)
	}
	claimed, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, 200*time.Millisecond)
	if err != nil || claimed.ErasureDeliveryID != erasureDeliveryID || claimed.AttemptToken != second.AttemptToken {
		t.Fatalf("first erasure claim=%+v err=%v", claimed, err)
	}
	if _, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, time.Minute); memory.ErrorCode(err) != memory.CodeNotFound {
		t.Fatalf("second active erasure permit err=%v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(0.25)`); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, time.Minute)
	if err != nil || resumed.ID != claimed.ID || resumed.ErasureDeliveryID != erasureDeliveryID {
		t.Fatalf("resumed erasure claim=%+v first=%+v err=%v", resumed, claimed, err)
	}
	var attempts, activePermits, attemptCount int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_erasure_delivery_attempts
		   WHERE erasure_delivery_id=$1 AND store_kind='nocturne_paths'),
		  (SELECT count(*) FROM memory_erasure_delivery_attempt_heads
		   WHERE erasure_delivery_id=$1 AND state IN ('reconciling','delete_pending')),
		  (SELECT attempt_count FROM memory_erasure_delivery_scopes
		   WHERE erasure_delivery_id=$1 AND store_kind='nocturne_paths')`,
		erasureDeliveryID).Scan(&attempts, &activePermits, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || activePermits != 1 || attemptCount != 2 {
		t.Fatalf("erasure attempts=%d active=%d claim_count=%d", attempts, activePermits, attemptCount)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM memory_erasure_deliveries WHERE id=$1`, erasureDeliveryID); err == nil {
		t.Fatal("immutable erasure delivery accepted delete")
	}
}

func TestPostgreSQLMaintenanceLatestExternalHashConflictBlocksHistoricalClaim(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	initial := automaticPlan(now)
	initial.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, initial); err != nil {
		t.Fatal(err)
	}
	finalizeAppliedDelivery(t, store, ctx, initial.DeliveryID, "conflict-initial")
	correction := automaticCorrectionPlan(dbClock(t, pool), initial.LogicalMemoryID, 1, 1)
	if _, err := store.CreateCandidate(ctx, correction); err != nil {
		t.Fatal(err)
	}
	finalizeAppliedDelivery(t, store, ctx, correction.DeliveryID, "conflict-correction")
	auth := startMaintenancePrivacyCleanup(t, pool, store)
	latest, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, time.Minute)
	if err != nil || latest.DeliveryID != correction.DeliveryID {
		t.Fatalf("latest conflict claim=%+v err=%v", latest, err)
	}
	if _, err := store.FinalizeMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationFinalization{
		ReconciliationID: latest.ID, LeaseToken: latest.LeaseToken, From: memory.ReconciliationReconciling,
		Result: memory.ReconciliationConflictResult, ReceiptID: uuid.NewString(), Reason: "maintenance_remote_hash_conflict",
		EvidenceDigest: latest.ContentHash, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.MaintenanceReconciliationSummary(ctx, auth)
	if err != nil || summary.Pending != 0 || summary.Conflicts != 1 {
		t.Fatalf("external conflict delivery summary=%+v err=%v", summary, err)
	}
	if _, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, time.Minute); memory.ErrorCode(err) != memory.CodeNotFound {
		t.Fatalf("historical hash claimed behind latest conflict: %v", err)
	}
	var deliveryStatus, recordStatus string
	if err := pool.QueryRow(ctx, `
		SELECT h.status,r.status
		FROM memory_delivery_heads h
		JOIN memory_deliveries d ON d.id=h.delivery_id
		JOIN memory_record_heads r ON r.logical_memory_id=d.logical_memory_id
		WHERE h.delivery_id=$1`, correction.DeliveryID).Scan(&deliveryStatus, &recordStatus); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "fenced" || recordStatus != "delete_pending" {
		t.Fatalf("external conflict delivery=%s record=%s", deliveryStatus, recordStatus)
	}
}

func TestPostgreSQLRestartAuthorizationCreatesSendableAttempt(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err = store.TransitionAttempt(ctx, memory.AttemptTransition{AttemptID: first.ID, AttemptToken: first.AttemptToken, LeaseToken: first.LeaseToken, From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "boot-1", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_delivery_attempt_heads SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE attempt_id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	reconciling, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil || reconciling.State != memory.AttemptReconciling {
		t.Fatalf("reconcile=%+v err=%v", reconciling, err)
	}
	evidence := memory.SHA256String("restart-evidence")
	replacement, err := store.AuthorizeAttemptRetry(ctx, memory.AttemptRetryAuthorization{AttemptID: reconciling.ID, AttemptToken: reconciling.AttemptToken, LeaseToken: reconciling.LeaseToken, From: memory.AttemptReconciling, ObservedBootEpoch: "boot-2", AbsenceObservations: 2, EvidenceDigest: evidence, At: time.Now().UTC()})
	if err != nil || replacement.ID == first.ID || replacement.State != memory.AttemptPrepared {
		t.Fatalf("replacement=%+v err=%v", replacement, err)
	}
	var oldState, headAttempt, parent, boot, storedEvidence string
	if err := pool.QueryRow(ctx, `SELECT old.state,dh.current_attempt_id::text,a.authorized_by_attempt_id::text,a.authorization_boot_epoch,encode(a.authorization_evidence_digest,'hex') FROM memory_delivery_attempt_heads old JOIN memory_delivery_heads dh ON dh.delivery_id=old.delivery_id JOIN memory_delivery_attempts a ON a.id=dh.current_attempt_id WHERE old.attempt_id=$1`, first.ID).Scan(&oldState, &headAttempt, &parent, &boot, &storedEvidence); err != nil {
		t.Fatal(err)
	}
	if oldState != "fenced" || headAttempt != replacement.ID || parent != first.ID || boot != "boot-2" || storedEvidence != evidence {
		t.Fatalf("restart state=%s head=%s parent=%s boot=%s evidence=%s", oldState, headAttempt, parent, boot, storedEvidence)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_delivery_attempt_heads SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE attempt_id=$1`, replacement.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil || reclaimed.ID != replacement.ID || reclaimed.State != memory.AttemptPrepared {
		t.Fatalf("authorized prepared reclaim=%+v err=%v", reclaimed, err)
	}
}

func TestPostgreSQLRecordDeleteCASReplayAndFinalize(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "delete-fixture", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptSucceeded, Reason: "applied", VerificationMethod: "fixture", ExternalNodeID: uuid.NewString(), ExternalMemoryID: 9, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	operation := memory.Operation{DeviceID: integrationDevice, OperationID: uuid.NewString(), RequestHash: memory.SHA256String("delete-request"), Kind: memory.OperationRecordDelete}
	deletePlan := memory.DeletePlan{Operation: operation, LogicalMemoryID: plan.LogicalMemoryID, ExpectedRevision: 1, ExpectedRecordGeneration: 1, DeliveryID: uuid.NewString(), DeliveryPayloadID: uuid.NewString(), ReceiptID: uuid.NewString(), OutboxID: uuid.NewString(), ValidUntil: now.Add(24 * time.Hour)}
	result, err := store.DeleteRecord(ctx, deletePlan)
	if err != nil || result.Record == nil || result.Delivery == nil || result.Record.RecordGeneration != 2 || result.Record.Status != memory.RecordDeletePending || result.Delivery.Kind != memory.DeliveryDelete {
		t.Fatalf("delete result=%+v err=%v", result, err)
	}
	replay, err := store.DeleteRecord(ctx, deletePlan)
	if err != nil || !replay.Replayed || replay.Delivery.ID != deletePlan.DeliveryID {
		t.Fatalf("delete replay=%+v err=%v", replay, err)
	}
	conflict := deletePlan
	conflict.Operation.OperationID = uuid.NewString()
	conflict.Operation.RequestHash = memory.SHA256String("stale-delete")
	conflict.DeliveryID = uuid.NewString()
	conflict.DeliveryPayloadID = uuid.NewString()
	conflict.ReceiptID = uuid.NewString()
	conflict.OutboxID = uuid.NewString()
	if _, err := store.DeleteRecord(ctx, conflict); memory.ErrorCode(err) != memory.CodeMemoryConflict && memory.ErrorCode(err) != memory.CodeInvalidMemoryTransition {
		t.Fatalf("stale delete err=%v", err)
	}
	var tombstones, payloads int
	var outboxStatus string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM memory_record_tombstones WHERE delivery_id=$1),(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$1),(SELECT status FROM outbox_messages WHERE id=$2)`, deletePlan.DeliveryID, deletePlan.OutboxID).Scan(&tombstones, &payloads, &outboxStatus); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 || payloads != 0 || outboxStatus != "pending" {
		t.Fatalf("tombstones=%d payloads=%d outbox=%s", tombstones, payloads, outboxStatus)
	}
	deleteAttempt, err := store.ClaimAttempt(ctx, deletePlan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deleteAttempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{AttemptID: deleteAttempt.ID, AttemptToken: deleteAttempt.AttemptToken, LeaseToken: deleteAttempt.LeaseToken, From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "delete", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: deleteAttempt.ID, AttemptToken: deleteAttempt.AttemptToken, LeaseToken: deleteAttempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeDeleted, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptSucceeded, Reason: "deleted", VerificationMethod: "fixture", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var recordStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM memory_record_heads WHERE logical_memory_id=$1`, plan.LogicalMemoryID).Scan(&recordStatus); err != nil || recordStatus != "deleted" {
		t.Fatalf("record status=%s err=%v", recordStatus, err)
	}
}

func TestPostgreSQLMaintenanceReconciliationBypassesOnlyClosedBusinessGate(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Second)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "old-boot", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ExpireDeliveries(ctx, time.Time{}, 1); err != nil || n != 1 {
		t.Fatalf("expire=%d err=%v", n, err)
	}
	erasureID, generation := insertBarrier(t, pool, "maintenance_reconcile")
	receiptID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO privacy_erasure_step_receipts(id,erasure_id,store_kind,version,scope_digest,started_at,status,stable_reason,verification_method) VALUES($1,$2,'nocturne_paths',1,decode(repeat('81',32),'hex'),clock_timestamp(),'pending','remote cleanup','maintenance')`, receiptID, erasureID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO privacy_erasure_receipt_heads(erasure_id,store_kind,current_receipt_id,current_version,updated_at) VALUES($1,'nocturne_paths',$2,1,clock_timestamp())`, erasureID, receiptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE privacy_owner_generation_gates SET learner_generation=$2,read_open=FALSE,write_open=FALSE,active_erasure_id=$1,updated_at=clock_timestamp() WHERE owner_kind IN ('memory','outbox')`, erasureID, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExpiryReconciliation(ctx, time.Time{}, time.Minute); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("normal claim crossed gate err=%v", err)
	}
	auth := memory.MaintenanceAuthorization{ErasureID: erasureID, ReceiptID: receiptID, TargetLearnerGeneration: generation}
	rec, err := store.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, time.Minute)
	if err != nil || rec.LearnerGeneration >= generation {
		t.Fatalf("maintenance claim=%+v err=%v", rec, err)
	}
	if _, err := store.FinalizeMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationFinalization{ReconciliationID: rec.ID, LeaseToken: rec.LeaseToken, From: rec.Status, Result: memory.ReconciliationAbsenceResult, ReceiptID: uuid.NewString(), Reason: "new_boot_double_absence", EvidenceDigest: memory.SHA256String("absence"), At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var deliveryStatus, recordStatus string
	if err := pool.QueryRow(ctx, `SELECT dh.status,rh.status FROM memory_delivery_heads dh JOIN memory_deliveries d ON d.id=dh.delivery_id JOIN memory_record_heads rh ON rh.logical_memory_id=d.logical_memory_id WHERE dh.delivery_id=$1`, plan.DeliveryID).Scan(&deliveryStatus, &recordStatus); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "deleted" || recordStatus != "deleted" {
		t.Fatalf("maintenance privacy finalization delivery=%s record=%s", deliveryStatus, recordStatus)
	}
}

func TestPostgreSQLMemoryAtomicReplayRaceAttemptAndExpiryFence(t *testing.T) {
	pool := memoryIntegrationPool(t)
	ctx := context.Background()
	now := time.Time{}
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	now = now.UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'memory-test',$2)`, integrationDevice, now); err != nil {
		t.Fatal(err)
	}
	store := postgresstore.New(pool)

	auto := automaticPlan(now)
	result, err := store.CreateCandidate(ctx, auto)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record == nil || result.Delivery == nil || result.Candidate.ContentStatus != "scrubbed" {
		t.Fatalf("unexpected automatic admission: %+v", result)
	}
	var candidatePayloads, deliveryPayloads, outboxBodies int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM memory_candidate_payloads WHERE candidate_id=$1),
		(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$2),
		(SELECT count(*) FROM outbox_messages WHERE idempotency_key=$3 AND payload ? 'content')`,
		auto.Candidate.ID, auto.DeliveryID, "memory.delivery:"+auto.DeliveryID).Scan(&candidatePayloads, &deliveryPayloads, &outboxBodies); err != nil {
		t.Fatal(err)
	}
	if candidatePayloads != 0 || deliveryPayloads != 1 || outboxBodies != 0 {
		t.Fatalf("payload transfer candidate=%d delivery=%d outbox_content=%d", candidatePayloads, deliveryPayloads, outboxBodies)
	}
	replay, err := store.CreateCandidate(ctx, auto)
	if err != nil || !replay.Replayed || replay.Delivery == nil || replay.Delivery.ID != auto.DeliveryID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	conflict := auto
	conflict.Operation.RequestHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	conflict.AutomaticDecision.RequestHash = conflict.Operation.RequestHash
	if _, err := store.CreateCandidate(ctx, conflict); memory.ErrorCode(err) != memory.CodeIdempotencyConflict {
		t.Fatalf("idempotency conflict err=%v", err)
	}
	kindReplay := memory.DecisionPlan{
		Operation: memory.Operation{
			DeviceID: integrationDevice, OperationID: auto.Operation.OperationID,
			RequestHash: auto.Operation.RequestHash, Kind: memory.OperationCandidateDecision,
		},
		CandidateID: auto.Candidate.ID, ExpectedRevision: 1,
		Decision: memory.CandidateDecision{
			ID: uuid.NewString(), CandidateID: auto.Candidate.ID, Revision: 2, Decision: memory.DecisionReject,
			Reason: "kind replay", ActorID: integrationDevice, ActorKind: "device",
			OperationID: auto.Operation.OperationID, RequestHash: auto.Operation.RequestHash, CreatedAt: now,
		},
	}
	if _, err := store.DecideCandidate(ctx, kindReplay); memory.ErrorCode(err) != memory.CodeIdempotencyConflict {
		t.Fatalf("operation kind replay err=%v", err)
	}

	pending := pendingPlan(now)
	if _, err := store.CreateCandidate(ctx, pending); err != nil {
		t.Fatal(err)
	}
	plans := []memory.DecisionPlan{admitPlan(now, pending.Candidate.ID), rejectPlan(now, pending.Candidate.ID)}
	var wg sync.WaitGroup
	errs := make(chan error, len(plans))
	for _, plan := range plans {
		wg.Add(1)
		go func(value memory.DecisionPlan) {
			defer wg.Done()
			_, err := store.DecideCandidate(context.Background(), value)
			errs <- err
		}(plan)
	}
	wg.Wait()
	close(errs)
	var succeeded, conflicted int
	for err := range errs {
		switch memory.ErrorCode(err) {
		case "":
			succeeded++
		case memory.CodeCandidateConflict:
			conflicted++
		default:
			t.Fatalf("decision race err=%v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("decision race succeeded=%d conflicted=%d", succeeded, conflicted)
	}

	type claimResult struct {
		claim memory.Attempt
		err   error
	}
	claimResults := make(chan claimResult, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, claimErr := store.ClaimAttempt(ctx, auto.DeliveryID, now.Add(time.Minute), time.Minute)
			claimResults <- claimResult{claim: claim, err: claimErr}
		}()
	}
	wg.Wait()
	close(claimResults)
	var claimed *memory.Attempt
	var claimConflicts int
	for result := range claimResults {
		if result.claim.ID != "" {
			claim := result.claim
			claimed = &claim
		}
		if result.err != nil {
			if memory.ErrorCode(result.err) != memory.CodeDeliveryConflict {
				t.Fatalf("attempt claim err=%v", result.err)
			}
			claimConflicts++
		}
	}
	if claimed == nil || claimConflicts != 1 {
		t.Fatalf("claim=%+v conflicts=%d", claimed, claimConflicts)
	}
	sent, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: claimed.ID, AttemptToken: claimed.AttemptToken, LeaseToken: claimed.LeaseToken, From: memory.AttemptPrepared,
		To: memory.AttemptSent, BootEpoch: "boot-1", At: now.Add(90 * time.Second),
	})
	if err != nil || sent.State != memory.AttemptSent {
		t.Fatalf("mark sent=%+v err=%v", sent, err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, auto.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ExpireDeliveries(ctx, time.Time{}, 10); err != nil || count != 1 {
		t.Fatalf("expire deliveries count=%d err=%v", count, err)
	}
	var deliveryStatus, outboxStatus string
	var reconciliations, payloads int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT status FROM memory_delivery_heads WHERE delivery_id=$1),
		(SELECT status FROM outbox_messages WHERE idempotency_key=$2),
		(SELECT count(*) FROM memory_expiry_reconciliations WHERE delivery_id=$1),
		(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$1)`,
		auto.DeliveryID, "memory.delivery:"+auto.DeliveryID).Scan(&deliveryStatus, &outboxStatus, &reconciliations, &payloads); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != string(memory.DeliveryStatusExpiryReconciling) || outboxStatus != "canceled" || reconciliations != 1 || payloads != 0 {
		t.Fatalf("expiry status=%s outbox=%s reconciliations=%d payloads=%d", deliveryStatus, outboxStatus, reconciliations, payloads)
	}
}

func TestPostgreSQLOperationReplayHonorsReadGateAndLazyExpiry(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)

	expired := pendingPlan(now)
	expired.Candidate.ValidUntil = now.Add(200 * time.Millisecond)
	if _, err := store.CreateCandidate(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, expired.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.CreateCandidate(ctx, expired)
	if err != nil || !replayed.Replayed || replayed.Candidate.Candidate.Status != memory.CandidateExpired || replayed.Candidate.ContentStatus != "scrubbed" || replayed.Candidate.ProposedContent != "" {
		t.Fatalf("expired replay=%+v err=%v", replayed, err)
	}
	var payloads, expiryDecisions int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM memory_candidate_payloads WHERE candidate_id=$1),
		(SELECT count(*) FROM memory_candidate_decisions WHERE candidate_id=$1 AND decision='expire' AND actor_kind='system')`,
		expired.Candidate.ID).Scan(&payloads, &expiryDecisions); err != nil {
		t.Fatal(err)
	}
	if payloads != 0 || expiryDecisions != 1 {
		t.Fatalf("expired replay payloads=%d decisions=%d", payloads, expiryDecisions)
	}

	automatic := automaticPlan(dbClock(t, pool))
	automatic.Candidate.ValidUntil = automatic.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, automatic); err != nil {
		t.Fatal(err)
	}
	pending := pendingPlan(dbClock(t, pool))
	if _, err := store.CreateCandidate(ctx, pending); err != nil {
		t.Fatal(err)
	}
	decision := rejectPlan(dbClock(t, pool), pending.Candidate.ID)
	if _, err := store.DecideCandidate(ctx, decision); err != nil {
		t.Fatal(err)
	}
	closeMemoryGate(t, pool, true)

	for _, test := range []struct {
		name string
		run  func() (memory.OperationResult, error)
	}{
		{name: "create", run: func() (memory.OperationResult, error) { return store.CreateCandidate(ctx, automatic) }},
		{name: "decision", run: func() (memory.OperationResult, error) { return store.DecideCandidate(ctx, decision) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.run()
			if memory.ErrorCode(err) != memory.CodeContentRedacted {
				t.Fatalf("closed replay result=%+v err=%v", result, err)
			}
			var memoryErr *memory.Error
			if !errors.As(err, &memoryErr) || memoryErr.CandidateID != "" || memoryErr.ExpectedRevision != 0 || memoryErr.CurrentRevision != 0 || result.Candidate.Candidate.ID != "" || result.Record != nil || result.Delivery != nil {
				t.Fatalf("closed replay leaked metadata result=%+v err=%+v", result, memoryErr)
			}
		})
	}
}

func TestPostgreSQLCreateAndDecisionUseDBClockAfterLockWait(t *testing.T) {
	t.Run("create generation lock", func(t *testing.T) {
		pool, store, ctx, now := memoryHarness(t)
		plan := automaticPlan(now)
		plan.Candidate.ValidUntil = now.Add(250 * time.Millisecond)
		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err := blocker.Exec(ctx, `SELECT owner_kind FROM privacy_owner_generation_gates WHERE owner_kind='memory' FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, err := store.CreateCandidate(context.Background(), plan); result <- err }()
		select {
		case err := <-result:
			t.Fatalf("create did not wait for generation lock: %v", err)
		case <-time.After(75 * time.Millisecond):
		}
		if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if memory.ErrorCode(err) != memory.CodeCandidateConflict || !strings.Contains(err.Error(), "candidate_expired") {
				t.Fatalf("create after ttl lock wait err=%v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("create remained blocked after generation lock release")
		}
		var candidates, candidatePayloads, deliveries, outboxRows int
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM memory_candidates WHERE id=$1),
			(SELECT count(*) FROM memory_candidate_payloads WHERE id=$2),
			(SELECT count(*) FROM memory_deliveries WHERE id=$3),
			(SELECT count(*) FROM outbox_messages WHERE id=$4)`,
			plan.Candidate.ID, plan.Candidate.PayloadID, plan.DeliveryID, plan.OutboxID).
			Scan(&candidates, &candidatePayloads, &deliveries, &outboxRows); err != nil {
			t.Fatal(err)
		}
		if candidates != 0 || candidatePayloads != 0 || deliveries != 0 || outboxRows != 0 {
			t.Fatalf("expired create residue candidate=%d payload=%d delivery=%d outbox=%d", candidates, candidatePayloads, deliveries, outboxRows)
		}
	})

	t.Run("decision candidate head lock", func(t *testing.T) {
		pool, store, ctx, now := memoryHarness(t)
		pending := pendingPlan(now)
		pending.Candidate.ValidUntil = now.Add(300 * time.Millisecond)
		if _, err := store.CreateCandidate(ctx, pending); err != nil {
			t.Fatal(err)
		}
		plan := admitPlan(now, pending.Candidate.ID)
		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err := blocker.Exec(ctx, `SELECT candidate_id FROM memory_candidate_heads WHERE candidate_id=$1 FOR UPDATE`, pending.Candidate.ID); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, err := store.DecideCandidate(context.Background(), plan); result <- err }()
		select {
		case err := <-result:
			t.Fatalf("decision did not wait for candidate head: %v", err)
		case <-time.After(75 * time.Millisecond):
		}
		if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, pending.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if memory.ErrorCode(err) != memory.CodeCandidateConflict || !strings.Contains(err.Error(), "candidate_expired") {
				t.Fatalf("decision after ttl lock wait err=%v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("decision remained blocked after candidate head release")
		}
		var status string
		var revision int64
		var payloads, logicalRows, deliveryRows, outboxRows, operationRows int
		if err := pool.QueryRow(ctx, `SELECT h.status,h.revision,
			(SELECT count(*) FROM memory_candidate_payloads WHERE candidate_id=h.candidate_id),
			(SELECT count(*) FROM memory_logical_memories WHERE id=$2),
			(SELECT count(*) FROM memory_deliveries WHERE id=$3),
			(SELECT count(*) FROM outbox_messages WHERE id=$4),
			(SELECT count(*) FROM memory_operation_inbox WHERE device_id=$5 AND operation_id=$6)
			FROM memory_candidate_heads h WHERE h.candidate_id=$1`, pending.Candidate.ID,
			plan.LogicalMemoryID, plan.DeliveryID, plan.OutboxID, plan.Operation.DeviceID, plan.Operation.OperationID).
			Scan(&status, &revision, &payloads, &logicalRows, &deliveryRows, &outboxRows, &operationRows); err != nil {
			t.Fatal(err)
		}
		if status != "expired" || revision != 2 || payloads != 0 || logicalRows != 0 || deliveryRows != 0 || outboxRows != 0 || operationRows != 0 {
			t.Fatalf("expired decision state=%s/%d payload=%d logical=%d delivery=%d outbox=%d operation=%d", status, revision, payloads, logicalRows, deliveryRows, outboxRows, operationRows)
		}
	})
}

func TestPostgreSQLAttemptBarrierLeaseAndAtomicOutcome(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)

	preSend := automaticPlan(now)
	preSend.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, preSend); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimAttempt(ctx, preSend.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	closeMemoryGate(t, pool, false)
	if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: claim.ID, AttemptToken: claim.AttemptToken, LeaseToken: claim.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "boot-pre-send", At: time.Now().UTC(),
	}); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("pre-send barrier transition err=%v", err)
	}
	var preSendState string
	var preSendPayloads int
	if err := pool.QueryRow(ctx, `SELECT state,(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$2) FROM memory_delivery_attempt_heads WHERE attempt_id=$1`, claim.ID, preSend.DeliveryID).Scan(&preSendState, &preSendPayloads); err != nil {
		t.Fatal(err)
	}
	if preSendState != "prepared" || preSendPayloads != 1 {
		t.Fatalf("barrier mutated pre-send attempt state=%s payloads=%d", preSendState, preSendPayloads)
	}
	openMemoryGate(t, pool)

	postSend := automaticPlan(dbClock(t, pool))
	postSend.Candidate.ValidUntil = postSend.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, postSend); err != nil {
		t.Fatal(err)
	}
	firstLease, err := store.ClaimAttempt(ctx, postSend.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: firstLease.ID, AttemptToken: firstLease.AttemptToken, LeaseToken: firstLease.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "boot-post-send", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_delivery_attempt_heads SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE attempt_id=$1`, firstLease.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimAttempt(ctx, postSend.DeliveryID, time.Time{}, time.Minute)
	if err != nil || reclaimed.ID != firstLease.ID || reclaimed.State != memory.AttemptReconciling {
		t.Fatalf("sent attempt was replaced: first=%+v reclaimed=%+v err=%v", firstLease, reclaimed, err)
	}
	var attemptHeadState, deliveryHeadState string
	if err := pool.QueryRow(ctx, `SELECT ah.state,dh.attempt_state
		FROM memory_delivery_attempt_heads ah
		JOIN memory_delivery_heads dh ON dh.delivery_id=ah.delivery_id
		WHERE ah.attempt_id=$1`, firstLease.ID).Scan(&attemptHeadState, &deliveryHeadState); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.CreateCandidate(ctx, postSend)
	if err != nil || loaded.Delivery == nil {
		t.Fatalf("load takeover delivery=%+v err=%v", loaded, err)
	}
	if attemptHeadState != "reconciling" || deliveryHeadState != "reconciling" || loaded.Delivery.AttemptState != memory.AttemptReconciling {
		t.Fatalf("takeover state attempt=%s delivery=%s loaded=%s", attemptHeadState, deliveryHeadState, loaded.Delivery.AttemptState)
	}
	oldOutcome := memory.AttemptOutcome{
		AttemptID: firstLease.ID, AttemptToken: firstLease.AttemptToken, LeaseToken: firstLease.LeaseToken,
		From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(),
		ReceiptStatus: memory.ReceiptSucceeded, Reason: "verified", VerificationMethod: "uri_hash",
		ExternalNodeID: uuid.NewString(), ExternalMemoryID: 11, At: time.Now().UTC(),
	}
	if _, err := store.FinalizeAttempt(ctx, oldOutcome); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("old lease finalized outcome: %v", err)
	}
	closeMemoryGate(t, pool, false)
	oldOutcome.From = memory.AttemptReconciling
	oldOutcome.LeaseToken = reclaimed.LeaseToken
	oldOutcome.ReceiptID = uuid.NewString()
	if _, err := store.FinalizeAttempt(ctx, oldOutcome); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("barrier outcome err=%v", err)
	}
	var deliveryStatus, recordStatus string
	var payloads int
	if err := pool.QueryRow(ctx, `SELECT h.status,rh.status,(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$1) FROM memory_delivery_heads h JOIN memory_deliveries d ON d.id=h.delivery_id JOIN memory_record_heads rh ON rh.logical_memory_id=d.logical_memory_id WHERE h.delivery_id=$1`, postSend.DeliveryID).Scan(&deliveryStatus, &recordStatus, &payloads); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "queued" || recordStatus != "queued" || payloads != 1 {
		t.Fatalf("barrier mutated outcome delivery=%s record=%s payloads=%d", deliveryStatus, recordStatus, payloads)
	}
	openMemoryGate(t, pool)

	appliedPlan := automaticPlan(dbClock(t, pool))
	appliedPlan.Candidate.ValidUntil = appliedPlan.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, appliedPlan); err != nil {
		t.Fatal(err)
	}
	appliedAttempt, err := store.ClaimAttempt(ctx, appliedPlan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: appliedAttempt.ID, AttemptToken: appliedAttempt.AttemptToken, LeaseToken: appliedAttempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "boot-applied", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	receiptID, nodeID := uuid.NewString(), uuid.NewString()
	confirmed, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{
		AttemptID: appliedAttempt.ID, AttemptToken: appliedAttempt.AttemptToken, LeaseToken: appliedAttempt.LeaseToken,
		From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: receiptID,
		ReceiptStatus: memory.ReceiptSucceeded, Reason: "remote_hash_verified", VerificationMethod: "uri_hash",
		ExternalNodeID: nodeID, ExternalMemoryID: 42, At: time.Now().UTC(),
	})
	if err != nil || confirmed.State != memory.AttemptConfirmed {
		t.Fatalf("atomic outcome=%+v err=%v", confirmed, err)
	}
	var deliveryReceipt, recordReceipt, externalNode string
	var externalMemory int64
	if err := pool.QueryRow(ctx, `SELECT h.status,h.current_receipt_id,rh.status,rh.receipt_id,rh.external_node_id,rh.external_memory_id,(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$1) FROM memory_delivery_heads h JOIN memory_deliveries d ON d.id=h.delivery_id JOIN memory_record_heads rh ON rh.logical_memory_id=d.logical_memory_id WHERE h.delivery_id=$1`, appliedPlan.DeliveryID).Scan(&deliveryStatus, &deliveryReceipt, &recordStatus, &recordReceipt, &externalNode, &externalMemory, &payloads); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "applied" || recordStatus != "applied" || deliveryReceipt != receiptID || recordReceipt != receiptID || externalNode != nodeID || externalMemory != 42 || payloads != 0 {
		t.Fatalf("non-atomic outcome delivery=%s/%s record=%s/%s node=%s memory=%d payloads=%d", deliveryStatus, deliveryReceipt, recordStatus, recordReceipt, externalNode, externalMemory, payloads)
	}
}

func TestPostgreSQLAllSentExpiryReconciliation(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(500 * time.Millisecond)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "boot-current", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	historicalID, historicalToken := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO memory_delivery_attempts(id,delivery_id,attempt_token,created_at) VALUES($1,$2,$3,clock_timestamp()-interval '1 second')`, historicalID, plan.DeliveryID, historicalToken); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memory_delivery_attempt_heads(attempt_id,delivery_id,state,boot_epoch,sent_at,error_category,updated_at) VALUES($1,$2,'fenced','boot-historical',clock_timestamp()-interval '1 second','historical_sent_fence',clock_timestamp())`, historicalID, plan.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ExpireDeliveries(ctx, time.Time{}, 10); err != nil || count != 1 {
		t.Fatalf("expire count=%d err=%v", count, err)
	}
	var reconciliations int
	var status string
	if err := pool.QueryRow(ctx, `SELECT count(*),(SELECT status FROM memory_delivery_heads WHERE delivery_id=$1) FROM memory_expiry_reconciliations WHERE delivery_id=$1`, plan.DeliveryID).Scan(&reconciliations, &status); err != nil {
		t.Fatal(err)
	}
	if reconciliations != 2 || status != "expiry_reconciling" {
		t.Fatalf("reconciliations=%d status=%s", reconciliations, status)
	}
	first, err := store.ClaimExpiryReconciliation(ctx, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{
		ReconciliationID: first.ID, LeaseToken: first.LeaseToken, From: first.Status,
		Result: memory.ReconciliationDeleteResult, ReceiptID: uuid.NewString(), Reason: "illegal_stage_skip", At: time.Now().UTC(),
	}); memory.ErrorCode(err) != memory.CodeInvalidRequest {
		t.Fatalf("reconciling accepted delete verification err=%v", err)
	}
	if _, err := store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{
		ReconciliationID: first.ID, LeaseToken: first.LeaseToken, From: first.Status,
		Result: memory.ReconciliationAbsenceResult, ReceiptID: uuid.NewString(), Reason: "new_epoch_double_404", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM memory_delivery_heads WHERE delivery_id=$1`, plan.DeliveryID).Scan(&status); err != nil || status != "expiry_reconciling" {
		t.Fatalf("delivery finalized before all sent tokens status=%s err=%v", status, err)
	}
	second, err := store.ClaimExpiryReconciliation(ctx, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err = store.TransitionExpiryReconciliation(ctx, memory.ReconciliationTransition{
		ReconciliationID: second.ID, LeaseToken: second.LeaseToken, From: second.Status,
		To: memory.ReconciliationDeletePending, At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{
		ReconciliationID: second.ID, LeaseToken: second.LeaseToken, From: second.Status,
		Result: memory.ReconciliationAbsenceResult, ReceiptID: uuid.NewString(), Reason: "illegal_stage_regression", At: time.Now().UTC(),
	}); memory.ErrorCode(err) != memory.CodeInvalidRequest {
		t.Fatalf("delete_pending accepted absence verification err=%v", err)
	}
	if _, err := store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{
		ReconciliationID: second.ID, LeaseToken: second.LeaseToken, From: second.Status,
		Result: memory.ReconciliationDeleteResult, ReceiptID: uuid.NewString(), Reason: "remote_delete_verified", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var recordStatus string
	if err := pool.QueryRow(ctx, `SELECT h.status,rh.status FROM memory_delivery_heads h JOIN memory_deliveries d ON d.id=h.delivery_id JOIN memory_record_heads rh ON rh.logical_memory_id=d.logical_memory_id WHERE h.delivery_id=$1`, plan.DeliveryID).Scan(&status, &recordStatus); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || recordStatus != "permanently_rejected" {
		t.Fatalf("verified expiry delivery=%s record=%s", status, recordStatus)
	}
}

func TestPostgreSQLCandidateReadGateLazyExpiryAndCursor(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := pendingPlan(now)
	plan.Candidate.ValidUntil = now.Add(200 * time.Millisecond)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	view, err := store.Candidate(ctx, plan.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Candidate.Status != memory.CandidateExpired || view.ContentStatus != "scrubbed" || view.ProposedContent != "" || view.ReadGeneration.LearnerGeneration != 1 {
		t.Fatalf("lazy expiry view=%+v", view)
	}
	var decisions, payloads int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM memory_candidate_decisions WHERE candidate_id=$1 AND decision='expire'),(SELECT count(*) FROM memory_candidate_payloads WHERE candidate_id=$1)`, plan.Candidate.ID).Scan(&decisions, &payloads); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || payloads != 0 {
		t.Fatalf("lazy expiry decisions=%d payloads=%d", decisions, payloads)
	}
	closeMemoryGate(t, pool, true)
	if _, err := store.Candidate(ctx, plan.Candidate.ID); memory.ErrorCode(err) != memory.CodeContentRedacted {
		t.Fatalf("closed detail gate err=%v", err)
	}
	if _, err := store.ListCandidates(ctx, memory.PageRequest{}); memory.ErrorCode(err) != memory.CodeContentRedacted {
		t.Fatalf("closed list gate err=%v", err)
	}
	openMemoryGate(t, pool)
	for range 2 {
		value := pendingPlan(dbClock(t, pool))
		if _, err := store.CreateCandidate(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.ListCandidates(ctx, memory.PageRequest{Limit: 1})
	if err != nil || page.NextCursor == "" || page.ReadGeneration.LearnerGeneration != 2 || page.ReadGeneration.MemoryGeneration != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err := store.ListCandidates(ctx, memory.PageRequest{Cursor: page.NextCursor}); err != nil {
		t.Fatalf("valid cursor err=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE privacy_owner_generation_gates SET learner_generation=learner_generation+1,updated_at=clock_timestamp() WHERE owner_kind='memory'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListCandidates(ctx, memory.PageRequest{Cursor: page.NextCursor}); memory.ErrorCode(err) != memory.CodeStaleCursor {
		t.Fatalf("generation-stale cursor err=%v", err)
	}
	encode := func(value any) string {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(body)
	}
	badCursors := []string{
		encode(map[string]any{"time": now.Format(time.RFC3339Nano), "id": strings.ToUpper("10000000-0000-4000-8000-00000000000a")}),
		encode(map[string]any{"time": now.Format("2006-01-02T15:04:05+00:00"), "id": uuid.NewString()}),
		encode(map[string]any{"time": now.Format(time.RFC3339Nano), "id": uuid.NewString(), "extra": true}),
	}
	for _, cursor := range badCursors {
		if _, err := store.ListCandidates(ctx, memory.PageRequest{Cursor: cursor}); memory.ErrorCode(err) != memory.CodeStaleCursor {
			t.Fatalf("cursor=%q err=%v", cursor, err)
		}
	}
}

func TestPostgreSQLLateCallbacksPreserveExpiryReconciliation(t *testing.T) {
	for _, test := range []struct {
		name     string
		finalize bool
	}{
		{name: "transition"},
		{name: "finalize unknown", finalize: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool, store, ctx, now := memoryHarness(t)
			plan := automaticPlan(now)
			plan.Candidate.ValidUntil = now.Add(250 * time.Millisecond)
			if _, err := store.CreateCandidate(ctx, plan); err != nil {
				t.Fatal(err)
			}
			attempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
				AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
				From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "late-callback", At: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.finalize {
				attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
					AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
					From: memory.AttemptSent, To: memory.AttemptUnknown, At: time.Now().UTC(),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if test.finalize {
				attempt, err = store.FinalizeAttempt(ctx, memory.AttemptOutcome{
					AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
					From: memory.AttemptUnknown, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(),
					ReceiptStatus: memory.ReceiptSucceeded, Reason: "late_remote_apply", VerificationMethod: "uri_hash",
					ExternalNodeID: uuid.NewString(), ExternalMemoryID: 99, At: time.Now().UTC(),
				})
			} else {
				attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
					AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
					From: memory.AttemptSent, To: memory.AttemptUnknown, At: time.Now().UTC(),
				})
			}
			if err != nil || attempt.State != memory.AttemptFenced {
				t.Fatalf("late callback attempt=%+v err=%v", attempt, err)
			}
			var status, outboxStatus string
			var reconciliations, payloads int
			if err := pool.QueryRow(ctx, `SELECT h.status,o.status,
				(SELECT count(*) FROM memory_expiry_reconciliations r WHERE r.delivery_id=h.delivery_id AND r.attempt_token=$2),
				(SELECT count(*) FROM memory_delivery_payloads p WHERE p.delivery_id=h.delivery_id)
				FROM memory_delivery_heads h JOIN memory_deliveries d ON d.id=h.delivery_id
				JOIN outbox_messages o ON o.id=d.outbox_id WHERE h.delivery_id=$1`, plan.DeliveryID, attempt.AttemptToken).
				Scan(&status, &outboxStatus, &reconciliations, &payloads); err != nil {
				t.Fatal(err)
			}
			if status != "expiry_reconciling" || outboxStatus != "canceled" || reconciliations != 1 || payloads != 0 {
				t.Fatalf("late callback status=%s outbox=%s reconciliations=%d payloads=%d", status, outboxStatus, reconciliations, payloads)
			}
		})
	}
}

func TestPostgreSQLFinalizePairingAndTerminalFence(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	admit := automaticPlan(now)
	admit.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, admit); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ClaimAttempt(ctx, admit.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "pairing-admit", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeDeleted, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptSucceeded, Reason: "illegal_delete", VerificationMethod: "remote", At: time.Now().UTC()}); memory.ErrorCode(err) != memory.CodeInvalidRequest {
		t.Fatalf("admit accepted deleted outcome err=%v", err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptFailed, Reason: "bad_receipt", VerificationMethod: "remote", ExternalNodeID: uuid.NewString(), ExternalMemoryID: 1, At: time.Now().UTC()}); memory.ErrorCode(err) != memory.CodeInvalidRequest {
		t.Fatalf("applied accepted failed receipt err=%v", err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptSucceeded, Reason: "applied", VerificationMethod: "remote", ExternalNodeID: uuid.NewString(), ExternalMemoryID: 1, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.FenceDelivery(ctx, admit.DeliveryID, "fenced", time.Time{}); memory.ErrorCode(err) != memory.CodeDeliveryConflict {
		t.Fatalf("applied delivery was fenced err=%v", err)
	}

	deleted := automaticPlan(dbClock(t, pool))
	deleted.Candidate.ValidUntil = deleted.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	deleteAttempt, err := store.ClaimAttempt(ctx, deleted.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deleteAttempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{AttemptID: deleteAttempt.ID, AttemptToken: deleteAttempt.AttemptToken, LeaseToken: deleteAttempt.LeaseToken, From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "pairing-delete", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE memory_deliveries DISABLE TRIGGER memory_deliveries_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_deliveries SET kind='delete' WHERE id=$1`, deleted.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE memory_deliveries ENABLE TRIGGER memory_deliveries_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_delivery_heads SET status='delete_pending' WHERE delivery_id=$1`, deleted.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_record_heads SET status='delete_pending' WHERE current_delivery_id=$1`, deleted.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: deleteAttempt.ID, AttemptToken: deleteAttempt.AttemptToken, LeaseToken: deleteAttempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeDeleted, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptPartial, Reason: "bad_delete_receipt", VerificationMethod: "remote", At: time.Now().UTC()}); memory.ErrorCode(err) != memory.CodeInvalidRequest {
		t.Fatalf("deleted accepted partial receipt err=%v", err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: deleteAttempt.ID, AttemptToken: deleteAttempt.AttemptToken, LeaseToken: deleteAttempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptSucceeded, Reason: "illegal_apply", VerificationMethod: "remote", ExternalNodeID: uuid.NewString(), ExternalMemoryID: 2, At: time.Now().UTC()}); memory.ErrorCode(err) != memory.CodeInvalidRequest {
		t.Fatalf("delete delivery accepted applied outcome err=%v", err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: deleteAttempt.ID, AttemptToken: deleteAttempt.AttemptToken, LeaseToken: deleteAttempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeDeleted, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptSucceeded, Reason: "deleted", VerificationMethod: "remote", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.FenceDelivery(ctx, deleted.DeliveryID, "fenced", time.Time{}); memory.ErrorCode(err) != memory.CodeDeliveryConflict {
		t.Fatalf("deleted delivery was fenced err=%v", err)
	}

	expired := automaticPlan(dbClock(t, pool))
	expired.Candidate.ValidUntil = expired.Candidate.CreatedAt.Add(200 * time.Millisecond)
	if _, err := store.CreateCandidate(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, expired.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ExpireDeliveries(ctx, time.Time{}, 10); err != nil || count != 1 {
		t.Fatalf("expire terminal fixture count=%d err=%v", count, err)
	}
	if err := store.FenceDelivery(ctx, expired.DeliveryID, "fenced", time.Time{}); memory.ErrorCode(err) != memory.CodeDeliveryConflict {
		t.Fatalf("expired delivery was fenced err=%v", err)
	}

	stale := automaticPlan(dbClock(t, pool))
	stale.Candidate.ValidUntil = stale.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, stale); err != nil {
		t.Fatal(err)
	}
	closeMemoryGate(t, pool, false)
	openMemoryGate(t, pool)
	if err := store.FenceDelivery(ctx, stale.DeliveryID, "fenced", time.Time{}); memory.ErrorCode(err) != memory.CodeDeliveryConflict {
		t.Fatalf("stale generation delivery was fenced err=%v", err)
	}
}

func TestPostgreSQLCandidateExpiryWaitsForContendedHead(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	for _, list := range []bool{false, true} {
		plan := pendingPlan(now)
		plan.Candidate.ValidUntil = now.Add(200 * time.Millisecond)
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		blocker, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := blocker.Exec(ctx, `SELECT candidate_id FROM memory_candidate_heads WHERE candidate_id=$1 FOR UPDATE`, plan.Candidate.ID); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			if list {
				page, err := store.ListCandidates(context.Background(), memory.PageRequest{})
				if err == nil {
					for _, item := range page.Items {
						if item.Candidate.ID == plan.Candidate.ID && item.ProposedContent != "" {
							err = errors.New("expired list payload leaked")
						}
					}
				}
				result <- err
				return
			}
			view, err := store.Candidate(context.Background(), plan.Candidate.ID)
			if err == nil && view.ProposedContent != "" {
				err = errors.New("expired detail payload leaked")
			}
			result <- err
		}()
		select {
		case err := <-result:
			t.Fatalf("contended expiry did not wait list=%t err=%v", list, err)
		case <-time.After(100 * time.Millisecond):
		}
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		now = dbClock(t, pool)
	}
}

func TestPostgreSQLOutboxBarrierBlocksMemoryMutations(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	expiring := automaticPlan(now)
	expiring.Candidate.ValidUntil = now.Add(250 * time.Millisecond)
	fencePlan := automaticPlan(now.Add(time.Millisecond))
	fencePlan.Candidate.ValidUntil = now.Add(time.Hour)
	outcomePlan := automaticPlan(now.Add(2 * time.Millisecond))
	outcomePlan.Candidate.ValidUntil = now.Add(time.Hour)
	pending := pendingPlan(now.Add(3 * time.Millisecond))
	for _, plan := range []memory.CreatePlan{expiring, fencePlan, outcomePlan, pending} {
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	attempt, err := store.ClaimAttempt(ctx, outcomePlan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "outbox-barrier", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	closeOutboxGate(t, pool)
	blockedCreate := automaticPlan(dbClock(t, pool))
	blockedCreate.Candidate.ValidUntil = blockedCreate.Candidate.CreatedAt.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, blockedCreate); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("create crossed outbox barrier err=%v", err)
	}
	decision := admitPlan(now, pending.Candidate.ID)
	decision.Decision.CreatedAt = now.Add(10 * time.Millisecond)
	if _, err := store.DecideCandidate(ctx, decision); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("decision crossed outbox barrier err=%v", err)
	}
	if err := store.FenceDelivery(ctx, fencePlan.DeliveryID, "fenced", time.Time{}); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("fence crossed outbox barrier err=%v", err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken, From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(), ReceiptStatus: memory.ReceiptSucceeded, Reason: "blocked", VerificationMethod: "remote", ExternalNodeID: uuid.NewString(), ExternalMemoryID: 4, At: time.Now().UTC()}); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("outcome crossed outbox barrier err=%v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, expiring.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExpireDeliveries(ctx, time.Time{}, 10); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("expiry crossed outbox barrier err=%v", err)
	}
	var status string
	var payloads int
	if err := pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$1) FROM memory_delivery_heads WHERE delivery_id=$1`, expiring.DeliveryID).Scan(&status, &payloads); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || payloads != 1 {
		t.Fatalf("barrier mutated expiry status=%s payloads=%d", status, payloads)
	}
}

func TestPostgreSQLExpiryLosesRaceToOutboxBarrier(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(200 * time.Millisecond)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	erasureID, generation := insertBarrier(t, pool, "expiry_gate_race")
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT owner_kind FROM privacy_owner_generation_gates WHERE owner_kind='outbox' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := store.ExpireDeliveries(context.Background(), time.Time{}, 10); result <- err }()
	time.Sleep(100 * time.Millisecond)
	if _, err := blocker.Exec(ctx, `UPDATE privacy_owner_generation_gates SET learner_generation=$2,write_open=FALSE,active_erasure_id=$1,updated_at=clock_timestamp() WHERE owner_kind='outbox'`, erasureID, generation); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
		t.Fatalf("expiry barrier race err=%v", err)
	}
	var status string
	var payloads int
	if err := pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM memory_delivery_payloads WHERE delivery_id=$1) FROM memory_delivery_heads WHERE delivery_id=$1`, plan.DeliveryID).Scan(&status, &payloads); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || payloads != 1 {
		t.Fatalf("expiry won barrier race status=%s payloads=%d", status, payloads)
	}
}

func TestPostgreSQLCompositeOwnershipAndSentAttemptConstraints(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	first := automaticPlan(now)
	first.Candidate.ValidUntil = now.Add(time.Hour)
	second := automaticPlan(now.Add(time.Millisecond))
	second.Candidate.ValidUntil = now.Add(time.Hour)
	for _, plan := range []memory.CreatePlan{first, second} {
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	firstAttempt, err := store.ClaimAttempt(ctx, first.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secondAttempt, err := store.ClaimAttempt(ctx, second.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		constraint string
		mutate     func(context.Context, pgx.Tx) error
	}{
		{
			name: "candidate head decision", constraint: "memory_candidate_head_decision",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE memory_candidate_heads SET current_decision_id=$2 WHERE candidate_id=$1`, first.Candidate.ID, second.AutomaticDecision.ID)
				return err
			},
		},
		{
			name: "record head revision", constraint: "memory_record_head_revision_owner",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM memory_record_heads WHERE logical_memory_id=$1`, second.LogicalMemoryID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `UPDATE memory_record_heads SET current_record_revision_id=$2 WHERE logical_memory_id=$1`, first.LogicalMemoryID, second.RecordRevisionID)
				return err
			},
		},
		{
			name: "delivery record tuple", constraint: "memory_delivery_record_owner",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				outboxID, deliveryID, payloadID := uuid.NewString(), uuid.NewString(), uuid.NewString()
				if _, err := tx.Exec(ctx, `
					INSERT INTO outbox_messages(
						id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,
						status,available_at,attempts,max_attempts,created_at,updated_at)
					VALUES($1,'memory.delivery',$2,$3,1,1,'{}','{}','pending',clock_timestamp(),0,3,clock_timestamp(),clock_timestamp())`,
					outboxID, deliveryID, "cross-owner:"+deliveryID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO memory_deliveries(
						id,kind,logical_memory_id,record_revision_id,record_revision,learner_generation,
						record_generation,payload_id,payload_hash,external_uri,outbox_id,outbox_idempotency_key,
						valid_until,created_at)
					SELECT $1,'correction',d.logical_memory_id,$2,d.record_revision,d.learner_generation,
					       d.record_generation,$3,d.payload_hash,d.external_uri,$4,$5,d.valid_until,d.created_at
					FROM memory_deliveries d WHERE d.id=$6`,
					deliveryID, first.RecordRevisionID, payloadID, outboxID, "cross-owner:"+deliveryID, second.DeliveryID)
				return err
			},
		},
		{
			name: "record previous revision owner", constraint: "memory_record_previous_owner",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM memory_record_heads WHERE logical_memory_id=$1`, first.LogicalMemoryID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `ALTER TABLE memory_record_revisions DISABLE TRIGGER memory_record_revisions_immutable`); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE memory_record_revisions SET revision=2,previous_revision_id=$2 WHERE id=$1`, first.RecordRevisionID, second.RecordRevisionID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `SET CONSTRAINTS memory_record_previous_owner IMMEDIATE`)
				return err
			},
		},
		{
			name: "record reverse delivery owner", constraint: "memory_record_delivery_owner",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `ALTER TABLE memory_record_revisions DISABLE TRIGGER memory_record_revisions_immutable`); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE memory_record_revisions SET delivery_id=$2 WHERE id=$1`, first.RecordRevisionID, second.DeliveryID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `SET CONSTRAINTS memory_record_delivery_owner IMMEDIATE`)
				return err
			},
		},
		{
			name: "delivery head receipt", constraint: "memory_delivery_head_receipt_owner",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE memory_delivery_heads SET current_receipt_id=$2 WHERE delivery_id=$1`, first.DeliveryID, second.ReceiptID)
				return err
			},
		},
		{
			name: "delivery head attempt", constraint: "memory_delivery_head_attempt_fk",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE memory_delivery_heads SET current_attempt_id=$2 WHERE delivery_id=$1`, first.DeliveryID, secondAttempt.ID)
				return err
			},
		},
		{
			name: "expiry reconciliation delivery tuple", constraint: "memory_expiry_reconciliation_delivery_fk",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO memory_expiry_reconciliations(
						id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,
						sent_boot_epoch,learner_generation,record_generation,status,created_at,updated_at)
					SELECT $1,d.id,other.logical_memory_id,d.external_uri,d.payload_hash,a.attempt_token,
					       'cross-delivery-tuple',d.learner_generation,d.record_generation,'pending',clock_timestamp(),clock_timestamp()
					FROM memory_deliveries d,memory_deliveries other,memory_delivery_attempts a
					WHERE d.id=$2 AND other.id=$3 AND a.id=$4`,
					uuid.NewString(), first.DeliveryID, second.DeliveryID, firstAttempt.ID)
				return err
			},
		},
		{
			name: "expiry reconciliation attempt token", constraint: "memory_expiry_reconciliation_attempt_fk",
			mutate: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO memory_expiry_reconciliations(
						id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,
						sent_boot_epoch,learner_generation,record_generation,status,created_at,updated_at)
					SELECT $1,d.id,d.logical_memory_id,d.external_uri,d.payload_hash,$2,
					       'cross-attempt-token',d.learner_generation,d.record_generation,'pending',clock_timestamp(),clock_timestamp()
					FROM memory_deliveries d WHERE d.id=$3`,
					uuid.NewString(), secondAttempt.AttemptToken, first.DeliveryID)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			mutateErr := test.mutate(ctx, tx)
			if mutateErr == nil {
				_, mutateErr = tx.Exec(ctx, "SET CONSTRAINTS "+test.constraint+" IMMEDIATE")
			}
			assertPostgreSQLConstraint(t, mutateErr, "23503", test.constraint)
		})
	}

	reconciliationID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_expiry_reconciliations(
			id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,
			sent_boot_epoch,learner_generation,record_generation,status,created_at,updated_at)
		SELECT $1,d.id,d.logical_memory_id,d.external_uri,d.payload_hash,$2,
		       'identity-original',d.learner_generation,d.record_generation,'pending',clock_timestamp(),clock_timestamp()
		FROM memory_deliveries d WHERE d.id=$3`, reconciliationID, firstAttempt.AttemptToken, first.DeliveryID); err != nil {
		t.Fatal(err)
	}
	leaseToken := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		UPDATE memory_expiry_reconciliations
		SET status='reconciling',lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute',
		    reason='status fields remain mutable',updated_at=clock_timestamp()
		WHERE id=$1`, reconciliationID, leaseToken); err != nil {
		t.Fatalf("mutable reconciliation state rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_expiry_reconciliations SET sent_boot_epoch='identity-tampered' WHERE id=$1`, reconciliationID); err == nil || !strings.Contains(err.Error(), "memory expiry reconciliation identity is immutable") {
		t.Fatalf("reconciliation identity mutation err=%v", err)
	}

	firstErasure, _ := insertVerifiedPrivacyReceipt(t, pool, now, "memory_candidate_delivery")
	_, secondReceipt := insertVerifiedPrivacyReceipt(t, pool, now.Add(time.Millisecond), "memory_candidate_delivery")
	t.Run("privacy receipt head", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `DELETE FROM privacy_erasure_receipt_heads WHERE erasure_id=(SELECT erasure_id FROM privacy_erasure_step_receipts WHERE id=$1)`, secondReceipt); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE privacy_erasure_receipt_heads SET current_receipt_id=$2 WHERE erasure_id=$1 AND store_kind='memory_candidate_delivery'`, firstErasure, secondReceipt); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `SET CONSTRAINTS privacy_erasure_receipt_head_owner IMMEDIATE`)
		assertPostgreSQLConstraint(t, err, "23503", "privacy_erasure_receipt_head_owner")
	})

	if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: firstAttempt.ID, AttemptToken: firstAttempt.AttemptToken, LeaseToken: firstAttempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "sent-failure-check", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state='failed',lease_token=NULL,lease_expires_at=NULL,boot_epoch=NULL,updated_at=clock_timestamp()
		WHERE attempt_id=$1`, firstAttempt.ID)
	if err == nil {
		t.Fatal("sent attempt transitioned to failed")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" ||
		(pgErr.ConstraintName != "memory_delivery_attempt_sent_not_failed" && pgErr.ConstraintName != "memory_delivery_attempt_send_shape") {
		t.Fatalf("sent attempt failed transition got err=%v", err)
	}
}

func TestPostgreSQLPrivacyScrubPermitBoundary(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	privacyAttempt, err := store.ClaimAttempt(ctx, plan.DeliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	reconciliationID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_expiry_reconciliations(
			id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,
			sent_boot_epoch,learner_generation,record_generation,status,created_at,updated_at)
		SELECT $1,d.id,d.logical_memory_id,d.external_uri,d.payload_hash,$2,
		       'permit-original',d.learner_generation,d.record_generation,'pending',clock_timestamp(),clock_timestamp()
		FROM memory_deliveries d WHERE d.id=$3`, reconciliationID, privacyAttempt.AttemptToken, plan.DeliveryID); err != nil {
		t.Fatal(err)
	}
	learningOperationID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_inbox(
			device_id,operation_id,request_hash,aggregate_type,aggregate_id,terminal_status,result,completed_at)
		VALUES($1,$2,decode(repeat('31',32),'hex'),'goal',$3,'succeeded','{"secret":"learning"}',$4)`,
		integrationDevice, learningOperationID, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}

	erasureID := uuid.NewString()
	generation := int64(2)
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('41',32),'hex'),'learner_request',$2,$4::timestamptz,$5,$4::timestamptz+interval '1 day')`,
		erasureID, integrationDevice, uuid.NewString(), now, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at) VALUES($1,'barrier_committed',1,'permit_boundary',$2)`, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_redaction_barriers(erasure_id,learner_generation,redacted_through_event_seq,policy_version,reason_code,event_id,committed_at)
		VALUES($1,$2,0,'permit-test-v1','learner_request',$3,$4)`, erasureID, generation, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	receipts := map[string]string{
		"memory":   uuid.NewString(),
		"learning": uuid.NewString(),
	}
	for owner, receiptID := range receipts {
		storeKind := "memory_candidate_delivery"
		if owner == "learning" {
			storeKind = "learning_event_payload"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO privacy_erasure_step_receipts(
				id,erasure_id,store_kind,version,scope_digest,started_at,status,stable_reason,verification_method)
			VALUES($1,$2,$3,1,decode(repeat('51',32),'hex'),$4,'pending','permit_boundary','transaction_local')`,
			receiptID, erasureID, storeKind, now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO privacy_erasure_receipt_heads(erasure_id,store_kind,current_receipt_id,current_version,updated_at)
			VALUES($1,$2,$3,1,$4)`, erasureID, storeKind, receiptID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$2,read_open=FALSE,write_open=FALSE,active_erasure_id=$1,updated_at=clock_timestamp()
		WHERE owner_kind IN ('memory','learning')`, erasureID, generation); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE memory_candidates SET reason='unauthorized memory scrub' WHERE id=$1`, plan.Candidate.ID); err == nil || !strings.Contains(err.Error(), "memory history is append-only") {
		t.Fatalf("memory mutation without permit err=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_expiry_reconciliations SET external_uri='unauthorized://scrub' WHERE id=$1`, reconciliationID); err == nil || !strings.Contains(err.Error(), "memory expiry reconciliation identity is immutable") {
		t.Fatalf("reconciliation mutation without permit err=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE learning_inbox SET result='{}' WHERE device_id=$1 AND operation_id=$2`, integrationDevice, learningOperationID); err == nil || !strings.Contains(err.Error(), "learning history is append-only") {
		t.Fatalf("learning mutation without permit err=%v", err)
	}

	permitScrub := func(owner, receiptID, statement string, arguments ...any) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var permit string
		if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,$3,$4)::text`, erasureID, generation, owner, receiptID).Scan(&permit); err != nil {
			t.Fatalf("begin %s scrub permit: %v", owner, err)
		}
		if permit == "" {
			t.Fatalf("empty %s permit", owner)
		}
		if _, err := tx.Exec(ctx, statement, arguments...); err != nil {
			t.Fatalf("permitted %s scrub: %v", owner, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s scrub: %v", owner, err)
		}
	}
	permitScrub("memory", receipts["memory"], `UPDATE memory_candidates SET reason='privacy scrubbed' WHERE id=$1`, plan.Candidate.ID)
	permitScrub("memory", receipts["memory"], `
		WITH scrubbed_delivery AS (
			UPDATE memory_deliveries SET external_uri='privacy://scrubbed' WHERE id=$1 RETURNING external_uri
		)
		UPDATE memory_expiry_reconciliations
		SET external_uri=(SELECT external_uri FROM scrubbed_delivery)
		WHERE id=$2`, plan.DeliveryID, reconciliationID)
	permitScrub("learning", receipts["learning"], `UPDATE learning_inbox SET result='{}' WHERE device_id=$1 AND operation_id=$2`, integrationDevice, learningOperationID)

	var memoryReason, reconciliationURI, deliveryURI, learningResult string
	if err := pool.QueryRow(ctx, `SELECT reason FROM memory_candidates WHERE id=$1`, plan.Candidate.ID).Scan(&memoryReason); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT r.external_uri,d.external_uri FROM memory_expiry_reconciliations r JOIN memory_deliveries d ON d.id=r.delivery_id WHERE r.id=$1`, reconciliationID).Scan(&reconciliationURI, &deliveryURI); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT result::text FROM learning_inbox WHERE device_id=$1 AND operation_id=$2`, integrationDevice, learningOperationID).Scan(&learningResult); err != nil {
		t.Fatal(err)
	}
	if memoryReason != "privacy scrubbed" || reconciliationURI != "privacy://scrubbed" || deliveryURI != reconciliationURI || learningResult != "{}" {
		t.Fatalf("permitted scrub not persisted memory=%q reconciliation=%q delivery=%q learning=%q", memoryReason, reconciliationURI, deliveryURI, learningResult)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_candidates SET reason='permit leaked' WHERE id=$1`, plan.Candidate.ID); err == nil || !strings.Contains(err.Error(), "memory history is append-only") {
		t.Fatalf("memory permit survived transaction err=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE learning_inbox SET result='{"leaked":true}' WHERE device_id=$1 AND operation_id=$2`, integrationDevice, learningOperationID); err == nil || !strings.Contains(err.Error(), "learning history is append-only") {
		t.Fatalf("learning permit survived transaction err=%v", err)
	}
}

func TestPostgreSQLRevisionExternalReferencesAreImmutableAndRevisionScoped(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	initial := automaticPlan(now)
	initial.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, initial); err != nil {
		t.Fatal(err)
	}
	nodeID := uuid.NewString()
	_, initialReceiptID := finalizeAppliedDeliveryWithReference(t, store, ctx, initial.DeliveryID, "revision-ref-initial", nodeID, 41)
	correction := automaticCorrectionPlan(dbClock(t, pool), initial.LogicalMemoryID, 1, 1)
	if _, err := store.CreateCandidate(ctx, correction); err != nil {
		t.Fatal(err)
	}
	_, correctionReceiptID := finalizeAppliedDeliveryWithReference(t, store, ctx, correction.DeliveryID, "revision-ref-correction", nodeID, 42)

	initialReplay, err := store.CreateCandidate(ctx, initial)
	if err != nil || initialReplay.Record == nil || initialReplay.Record.ExternalNodeID != nodeID || initialReplay.Record.ExternalMemoryID != 41 {
		t.Fatalf("initial replay=%+v err=%v", initialReplay, err)
	}
	correctionReplay, err := store.CreateCandidate(ctx, correction)
	if err != nil || correctionReplay.Record == nil || correctionReplay.Record.ExternalNodeID != nodeID || correctionReplay.Record.ExternalMemoryID != 42 {
		t.Fatalf("correction replay=%+v err=%v", correctionReplay, err)
	}
	current, err := store.Record(ctx, initial.LogicalMemoryID)
	if err != nil || current.Record.ExternalNodeID != nodeID || current.Record.ExternalMemoryID != 42 {
		t.Fatalf("current record=%+v err=%v", current.Record, err)
	}
	var refs, attempts, receipts int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(DISTINCT delivery_attempt_id),count(DISTINCT delivery_receipt_id)
		FROM memory_record_external_refs
		WHERE record_revision_id IN ($1,$2)
		  AND external_node_id=$3
		  AND ((record_revision_id=$1 AND external_memory_id=41 AND delivery_receipt_id=$4)
		    OR (record_revision_id=$2 AND external_memory_id=42 AND delivery_receipt_id=$5))`,
		initial.RecordRevisionID, correction.RecordRevisionID, nodeID, initialReceiptID, correctionReceiptID).Scan(
		&refs, &attempts, &receipts); err != nil {
		t.Fatal(err)
	}
	if refs != 2 || attempts != 2 || receipts != 2 {
		t.Fatalf("revision refs=%d attempts=%d receipts=%d", refs, attempts, receipts)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_record_external_refs SET external_memory_id=43 WHERE record_revision_id=$1`, correction.RecordRevisionID); err == nil {
		t.Fatal("revision external reference accepted an update")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM memory_record_external_refs WHERE record_revision_id=$1`, initial.RecordRevisionID); err == nil {
		t.Fatal("revision external reference accepted a delete")
	}
}

func TestPostgreSQLCorrectionCandidateCASAndConfirmedSupersede(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	initial := automaticPlan(now)
	initial.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, initial); err != nil {
		t.Fatal(err)
	}
	finalizeAppliedDelivery(t, store, ctx, initial.DeliveryID, "correction-base")

	pendingPlans := []memory.CreatePlan{
		pendingCorrectionPlan(dbClock(t, pool), initial.LogicalMemoryID, 1, 1),
		pendingCorrectionPlan(dbClock(t, pool), initial.LogicalMemoryID, 1, 1),
	}
	for _, plan := range pendingPlans {
		result, err := store.CreateCandidate(ctx, plan)
		if err != nil || result.Record != nil || result.Delivery != nil ||
			result.Candidate.Candidate.LogicalMemoryID != initial.LogicalMemoryID ||
			result.Candidate.Candidate.Status != memory.CandidatePending {
			t.Fatalf("pending correction result=%+v err=%v", result, err)
		}
	}

	type decisionResult struct {
		plan   memory.DecisionPlan
		result memory.OperationResult
		err    error
	}
	decisions := []memory.DecisionPlan{
		admitCorrectionPlan(dbClock(t, pool), pendingPlans[0].Candidate.ID, 1, 1),
		admitCorrectionPlan(dbClock(t, pool), pendingPlans[1].Candidate.ID, 1, 1),
	}
	results := make(chan decisionResult, len(decisions))
	var wg sync.WaitGroup
	for _, plan := range decisions {
		wg.Add(1)
		go func(value memory.DecisionPlan) {
			defer wg.Done()
			result, err := store.DecideCandidate(context.Background(), value)
			results <- decisionResult{plan: value, result: result, err: err}
		}(plan)
	}
	wg.Wait()
	close(results)
	var winner decisionResult
	var succeeded, conflicted int
	for result := range results {
		if result.err == nil {
			succeeded++
			winner = result
			continue
		}
		if memory.ErrorCode(result.err) == memory.CodeMemoryConflict {
			conflicted++
			continue
		}
		t.Fatalf("concurrent correction decision err=%v", result.err)
	}
	if succeeded != 1 || conflicted != 1 || winner.result.Record == nil || winner.result.Delivery == nil {
		t.Fatalf("correction race succeeded=%d conflicted=%d winner=%+v", succeeded, conflicted, winner)
	}
	if winner.result.Record.Revision != 2 || winner.result.Record.RecordGeneration != 2 ||
		winner.result.Record.PreviousRevisionID != initial.RecordRevisionID ||
		winner.result.Delivery.Kind != memory.DeliveryCorrection ||
		winner.result.Record.ExternalURI != memory.DeterministicExternalURI(initial.LogicalMemoryID) {
		t.Fatalf("manual correction result=%+v", winner.result)
	}
	before, err := store.CreateCandidate(ctx, initial)
	if err != nil || before.Record == nil || before.Record.Status != memory.RecordApplied || before.Record.SupersededAt != nil {
		t.Fatalf("old record superseded before confirmation result=%+v err=%v", before, err)
	}
	finalizeAppliedDelivery(t, store, ctx, winner.result.Delivery.ID, "manual-correction")
	after, err := store.CreateCandidate(ctx, initial)
	if err != nil || after.Record == nil || after.Record.Status != memory.RecordSuperseded || after.Record.SupersededAt == nil {
		t.Fatalf("old record not superseded after confirmation result=%+v err=%v", after, err)
	}

	automatic := automaticCorrectionPlan(dbClock(t, pool), initial.LogicalMemoryID, 2, 2)
	automaticResult, err := store.CreateCandidate(ctx, automatic)
	if err != nil || automaticResult.Record == nil || automaticResult.Delivery == nil ||
		automaticResult.Record.Revision != 3 || automaticResult.Record.RecordGeneration != 3 ||
		automaticResult.Delivery.Kind != memory.DeliveryCorrection {
		t.Fatalf("automatic correction result=%+v err=%v", automaticResult, err)
	}
	view, err := store.Record(ctx, initial.LogicalMemoryID)
	if err != nil || view.Record.ID != automaticResult.Record.ID || view.Delivery.ID != automaticResult.Delivery.ID ||
		view.Receipt.Status != memory.ReceiptPending || view.ReadGeneration.LearnerGeneration != 1 {
		t.Fatalf("record detail=%+v err=%v", view, err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"content":`) || strings.Contains(string(encoded), `"proposed_content":`) {
		t.Fatalf("record detail exposed body: %s", encoded)
	}
	closeMemoryGate(t, pool, true)
	if _, err := store.Record(ctx, initial.LogicalMemoryID); memory.ErrorCode(err) != memory.CodeContentRedacted {
		t.Fatalf("record detail crossed persistent read gate err=%v", err)
	}
}

func TestPostgreSQLDeadDeliveryReplayFencesAndIdempotency(t *testing.T) {
	t.Run("success duplicate conflict", func(t *testing.T) {
		pool, store, ctx, now := memoryHarness(t)
		plan := automaticPlan(now)
		plan.Candidate.ValidUntil = now.Add(time.Hour)
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
		markDeliveryOutboxDead(t, pool, plan.OutboxID)
		var receiptsBefore int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory_delivery_receipts WHERE delivery_id=$1`, plan.DeliveryID).Scan(&receiptsBefore); err != nil {
			t.Fatal(err)
		}
		replayPlan := deliveryReplayPlan(plan.DeliveryID)
		result, err := store.ReplayDelivery(ctx, replayPlan)
		if err != nil || result.Delivery == nil || result.Record == nil || result.Delivery.ID != plan.DeliveryID {
			t.Fatalf("dead replay result=%+v err=%v", result, err)
		}
		duplicate, err := store.ReplayDelivery(ctx, replayPlan)
		if err != nil || !duplicate.Replayed || duplicate.Delivery == nil || duplicate.Delivery.ID != plan.DeliveryID {
			t.Fatalf("duplicate replay result=%+v err=%v", duplicate, err)
		}
		conflict := replayPlan
		conflict.Operation.RequestHash = memory.SHA256String("different-dead-replay")
		if _, err := store.ReplayDelivery(ctx, conflict); memory.ErrorCode(err) != memory.CodeIdempotencyConflict {
			t.Fatalf("different replay hash err=%v", err)
		}
		var outboxStatus string
		var attempts, receiptsAfter int
		if err := pool.QueryRow(ctx, `
			SELECT status,attempts,
			       (SELECT count(*) FROM memory_delivery_receipts WHERE delivery_id=$2)
			FROM outbox_messages WHERE id=$1`, plan.OutboxID, plan.DeliveryID).Scan(
			&outboxStatus, &attempts, &receiptsAfter); err != nil {
			t.Fatal(err)
		}
		if outboxStatus != "pending" || attempts != 0 || receiptsAfter != receiptsBefore {
			t.Fatalf("dead replay outbox=%s attempts=%d receipts=%d/%d", outboxStatus, attempts, receiptsBefore, receiptsAfter)
		}
	})

	t.Run("ttl expired", func(t *testing.T) {
		pool, store, ctx, now := memoryHarness(t)
		plan := automaticPlan(now)
		plan.Candidate.ValidUntil = now.Add(200 * time.Millisecond)
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
		markDeliveryOutboxDead(t, pool, plan.OutboxID)
		if _, err := pool.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))))`, plan.Candidate.ValidUntil.Add(10*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReplayDelivery(ctx, deliveryReplayPlan(plan.DeliveryID)); memory.ErrorCode(err) != memory.CodeDeliveryConflict {
			t.Fatalf("expired delivery replay err=%v", err)
		}
	})

	t.Run("barrier", func(t *testing.T) {
		pool, store, ctx, now := memoryHarness(t)
		plan := automaticPlan(now)
		plan.Candidate.ValidUntil = now.Add(time.Hour)
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
		markDeliveryOutboxDead(t, pool, plan.OutboxID)
		closeMemoryGate(t, pool, false)
		if _, err := store.ReplayDelivery(ctx, deliveryReplayPlan(plan.DeliveryID)); memory.ErrorCode(err) != memory.CodePrivacyClearInProgress {
			t.Fatalf("barrier replay err=%v", err)
		}
	})

	t.Run("payload absent", func(t *testing.T) {
		pool, store, ctx, now := memoryHarness(t)
		plan := automaticPlan(now)
		plan.Candidate.ValidUntil = now.Add(time.Hour)
		if _, err := store.CreateCandidate(ctx, plan); err != nil {
			t.Fatal(err)
		}
		markDeliveryOutboxDead(t, pool, plan.OutboxID)
		if _, err := pool.Exec(ctx, `DELETE FROM memory_delivery_payloads WHERE delivery_id=$1`, plan.DeliveryID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReplayDelivery(ctx, deliveryReplayPlan(plan.DeliveryID)); memory.ErrorCode(err) != memory.CodeDeliveryConflict {
			t.Fatalf("payload-free replay err=%v", err)
		}
	})

	t.Run("noncurrent delivery", func(t *testing.T) {
		pool, store, ctx, now := memoryHarness(t)
		initial := automaticPlan(now)
		initial.Candidate.ValidUntil = now.Add(time.Hour)
		if _, err := store.CreateCandidate(ctx, initial); err != nil {
			t.Fatal(err)
		}
		finalizeAppliedDelivery(t, store, ctx, initial.DeliveryID, "replay-noncurrent-base")
		correction := automaticCorrectionPlan(dbClock(t, pool), initial.LogicalMemoryID, 1, 1)
		if _, err := store.CreateCandidate(ctx, correction); err != nil {
			t.Fatal(err)
		}
		markDeliveryOutboxDead(t, pool, initial.OutboxID)
		if _, err := store.ReplayDelivery(ctx, deliveryReplayPlan(initial.DeliveryID)); memory.ErrorCode(err) != memory.CodeDeliveryConflict {
			t.Fatalf("noncurrent delivery replay err=%v", err)
		}
	})
}

func TestPostgreSQLDeliveryWorkLoadsCompleteAdmissionPolicy(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Hour)
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_messages WHERE id=$1`, plan.OutboxID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	intent, err := memory.DecodeOutboxIntent(raw)
	if err != nil {
		t.Fatal(err)
	}
	work, decision, err := store.LoadDeliveryWork(ctx, intent)
	if err != nil || !decision.Apply {
		t.Fatalf("work=%+v decision=%+v err=%v", work, decision, err)
	}
	policy := work.Policy
	if policy.CandidateID != plan.Candidate.ID || policy.Source != plan.Candidate.Source ||
		policy.Category != plan.Candidate.Category || policy.Sensitivity != plan.Candidate.Sensitivity ||
		policy.Stability != plan.Candidate.Stability || policy.PolicyVersion != memory.AdmissionPolicyVersion ||
		policy.ContentHash != plan.Candidate.ContentHash || policy.AdmissionDecision.ID != plan.AutomaticDecision.ID ||
		policy.AdmissionDecision.ActorKind != "system" || policy.AdmissionDecision.OperationID != plan.Operation.OperationID ||
		policy.AdmissionDecision.RequestHash != plan.Operation.RequestHash {
		t.Fatalf("incomplete delivery policy=%+v", policy)
	}
}

func TestPostgreSQLReviewedTrustedSourcesCreateDeliveries(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    memory.SourceKind
		category  memory.Category
		reference memory.SourceReference
	}{
		{name: "model inference", source: memory.SourceModelInference, category: memory.CategoryPersonalContext,
			reference: memory.SourceReference{ModelID: "configured-model", PromptRevision: "prompt-v7", SourceHashes: []string{memory.SHA256String("source")}}},
		{name: "long term background", source: memory.SourceLongTermBackground, category: memory.CategoryPersonalContext},
		{name: "generated summary", source: memory.SourceGeneratedSummary, category: memory.CategoryGeneratedSummary,
			reference: memory.SourceReference{ModelID: "configured-model", PromptRevision: "prompt-v7", SourceHashes: []string{memory.SHA256String("source")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, ctx, now := memoryHarness(t)
			plan := pendingPlan(now)
			plan.Candidate.Source = test.source
			plan.Candidate.SourceReference = test.reference
			plan.Candidate.Category = test.category
			if _, err := store.CreateCandidate(ctx, plan); err != nil {
				t.Fatal(err)
			}
			result, err := store.DecideCandidate(ctx, admitPlan(now, plan.Candidate.ID))
			if err != nil || result.Record == nil || result.Delivery == nil {
				t.Fatalf("reviewed source result=%+v err=%v", result, err)
			}
			work, err := store.LoadDeliveryWorkByID(ctx, result.Delivery.ID)
			if err != nil || work.Policy.Source != test.source || work.Policy.Category != test.category ||
				work.Policy.SourceReference.ModelID != test.reference.ModelID ||
				len(work.Policy.SourceReference.SourceHashes) != len(test.reference.SourceHashes) {
				t.Fatalf("reviewed source work=%+v err=%v", work, err)
			}
		})
	}
}

func TestPostgreSQLSensitiveManualAdmissionCreatesReviewedDelivery(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := pendingPlan(now)
	plan.Candidate.Sensitivity = memory.SensitivitySensitive
	if _, err := store.CreateCandidate(ctx, plan); err != nil {
		t.Fatal(err)
	}
	result, err := store.DecideCandidate(ctx, admitPlan(now, plan.Candidate.ID))
	if err != nil || result.Record == nil || result.Delivery == nil || result.Candidate.Candidate.Sensitivity != memory.SensitivitySensitive {
		t.Fatalf("sensitive manual admission result=%+v err=%v", result, err)
	}
	var records, deliveries int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM memory_record_revisions WHERE candidate_id=$1),
		       (SELECT count(*) FROM memory_deliveries d JOIN memory_record_revisions r ON r.id=d.record_revision_id WHERE r.candidate_id=$1)`,
		plan.Candidate.ID).Scan(&records, &deliveries); err != nil {
		t.Fatal(err)
	}
	if records != 1 || deliveries != 1 {
		t.Fatalf("sensitive manual admission records=%d deliveries=%d", records, deliveries)
	}
}

func TestPostgreSQLDeleteDeadReplayAfterSidecarBudgetExhaustionAndVerification(t *testing.T) {
	pool, store, ctx, now := memoryHarness(t)
	plan := automaticPlan(now)
	plan.Candidate.ValidUntil = now.Add(time.Hour)
	created, err := store.CreateCandidate(ctx, plan)
	if err != nil || created.Record == nil {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	finalizeAppliedDelivery(t, store, ctx, plan.DeliveryID, "delete-replay-base")
	view, err := store.Record(ctx, plan.LogicalMemoryID)
	if err != nil {
		t.Fatal(err)
	}
	deleteOperation := memory.Operation{
		DeviceID: integrationDevice, OperationID: uuid.NewString(),
		RequestHash: memory.SHA256String("delete:" + plan.LogicalMemoryID), Kind: memory.OperationRecordDelete,
	}
	deleted, err := store.DeleteRecord(ctx, memory.DeletePlan{
		Operation: deleteOperation, LogicalMemoryID: plan.LogicalMemoryID,
		ExpectedRevision: view.Record.Revision, ExpectedRecordGeneration: view.Record.RecordGeneration,
		DeliveryID: uuid.NewString(), DeliveryPayloadID: uuid.NewString(), ReceiptID: uuid.NewString(),
		OutboxID: uuid.NewString(), ValidUntil: dbClock(t, pool).Add(time.Hour),
	})
	if err != nil || deleted.Delivery == nil || deleted.Delivery.Kind != memory.DeliveryDelete {
		t.Fatalf("delete=%+v err=%v", deleted, err)
	}
	// The sidecar failed before the prepared attempt could be marked sent, and the
	// worker exhausted its bounded outbox attempts.
	if _, err := store.ClaimAttempt(ctx, deleted.Delivery.ID, time.Time{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	var outboxID string
	if err := pool.QueryRow(ctx, `SELECT outbox_id FROM memory_deliveries WHERE id=$1`, deleted.Delivery.ID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	markDeliveryOutboxDead(t, pool, outboxID)
	if _, err := pool.Exec(ctx, `DELETE FROM memory_delivery_payloads WHERE delivery_id=$1`, deleted.Delivery.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplayDelivery(ctx, deliveryReplayPlan(deleted.Delivery.ID)); err != nil {
		t.Fatalf("delete dead replay err=%v", err)
	}
	attempt, err := store.ClaimAttempt(ctx, deleted.Delivery.ID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatalf("replayed delete attempt was not recoverable: %v", err)
	}
	attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "delete-recovery", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptSent, Kind: memory.AttemptOutcomeDeleted, ReceiptID: uuid.NewString(),
		ReceiptStatus: memory.ReceiptSucceeded, Reason: "remote_logical_delete_verified",
		VerificationMethod: "nocturne_complete_reference_absence", EvidenceDigest: deleted.Delivery.PayloadHash,
		At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	final, err := store.Record(ctx, plan.LogicalMemoryID)
	if err != nil || final.Record.Status != memory.RecordDeleted || final.Delivery.Status != memory.DeliveryStatusDeleted ||
		final.Receipt.Status != memory.ReceiptSucceeded {
		t.Fatalf("verified delete final=%+v err=%v", final, err)
	}
}

func finalizeAppliedDelivery(t *testing.T, store *postgresstore.Store, ctx context.Context, deliveryID, bootEpoch string) {
	t.Helper()
	finalizeAppliedDeliveryWithReference(t, store, ctx, deliveryID, bootEpoch, uuid.NewString(), 42)
}

func finalizeAppliedDeliveryWithReference(
	t *testing.T,
	store *postgresstore.Store,
	ctx context.Context,
	deliveryID, bootEpoch, nodeID string,
	memoryID int64,
) (string, string) {
	t.Helper()
	attempt, err := store.ClaimAttempt(ctx, deliveryID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: bootEpoch, At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	receiptID := uuid.NewString()
	if _, err := store.FinalizeAttempt(ctx, memory.AttemptOutcome{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptSent, Kind: memory.AttemptOutcomeApplied, ReceiptID: receiptID,
		ReceiptStatus: memory.ReceiptSucceeded, Reason: "remote_hash_verified", VerificationMethod: "uri_hash",
		ExternalNodeID: nodeID, ExternalMemoryID: memoryID, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return attempt.ID, receiptID
}

func markDeliveryOutboxDead(t *testing.T, pool *pgxpool.Pool, outboxID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE outbox_messages
		SET status='dead',attempts=max_attempts,last_error_category='manual_replay_fixture',
		    last_error_at=clock_timestamp(),lease_token=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
		WHERE id=$1`, outboxID); err != nil {
		t.Fatal(err)
	}
}

func deliveryReplayPlan(deliveryID string) memory.ReplayPlan {
	return memory.ReplayPlan{
		Operation: memory.Operation{
			DeviceID: integrationDevice, OperationID: uuid.NewString(),
			RequestHash: memory.SHA256String("delivery-replay:" + deliveryID), Kind: memory.OperationDeliveryReplay,
		},
		DeliveryID: deliveryID,
	}
}

func startMaintenancePrivacyCleanup(t *testing.T, pool *pgxpool.Pool, store *postgresstore.Store) memory.MaintenanceAuthorization {
	t.Helper()
	erasureID, generation, localReceipt := installMemoryPrivacyBarrier(t, pool)
	request := privacy.LocalRedactionRequest{
		ErasureID: erasureID, Store: privacy.StoreMemoryCandidateDelivery,
		ReceiptID: localReceipt, LearnerGeneration: generation,
	}
	if err := store.RedactTx(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	remoteReceipt := insertMaintenancePrivacyReceipt(t, pool, erasureID, "nocturne_paths")
	return memory.MaintenanceAuthorization{
		ErasureID: erasureID, ReceiptID: remoteReceipt, TargetLearnerGeneration: generation,
	}
}

func completeMaintenancePrivacyReceipt(t *testing.T, pool *pgxpool.Pool, auth memory.MaintenanceAuthorization) string {
	t.Helper()
	ctx := context.Background()
	receiptID := uuid.NewString()
	now := dbClock(t, pool)
	if _, err := pool.Exec(ctx, `
		WITH previous AS (
			SELECT h.store_kind,h.current_version,r.scope_digest
			FROM privacy_erasure_receipt_heads h
			JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
			WHERE h.erasure_id=$1 AND h.current_receipt_id=$2
		)
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method)
		SELECT $3,$1,store_kind,current_version+1,scope_digest,$4,$4,'succeeded',
		       'all_old_generation_remote_reconciliations_verified','maintenance'
		FROM previous`, auth.ErasureID, auth.ReceiptID, receiptID, now); err != nil {
		t.Fatal(err)
	}
	tag, err := pool.Exec(ctx, `
		UPDATE privacy_erasure_receipt_heads
		SET current_receipt_id=$3,current_version=current_version+1,updated_at=$4
		WHERE erasure_id=$1 AND current_receipt_id=$2`, auth.ErasureID, auth.ReceiptID, receiptID, now)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("complete maintenance receipt affected %d rows", tag.RowsAffected())
	}
	return receiptID
}

func installMemoryPrivacyBarrier(t *testing.T, pool *pgxpool.Pool) (string, int64, string) {
	t.Helper()
	ctx := context.Background()
	erasureID, generation := insertBarrier(t, pool, "learner_request")
	receiptID := uuid.NewString()
	now := dbClock(t, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,status,stable_reason,verification_method)
		VALUES($1,$2,'memory_candidate_delivery',1,decode(repeat('91',32),'hex'),$3,
		       'pending','memory privacy cleanup','local_scrub')`, receiptID, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_receipt_heads(
			erasure_id,store_kind,current_receipt_id,current_version,updated_at)
		VALUES($1,'memory_candidate_delivery',$2,1,$3)`, erasureID, receiptID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$2,read_open=FALSE,write_open=FALSE,
		    active_erasure_id=$1,updated_at=clock_timestamp()
		WHERE owner_kind='memory'`, erasureID, generation); err != nil {
		t.Fatal(err)
	}
	return erasureID, generation, receiptID
}

func insertMaintenancePrivacyReceipt(t *testing.T, pool *pgxpool.Pool, erasureID, storeKind string) string {
	t.Helper()
	ctx := context.Background()
	receiptID := uuid.NewString()
	now := dbClock(t, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,status,stable_reason,verification_method)
		VALUES($1,$2,$3,1,decode(repeat('92',32),'hex'),$4,
		       'pending','remote privacy cleanup','maintenance')`, receiptID, erasureID, storeKind, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_receipt_heads(
			erasure_id,store_kind,current_receipt_id,current_version,updated_at)
		VALUES($1,$2,$3,1,$4)`, erasureID, storeKind, receiptID, now); err != nil {
		t.Fatal(err)
	}
	return receiptID
}

func rotateMaintenancePrivacyReceipt(t *testing.T, pool *pgxpool.Pool, erasureID, storeKind string, version int64) string {
	t.Helper()
	ctx := context.Background()
	receiptID := uuid.NewString()
	now := dbClock(t, pool)
	if _, err := pool.Exec(ctx, `
		WITH previous AS (
			SELECT r.scope_digest
			FROM privacy_erasure_receipt_heads h
			JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
			WHERE h.erasure_id=$1 AND h.store_kind=$2
		)
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method)
		SELECT $3,$1,$2,$4,scope_digest,$5,$5,'partial','remote privacy resume','maintenance'
		FROM previous`, erasureID, storeKind, receiptID, version, now); err != nil {
		t.Fatal(err)
	}
	tag, err := pool.Exec(ctx, `
		UPDATE privacy_erasure_receipt_heads
		SET current_receipt_id=$3,current_version=$4,updated_at=$5
		WHERE erasure_id=$1 AND store_kind=$2 AND current_version=$4-1`, erasureID, storeKind, receiptID, version, now)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("rotate %s receipt version %d affected %d rows", storeKind, version, tag.RowsAffected())
	}
	return receiptID
}

func assertPostgreSQLConstraint(t *testing.T, err error, sqlState, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("constraint %s accepted invalid row", constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlState || pgErr.ConstraintName != constraint {
		t.Fatalf("constraint %s got err=%v", constraint, err)
	}
}

func insertVerifiedPrivacyReceipt(t *testing.T, pool *pgxpool.Pool, now time.Time, storeKind string) (string, string) {
	t.Helper()
	ctx := context.Background()
	erasureID, receiptID := uuid.NewString(), uuid.NewString()
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(target_learner_generation),1)+1 FROM privacy_erasures`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('61',32),'hex'),'learner_request',$2,$4::timestamptz,$5,$4::timestamptz+interval '1 day')`,
		erasureID, integrationDevice, uuid.NewString(), now, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at)
		VALUES($1,'verified',1,'fk_boundary',$2)`, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method)
		VALUES($1,$2,$3,1,decode(repeat('71',32),'hex'),$4,$4,'succeeded','fk_boundary','constraint_test')`,
		receiptID, erasureID, storeKind, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_receipt_heads(erasure_id,store_kind,current_receipt_id,current_version,updated_at)
		VALUES($1,$2,$3,1,$4)`, erasureID, storeKind, receiptID, now); err != nil {
		t.Fatal(err)
	}
	return erasureID, receiptID
}

func insertBarrier(t *testing.T, pool *pgxpool.Pool, reason string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	now := dbClock(t, pool)
	erasureID := uuid.NewString()
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT GREATEST((SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='memory'),(SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='outbox'))+1`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('cd',32),'hex'),'learner_request',$2,$4,$5,$4::timestamptz+interval '1 day')`, erasureID, integrationDevice, uuid.NewString(), now, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at)
		VALUES($1,'barrier_committed',1,$2,$3)`, erasureID, reason, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_redaction_barriers(
			erasure_id,learner_generation,redacted_through_event_seq,policy_version,reason_code,event_id,committed_at)
		VALUES($1,$2,0,'privacy-redaction-v1','learner_request',$3,$4)`, erasureID, generation, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	return erasureID, generation
}

func closeOutboxGate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	erasureID, generation := insertBarrier(t, pool, "outbox_only_barrier")
	if _, err := pool.Exec(context.Background(), `UPDATE privacy_owner_generation_gates SET learner_generation=$2,write_open=FALSE,active_erasure_id=$1,updated_at=clock_timestamp() WHERE owner_kind='outbox'`, erasureID, generation); err != nil {
		t.Fatal(err)
	}
}

func closeMemoryGate(t *testing.T, pool *pgxpool.Pool, closeRead bool) {
	t.Helper()
	ctx := context.Background()
	erasureID := uuid.NewString()
	operationID := uuid.NewString()
	now := dbClock(t, pool)
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT learner_generation+1 FROM privacy_owner_generation_gates WHERE owner_kind='memory'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),'learner_request',$2,$4::timestamptz,$5,$4::timestamptz+interval '1 day')`,
		erasureID, integrationDevice, operationID, now, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at)
		VALUES($1,'barrier_committed',1,'test_barrier',$2)`, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$2,read_open=CASE WHEN $3 THEN FALSE ELSE read_open END,
		    write_open=FALSE,active_erasure_id=$1,updated_at=clock_timestamp()
		WHERE owner_kind IN ('memory','outbox')`, erasureID, generation, closeRead); err != nil {
		t.Fatal(err)
	}
}

func openMemoryGate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var erasureID string
	if err := pool.QueryRow(ctx, `SELECT active_erasure_id FROM privacy_owner_generation_gates WHERE owner_kind='memory'`).Scan(&erasureID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=clock_timestamp()
		WHERE owner_kind IN ('memory','outbox')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_erasure_heads SET status='verified',summary_version=summary_version+1,updated_at=clock_timestamp()
		WHERE erasure_id=$1`, erasureID); err != nil {
		t.Fatal(err)
	}
}

func memoryHarness(t *testing.T) (*pgxpool.Pool, *postgresstore.Store, context.Context, time.Time) {
	t.Helper()
	pool := memoryIntegrationPool(t)
	ctx := context.Background()
	now := dbClock(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'memory-test',$2)`, integrationDevice, now); err != nil {
		t.Fatal(err)
	}
	return pool, postgresstore.New(pool), ctx, now
}

func dbClock(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.UTC()
}

func automaticPlan(now time.Time) memory.CreatePlan {
	candidateID := uuid.NewString()
	logicalID := uuid.NewString()
	content := "Prefer concise examples"
	operationID := uuid.NewString()
	requestHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	candidate := memory.Candidate{
		ID: candidateID, URI: memory.CandidateURI(candidateID), LogicalMemoryID: logicalID,
		PayloadID: uuid.NewString(), ContentHash: memory.SHA256String(content),
		Source: memory.SourceUserStatement, ProposerID: integrationDevice, Reason: "explicit preference",
		Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivityNonSensitive,
		Stability: memory.StabilityStable, ValidUntil: now.Add(750 * time.Millisecond), PolicyVersion: memory.AdmissionPolicyVersion,
		Status: memory.CandidateAdmitted, Revision: 2, CreatedAt: now,
	}
	decision := memory.CandidateDecision{
		ID: uuid.NewString(), CandidateID: candidateID, Revision: 2,
		Decision: memory.DecisionAdmit, Reason: "automatic_policy_match", ActorKind: "system",
		OperationID: operationID, RequestHash: requestHash, CreatedAt: now,
	}
	return memory.CreatePlan{
		Operation: memory.Operation{DeviceID: integrationDevice, OperationID: operationID, RequestHash: requestHash, Kind: memory.OperationCreateCandidate},
		Candidate: candidate, Content: content, AutomaticDecision: &decision, LogicalMemoryID: logicalID,
		RecordRevisionID: uuid.NewString(), DeliveryID: uuid.NewString(), DeliveryPayloadID: uuid.NewString(),
		ReceiptID: uuid.NewString(), OutboxID: uuid.NewString(),
	}
}

func automaticCorrectionPlan(
	now time.Time,
	logicalMemoryID string,
	expectedRevision, expectedRecordGeneration int64,
) memory.CreatePlan {
	candidateID := uuid.NewString()
	operationID := uuid.NewString()
	content := "Prefer concise worked examples"
	requestHash := memory.SHA256String("automatic-correction:" + candidateID)
	candidate := memory.Candidate{
		ID: candidateID, URI: memory.CandidateURI(candidateID), LogicalMemoryID: logicalMemoryID,
		PayloadID: uuid.NewString(), ContentHash: memory.SHA256String(content),
		Source: memory.SourceUserStatement, ProposerID: integrationDevice, Reason: "explicit correction",
		Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivityNonSensitive,
		Stability: memory.StabilityStable, ValidUntil: now.Add(time.Hour),
		PolicyVersion: memory.AdmissionPolicyVersion, Status: memory.CandidateAdmitted,
		Revision: 2, CreatedAt: now,
	}
	decision := memory.CandidateDecision{
		ID: uuid.NewString(), CandidateID: candidateID, Revision: 2, Decision: memory.DecisionAdmit,
		Reason: "automatic_policy_match", ActorKind: "system", OperationID: operationID,
		RequestHash: requestHash, CreatedAt: now,
	}
	return memory.CreatePlan{
		Operation: memory.Operation{
			DeviceID: integrationDevice, OperationID: operationID,
			RequestHash: requestHash, Kind: memory.OperationCreateCandidate,
		},
		Candidate: candidate, Content: content, Correction: true,
		ExpectedRecordRevision: expectedRevision, ExpectedRecordGeneration: expectedRecordGeneration,
		AutomaticDecision: &decision, LogicalMemoryID: logicalMemoryID,
		RecordRevisionID: uuid.NewString(), DeliveryID: uuid.NewString(),
		DeliveryPayloadID: uuid.NewString(), ReceiptID: uuid.NewString(), OutboxID: uuid.NewString(),
	}
}

func pendingCorrectionPlan(
	now time.Time,
	logicalMemoryID string,
	expectedRevision, expectedRecordGeneration int64,
) memory.CreatePlan {
	candidateID := uuid.NewString()
	content := "Prefer detailed examples for unfamiliar topics"
	return memory.CreatePlan{
		Operation: memory.Operation{
			DeviceID: integrationDevice, OperationID: uuid.NewString(),
			RequestHash: memory.SHA256String("pending-correction:" + candidateID),
			Kind:        memory.OperationCreateCandidate,
		},
		Candidate: memory.Candidate{
			ID: candidateID, URI: memory.CandidateURI(candidateID), LogicalMemoryID: logicalMemoryID,
			PayloadID: uuid.NewString(), ContentHash: memory.SHA256String(content),
			Source: memory.SourceUserStatement, ProposerID: integrationDevice, Reason: "manual correction review",
			Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivityNonSensitive,
			Stability: memory.StabilityStable, ValidUntil: now.Add(time.Hour),
			PolicyVersion: memory.AdmissionPolicyVersion, Status: memory.CandidatePending,
			Revision: 1, CreatedAt: now,
		},
		Content: content, Correction: true, LogicalMemoryID: logicalMemoryID,
		ExpectedRecordRevision: expectedRevision, ExpectedRecordGeneration: expectedRecordGeneration,
	}
}

func admitCorrectionPlan(
	now time.Time,
	candidateID string,
	expectedRevision, expectedRecordGeneration int64,
) memory.DecisionPlan {
	operation := memory.Operation{
		DeviceID: integrationDevice, OperationID: uuid.NewString(),
		RequestHash: memory.SHA256String("admit-correction:" + candidateID),
		Kind:        memory.OperationCandidateDecision,
	}
	return memory.DecisionPlan{
		Operation: operation, CandidateID: candidateID, ExpectedRevision: 1,
		ExpectedRecordRevision: expectedRevision, ExpectedRecordGeneration: expectedRecordGeneration,
		Decision: memory.CandidateDecision{
			ID: uuid.NewString(), CandidateID: candidateID, Revision: 2, Decision: memory.DecisionAdmit,
			Reason: "reviewed correction", ActorID: integrationDevice, ActorKind: "device",
			OperationID: operation.OperationID, RequestHash: operation.RequestHash, CreatedAt: now,
		},
		LogicalMemoryID: uuid.NewString(), RecordRevisionID: uuid.NewString(),
		DeliveryID: uuid.NewString(), DeliveryPayloadID: uuid.NewString(),
		ReceiptID: uuid.NewString(), OutboxID: uuid.NewString(),
	}
}

func pendingPlan(now time.Time) memory.CreatePlan {
	id := uuid.NewString()
	content := "I sometimes study in the evening"
	return memory.CreatePlan{
		Operation: memory.Operation{DeviceID: integrationDevice, OperationID: uuid.NewString(), RequestHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Kind: memory.OperationCreateCandidate},
		Candidate: memory.Candidate{
			ID: id, URI: memory.CandidateURI(id), PayloadID: uuid.NewString(),
			ContentHash: memory.SHA256String(content), Source: memory.SourceUserStatement,
			ProposerID: integrationDevice, Reason: "background context", Category: memory.CategoryPersonalContext,
			Sensitivity: memory.SensitivityNonSensitive, Stability: memory.StabilityStable, ValidUntil: now.Add(time.Hour),
			PolicyVersion: memory.AdmissionPolicyVersion, Status: memory.CandidatePending, Revision: 1, CreatedAt: now,
		}, Content: content,
	}
}

func admitPlan(now time.Time, candidateID string) memory.DecisionPlan {
	operation := memory.Operation{DeviceID: integrationDevice, OperationID: uuid.NewString(), RequestHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Kind: memory.OperationCandidateDecision}
	return memory.DecisionPlan{
		Operation: operation, CandidateID: candidateID, ExpectedRevision: 1,
		Decision:        memory.CandidateDecision{ID: uuid.NewString(), CandidateID: candidateID, Revision: 2, Decision: memory.DecisionAdmit, Reason: "reviewed", ActorID: integrationDevice, ActorKind: "device", OperationID: operation.OperationID, RequestHash: operation.RequestHash, CreatedAt: now.Add(time.Minute)},
		LogicalMemoryID: uuid.NewString(), RecordRevisionID: uuid.NewString(),
		DeliveryID: uuid.NewString(), DeliveryPayloadID: uuid.NewString(),
		ReceiptID: uuid.NewString(), OutboxID: uuid.NewString(),
	}
}

func rejectPlan(now time.Time, candidateID string) memory.DecisionPlan {
	operation := memory.Operation{DeviceID: integrationDevice, OperationID: uuid.NewString(), RequestHash: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Kind: memory.OperationCandidateDecision}
	return memory.DecisionPlan{Operation: operation, CandidateID: candidateID, ExpectedRevision: 1, Decision: memory.CandidateDecision{
		ID: uuid.NewString(), CandidateID: candidateID, Revision: 2,
		Decision: memory.DecisionReject, Reason: "not durable", ActorID: integrationDevice, ActorKind: "device",
		OperationID: operation.OperationID, RequestHash: operation.RequestHash, CreatedAt: now.Add(time.Minute),
	}}
}

func memoryIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL memory integration suite not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("memory_test_%d", time.Now().UnixNano())
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

var _ = errors.Is
