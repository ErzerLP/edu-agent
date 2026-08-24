//go:build cli_m1_contract

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/jackc/pgx/v5/pgxpool"
)

type projectionRefresher interface {
	RefreshActiveProjectionForContract(context.Context) error
}

func main() {
	dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "projection rebuild fixture requires TEST_DATABASE_URL")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "projection rebuild fixture failed: database connection")
		os.Exit(1)
	}
	defer pool.Close()
	store := learningpostgres.New(pool, tutoringpostgres.New(pool))
	refresher, ok := any(store).(projectionRefresher)
	if !ok {
		fmt.Fprintln(os.Stderr, "code=internal_error reason=contract_refresh_unavailable")
		os.Exit(1)
	}
	if err := refresher.RefreshActiveProjectionForContract(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "code=projection_unavailable reason=contract_refresh_failed")
		os.Exit(1)
	}
}
