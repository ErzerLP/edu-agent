// webdatabase 为单个浏览器候选文件提供独立的临时 PostgreSQL schema。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func main() {
	if len(os.Args) < 2 || os.Getenv("TEST_DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "需要 TEST_DATABASE_URL 和待执行的浏览器命令")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := withDatabase(ctx, os.Getenv("TEST_DATABASE_URL"), func(databaseURL string) error {
		cmd := exec.CommandContext(ctx, os.Args[1], os.Args[2:]...)
		cmd.Env = append(os.Environ(), "TEST_DATABASE_URL="+databaseURL)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		// 让 Playwright 有机会先关闭服务，再清理数据库夹具。
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 10 * time.Second
		return cmd.Run()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func withDatabase(ctx context.Context, databaseURL string, run func(string) error) (err error) {
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("连接浏览器测试数据库：%w", err)
	}
	defer connection.Close(context.Background())
	schema := "web_candidate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err = connection.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		return fmt.Errorf("创建浏览器测试 schema：%w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// 仅删除本次成功创建的随机 schema，不重置调用方的原始 schema。
		if _, cleanupErr := connection.Exec(cleanup, "DROP SCHEMA "+identifier+" CASCADE"); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("清理浏览器测试 schema %s：%w", schema, cleanupErr))
		}
	}()
	return run(scopedURL(databaseURL, schema))
}

func scopedURL(databaseURL, schema string) string {
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		// 连接已由 pgx 验证；保留密码、SSL 等原参数，只覆盖 search_path。
		parsed, _ := url.Parse(databaseURL)
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	// pgx 同样支持 libpq 关键字格式；schema 仅含程序生成的字母、数字和下划线。
	return databaseURL + " search_path=" + schema
}
