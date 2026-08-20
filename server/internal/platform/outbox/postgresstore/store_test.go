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
