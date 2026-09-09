package migrations

import (
	"context"
	"testing"

	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestLearningSpaceUpgradePreservesLegacyGoal(t *testing.T) {
	pool := migrationPoolThrough(t, 11)
	ctx := context.Background()
	actor, goal, revision := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'legacy',now())`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at) VALUES($1,$2,1,'legacy text','legacy',$3,now())`, revision, goal, actor); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(g)::text FROM learning_goal_revisions g WHERE id=$1`, revision).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT row_to_json(g)::text FROM learning_goal_revisions g WHERE id=$1`, revision).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("upgrade rewrote a legacy goal or reference")
	}
	var id string
	var count int
	if err := pool.QueryRow(ctx, `SELECT min(id::text),count(*) FROM learning_spaces`).Scan(&id, &count); err != nil || id != space.DefaultID || count != 1 {
		t.Fatalf("default=%s count=%d err=%v", id, count, err)
	}
}
