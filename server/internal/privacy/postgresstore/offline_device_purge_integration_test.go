package postgresstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOfflineDevicePurgeFailureKeepsParentPartial(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	keyring, err := privacy.NewOfflineChallengeKeyring(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	store := privacydb.New(pool, privacydb.WithOfflineChallengeKeyring(keyring))
	now := time.Now().UTC().Truncate(time.Microsecond)
	erasureID := uuid.NewString()
	deviceID := uuid.NewString()
	seedOfflinePurgeReceipt(t, pool, keyring, erasureID, []string{deviceID}, now)
	task, found, err := store.CurrentOfflineDevicePurge(ctx, deviceID)
	if err != nil || !found {
		t.Fatalf("load purge task: found=%t err=%v", found, err)
	}
	receipt, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, deviceID, privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision: task.ChallengeRevision, Challenge: task.Challenge,
		Outcome: privacy.OfflinePurgeOutcomeFailed, FailureCode: privacy.OfflinePurgeFailureProfileBusy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != privacy.OfflineDeviceChildFailed || receipt.StableReason != string(privacy.OfflinePurgeFailureProfileBusy) {
		t.Fatalf("receipt=%+v", receipt)
	}
	var parentStatus privacy.ErasureStatus
	var stepStatus privacy.StepStatus
	if err := pool.QueryRow(ctx, `
		SELECT h.status, r.status
		FROM privacy_erasure_heads h
		JOIN privacy_erasure_receipt_heads rh ON rh.erasure_id=h.erasure_id AND rh.store_kind='offline_device_cache'
		JOIN privacy_erasure_step_receipts r ON r.id=rh.current_receipt_id
		WHERE h.erasure_id=$1`, erasureID).Scan(&parentStatus, &stepStatus); err != nil {
		t.Fatal(err)
	}
	if parentStatus != privacy.StatusPartial || stepStatus != privacy.StepPartial {
		t.Fatalf("parent=%q step=%q", parentStatus, stepStatus)
	}
	retryTask, found, err := store.CurrentOfflineDevicePurge(ctx, deviceID)
	if err != nil || !found {
		t.Fatalf("load retry purge task: found=%t err=%v", found, err)
	}
	if retryTask.Status != privacy.OfflineDeviceChildPending || retryTask.ChallengeRevision != task.ChallengeRevision+1 || retryTask.Challenge == task.Challenge {
		t.Fatalf("retry purge task=%+v original=%+v", retryTask, task)
	}
	if _, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, deviceID, privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision: retryTask.ChallengeRevision, Challenge: retryTask.Challenge,
		Outcome: privacy.OfflinePurgeOutcomeSucceeded, ManagedObjectsAbsent: boolPointer(true),
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM privacy_erasure_heads WHERE erasure_id=$1`, erasureID).Scan(&parentStatus); err != nil {
		t.Fatal(err)
	}
	if parentStatus != privacy.StatusVerified {
		t.Fatalf("parent did not converge after successful retry: %q", parentStatus)
	}
}

func TestOfflineDevicePurgeCannotVerifyWhileAnotherStepIsPartial(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	keyring, err := privacy.NewOfflineChallengeKeyring(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	store := privacydb.New(pool, privacydb.WithOfflineChallengeKeyring(keyring))
	now := time.Now().UTC().Truncate(time.Microsecond)
	erasureID := uuid.NewString()
	deviceID := uuid.NewString()
	seedOfflinePurgeReceipt(t, pool, keyring, erasureID, []string{deviceID}, now)
	var currentReceiptID string
	var currentVersion int64
	var scope []byte
	var startedAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT r.id::text,r.version,r.scope_digest,r.started_at
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1 AND h.store_kind='managed_backup'`, erasureID).Scan(&currentReceiptID, &currentVersion, &scope, &startedAt); err != nil {
		t.Fatal(err)
	}
	partialReceiptID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method)
		VALUES($1,$2,'managed_backup',$3,$4,$5,$6,'partial','managed_backup_still_pending','fixture')`,
		partialReceiptID, erasureID, currentVersion+1, scope, startedAt, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_erasure_receipt_heads
		SET current_receipt_id=$2,current_version=$3,updated_at=$4
		WHERE erasure_id=$1 AND store_kind='managed_backup' AND current_receipt_id=$5`,
		erasureID, partialReceiptID, currentVersion+1, now, currentReceiptID); err != nil {
		t.Fatal(err)
	}
	task, found, err := store.CurrentOfflineDevicePurge(ctx, deviceID)
	if err != nil || !found {
		t.Fatalf("load purge task: found=%t err=%v", found, err)
	}
	if _, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, deviceID, privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision: task.ChallengeRevision, Challenge: task.Challenge,
		Outcome: privacy.OfflinePurgeOutcomeSucceeded, ManagedObjectsAbsent: boolPointer(true),
	}); err != nil {
		t.Fatal(err)
	}
	var parentStatus privacy.ErasureStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM privacy_erasure_heads WHERE erasure_id=$1`, erasureID).Scan(&parentStatus); err != nil {
		t.Fatal(err)
	}
	if parentStatus != privacy.StatusPartial {
		t.Fatalf("parent status=%q; another partial step was bypassed", parentStatus)
	}
}

func TestOfflineDevicePurgeHTTPPostgres(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	keyring, err := privacy.NewOfflineChallengeKeyring(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	store := privacydb.New(pool, privacydb.WithOfflineChallengeKeyring(keyring))
	now := time.Now().UTC().Truncate(time.Microsecond)
	erasureID := uuid.NewString()
	deviceID := uuid.NewString()
	seedOfflinePurgeReceipt(t, pool, keyring, erasureID, []string{deviceID}, now)
	handler := offlinePurgeHTTPHandler(t, offlinePurgePrivacyService{Store: store}, deviceID)

	var task privacy.OfflinePurgeChallenge
	if status := offlinePurgeHTTPRequest(t, handler, http.MethodGet, "/v1/privacy/erasures/"+erasureID+"/offline-device-purge", nil, &task); status != http.StatusOK {
		t.Fatalf("get purge task status=%d", status)
	}
	managedAbsent := true
	var receipt privacy.OfflineDeviceChildReceipt
	request := privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision: task.ChallengeRevision, Challenge: task.Challenge,
		Outcome: privacy.OfflinePurgeOutcomeSucceeded, ManagedObjectsAbsent: &managedAbsent,
	}
	if status := offlinePurgeHTTPRequest(t, handler, http.MethodPost, "/v1/privacy/erasures/"+erasureID+"/offline-device-purge/ack", request, &receipt); status != http.StatusOK {
		t.Fatalf("ack purge task status=%d", status)
	}
	if receipt.Status != privacy.OfflineDeviceChildSucceeded || receipt.StableReason != "device_acknowledged" {
		t.Fatalf("receipt=%+v", receipt)
	}
	var parentStatus privacy.ErasureStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM privacy_erasure_heads WHERE erasure_id=$1`, erasureID).Scan(&parentStatus); err != nil {
		t.Fatal(err)
	}
	if parentStatus != privacy.StatusVerified {
		t.Fatalf("parent status=%q", parentStatus)
	}
}

