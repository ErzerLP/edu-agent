package postgresstore

import (
	"context"
	"errors"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type assessmentItemRowsStub struct {
	rowReturned bool
	terminalErr error
}

func (r *assessmentItemRowsStub) Next() bool {
	if r.rowReturned {
		return false
	}
	r.rowReturned = true
	return true
}

func (*assessmentItemRowsStub) Scan(...any) error { return nil }

func (r *assessmentItemRowsStub) Err() error { return r.terminalErr }

func TestScanAssessmentItemsRejectsPartialRowsOnIterationError(t *testing.T) {
	injected := errors.New("injected assessment item iteration failure")
	items, err := scanAssessmentItems(&assessmentItemRowsStub{terminalErr: injected})
	if !errors.Is(err, injected) {
		t.Fatalf("scan error=%v want=%v", err, injected)
	}
	if len(items) != 0 {
		t.Fatalf("partial assessment items escaped iteration failure: %+v", items)
	}
}

type loaderGateRow struct {
	err error
}

func (r loaderGateRow) Scan(...any) error { return r.err }

type loaderGateDB struct {
	gateCalls int
	queries   int
}

func (*loaderGateDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (db *loaderGateDB) QueryRow(context.Context, string, ...any) pgx.Row {
	db.gateCalls++
	return loaderGateRow{err: &pgconn.PgError{Message: privacy.CodeContentRedacted}}
}

func (db *loaderGateDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	db.queries++
	return nil, errors.New("typed content query must not run behind a closed privacy gate")
}

func TestLearningReadGateStopsTypedLoaderBeforeContentOrModelWork(t *testing.T) {
	db := &loaderGateDB{}
	continued := false
	_, err := withLearningReadGate(context.Background(), db, func(learningLoaderDB) (string, error) {
		continued = true
		return "typed-secret", nil
	})
	if privacy.ErrorCode(err) != privacy.CodeContentRedacted {
		t.Fatalf("gate error=%v code=%q", err, privacy.ErrorCode(err))
	}
	if continued || db.gateCalls != 1 || db.queries != 0 {
		t.Fatalf("continued=%v gate_calls=%d content_queries=%d", continued, db.gateCalls, db.queries)
	}
}
