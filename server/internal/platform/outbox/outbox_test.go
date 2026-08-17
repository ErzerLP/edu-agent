package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMessageValidationAndTransitions(t *testing.T) {
	now := time.Now().UTC()
	message, err := NewMessage(NewMessageInput{
		BusinessType: "knowledge.publish", AggregateID: "note-1", IdempotencyKey: "publish:note-1:2:1",
		Revision: 2, Generation: 1, Payload: json.RawMessage(`{"private":"payload"}`),
		AuditMetadata: json.RawMessage(`{"kind":"publish"}`), MaxAttempts: 5,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if message.Status != StatusPending || message.ID == "" {
		t.Fatalf("unexpected message: %+v", message)
	}
	allowed := [][2]Status{{StatusPending, StatusProcessing}, {StatusProcessing, StatusPending}, {StatusProcessing, StatusApplied}, {StatusProcessing, StatusDead}}
	for _, transition := range allowed {
		if !CanTransition(transition[0], transition[1]) {
			t.Fatalf("transition should be allowed: %v", transition)
		}
	}
	if CanTransition(StatusApplied, StatusPending) || CanTransition(StatusDead, StatusPending) {
		t.Fatal("terminal states must not transition")
	}
}

type memoryWorkerStore struct {
	claimed []Message
	applied []string
	failed  []failure
}

type failure struct {
	id       string
	category string
	next     time.Time
	dead     bool
}

func (*memoryWorkerStore) Enqueue(context.Context, Message) (bool, error) { return true, nil }
func (s *memoryWorkerStore) Claim(context.Context, time.Time, time.Duration, int) ([]Message, error) {
	return append([]Message(nil), s.claimed...), nil
}
func (s *memoryWorkerStore) MarkApplied(_ context.Context, id string, _ time.Time) error {
	s.applied = append(s.applied, id)
	return nil
}
func (s *memoryWorkerStore) MarkFailed(_ context.Context, id, category string, _ time.Time, next time.Time, dead bool) error {
	s.failed = append(s.failed, failure{id: id, category: category, next: next, dead: dead})
	return nil
}

type testConsumer struct {
	allow bool
	err   error
	calls int
}

func (c *testConsumer) CanApply(context.Context, Message) (bool, error) { return c.allow, nil }
func (c *testConsumer) Apply(context.Context, Message) error {
	c.calls++
	return c.err
}

type permanentError struct{}

func (permanentError) Error() string    { return "permanent" }
func (permanentError) Category() string { return "invalid_target" }
func (permanentError) Permanent() bool  { return true }

func TestWorkerHonorsFenceAndRetriesWithBoundedBackoff(t *testing.T) {
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	store := &memoryWorkerStore{claimed: []Message{
		{ID: "stale", BusinessType: "stale", Attempts: 1, MaxAttempts: 3},
		{ID: "retry", BusinessType: "retry", Attempts: 2, MaxAttempts: 3},
		{ID: "dead", BusinessType: "dead", Attempts: 1, MaxAttempts: 3},
		{ID: "unsupported", BusinessType: "missing", Attempts: 1, MaxAttempts: 3},
	}}
	stale := &testConsumer{allow: false}
	retry := &testConsumer{allow: true, err: errors.New("temporary")}
	dead := &testConsumer{allow: true, err: permanentError{}}
	worker, err := NewWorker(store, map[string]Consumer{"stale": stale, "retry": retry, "dead": dead}, WorkerOptions{
		BatchSize: 10, Lease: time.Minute, BaseBackoff: time.Second, MaxBackoff: 10 * time.Second,
		Jitter: func(time.Duration) time.Duration { return 500 * time.Millisecond }, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := worker.RunOnce(context.Background())
	if err != nil || count != 4 {
		t.Fatalf("run worker: count=%d err=%v", count, err)
	}
	if stale.calls != 0 || len(store.applied) != 1 || store.applied[0] != "stale" {
		t.Fatalf("fenced message should be a successful no-op: %+v", store)
	}
	if len(store.failed) != 3 {
		t.Fatalf("expected three failures: %+v", store.failed)
	}
	if store.failed[0].dead || !store.failed[0].next.Equal(now.Add(2500*time.Millisecond)) {
		t.Fatalf("unexpected bounded retry: %+v", store.failed[0])
	}
	if !store.failed[1].dead || store.failed[1].category != "invalid_target" {
		t.Fatalf("permanent error should be dead: %+v", store.failed[1])
	}
	if !store.failed[2].dead || store.failed[2].category != "unsupported_business_type" {
		t.Fatalf("missing consumer should be dead: %+v", store.failed[2])
	}
}
