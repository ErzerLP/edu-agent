package postgresstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type remoteEraserFunc func(context.Context, privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error)

func (f remoteEraserFunc) Erase(ctx context.Context, request privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
	return f(ctx, request)
}

func TestRunNocturneEraseConcurrentCallsPreserveMaintenanceAuthorization(t *testing.T) {
	pool := privacyIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'并发清除测试',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	memoryStore := memorydb.New(pool)
	service, err := memory.NewService(memoryStore, memory.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateCandidate(ctx, memory.DevicePrincipal{DeviceID: deviceID}, memory.CreateCandidateCommand{
		OperationID: uuid.NewString(), Content: "我偏好简洁的分步解释", Reason: "用户明确表达的偏好",
		Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivityNonSensitive,
		Stability: memory.StabilityStable, ValidUntil: time.Now().UTC().Add(time.Hour),
	})
	if err != nil || created.Delivery == nil {
		t.Fatalf("创建交付失败：%+v，错误：%v", created, err)
	}
	attempt, err := memoryStore.ClaimAttempt(ctx, created.Delivery.ID, time.Time{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memoryStore.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "并发清除测试", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	store := newAuthorizedBarrierStore(pool, privacy.NewReadPermitManager())
	barrier, err := store.CommitBarrier(ctx, barrierRequest(deviceID, uuid.NewString(), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}

	// 独立连接池模拟 HTTP 与后台任务，排除仅靠单个 Store 内互斥的实现。
	otherPool, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	otherStore := newAuthorizedBarrierStore(otherPool, privacy.NewReadPermitManager())
	claimed := make(chan privacy.RemoteEraseRequest, 1)
	resume := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := store.RunNocturneErase(ctx, barrier.ErasureID, remoteEraserFunc(func(ctx context.Context, request privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
			auth := memory.MaintenanceAuthorization{
				ErasureID: request.ErasureID, ReceiptID: request.Receipt.ID, TargetLearnerGeneration: request.LearnerGeneration,
			}
			reconciliation, err := memoryStore.ClaimMaintenanceExpiryReconciliation(ctx, auth, time.Time{}, 2*time.Minute)
			if err != nil {
				return privacy.RemoteEraseResult{}, err
			}
			reconciliation, err = memoryStore.TransitionMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationTransition{
				ReconciliationID: reconciliation.ID, LeaseToken: reconciliation.LeaseToken,
				From: memory.ReconciliationReconciling, To: memory.ReconciliationDeletePending, At: time.Now().UTC(),
			})
			if err != nil {
				return privacy.RemoteEraseResult{}, err
			}
			claimed <- request
			select {
			case <-resume:
			case <-ctx.Done():
				return privacy.RemoteEraseResult{}, ctx.Err()
			}
			_, err = memoryStore.FinalizeMaintenanceExpiryReconciliation(ctx, auth, memory.ReconciliationFinalization{
				ReconciliationID: reconciliation.ID, LeaseToken: reconciliation.LeaseToken,
				From: memory.ReconciliationDeletePending, Result: memory.ReconciliationDeleteResult,
				ReceiptID: uuid.NewString(), EvidenceDigest: reconciliation.ContentHash, At: time.Now().UTC(),
			})
			return privacy.RemoteEraseResult{
				Status: privacy.StepSucceeded, StableReason: "all_old_generation_remote_reconciliations_verified",
				EvidenceDigest: reconciliation.ContentHash, CompletedAt: time.Now().UTC(),
			}, err
		}))
		finished <- err
	}()
	var active privacy.RemoteEraseRequest
	select {
	case active = <-claimed:
	case err := <-finished:
		t.Fatalf("未取得维护租约：%v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	competing := &scriptedRemoteEraser{results: []privacy.RemoteEraseResult{{
		Status: privacy.StepPartial, StableReason: "remote_reconciliation_pending",
		EvidenceDigest: strings.Repeat("11", 32), CompletedAt: time.Now().UTC(),
	}}}
	observed, competingErr := otherStore.RunNocturneErase(ctx, barrier.ErasureID, competing)
	close(resume)
	if err := <-finished; err != nil {
		t.Fatalf("并发推进使仍持租约的维护授权失效：%v", err)
	}
	if competingErr != nil || len(competing.requests) != 0 {
		t.Fatalf("竞争者执行了远端步骤：调用次数=%d，错误=%v", len(competing.requests), competingErr)
	}
	for _, step := range observed.Steps {
		if step.Store == privacy.StoreNocturnePaths && step.ID != active.Receipt.ID {
			t.Fatalf("竞争者替换了在途授权回执：%s != %s", step.ID, active.Receipt.ID)
		}
	}
	complete, err := otherStore.RunNocturneErase(ctx, barrier.ErasureID, competing)
	if err != nil || complete.Status != privacy.StatusRemotePurged || len(competing.requests) != 0 {
		t.Fatalf("完成后重试未复用回执：%+v，错误=%v", complete, err)
	}
	var pending int
	var gateOpen bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM memory_erasure_delivery_scopes WHERE status<>'succeeded'),
		(SELECT read_open AND write_open FROM privacy_owner_generation_gates WHERE owner_kind='memory')`).Scan(&pending, &gateOpen); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || !gateOpen {
		t.Fatalf("远端清除未收敛：剩余范围=%d，memory gate=%t", pending, gateOpen)
	}
}

func TestRunNocturneEraseReleasesLockOnFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "远端错误"
		if canceled {
			name = "请求取消"
		}
		t.Run(name, func(t *testing.T) {
			pool := privacyIntegrationPool(t)
			ctx := context.Background()
			deviceID := uuid.NewString()
			if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'清除恢复测试',clock_timestamp())`, deviceID); err != nil {
				t.Fatal(err)
			}
			store := newAuthorizedBarrierStore(pool, privacy.NewReadPermitManager())
			barrier, err := store.CommitBarrier(ctx, barrierRequest(deviceID, uuid.NewString(), time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
				t.Fatal(err)
			}
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			failure := errors.New("远端暂时不可用")
			_, err = store.RunNocturneErase(requestCtx, barrier.ErasureID, remoteEraserFunc(func(context.Context, privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
				if canceled {
					cancel()
					return privacy.RemoteEraseResult{}, requestCtx.Err()
				}
				return privacy.RemoteEraseResult{}, failure
			}))
			if canceled {
				failure = context.Canceled
			}
			if !errors.Is(err, failure) {
				t.Fatalf("未保留原始错误：%v", err)
			}
			otherPool, err := pgxpool.NewWithConfig(ctx, pool.Config())
			if err != nil {
				t.Fatal(err)
			}
			defer otherPool.Close()
			otherStore := newAuthorizedBarrierStore(otherPool, privacy.NewReadPermitManager())
			eraser := &scriptedRemoteEraser{results: []privacy.RemoteEraseResult{{
				Status: privacy.StepSucceeded, StableReason: "nocturne_absence_verified",
				EvidenceDigest: strings.Repeat("22", 32), CompletedAt: time.Now().UTC(),
			}}}
			complete, err := otherStore.RunNocturneErase(ctx, barrier.ErasureID, eraser)
			if err != nil || complete.Status != privacy.StatusRemotePurged || len(eraser.requests) != 1 {
				t.Fatalf("失败后锁未释放或无法恢复：%+v，错误=%v", complete, err)
			}
		})
	}
}