func TestOfflineDevicePurgePossessionAndParentVerification(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	keyring, err := privacy.NewOfflineChallengeKeyring(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	store := privacydb.New(pool, privacydb.WithOfflineChallengeKeyring(keyring))
	now := time.Now().UTC().Truncate(time.Microsecond)
	erasureID := uuid.NewString()
	firstDevice := uuid.NewString()
	secondDevice := uuid.NewString()
	seedOfflinePurgeReceipt(t, pool, keyring, erasureID, []string{firstDevice, secondDevice}, now)

	firstTask, found, err := store.CurrentOfflineDevicePurge(ctx, firstDevice)
	if err != nil || !found {
		t.Fatalf("load first purge task: found=%t err=%v", found, err)
	}
	if firstTask.ErasureID != erasureID || firstTask.DeviceID != firstDevice || firstTask.OldGeneration != 1 || firstTask.CurrentGeneration != 2 || firstTask.Status != privacy.OfflineDeviceChildPending {
		t.Fatalf("first purge task=%+v", firstTask)
	}
	if _, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, secondDevice, privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision:    firstTask.ChallengeRevision,
		Challenge:            firstTask.Challenge,
		Outcome:              privacy.OfflinePurgeOutcomeSucceeded,
		ManagedObjectsAbsent: boolPointer(true),
	}); !privacyErrorCode(err, privacy.CodeOfflineChallengeInvalid) {
		t.Fatalf("cross-device challenge err=%v", err)
	}

	firstAck := privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision:    firstTask.ChallengeRevision,
		Challenge:            firstTask.Challenge,
		Outcome:              privacy.OfflinePurgeOutcomeSucceeded,
		ManagedObjectsAbsent: boolPointer(true),
	}
	firstReceipt, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, firstDevice, firstAck)
	if err != nil || firstReceipt.Status != privacy.OfflineDeviceChildSucceeded {
		t.Fatalf("ack first device receipt=%+v err=%v", firstReceipt, err)
	}
	replay, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, firstDevice, firstAck)
	if err != nil || replay.Status != privacy.OfflineDeviceChildSucceeded || replay.StableReason != firstReceipt.StableReason {
		t.Fatalf("replay first acknowledgment receipt=%+v err=%v", replay, err)
	}
	if _, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, firstDevice, privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision: firstTask.ChallengeRevision,
		Challenge:         firstTask.Challenge,
		Outcome:           privacy.OfflinePurgeOutcomeFailed,
		FailureCode:       privacy.OfflinePurgeFailureVerification,
	}); !privacyErrorCode(err, privacy.CodeOfflinePurgeAckConflict) {
		t.Fatalf("conflicting replay err=%v", err)
	}

	var parentStatus privacy.ErasureStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM privacy_erasure_heads WHERE erasure_id=$1`, erasureID).Scan(&parentStatus); err != nil {
		t.Fatal(err)
	}
	if parentStatus == privacy.StatusVerified {
		t.Fatalf("parent verified before every device acknowledged")
	}

	secondTask, found, err := store.CurrentOfflineDevicePurge(ctx, secondDevice)
	if err != nil || !found {
		t.Fatalf("load second purge task: found=%t err=%v", found, err)
	}
	if _, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, secondDevice, privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision:    secondTask.ChallengeRevision,
		Challenge:            secondTask.Challenge,
		Outcome:              privacy.OfflinePurgeOutcomeSucceeded,
		ManagedObjectsAbsent: boolPointer(true),
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM privacy_erasure_heads WHERE erasure_id=$1`, erasureID).Scan(&parentStatus); err != nil {
		t.Fatal(err)
	}
	if parentStatus != privacy.StatusVerified {
		t.Fatalf("parent status=%q", parentStatus)
	}
	var stepStatus privacy.StepStatus
	var pending, succeeded int
	if err := pool.QueryRow(ctx, `
		SELECT r.status,
		       count(*) FILTER (WHERE h.status='pending'),
		       count(*) FILTER (WHERE h.status='succeeded')
		FROM privacy_erasure_receipt_heads rh
		JOIN privacy_erasure_step_receipts r ON r.id=rh.current_receipt_id
		JOIN privacy_offline_device_children c ON c.erasure_id=rh.erasure_id
		JOIN privacy_offline_device_child_heads h ON h.child_id=c.id
		WHERE rh.erasure_id=$1 AND rh.store_kind='offline_device_cache'
		GROUP BY r.status`, erasureID).Scan(&stepStatus, &pending, &succeeded); err != nil {
		t.Fatal(err)
	}
	if stepStatus != privacy.StepSucceeded || succeeded != 2 || pending != 0 {
		t.Fatalf("offline-device step=%q pending=%d succeeded=%d", stepStatus, pending, succeeded)
	}
}

