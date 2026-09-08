package postgresstore

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; PostgreSQL checks not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "space_" + uuid.New().String()[:8]
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if err = migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *space.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err=%v want %s", err, code)
	}
}
func TestPostgreSQLSpaceLifecycleRetriesConcurrencyAndRestart(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := New(pool)
	actor := uuid.NewString()
	var first space.Space
	var create space.Command
	for i, name := range []string{"Go 后端", "算法竞赛", "英语"} {
		c := space.Command{OperationID: uuid.NewString(), Name: name, Status: "active", Description: "long term"}
		item, err := s.Mutate(ctx, actor, "", c)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = item
			create = c
		}
	}
	replay, err := s.Mutate(ctx, actor, "", create)
	if err != nil || replay.ID != first.ID || replay.Version != 1 {
		t.Fatalf("retry=%+v err=%v", replay, err)
	}
	changed := create
	changed.Name = "other"
	_, err = s.Mutate(ctx, actor, "", changed)
	assertCode(t, err, "idempotency_conflict")
	// A fresh connection pool and store must observe persisted metadata.
	fresh, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	s = New(fresh)
	page, err := s.List(ctx, space.Query{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	next, err := s.List(ctx, space.Query{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(next.Items) != 2 || next.NextCursor != "" {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	search, err := s.List(ctx, space.Query{Limit: 10, Search: "后端"})
	if err != nil || len(search.Items) != 1 || search.Items[0].ID != first.ID {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	empty, err := s.List(ctx, space.Query{Limit: 10, Search: "absent"})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	_, err = s.List(ctx, space.Query{Limit: 101})
	assertCode(t, err, "invalid_learning_space")
	_, err = s.List(ctx, space.Query{Limit: 2, Cursor: page.NextCursor, Search: "changed"})
	assertCode(t, err, "invalid_learning_space")
	_, err = s.Get(ctx, uuid.NewString())
	assertCode(t, err, "learning_space_not_found")
	c := space.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "Go systems", Description: first.Description, Status: "archived"}
	archived, err := s.Mutate(ctx, actor, first.ID, c)
	if err != nil || archived.ID != first.ID || archived.Version != 2 {
		t.Fatalf("archive=%+v err=%v", archived, err)
	}
	c.OperationID = uuid.NewString()
	_, err = s.Mutate(ctx, actor, first.ID, c)
	assertCode(t, err, "version_conflict")
	c.ExpectedVersion = 2
	c.Status = "active"
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := c
			cmd.OperationID = uuid.NewString()
			_, err := s.Mutate(ctx, actor, first.ID, cmd)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			assertCode(t, err, "version_conflict")
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent winners=%d", wins)
	}
}
func TestPostgreSQLDefaultArchiveBlocksWritesAndPreservesHistory(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := New(pool)
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'test',now())`, actor); err != nil {
		t.Fatal(err)
	}
	insert := func() error {
		_, err := pool.Exec(ctx, `INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at) VALUES($1,$2,1,'history','test',$3,now())`, uuid.NewString(), uuid.NewString(), actor)
		return err
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	c := space.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "default", Status: "archived"}
	if _, err := s.Mutate(ctx, actor, space.DefaultID, c); err != nil {
		t.Fatal(err)
	}
	if err := insert(); err == nil {
		t.Fatal("archived default accepted a new goal")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_goal_revisions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("history=%d err=%v", count, err)
	}
	c.OperationID = uuid.NewString()
	c.ExpectedVersion = 2
	c.Status = "active"
	if _, err := s.Mutate(ctx, actor, space.DefaultID, c); err != nil {
		t.Fatal(err)
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
}
