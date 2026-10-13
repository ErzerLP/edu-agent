package postgresstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learningdirection"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/google/uuid"
)

// A failed direction write must roll back space metadata and its operation receipt.
func TestPostgreSQLDirectionFailureRollsBackSpaceAndReceipt(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	actor := uuid.NewString()
	direction := "理解向量空间"
	unavailable, err := learningdirection.New(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := New(pool, unavailable)
	command := space.Command{OperationID: uuid.NewString(), Name: "不能留下半个学习区", Status: "active", Direction: &direction}
	if _, err = store.Mutate(ctx, actor, "", command); !errors.Is(err, learningdirection.ErrUnavailable) {
		t.Fatalf("expected unavailable direction storage, got %v", err)
	}
	var spaces, receipts, directions int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_spaces WHERE name=$1), (SELECT count(*) FROM learning_space_operations WHERE device_id=$2), (SELECT count(*) FROM learning_space_directions)`, command.Name, actor).Scan(&spaces, &receipts, &directions); err != nil || spaces != 0 || receipts != 0 || directions != 0 {
		t.Fatalf("failed create survived: spaces=%d receipts=%d directions=%d err=%v", spaces, receipts, directions, err)
	}
	legacy, err := New(pool).Mutate(ctx, actor, "", space.Command{OperationID: uuid.NewString(), Name: "原名称", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	command.ExpectedVersion = legacy.Version
	if _, err = store.Mutate(ctx, actor, legacy.ID, command); !errors.Is(err, learningdirection.ErrUnavailable) {
		t.Fatalf("expected unavailable direction storage, got %v", err)
	}
	unchanged, err := New(pool).Get(ctx, legacy.ID)
	if err != nil || unchanged.Name != "原名称" || unchanged.Version != 1 {
		t.Fatalf("failed edit changed metadata: %+v %v", unchanged, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM learning_space_operations WHERE device_id=$1 AND operation_id=$2`, actor, command.OperationID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("failed edit wrote receipt", err)
	}
	// Retry the same operation after restoring storage; it must succeed once.
	available, err := learningdirection.New(pool, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store = New(pool, available)
	updated, err := store.Mutate(ctx, actor, legacy.ID, command)
	if err != nil || updated.Version != 2 || updated.DirectionVersion != 1 || updated.Direction != direction {
		t.Fatalf("retry after storage recovery: %+v %v", updated, err)
	}
	command.OperationID = uuid.NewString()
	command.ExpectedVersion = updated.Version
	command.Name = "只改名称"
	same := "  " + direction + "  "
	command.Direction = &same
	renamed, err := store.Mutate(ctx, actor, legacy.ID, command)
	if err != nil || renamed.DirectionVersion != 1 || renamed.Direction != direction {
		t.Fatalf("same normalized direction created a revision: %+v %v", renamed, err)
	}
}

// Dropping direction from a create/update must not silently succeed.
func TestPostgreSQLDirectionCreateAndLegacyCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	directions, err := learningdirection.New(pool, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := New(pool, directions)
	var cmd space.Command
	raw := `{"operation_id":"` + uuid.NewString() + `","expected_version":0,"name":"代数","description":"PRIVATE_DESCRIPTION","status":"active","direction":"  理解线性无关  "}`
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		t.Fatal(err)
	}
	item, err := store.Mutate(ctx, uuid.NewString(), "", cmd)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(item)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	if result["direction"] != "理解线性无关" || result["direction_version"] != float64(1) {
		t.Fatalf("confirmed direction not persisted: %s", encoded)
	}
	// Legacy updates rename metadata without inventing or changing intent.
	legacy := space.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "新名称", Description: "PRIVATE_DESCRIPTION", Status: "active"}
	renamed, err := store.Mutate(ctx, uuid.NewString(), item.ID, legacy)
	if err != nil || renamed.DirectionVersion != 1 || renamed.Direction != "理解线性无关" {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	direction := "理解矩阵的秩"
	change := legacy
	change.OperationID = uuid.NewString()
	change.ExpectedVersion = 2
	change.Direction = &direction
	actor := uuid.NewString()
	updated, err := store.Mutate(ctx, actor, item.ID, change)
	if err != nil || updated.DirectionVersion != 2 {
		t.Fatalf("update: %+v %v", updated, err)
	}
	replay, err := store.Mutate(ctx, actor, item.ID, change)
	if err != nil || replay.DirectionVersion != 2 {
		t.Fatal("replay", err)
	}
	change.OperationID = uuid.NewString()
	_, err = store.Mutate(ctx, actor, item.ID, change)
	assertCode(t, err, "version_conflict")
	var receipt string
	if err = pool.QueryRow(ctx, `SELECT result::text FROM learning_space_operations WHERE device_id=$1`, actor).Scan(&receipt); err != nil || strings.Contains(receipt, direction) {
		t.Fatal("direction leaked into operation receipt", err)
	}
	rewrap, _ := keyrotation.New(bytes.Repeat([]byte{42}, 32), bytes.Repeat([]byte{43}, 32))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = learningdirection.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rotated, _ := learningdirection.New(pool, bytes.Repeat([]byte{43}, 32))
	restored, err := New(pool, rotated).Get(ctx, item.ID)
	if err != nil || restored.Direction != direction {
		t.Fatalf("rotation: %+v %v", restored, err)
	}
}

