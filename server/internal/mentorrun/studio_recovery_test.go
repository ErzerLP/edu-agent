package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/google/uuid"
)

func studioReply(w http.ResponseWriter, content, finish string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]any{"content": content}}}})
}
func TestPostgreSQLStudioGenerationStates(t *testing.T) {
	for _, tc := range []struct {
		name, content, finish, status, reason string
		budget                                int
		published                             int
	}{
		{"partial", studioCandidate("guide", "partial"), "stop", "partial", "source_coverage_partial", 50000, 1},
		{"truncated", studioCandidate("guide", "complete"), "length", "failed", "output_budget_exceeded", 50000, 0},
		{"incomplete", studioCandidate("guide", "complete"), "", "failed", "response_not_completed", 50000, 0},
		{"schema", `{"title":"bad"}`, "stop", "failed", "schema_mismatch", 50000, 0},
		{"citation", strings.ReplaceAll(studioCandidate("guide", "complete"), `"source":1`, `"source":2`), "stop", "failed", "invalid_source_citation", 50000, 0},
		{"budget", studioCandidate("guide", "complete"), "stop", "paused_budget", "budget_exhausted", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := studioFixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) { studioReply(w, tc.content, tc.finish) })
			c.TokenBudget = tc.budget
			receipt, err := f.service.CreateStudio(t.Context(), f.actor, learningspace.DefaultID, c)
			if err != nil {
				t.Fatal(err)
			}
			f.work(t)
			state := f.snapshot(t, receipt.RunID)
			if state.Status != tc.status || state.Reason != tc.reason || state.ResultUnknown {
				t.Fatalf("unexpected state: %+v", state)
			}
			var count int
			if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM learning_studio_revisions`).Scan(&count); err != nil || count != tc.published {
				t.Fatal("publication mismatch", count, err)
			}
			if tc.name == "budget" {
				if f.calls.Load() != 0 {
					t.Fatal("budget bypass")
				}
				_, err = f.service.Command(t.Context(), f.actor, learningspace.DefaultID, receipt.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: state.Version, Kind: "continue_budget", RequestBudget: 1, TokenBudget: 50000})
				if err != nil {
					t.Fatal(err)
				}
				f.work(t)
				if f.snapshot(t, receipt.RunID).Status != "succeeded" {
					t.Fatal("budget continuation failed")
				}
			}
		})
	}
}

func TestPostgreSQLStudioGenerationRestartAndUnknown(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "unknown"}[inFlight], func(t *testing.T) {
			f, c := studioFixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				studioReply(w, studioCandidate("guide", "complete"), "stop")
			})
			receipt, err := f.service.CreateStudio(t.Context(), f.actor, learningspace.DefaultID, c)
			if err != nil {
				t.Fatal(err)
			}
			if inFlight {
				owned, err := f.service.claim(t.Context())
				if err != nil || owned == nil {
					t.Fatal(err)
				}
				if err = f.service.mutate(t.Context(), owned, "model_started", func(item *row) error { item.callStarted = true; return nil }); err != nil {
					t.Fatal(err)
				}
				if _, err = f.pool.Exec(t.Context(), `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, receipt.RunID); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
			if err != nil {
				t.Fatal(err)
			}
			restarted.ConfigureStudio(f.service.studio)
			f.service = restarted
			f.work(t)
			state := f.snapshot(t, receipt.RunID)
			if inFlight {
				if state.Status != "failed" || !state.ResultUnknown || f.calls.Load() != 0 || state.Studio.Result != nil {
					t.Fatalf("unknown request replayed: %+v", state)
				}
			} else if state.Status != "succeeded" || state.Studio.Result == nil {
				t.Fatalf("queued run not recovered: %+v", state)
			}
		})
	}
}

