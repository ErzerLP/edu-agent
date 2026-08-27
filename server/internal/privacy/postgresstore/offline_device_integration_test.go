package postgresstore_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
)

func TestOfflineDevicePurgeChallengeAckIsDeviceBoundAppendOnlyAndGatesParent(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	deviceOne, deviceTwo, otherDevice := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for index, deviceID := range []string{deviceOne, deviceTwo, otherDevice} {
		if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,$2,$3)`, deviceID, "offline-device", now.Add(time.Duration(index)*time.Microsecond)); err != nil {
			t.Fatal(err)
		}
	}
	sessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO tutoring_sessions(id,aggregate_version,state,started_at,updated_at) VALUES($1,1,'GoalReady',$2,$2)`, sessionID, now); err != nil {
		t.Fatal(err)
	}
	seedOfflinePossession(t, pool, deviceOne, sessionID, now)
	seedOfflinePossession(t, pool, deviceTwo, sessionID, now)

	keyring, err := privacy.NewOfflineChallengeKeyring(map[int][]byte{
		1: bytes.Repeat([]byte{0x5a}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := newAuthorizedBarrierStore(pool, privacy.NewReadPermitManager(), privacydb.WithOfflineChallengeKeyring(keyring))
	request := barrierRequest(deviceOne, uuid.NewString(), now)
	barrier, err := store.CommitBarrier(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if barrier.Status != privacy.StatusBarrierCommitted || barrier.LearnerGeneration != 2 {
		t.Fatalf("barrier=%+v", barrier)
	}

	challengeOne, found, err := store.CurrentOfflineDevicePurge(ctx, deviceOne)
	if err != nil || !found {
		t.Fatalf("device one challenge=%+v found=%v err=%v", challengeOne, found, err)
	}
	challengeTwo, found, err := store.CurrentOfflineDevicePurge(ctx, deviceTwo)
	if err != nil || !found {
		t.Fatalf("device two challenge=%+v found=%v err=%v", challengeTwo, found, err)
	}
	if challengeOne.ErasureID != barrier.ErasureID || challengeOne.DeviceID != deviceOne || challengeOne.OldGeneration != 1 || challengeOne.CurrentGeneration != 2 || challengeOne.ChallengeRevision != 1 || challengeOne.Challenge == challengeTwo.Challenge {
		t.Fatalf("challenges one=%+v two=%+v", challengeOne, challengeTwo)
	}
	if _, found, err := store.CurrentOfflineDevicePurge(ctx, otherDevice); err != nil || found {
		t.Fatalf("unrelated device found=%v err=%v", found, err)
	}

	managedAbsent := true
	bad := privacy.OfflineDevicePurgeAcknowledgment{ChallengeRevision: 1, Challenge: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Outcome: privacy.OfflinePurgeOutcomeSucceeded, ManagedObjectsAbsent: &managedAbsent}
	if _, err := store.AcknowledgeOfflineDevicePurge(ctx, barrier.ErasureID, deviceOne, bad); privacy.ErrorCode(err) != privacy.CodeOfflineChallengeInvalid {
		t.Fatalf("invalid challenge err=%v", err)
	}
	ack := privacy.OfflineDevicePurgeAcknowledgment{ChallengeRevision: 1, Challenge: challengeOne.Challenge, Outcome: privacy.OfflinePurgeOutcomeSucceeded, ManagedObjectsAbsent: &managedAbsent}
	childOne, err := store.AcknowledgeOfflineDevicePurge(ctx, barrier.ErasureID, deviceOne, ack)
	if err != nil || childOne.Status != privacy.OfflineDeviceChildSucceeded || childOne.StableReason != "device_acknowledged" {
		t.Fatalf("device one receipt=%+v err=%v", childOne, err)
	}
	replayed, err := store.AcknowledgeOfflineDevicePurge(ctx, barrier.ErasureID, deviceOne, ack)
	if err != nil || replayed.Status != privacy.OfflineDeviceChildSucceeded {
		t.Fatalf("device one replay=%+v err=%v", replayed, err)
	}
	conflict := ack
	conflict.Outcome = privacy.OfflinePurgeOutcomeFailed
	conflict.FailureCode = privacy.OfflinePurgeFailureProfileBusy
	conflict.ManagedObjectsAbsent = nil
	if _, err := store.AcknowledgeOfflineDevicePurge(ctx, barrier.ErasureID, deviceOne, conflict); privacy.ErrorCode(err) != privacy.CodeOfflinePurgeAckConflict {
		t.Fatalf("terminal conflict err=%v", err)
	}

	current, err := store.Receipt(ctx, barrier.ErasureID)
	if err != nil || current.Status == privacy.StatusVerified {
		t.Fatalf("one device acknowledgment prematurely verified parent=%+v err=%v", current, err)
	}
	var deviceOneRevisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM privacy_offline_device_child_revisions r JOIN privacy_offline_device_children c ON c.id=r.child_id WHERE c.erasure_id=$1 AND c.device_id=$2`, barrier.ErasureID, deviceOne).Scan(&deviceOneRevisions); err != nil || deviceOneRevisions != 2 {
		t.Fatalf("device one revisions=%d err=%v", deviceOneRevisions, err)
	}

	failed := privacy.OfflineDevicePurgeAcknowledgment{ChallengeRevision: 1, Challenge: challengeTwo.Challenge, Outcome: privacy.OfflinePurgeOutcomeFailed, FailureCode: privacy.OfflinePurgeFailureProfileBusy}
	childTwo, err := store.AcknowledgeOfflineDevicePurge(ctx, barrier.ErasureID, deviceTwo, failed)
	if err != nil || childTwo.Status != privacy.OfflineDeviceChildFailed || childTwo.StableReason != string(privacy.OfflinePurgeFailureProfileBusy) {
		t.Fatalf("device two receipt=%+v err=%v", childTwo, err)
	}
	current, err = store.Receipt(ctx, barrier.ErasureID)
	if err != nil || current.Status != privacy.StatusPartial {
		t.Fatalf("failed child parent=%+v err=%v", current, err)
	}
	if _, found, err := store.CurrentOfflineDevicePurge(ctx, deviceOne); err != nil || found {
		t.Fatalf("terminal device one found=%v err=%v", found, err)
	}
	retry, found, err := store.CurrentOfflineDevicePurge(ctx, deviceTwo)
	if err != nil || !found {
		t.Fatalf("failed device retry found=%v err=%v", found, err)
	}
	if retry.Status != privacy.OfflineDeviceChildPending || retry.ChallengeRevision != 2 || retry.Challenge == challengeTwo.Challenge {
		t.Fatalf("failed device retry=%+v", retry)
	}
}

func seedOfflinePossession(t *testing.T, pool *pgxpool.Pool, deviceID, sessionID string, now time.Time) {
	t.Helper()
	packID := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO offline_packs(
			id,revision,prepare_device_id,prepare_operation_id,learner_generation,parent_session_id,
			response_body,response_hash,signer_key_id,signature,issued_at,eligible_until,archive_until,created_at)
		VALUES($1,1,$2,$3,1,$4,'{}',decode(repeat('00',32),'hex'),'test-key',decode(repeat('00',64),'hex'),$5::timestamptz,$5::timestamptz+interval '1 hour',$5::timestamptz+interval '24 hours',$5::timestamptz)`,
		packID, deviceID, uuid.NewString(), sessionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO offline_device_possessions(id,device_id,learner_generation,first_pack_id,first_seen_at)
		VALUES($1,$2,1,$3,$4)`, uuid.NewString(), deviceID, packID, now); err != nil {
		t.Fatal(err)
	}
}