func TestPostgreSQLDirectionConcurrentEnsureAndRevision(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	directions, _ := learningdirection.New(pool, bytes.Repeat([]byte{42}, 32))
	spaces := New(pool, directions)
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'direction test',now())`, actor); err != nil {
		t.Fatal(err)
	}
	direction := "理解线性无关"
	item, err := spaces.Mutate(ctx, actor, "", space.Command{OperationID: uuid.NewString(), Name: "代数", Direction: &direction, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	ensure := func(version int64) (learningdirection.Revision, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return learningdirection.Revision{}, err
		}
		defer tx.Rollback(ctx)
		var generation int64
		if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&generation); err != nil {
			return learningdirection.Revision{}, err
		}
		d, err := directions.EnsureTx(ctx, tx, actor, item.ID, generation, version)
		if err != nil {
			return d, err
		}
		return d, tx.Commit(ctx)
	}
	var wg sync.WaitGroup
	results := make(chan learningdirection.Revision, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); d, e := ensure(1); results <- d; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var original learningdirection.Revision
	for d := range results {
		if original.ID == "" {
			original = d
		}
		if d.GoalID != original.GoalID || d.GoalRevisionID != original.GoalRevisionID {
			t.Fatal("concurrent ensure duplicated goal")
		}
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM learning_goal_revisions WHERE space_id=$1`, item.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("goals=%d %v", count, err)
	}
	direction = "理解矩阵的秩"
	_, err = spaces.Mutate(ctx, actor, item.ID, space.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: item.Name, Direction: &direction, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ensure(1); err == nil {
		t.Fatal("stale direction accepted")
	}
	next, err := ensure(2)
	if err != nil {
		t.Fatal(err)
	}
	if next.GoalID != original.GoalID || next.GoalRevisionID == original.GoalRevisionID || next.GoalVersion != 2 {
		t.Fatalf("lineage lost: %+v", next)
	}
	var previous, text string
	if err = pool.QueryRow(ctx, `SELECT previous_revision_id::text,goal_text FROM learning_goal_revisions WHERE id=$1`, next.GoalRevisionID).Scan(&previous, &text); err != nil || previous != original.GoalRevisionID || text != direction {
		t.Fatal("goal revision", err)
	}
}

func TestPostgreSQLDirectionLegacyReceiptDoesNotAcquireNewIntent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	directions, _ := learningdirection.New(pool, bytes.Repeat([]byte{42}, 32))
	store := New(pool, directions)
	actor := uuid.NewString()
	legacy := space.Command{OperationID: uuid.NewString(), Name: "old", Description: "private", Status: "active"}
	item, err := store.Mutate(ctx, actor, "", legacy)
	if err != nil {
		t.Fatal(err)
	}
	direction := "confirmed later"
	if _, err = store.Mutate(ctx, actor, item.ID, space.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: item.Name, Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Mutate(ctx, actor, "", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Direction != "" || replay.DirectionVersion != 0 || replay.Version != 1 {
		t.Fatal("old receipt silently acquired later direction")
	}
}