func seedOfflinePurgeReceipt(t *testing.T, pool *pgxpool.Pool, keyring *privacy.OfflineChallengeKeyring, erasureID string, devices []string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, deviceID := range devices {
		if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'offline device',$2)`, deviceID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),'learner_request',$2,$4::timestamptz,2,$4::timestamptz+interval '1 day')`, erasureID, devices[0], uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at) VALUES($1,'remote_purged',1,'offline_device_ack_pending',$2)`, erasureID, now); err != nil {
		t.Fatal(err)
	}
	stepID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method)
		VALUES($1,$2,'offline_device_cache',1,decode(repeat('cd',32),'hex'),$3,$3,'partial','offline_device_ack_pending','authenticated_device_challenge')`, stepID, erasureID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO privacy_erasure_receipt_heads(erasure_id,store_kind,current_receipt_id,current_version,updated_at) VALUES($1,'offline_device_cache',$2,1,$3)`, erasureID, stepID, now); err != nil {
		t.Fatal(err)
	}
	for _, store := range privacy.ReceiptSlots {
		if store == privacy.StoreOfflineDeviceCache {
			continue
		}
		status := privacy.StepSucceeded
		if store == privacy.StoreExternalProvider {
			status = privacy.StepUnsupported
		}
		receiptID := uuid.NewString()
		if _, err := pool.Exec(ctx, `
			INSERT INTO privacy_erasure_step_receipts(
				id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method)
			VALUES($1,$2,$3,1,decode(repeat('ce',32),'hex'),$4,$4,$5,'offline_purge_fixture_complete','fixture')`,
			receiptID, erasureID, store, now, status); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO privacy_erasure_receipt_heads(erasure_id,store_kind,current_receipt_id,current_version,updated_at)
			VALUES($1,$2,$3,1,$4)`, erasureID, store, receiptID, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, deviceID := range devices {
		childID := uuid.NewString()
		revisionID := uuid.NewString()
		challenge, err := keyring.Challenge(1, erasureID, deviceID, 1, 2, 1)
		if err != nil {
			t.Fatal(err)
		}
		digest := privacy.OfflineChallengeDigest(challenge)
		if _, err := pool.Exec(ctx, `INSERT INTO privacy_offline_device_children(id,erasure_id,device_id,source_generation,created_at) VALUES($1,$2,$3,1,$4)`, childID, erasureID, deviceID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO privacy_offline_device_child_revisions(id,child_id,revision,status,challenge_key_version,challenge_hash,stable_reason,updated_at,challenge_revision,issued_at)
			VALUES($1,$2,1,'pending',1,$3,'offline_device_ack_pending',$4,1,$4)`, revisionID, childID, digest[:], now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO privacy_offline_device_child_heads(child_id,current_revision_id,current_revision,status,updated_at) VALUES($1,$2,1,'pending',$3)`, childID, revisionID, now); err != nil {
			t.Fatal(err)
		}
	}
}

