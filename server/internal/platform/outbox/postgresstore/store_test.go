package postgresstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type enqueueTestDB struct {
	inserted  bool
	identical bool
	queried   bool
}

func (db *enqueueTestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	if db.inserted {
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	return pgconn.NewCommandTag("INSERT 0 0"), nil
}

func (db *enqueueTestDB) QueryRow(context.Context, string, ...any) pgx.Row {
	db.queried = true
	return enqueueBoolRow{value: db.identical}
}

type enqueueBoolRow struct {
	value bool
}

func (row enqueueBoolRow) Scan(dest ...any) error {
	value, ok := dest[0].(*bool)
	if !ok {
		return errors.New("expected bool scan destination")
	}
	*value = row.value
	return nil
}

func TestEnqueueWithDistinguishesInsertReplayAndConflict(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "learning.test", AggregateID: "aggregate-1", IdempotencyKey: "key-1",
		Revision: 3, Generation: 2, Payload: []byte(`{"value":1}`),
		AuditMetadata: []byte(`{"actor":"device-1"}`), MaxAttempts: 4,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	insert := &enqueueTestDB{inserted: true}
	inserted, err := EnqueueWith(context.Background(), insert, message)
	if err != nil || !inserted || insert.queried {
		t.Fatalf("insert result inserted=%v queried=%v err=%v", inserted, insert.queried, err)
	}

	replay := &enqueueTestDB{identical: true}
	inserted, err = EnqueueWith(context.Background(), replay, message)
	if err != nil || inserted || !replay.queried {
		t.Fatalf("replay result inserted=%v queried=%v err=%v", inserted, replay.queried, err)
	}

	conflict := &enqueueTestDB{identical: false}
	inserted, err = EnqueueWith(context.Background(), conflict, message)
	if inserted || !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict result inserted=%v err=%v", inserted, err)
	}
}

type requeueTestDB struct {
	changed      bool
	status       outbox.Status
	tupleMatches bool
}

func (*requeueTestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected Exec")
}

func (db *requeueTestDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return requeueStateRow{changed: db.changed, status: db.status, tupleMatches: db.tupleMatches}
}

type requeueStateRow struct {
	changed      bool
	status       outbox.Status
	tupleMatches bool
}

func (row requeueStateRow) Scan(dest ...any) error {
	*dest[0].(*bool) = row.changed
	*dest[1].(*outbox.Status) = row.status
	*dest[2].(*bool) = row.tupleMatches
	return nil
}

func TestRequeueDeadWithRequiresMatchingTupleAndIsIdempotent(t *testing.T) {
	request := outbox.RequeueRequest{
		BusinessType: "memory.delivery", AggregateID: "logical-1", IdempotencyKey: "memory.delivery:1",
		Revision: 2, Generation: 3, Payload: []byte(`{"delivery_id":"one"}`),
		AvailableAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	}
	changed := &requeueTestDB{changed: true, status: outbox.StatusPending, tupleMatches: true}
	if err := RequeueDeadWith(context.Background(), changed, request); err != nil {
		t.Fatalf("dead requeue err=%v", err)
	}
	pending := &requeueTestDB{status: outbox.StatusPending, tupleMatches: true}
	if err := RequeueDeadWith(context.Background(), pending, request); err != nil {
		t.Fatalf("idempotent pending requeue err=%v", err)
	}
	mismatch := &requeueTestDB{status: outbox.StatusDead}
	if err := RequeueDeadWith(context.Background(), mismatch, request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("tuple mismatch err=%v", err)
	}
	applied := &requeueTestDB{status: outbox.StatusApplied, tupleMatches: true}
	if err := RequeueDeadWith(context.Background(), applied, request); !errors.Is(err, outbox.ErrInvalidTransition) {
		t.Fatalf("applied requeue err=%v", err)
	}
}

type cancelTestDB struct {
	changed        bool
	status         outbox.Status
	disposition    outbox.TerminalDisposition
	payloadMatches bool
	queried        bool
}

func (*cancelTestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected Exec")
}

func (db *cancelTestDB) QueryRow(context.Context, string, ...any) pgx.Row {
	db.queried = true
	return cancelStateRow{
		changed: db.changed, status: db.status, disposition: db.disposition,
		payloadMatches: db.payloadMatches,
	}
}

type cancelStateRow struct {
	changed        bool
	status         outbox.Status
	disposition    outbox.TerminalDisposition
	payloadMatches bool
}

func (row cancelStateRow) Scan(dest ...any) error {
	*dest[0].(*bool) = row.changed
	*dest[1].(*outbox.Status) = row.status
	*dest[2].(*outbox.TerminalDisposition) = row.disposition
	*dest[3].(*bool) = row.payloadMatches
	return nil
}

func TestCancelWithRequiresProcessingLeaseAndIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	request := outbox.CancelRequest{IdempotencyKey: "key-1", Disposition: outbox.DispositionExpired, CanceledAt: now}

	pending := &cancelTestDB{changed: true, status: outbox.StatusCanceled, disposition: outbox.DispositionExpired, payloadMatches: true}
	if err := CancelWith(context.Background(), pending, request); err != nil || !pending.queried {
		t.Fatalf("pending cancel err=%v queried=%v", err, pending.queried)
	}

	leaseLost := &cancelTestDB{status: outbox.StatusProcessing}
	request.LeaseToken = "00000000-0000-4000-8000-000000000001"
	if err := CancelWith(context.Background(), leaseLost, request); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("wrong processing lease err=%v", err)
	}

	alreadyCanceled := &cancelTestDB{status: outbox.StatusCanceled, disposition: outbox.DispositionExpired, payloadMatches: true}
	if err := CancelWith(context.Background(), alreadyCanceled, request); err != nil {
		t.Fatalf("idempotent cancel err=%v", err)
	}

	differentDisposition := &cancelTestDB{status: outbox.StatusCanceled, disposition: outbox.DispositionDeleted, payloadMatches: true}
	if err := CancelWith(context.Background(), differentDisposition, request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different cancellation disposition err=%v", err)
	}

	differentTombstone := &cancelTestDB{status: outbox.StatusCanceled, disposition: outbox.DispositionExpired}
	request.TombstonePayload = []byte(`{"tombstone":true}`)
	if err := CancelWith(context.Background(), differentTombstone, request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different cancellation tombstone err=%v", err)
	}

	terminal := &cancelTestDB{status: outbox.StatusApplied}
	if err := CancelWith(context.Background(), terminal, request); !errors.Is(err, outbox.ErrInvalidTransition) {
		t.Fatalf("applied cancel err=%v", err)
	}
}
