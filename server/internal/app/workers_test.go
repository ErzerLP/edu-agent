package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPeriodicWorkerBacksOffAndResetsAfterSuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		retry    time.Duration
		want     []time.Duration
	}{
		{"cleanup", time.Second, 0, []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 31 * time.Second, 63 * time.Second, 123 * time.Second, 183 * time.Second, 184 * time.Second, 185 * time.Second}},
		{"custom retry", 30 * time.Second, 2 * time.Second, []time.Duration{0, 2 * time.Second, 6 * time.Second, 14 * time.Second, 30 * time.Second, 62 * time.Second, 122 * time.Second, 182 * time.Second, 242 * time.Second, 272 * time.Second, 274 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				start := time.Now()
				var attempts []time.Duration
				spec := periodicWorker("cleanup", tc.interval, 1, func(context.Context) (int, error) {
					attempts = append(attempts, time.Since(start))
					switch len(attempts) {
					case 9:
						return 0, nil
					case 11:
						cancel()
						return 0, ctx.Err()
					default:
						return 0, errors.New("private failure")
					}
				})
				spec.retryInterval = tc.retry
				runPeriodicWorker(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)), spec)
				if len(attempts) != len(tc.want) {
					t.Fatalf("attempts=%v want=%v", attempts, tc.want)
				}
				for i, want := range tc.want {
					if attempts[i] != want {
						t.Fatalf("attempt %d started at %s, want %s (all: %v)", i+1, attempts[i], want, attempts)
					}
				}
			})
		})
	}
}

func TestPeriodicWorkerSummarizesRepeatedFailuresAndRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var logs bytes.Buffer
		attempts := 0
		spec := periodicWorker("cleanup", time.Second, 1, func(context.Context) (int, error) {
			attempts++
			switch {
			case attempts <= 2:
				return 0, errors.New("sensitive query details")
			case attempts <= 7:
				return 0, privacy.ErrGenerationKeyUnavailable
			case attempts == 8:
				return 0, nil
			default:
				cancel()
				return 0, ctx.Err()
			}
		})
		runPeriodicWorker(ctx, slog.New(slog.NewJSONHandler(&logs, nil)), spec)
		if strings.Contains(logs.String(), "sensitive query details") {
			t.Fatal("raw error leaked into worker diagnostics")
		}
		type logEntry struct {
			Message    string `json:"msg"`
			Category   string `json:"error_category"`
			Failures   int    `json:"consecutive_failures"`
			Suppressed int    `json:"suppressed_failures"`
		}
		var entries []logEntry
		decoder := json.NewDecoder(&logs)
		for decoder.More() {
			var entry logEntry
			if err := decoder.Decode(&entry); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, entry)
		}
		if len(entries) != 4 {
			t.Fatalf("failure logs were not coalesced: %+v", entries)
		}
		for i, want := range []struct {
			category   string
			failures   int
			suppressed int
		}{{"unavailable", 1, 0}, {"deferred", 3, 1}, {"deferred", 7, 3}, {"", 7, 0}} {
			if got := entries[i]; got.Category != want.category || got.Failures != want.failures || got.Suppressed != want.suppressed {
				t.Fatalf("entry %d=%+v want=%+v", i, got, want)
			}
		}
		if entries[3].Message != "background worker recovered" {
			t.Fatalf("recovery was not reported: %+v", entries[3])
		}
	})
}

func TestPeriodicWorkerCancellationInterruptsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		attempts := 0
		done := make(chan struct{})
		go func() {
			defer close(done)
			runPeriodicWorker(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)), periodicWorker("cleanup", time.Second, 1, func(context.Context) (int, error) {
				attempts++
				return 0, errors.New("unavailable")
			}))
		}()
		time.Sleep(64 * time.Second)
		synctest.Wait()
		if attempts != 7 {
			t.Fatalf("worker did not reach the capped backoff: attempts=%d", attempts)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("worker did not stop during backoff")
		}
	})
}

func TestWorkerErrorCategoryPreservesStorageAndDatabaseCauses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{mentorrun.ErrStorage, "run_storage_unavailable"},
		{mentorrun.ErrHistorySchema, "tutor_history_schema_unsupported"},
		{&pgconn.PgError{Code: "40P01", Message: "private SQL details"}, "postgres_40P01"},
	} {
		if got := workerErrorCategory(fmt.Errorf("cleanup: %w", tc.err)); got != tc.want {
			t.Fatalf("category=%q want=%q", got, tc.want)
		}
	}
}
