package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type ClassifiedError interface {
	error
	Category() string
	Permanent() bool
}

type WorkerOptions struct {
	BatchSize   int
	Lease       time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Jitter      func(time.Duration) time.Duration
	Now         func() time.Time
}

type Worker struct {
	store       Store
	consumers   map[string]Consumer
	batchSize   int
	lease       time.Duration
	baseBackoff time.Duration
	maxBackoff  time.Duration
	jitter      func(time.Duration) time.Duration
	now         func() time.Time
}

func NewWorker(store Store, consumers map[string]Consumer, options WorkerOptions) (*Worker, error) {
	if store == nil || options.BatchSize <= 0 || options.Lease <= 0 || options.BaseBackoff <= 0 || options.MaxBackoff < options.BaseBackoff {
		return nil, errors.New("valid outbox store, batch, lease, and backoff settings are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Jitter == nil {
		options.Jitter = func(delay time.Duration) time.Duration {
			// Time-derived jitter is sufficient here; it does not protect a secret.
			return time.Duration(time.Now().UnixNano() % int64(delay/4+1))
		}
	}
	return &Worker{
		store: store, consumers: consumers, batchSize: options.BatchSize, lease: options.Lease,
		baseBackoff: options.BaseBackoff, maxBackoff: options.MaxBackoff,
		jitter: options.Jitter, now: options.Now,
	}, nil
}

func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	now := w.now().UTC()
	messages, err := w.store.Claim(ctx, now, w.lease, w.batchSize)
	if err != nil {
		return 0, fmt.Errorf("claim outbox messages: %w", err)
	}
	for _, message := range messages {
		consumer, ok := w.consumers[message.BusinessType]
		if !ok {
			if err := w.store.MarkFailed(ctx, message.ID, message.LeaseToken, "unsupported_business_type", now, now, true); err != nil {
				return 0, err
			}
			continue
		}
		decision, err := consumer.CanApply(ctx, message)
		if err == nil {
			err = decision.Validate()
		}
		if err == nil && !decision.Apply {
			if err := w.store.Cancel(ctx, CancelRequest{
				IdempotencyKey: message.IdempotencyKey, LeaseToken: message.LeaseToken,
				Disposition: decision.TerminalDisposition, CanceledAt: now,
			}); err != nil {
				return 0, err
			}
			continue
		}
		if err == nil {
			err = consumer.Apply(ctx, message)
		}
		if errors.Is(err, ErrConsumerFinalized) {
			continue
		}
		if err == nil {
			if err := w.store.MarkApplied(ctx, message.ID, message.LeaseToken, now); err != nil {
				return 0, err
			}
			continue
		}
		category, permanent := classify(err)
		dead := permanent || message.Attempts >= message.MaxAttempts
		next := now
		if !dead {
			next = now.Add(w.backoff(message.Attempts))
		}
		if err := w.store.MarkFailed(ctx, message.ID, message.LeaseToken, category, now, next, dead); err != nil {
			return 0, err
		}
	}
	return len(messages), nil
}

func (w *Worker) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := w.baseBackoff
	for i := 1; i < attempt && delay < w.maxBackoff; i++ {
		if delay > w.maxBackoff/2 {
			delay = w.maxBackoff
			break
		}
		delay *= 2
	}
	if delay > w.maxBackoff {
		delay = w.maxBackoff
	}
	jitter := w.jitter(delay)
	if jitter < 0 {
		jitter = 0
	}
	if delay+jitter > w.maxBackoff {
		return w.maxBackoff
	}
	return delay + jitter
}

func classify(err error) (string, bool) {
	var classified ClassifiedError
	if errors.As(err, &classified) {
		category := classified.Category()
		if category == "" {
			category = "consumer_error"
		}
		return category, classified.Permanent()
	}
	return "consumer_error", false
}
