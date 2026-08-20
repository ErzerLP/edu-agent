package postgresstore

import (
	"errors"
	"testing"
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
