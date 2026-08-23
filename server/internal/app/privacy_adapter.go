package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/nocturne"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

type privacyOrchestratorStore interface {
	CommitBarrierAuthorized(context.Context, privacy.ErasureRequest, privacy.ErasureGrantAuthorization) (privacy.ErasureReceipt, error)
	Receipt(context.Context, string) (privacy.ErasureReceipt, error)
	RunLocalScrub(context.Context, string) (privacy.ErasureReceipt, error)
	RunNocturneErase(context.Context, string, privacy.RemoteEraser) (privacy.ErasureReceipt, error)
	RunManagedBackupVerification(context.Context, string, privacy.ManagedBackupVerifier) (privacy.ErasureReceipt, error)
}

// privacyHTTPAdapter maps the transport's small orchestration port to the
// resumable privacy store. Optional remote failures deliberately remain ordinary
// errors so the HTTP layer returns the already durable receipt as queued.
type privacyHTTPAdapter struct {
	store     privacyOrchestratorStore
	eraser    privacy.RemoteEraser
	verifier  privacy.ManagedBackupVerifier
	preflight *nocturnePreflightGate
}

func (a *privacyHTTPAdapter) AuthorizeAndCommitBarrier(ctx context.Context, request privacy.ErasureRequest, authorization privacy.ErasureGrantAuthorization) (privacy.ErasureReceipt, error) {
	return a.store.CommitBarrierAuthorized(ctx, request, authorization)
}

func (a *privacyHTTPAdapter) Receipt(ctx context.Context, erasureID string) (privacy.ErasureReceipt, error) {
	return a.store.Receipt(ctx, erasureID)
}

func (a *privacyHTTPAdapter) RunLocal(ctx context.Context, erasureID string) (privacy.ErasureReceipt, error) {
	return a.store.RunLocalScrub(ctx, erasureID)
}

func (a *privacyHTTPAdapter) RunNocturne(ctx context.Context, erasureID string) (privacy.ErasureReceipt, error) {
	if a.preflight != nil {
		if err := a.preflight.Check(ctx); err != nil && nocturne.IsContractMismatch(err) {
			receipt, receiptErr := a.store.Receipt(ctx, erasureID)
			if receiptErr != nil {
				return privacy.ErasureReceipt{}, receiptErr
			}
			if nocturneContractMismatchRecorded(receipt) {
				return receipt, nil
			}
			return a.store.RunNocturneErase(ctx, erasureID, contractMismatchRemoteEraser{})
		}
	}
	if a.eraser == nil {
		receipt, err := a.store.Receipt(ctx, erasureID)
		if err != nil {
			return privacy.ErasureReceipt{}, err
		}
		return receipt, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "remote_eraser_missing"}
	}
	return a.store.RunNocturneErase(ctx, erasureID, a.eraser)
}

type contractMismatchRemoteEraser struct{}

func (contractMismatchRemoteEraser) Erase(_ context.Context, request privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
	evidence := sha256.Sum256([]byte("nocturne-contract-mismatch\n" + request.ErasureID))
	return privacy.RemoteEraseResult{
		Status: privacy.StepPartial, StableReason: "nocturne_contract_mismatch",
		EvidenceDigest: hex.EncodeToString(evidence[:]), CompletedAt: time.Now().UTC(),
	}, nil
}

func nocturneContractMismatchRecorded(receipt privacy.ErasureReceipt) bool {
	matched := 0
	for _, step := range receipt.Steps {
		switch step.Store {
		case privacy.StoreNocturnePaths, privacy.StoreNocturneOrphanHistory, privacy.StoreNocturneSnapshotChangeset:
			if step.Status != privacy.StepPartial || step.StableReason != "nocturne_contract_mismatch" {
				return false
			}
			matched++
		}
	}
	return matched == 3
}

func (a *privacyHTTPAdapter) verifyManagedBackup(ctx context.Context, erasureID string) (privacy.ErasureReceipt, error) {
	if a.verifier == nil {
		return a.store.Receipt(ctx, erasureID)
	}
	return a.store.RunManagedBackupVerification(ctx, erasureID, a.verifier)
}
