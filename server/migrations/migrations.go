package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var migrationFiles embed.FS

var filenamePattern = regexp.MustCompile(`^(\d{6})_[a-z0-9_]+\.sql$`)

const advisoryLockID int64 = 0x4544554147454e54

type migration struct {
	version  int64
	name     string
	checksum string
	body     []byte
}

func Run(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := load()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockID) }()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create migration metadata: %w", err)
	}
	for _, item := range migrations {
		var checksum string
		err := conn.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, item.version).Scan(&checksum)
		switch {
		case err == nil:
			if checksum != item.checksum {
				return fmt.Errorf("migration %s was rewritten after being applied", item.name)
			}
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read migration %s state: %w", item.name, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", item.name, err)
		}
		if _, err = tx.Exec(ctx, string(item.body)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES($1,$2,$3)`, item.version, item.name, item.checksum)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", item.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", item.name, err)
		}
	}
	return nil
}

func Check(ctx context.Context, pool *pgxpool.Pool) error {
	items, err := load()
	if err != nil {
		return err
	}
	for _, item := range items {
		var checksum string
		if err := pool.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, item.version).Scan(&checksum); err != nil {
			return fmt.Errorf("migration %s is not applied: %w", item.name, err)
		}
		if checksum != item.checksum {
			return fmt.Errorf("migration %s checksum mismatch", item.name)
		}
	}
	return nil
}

func load() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	items := make([]migration, 0, len(entries))
	seen := make(map[int64]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !filenamePattern.MatchString(entry.Name()) {
			continue
		}
		match := filenamePattern.FindStringSubmatch(entry.Name())
		version, _ := strconv.ParseInt(match[1], 10, 64)
		if _, exists := seen[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %06d", version)
		}
		seen[version] = struct{}{}
		body, err := migrationFiles.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		items = append(items, migration{version: version, name: entry.Name(), checksum: hex.EncodeToString(sum[:]), body: body})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].version < items[j].version })
	if len(items) == 0 {
		return nil, errors.New("no embedded migrations found")
	}
	return items, nil
}
