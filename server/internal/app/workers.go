package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type workerSpec struct {
	name          string
	interval      time.Duration
	retryInterval time.Duration
	batch         int
	runOnce       func(context.Context) (int, error)
}

func periodicWorker(name string, interval time.Duration, batch int, runOnce func(context.Context) (int, error)) workerSpec {
	return workerSpec{name: name, interval: interval, batch: batch, runOnce: runOnce}
}

type workerGroup struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startWorkerGroup(parent context.Context, logger *slog.Logger, specs []workerSpec) *workerGroup {
	ctx, cancel := context.WithCancel(parent)
	group := &workerGroup{cancel: cancel, done: make(chan struct{})}
	var wait sync.WaitGroup
	for _, candidate := range specs {
		spec := candidate
		wait.Add(1)
		go func() {
			defer wait.Done()
			runPeriodicWorker(ctx, logger, spec)
		}()
	}
	go func() {
		wait.Wait()
		close(group.done)
	}()
	return group
}

func (g *workerGroup) Stop(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.cancel()
	select {
	case <-g.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runPeriodicWorker(ctx context.Context, logger *slog.Logger, spec workerSpec) {
	if spec.interval <= 0 || spec.batch <= 0 || spec.runOnce == nil {
		logger.Error("background worker configuration rejected", "worker", spec.name, "error_category", "invalid_configuration")
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		_, err := spec.runOnce(ctx)
		nextInterval := spec.interval
		if err != nil && ctx.Err() == nil {
			logger.Warn("background worker operation failed", "worker", spec.name, "error_category", workerErrorCategory(err))
			if spec.retryInterval > 0 && spec.retryInterval < nextInterval {
				nextInterval = spec.retryInterval
			}
		}
		timer.Reset(nextInterval)
	}
}

func workerErrorCategory(err error) string {
	if errors.Is(err, privacy.ErrGenerationKeyUnavailable) || errors.Is(err, privacy.ErrGenerationKeyDestroyed) {
		return "deferred"
	}
	var classified interface {
		Category() string
		Permanent() bool
	}
	if errors.As(err, &classified) && classified.Category() != "" {
		return classified.Category()
	}
	if code := memory.ErrorCode(err); code != "" {
		return code
	}
	if code := privacy.ErrorCode(err); code != "" {
		return code
	}
	return "unavailable"
}

func currentMemoryGeneration(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var generation int64
	if err := pool.QueryRow(ctx, `
		SELECT learner_generation
		FROM privacy_owner_generation_gates
		WHERE owner_kind='memory'`).Scan(&generation); err != nil {
		return 0, err
	}
	return generation, nil
}

func resumeActivePrivacyErasure(ctx context.Context, pool *pgxpool.Pool, adapter *privacyHTTPAdapter) (int, error) {
	var erasureID string
	err := pool.QueryRow(ctx, `
		SELECT erasure_id
		FROM privacy_erasure_heads
		WHERE status<>'verified'
		ORDER BY updated_at,erasure_id
		LIMIT 1`).Scan(&erasureID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	receipt, err := adapter.Receipt(ctx, erasureID)
	if err != nil {
		return 1, err
	}
	_, err = resumePrivacyErasure(ctx, adapter, receipt)
	return 1, err
}

func resumePrivacyErasure(ctx context.Context, adapter *privacyHTTPAdapter, receipt privacy.ErasureReceipt) (privacy.ErasureReceipt, error) {
	if receipt.Status == privacy.StatusVerified {
		return receipt, nil
	}
	var err error
	if receipt.Status == privacy.StatusBarrierCommitted {
		receipt, err = adapter.RunLocal(ctx, receipt.ErasureID)
		if err != nil {
			return receipt, err
		}
	}
	if !nocturneReceiptStepsComplete(receipt) {
		switch receipt.Status {
		case privacy.StatusLocalScrubbed, privacy.StatusRemoteDraining, privacy.StatusPartial:
			receipt, err = adapter.RunNocturne(ctx, receipt.ErasureID)
			if err != nil {
				return receipt, err
			}
		}
	}
	if nocturneReceiptStepsComplete(receipt) &&
		(receipt.Status == privacy.StatusRemotePurged || receipt.Status == privacy.StatusPartial) {
		return adapter.verifyManagedBackup(ctx, receipt.ErasureID)
	}
	return receipt, nil
}

func nocturneReceiptStepsComplete(receipt privacy.ErasureReceipt) bool {
	complete := 0
	for _, step := range receipt.Steps {
		switch step.Store {
		case privacy.StoreNocturnePaths, privacy.StoreNocturneOrphanHistory, privacy.StoreNocturneSnapshotChangeset:
			if step.Status == privacy.StepSucceeded || step.Status == privacy.StepNotApplicable {
				complete++
			}
		}
	}
	return complete == 3
}
