package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestScopedURL(t *testing.T) {
	for _, raw := range []string{
		"postgres://user:p%40ss@localhost/example?sslmode=disable&search_path=previous",
		"postgresql://user:p%40ss@localhost/example?sslmode=disable",
		"host=localhost user=user password='p@ss' dbname=example sslmode=disable search_path=previous",
	} {
		config, err := pgx.ParseConfig(scopedURL(raw, "web_candidate_example"))
		if err != nil {
			t.Fatal(err)
		}
		if config.RuntimeParams["search_path"] != "web_candidate_example" || config.Database != "example" || config.User != "user" || config.Password != "p@ss" || config.TLSConfig != nil {
			t.Fatal("隔离连接没有保留原连接参数或覆盖旧 schema")
		}
	}
}

func TestPostgreSQLBrowserSchemaIsolationAndCleanup(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 TEST_DATABASE_URL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	injected := errors.New("模拟浏览器用例失败")
	// 外层模拟已有候选状态，内层模拟后续浏览器；两者使用同一数据库。
	err = withDatabase(ctx, databaseURL, func(priorURL string) error {
		prior, err := pgx.Connect(ctx, priorURL)
		if err != nil {
			return err
		}
		defer prior.Close(ctx)
		if _, err = prior.Exec(ctx, "CREATE TABLE candidate_state (value TEXT); INSERT INTO candidate_state VALUES ('原有状态')"); err != nil {
			return err
		}
		for _, outcome := range []error{nil, injected} {
			var schema string
			runErr := withDatabase(ctx, priorURL, func(nextURL string) error {
				next, err := pgx.Connect(ctx, nextURL)
				if err != nil {
					return err
				}
				defer next.Close(ctx)
				var empty bool
				if err = next.QueryRow(ctx, "SELECT current_schema(), to_regclass('candidate_state') IS NULL").Scan(&schema, &empty); err != nil {
					return err
				}
				if !empty {
					t.Error("新候选读到了前一候选的持久状态")
				}
				if _, err = next.Exec(ctx, "CREATE TABLE candidate_state (value TEXT)"); err != nil {
					return err
				}
				return outcome
			})
			if !errors.Is(runErr, outcome) {
				t.Fatalf("浏览器执行结果被吞掉或改写：%v", runErr)
			}
			var exists bool
			if err = admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&exists); err != nil {
				return err
			}
			if exists {
				t.Error("浏览器结束后仍有临时 schema")
			}
			var value string
			if err = prior.QueryRow(ctx, "SELECT value FROM candidate_state").Scan(&value); err != nil {
				return err
			}
			if value != "原有状态" {
				t.Error("隔离或清理修改了调用方原有数据")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
