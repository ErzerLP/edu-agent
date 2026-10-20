package mentorrun

import (
	"bytes"
	"context"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"testing"
)

func TestWebResourceDeduplicationSurvivesKeyRotation(t *testing.T) {
	f, h := newWebHost(t)
	ctx := context.Background()
	before, err := h.registerWebResource(ctx, "https://example.org/same", "同一网页", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{43}, 32)
	rewrap, err := keyrotation.New(bytes.Repeat([]byte{42}, 32), key)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.pool, f.settings, key)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := scan(f.pool.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1`, h.owned.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.decode(&owned); err != nil {
		t.Fatal(err)
	}
	next := &executionHost{service: restarted, owned: owned, body: owned.body}
	after, err := next.registerWebResource(ctx, before.URL, "同一网页", "", "search")
	if err != nil || after.ID != before.ID {
		t.Fatalf("rotation duplicated a resource: %s %s %v", before.ID, after.ID, err)
	}
}
