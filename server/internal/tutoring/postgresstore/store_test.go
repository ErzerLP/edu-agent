package postgresstore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type recordingDBTX struct {
	statements []string
}

type scriptedRow func(...any) error

func (row scriptedRow) Scan(dest ...any) error {
	return row(dest...)
}

type scriptedDBTX struct {
	queries []string
	rows    []scriptedRow
}

func (db *scriptedDBTX) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	db.queries = append(db.queries, sql)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *scriptedDBTX) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	db.queries = append(db.queries, sql)
	row := db.rows[0]
	db.rows = db.rows[1:]
	return row
}

func openTutoringGateRow() scriptedRow {
	return func(dest ...any) error {
		*dest[0].(*int64) = 1
		return nil
	}
}

func sessionRow() scriptedRow {
	return func(dest ...any) error {
		*dest[0].(*string) = "session"
		*dest[1].(*int64) = 4
		*dest[2].(*string) = string(tutoring.StateRouteActive)
		for index := 3; index <= 9; index++ {
			*dest[index].(**string) = nil
		}
		*dest[10].(*bool) = false
		return nil
	}
}

func (db *recordingDBTX) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	db.statements = append(db.statements, sql)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (*recordingDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("query not expected")
}

func TestLoadSessionWithCarriesOnlyLatestUnresumedInvalidationMarker(t *testing.T) {
	for _, test := range []struct {
		name                 string
		invalidated, resumed bool
		wantMarker           bool
	}{
		{name: "latest invalidated", invalidated: true, resumed: false, wantMarker: true},
		{name: "latest resumed", invalidated: true, resumed: true, wantMarker: false},
		{name: "latest active", invalidated: false, resumed: false, wantMarker: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := &scriptedDBTX{rows: []scriptedRow{
				openTutoringGateRow(),
				sessionRow(),
				func(...any) error { return pgx.ErrNoRows },
				func(dest ...any) error {
					*dest[0].(*bool) = test.invalidated
					*dest[1].(*bool) = !test.resumed
					return nil
				},
			}}
			value, err := New(nil).LoadSessionWith(context.Background(), db, "session")
			if err != nil {
				t.Fatal(err)
			}
			if value.FocusFrameInvalidated != test.wantMarker {
				t.Fatalf("marker=%v want=%v session=%+v", value.FocusFrameInvalidated, test.wantMarker, value)
			}
			if len(db.queries) != 4 || !strings.Contains(db.queries[3], "ORDER BY created_event_seq DESC,id DESC LIMIT 1") {
				t.Fatalf("queries=%v", db.queries)
			}
		})
	}
}

func TestPersistUsesCallerOwnedDBTXForAllTutoringRecords(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	session := &tutoring.Session{ID: "session", State: tutoring.StateFreeAnswer, AggregateVer: 4}
	frame := &tutoring.FocusFrame{ID: "frame", SessionID: session.ID, SavedState: tutoring.StateRouteActive, SavedAggregateVersion: 3, CreatedEventSequence: 9}
	question := &tutoring.FreeQuestion{ID: "question", SessionID: session.ID, FocusFrameID: frame.ID, Text: "why", KnowledgeRevisionID: "knowledge", ReceivedAt: now}
	answer := &tutoring.FreeAnswer{ID: "answer", SessionID: session.ID, FocusFrameID: frame.ID, FreeQuestionID: question.ID, Text: "because", KnowledgeRevisionID: "knowledge", ReceivedAt: now}
	db := &recordingDBTX{}
	if err := New(nil).Persist(context.Background(), db, WriteSet{
		Session: session, FocusFrame: frame, InvalidateFrame: true, ResumeFrame: true,
		FreeQuestion: question, FreeAnswer: answer, ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	wantTables := []string{
		"tutoring_sessions", "tutoring_focus_frames", "tutoring_focus_frames", "tutoring_focus_frames",
		"tutoring_free_questions", "tutoring_free_answers",
	}
	if len(db.statements) != len(wantTables) {
		t.Fatalf("statements=%d want=%d", len(db.statements), len(wantTables))
	}
	for index, table := range wantTables {
		if !strings.Contains(db.statements[index], table) {
			t.Errorf("statement[%d] does not target %s: %s", index, table, db.statements[index])
		}
	}
}

func TestOwnerStoreNeverControlsCallerTransaction(t *testing.T) {
	content, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"BeginTx(", ".Commit(", ".Rollback("} {
		if strings.Contains(string(content), forbidden) {
			t.Fatalf("owner store controls caller transaction through %s", forbidden)
		}
	}
}
