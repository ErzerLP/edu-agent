package migrations

import (
	"context"
	"testing"
)

func TestStudyDirectionMigrationUpgrade(t *testing.T) {
	pool := migrationPoolThrough(t, 37)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE learning_spaces SET name='旧学习区',description='不可推断的私人描述' WHERE id='00000000-0000-4000-8000-000000000001'`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_space_directions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("migration inferred private intent", err)
	}
	var name, description string
	if err := pool.QueryRow(ctx, `SELECT name,description FROM learning_spaces WHERE id='00000000-0000-4000-8000-000000000001'`).Scan(&name, &description); err != nil || name != "旧学习区" || description != "不可推断的私人描述" {
		t.Fatal("legacy space changed", err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal("migration replay", err)
	}
}