func boolPointer(value bool) *bool { return &value }

type offlinePurgePrivacyService struct{ *privacydb.Store }

func (offlinePurgePrivacyService) AuthorizeAndCommitBarrier(context.Context, privacy.ErasureRequest, privacy.ErasureGrantAuthorization) (privacy.ErasureReceipt, error) {
	return privacy.ErasureReceipt{}, errors.New("not used")
}

func (offlinePurgePrivacyService) Receipt(context.Context, string) (privacy.ErasureReceipt, error) {
	return privacy.ErasureReceipt{}, errors.New("not used")
}

func (offlinePurgePrivacyService) RunLocal(context.Context, string) (privacy.ErasureReceipt, error) {
	return privacy.ErasureReceipt{}, errors.New("not used")
}

func (offlinePurgePrivacyService) RunNocturne(context.Context, string) (privacy.ErasureReceipt, error) {
	return privacy.ErasureReceipt{}, errors.New("not used")
}

type offlinePurgeIdentity struct{ deviceID string }

func (identityService offlinePurgeIdentity) ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error) {
	return identity.IssuedCredential{}, nil
}

func (identityService offlinePurgeIdentity) Authenticate(context.Context, string, string) (identity.Credential, error) {
	return identity.Credential{Device: identity.Device{ID: identityService.deviceID}, Scopes: []string{"privacy:device"}}, nil
}

func (identityService offlinePurgeIdentity) ListDevices(context.Context) ([]identity.Device, error) {
	return []identity.Device{{ID: identityService.deviceID}}, nil
}

func (offlinePurgeIdentity) RevokeDevice(context.Context, string) error { return nil }

type offlinePurgeReadiness struct{}

func (offlinePurgeReadiness) Ready(context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}
}

func offlinePurgeHTTPHandler(t *testing.T, service httpapi.PrivacyService, deviceID string) http.Handler {
	t.Helper()
	handler, err := httpapi.New(httpapi.Options{
		Identity:       offlinePurgeIdentity{deviceID: deviceID},
		Privacy:        service,
		Readiness:      offlinePurgeReadiness{},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		PairLimiter:    httpapi.NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter:    httpapi.NewFixedWindowLimiter(100, time.Minute),
		DeviceLimiter:  httpapi.NewFixedWindowLimiter(100, time.Minute),
		PrivacyLimiter: httpapi.NewFixedWindowLimiter(100, time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func offlinePurgeHTTPRequest(t *testing.T, handler http.Handler, method, path string, input, output any) int {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer offline-test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if output != nil && response.Code >= 200 && response.Code < 300 {
		if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
			t.Fatalf("decode response status=%d body=%s: %v", response.Code, response.Body.String(), err)
		}
	}
	return response.Code
}

func privacyErrorCode(err error, code string) bool {
	var target *privacy.Error
	return errors.As(err, &target) && target.Code == code
}