func TestPostgreSQLStudioGenerationCancelAndAtomicPublication(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		f, c := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			studioReply(w, studioCandidate("guide", "complete"), "stop")
		})
		f.service.Lease = time.Second
		receipt, err := f.service.CreateStudio(t.Context(), f.actor, learningspace.DefaultID, c)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := f.service.RunOnce(t.Context()); done <- err }()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("model not started")
		}
		state := f.snapshot(t, receipt.RunID)
		if _, err = f.service.Command(t.Context(), f.actor, learningspace.DefaultID, receipt.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: state.Version, Kind: "stop"}); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cancel did not stop worker")
		}
		state = f.snapshot(t, receipt.RunID)
		if state.Status != "cancelled" || state.Studio.Result != nil {
			t.Fatalf("cancel published: %+v", state)
		}
		var count int
		_ = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM learning_studio_revisions`).Scan(&count)
		if count != 0 {
			t.Fatal("cancel left revision")
		}
	})
	t.Run("rollback", func(t *testing.T) {
		f, c := studioFixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			studioReply(w, studioCandidate("guide", "complete"), "stop")
		})
		_, err := f.pool.Exec(t.Context(), `CREATE FUNCTION reject_studio_success() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state->>'status'='succeeded' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_studio_success BEFORE UPDATE ON learning_mentor_runs FOR EACH ROW EXECUTE FUNCTION reject_studio_success()`)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := f.service.CreateStudio(t.Context(), f.actor, learningspace.DefaultID, c)
		if err != nil {
			t.Fatal(err)
		}
		f.work(t)
		state := f.snapshot(t, receipt.RunID)
		var count int
		if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM learning_studio_revisions`).Scan(&count); err != nil || count != 0 || state.Status != "failed" || state.Studio.Result != nil {
			t.Fatal("artifact survived failed terminal commit", count, state.Status, err)
		}
	})
}

func TestPostgreSQLStudioGenerationVersionsRotationAndRevocation(t *testing.T) {
	f, c := studioFixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		studioReply(w, studioCandidate("guide", "complete"), "stop")
	})
	ctx := context.Background()
	receipt, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	first := f.snapshot(t, receipt.RunID).Studio.Result
	c.ArtifactID = &first.ArtifactID
	c.ExpectedVersion = 1
	c.OperationID = uuid.NewString()
	next, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if second := f.snapshot(t, next.RunID).Studio.Result; second.Version != 2 || second.ArtifactID != first.ArtifactID {
		t.Fatal("regeneration overwrote identity")
	}
	key := bytes.Repeat([]byte{43}, 32)
	rewrap, err := keyrotation.New(bytes.Repeat([]byte{42}, 32), key)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_mentor_runs,learning_tutor_conversations,learning_tutor_turns,learning_content_revisions IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = learningcontent.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	content, err := learningcontent.New(f.pool, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	content.ConfigureStudioSources(knowledgepg.New(f.pool))
	for _, version := range []int64{1, 2} {
		if r, err := content.StudioGet(ctx, f.actor, first.ArtifactID, version); err != nil || r.Version != version {
			t.Fatal("rotation lost history", err)
		}
	}
	restarted, err := New(f.pool, f.settings, key)
	if err != nil {
		t.Fatal(err)
	}
	restarted.ConfigureStudio(content)
	f.service = restarted
	c.OperationID = uuid.NewString()
	c.ExpectedVersion = 2
	revokedRun, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `DELETE FROM knowledge_collection_links WHERE space_id=$1`, learningspace.DefaultID); err != nil {
		t.Fatal(err)
	}
	if _, err = content.StudioGet(ctx, f.actor, first.ArtifactID, 1); err == nil {
		t.Fatal("revocation leaked historic body")
	}
	f.work(t)
	state := f.snapshot(t, revokedRun.RunID)
	var versions int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_studio_revisions`).Scan(&versions); err != nil || versions != 2 || state.Status != "failed" || state.BodyAvailable || f.calls.Load() != 2 {
		t.Fatal("revoked input reached model or published", versions, state.Status, err)
	}
}
