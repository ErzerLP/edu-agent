package mentorrun

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestPostgreSQLMentorStopIdempotency(t *testing.T) {
	for _, status := range []string{"queued", "running", "cancelling", "cancelled", "succeeded", "partial", "failed"} {
		t.Run(status, func(t *testing.T) {
			f := fixture(t, func(http.ResponseWriter, *http.Request, int) { t.Error("stop must not call the model") })
			r := f.accept(t)
			ctx := context.Background()
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 FOR UPDATE`, r.RunID))
			if err != nil {
				t.Fatal(err)
			}
			if err = f.service.decode(&item); err != nil {
				t.Fatal(err)
			}
			item.Status, item.Stage, item.Reason = status, status, "original_reason"
			item.body.Output = "已生成的内容"
			if err = f.service.save(ctx, tx, &item, "fixture"); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			f.service.cache(item)
			before := f.snapshot(t, r.RunID)
			c := Command{OperationID: uuid.NewString(), ExpectedVersion: r.Version, Kind: "stop"}
			accepted, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, c)
			if err != nil {
				t.Fatalf("stop with an observed older version: %v", err)
			}
			after := f.snapshot(t, r.RunID)
			wantStatus, wantReason, wantVersion := status, "original_reason", before.Version
			if status == "queued" || status == "running" {
				wantStatus, wantReason, wantVersion = "cancelled", "user_stopped", before.Version+1
				if status == "running" {
					wantStatus = "cancelling"
				}
			}
			if after.Status != wantStatus || after.Reason != wantReason || after.Version != wantVersion || after.Output != "已生成的内容" {
				t.Fatalf("unexpected stop result: %+v", after)
			}
			if accepted.Version != wantVersion {
				t.Fatalf("receipt version: %+v", accepted)
			}
			// An independent duplicate stop is a no-op, including at the latest version.
			duplicate := Command{OperationID: uuid.NewString(), ExpectedVersion: after.Version, Kind: "stop"}
			second, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, duplicate)
			if err != nil || second.Version != after.Version {
				t.Fatalf("duplicate stop: %+v %v", second, err)
			}
			if latest := f.snapshot(t, r.RunID); latest.Version != after.Version || latest.Watermark != after.Watermark || latest.Reason != after.Reason || latest.Output != after.Output {
				t.Fatalf("duplicate changed the run: %+v", latest)
			}
			if replay, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, c); err != nil || replay != accepted {
				t.Fatalf("original receipt replay: %+v %v", replay, err)
			}
			if stored, err := f.service.Operation(ctx, f.actor, learningspace.DefaultID, duplicate.OperationID); err != nil || stored != second {
				t.Fatalf("duplicate receipt lookup: %+v %v", stored, err)
			}
			c.ExpectedVersion++
			if _, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, c); !errors.Is(err, ErrOperation) {
				t.Fatalf("changed operation payload: %v", err)
			}
		})
	}
}

func TestPostgreSQLMentorStopVersionBoundaries(t *testing.T) {
	f := fixture(t, func(http.ResponseWriter, *http.Request, int) { t.Error("must not call model") })
	r := f.accept(t)
	owned, err := f.service.claim(context.Background())
	if err != nil || owned == nil {
		t.Fatalf("claim: %+v %v", owned, err)
	}
	for _, kind := range []string{"stop", "clear", "respond", "continue_budget", "retry_start"} {
		version := r.Version
		if kind == "stop" {
			version = owned.Version + 1
		}
		_, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: version, Kind: kind})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("%s version boundary: %v", kind, err)
		}
	}
}
