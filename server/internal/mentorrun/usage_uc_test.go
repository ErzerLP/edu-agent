package mentorrun

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

func legacyPause(t *testing.T, f *runtimeFixture, id string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE learning_mentor_runs SET state=state || '{"status":"paused_budget","stage":"paused_budget","reason":"budget_exhausted"}'::jsonb WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestPostgreSQLUsageLegacyBudgetDoesNotLimit(t *testing.T) {
	for _, budget := range []int{0, 1} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if n <= 3 {
					stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: fmt.Sprint("goal-", n), Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}}}})
					return
				}
				stream(w, modelclient.Message{Role: "assistant", Content: "已完成"})
			})
			f.create.RequestBudget, f.create.TokenBudget = budget, budget
			r := f.accept(t)
			f.work(t)
			s := f.snapshot(t, r.RunID)
			if s.Status != "succeeded" || s.RequestsLeft != 0 || s.TokensLeft != 0 || f.calls.Load() != 4 {
				t.Fatalf("legacy allowance blocked execution: %+v calls=%d", s, f.calls.Load())
			}
		})
	}
}

func TestPostgreSQLUsageToolStepsResume(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if n%2 == 0 {
			if len(request.Tools) != 0 {
				t.Error("summary still exposes tools")
			}
			// Complete uses a non-streaming response for the progress summary.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"已读取目标，还需整理例子。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
			return
		}
		stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: fmt.Sprint("read-", n), Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}}}})
	})
	var update settings.Update
	raw, _ := json.Marshal(map[string]any{"expected_revision": f.settings.View().Revision, "limits": map[string]any{"max_tool_steps": 1}})
	if err := json.Unmarshal(raw, &update); err != nil {
		t.Fatal(err)
	}
	if _, err := f.settings.Update(update); err != nil {
		t.Fatal(err)
	}
	r := f.accept(t)
	f.work(t)
	for round := 0; round < 2; round++ {
		s := f.snapshot(t, r.RunID)
		if s.Status != "paused_tool_steps" || s.Output == "" || f.calls.Load() != int32((round+1)*2) {
			t.Fatalf("no resumable summary: %+v calls=%d", s, f.calls.Load())
		}
		if round == 0 {
			_, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "resume"})
			if err != nil {
				t.Fatal(err)
			}
			f.work(t)
		}
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_model_calls`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("summary not accounted: %d %v", count, err)
	}
}

func TestPostgreSQLUsageLegacyReceiptAndPause(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "继续完成"})
	})
	r := f.accept(t)
	ctx := context.Background()
	var version int
	if err := f.pool.QueryRow(ctx, `SELECT hash_version FROM learning_mentor_operations WHERE operation_id=$1`, r.OperationID).Scan(&version); err != nil || version != 2 {
		t.Fatalf("new receipt version: %d %v", version, err)
	}
	changed := f.create
	changed.TokenBudget++
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, changed); !errors.Is(err, ErrOperation) {
		t.Fatalf("v2 lost submitted budgets: %v", err)
	}
	// Reproduce an operation and paused checkpoint from the previous release.
	legacyJSON := fmt.Sprintf(`{"Kind":"create","Space":%q,"Target":%q,"Value":{"operation_id":%q,"session_id":%q,"expected_version":1,"prompt":"请解释目标","save":true,"request_budget":4,"token_budget":50000}}`, learningspace.DefaultID, f.goal, f.create.OperationID, f.create.SessionID)
	hash := sha256.Sum256([]byte(legacyJSON))
	legacyConfiguration := fmt.Sprintf(`{"Connection":{"enabled":true,"provider":"openai_compatible","endpoint":%q,"model":"mentor-fixture","auth_mode":"none"},"Key":"","Limits":{"research_requests":20,"research_tokens":100000,"output_tokens":64,"context_tokens":32768,"idle_timeout_seconds":60,"concurrency":2,"storage_mib":512}}`, f.settings.View().EffectiveMentor.Endpoint)
	configuration := fmt.Sprintf("%x", sha256.Sum256([]byte(legacyConfiguration)))
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_operations SET request_hash=$1,hash_version=1 WHERE operation_id=$2`, hash[:], r.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET state=state || '{"status":"paused_budget","stage":"paused_budget","requests_left":7,"tokens_left":9000}'::jsonb || jsonb_build_object('configuration',$2::text) WHERE id=$1`, r.RunID, configuration); err != nil {
		t.Fatal(err)
	}
	if replay, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); err != nil || replay != r {
		t.Fatalf("legacy receipt: %+v %v", replay, err)
	}
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, changed); !errors.Is(err, ErrOperation) {
		t.Fatalf("payload change accepted: %v", err)
	}
	f.work(t)
	s := f.snapshot(t, r.RunID)
	if s.Status != "paused_budget" || f.calls.Load() != 0 || s.RequestsLeft != 0 || s.TokensLeft != 0 {
		t.Fatalf("legacy pause changed: %+v", s)
	}
	if _, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "continue_budget", RequestBudget: 1, TokenBudget: 1}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if s = f.snapshot(t, r.RunID); s.Status != "succeeded" {
		t.Fatal(s)
	}
}

func TestPostgreSQLUsageToolLimitPreservesPendingCalls(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{
				{ID: "first", Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}},
				{ID: "second", Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}},
			}})
			return
		}
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Tools) != 0 {
			t.Error("summary exposed tools")
		}
		for _, message := range request.Messages {
			for _, call := range message.ToolCalls {
				if n == 2 && call.ID == "second" {
					t.Error("unexecuted tool sent to summary")
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"已读取目标，还需整理。"},"finish_reason":"stop"}]}`)
	})
	l := f.settings.View().Limits
	steps := 1
	l.MaxToolSteps = &steps
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &l}); err != nil {
		t.Fatal(err)
	}
	r := f.accept(t)
	for round := 0; round < 2; round++ {
		f.work(t)
		s := f.snapshot(t, r.RunID)
		var current row
		if err := f.service.readOwned(context.Background(), row{Meta: s.Meta, device: f.actor.Device.ID, token: f.actor.TokenID}, &current); err != nil {
			t.Fatal(err)
		}
		if s.Status != "paused_tool_steps" || len(current.body.Pending) != 1-round || current.body.ToolSteps != 1 || f.calls.Load() != int32(round+2) {
			t.Fatalf("pending calls lost or repeated: status=%s pending=%d steps=%d calls=%d", s.Status, len(current.body.Pending), current.body.ToolSteps, f.calls.Load())
		}
		if round == 0 {
			if _, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "resume"}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPostgreSQLUsageResearchToolSteps(t *testing.T) {
	f, searches := researchFixture(t, false)
	limits := f.settings.View().Limits
	steps := 1
	limits.MaxToolSteps = &steps
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	f.modelReply = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Tools) != 0 {
			t.Error("research summary exposes tools")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"已完成部分资料查找，继续后读取剩余来源并整理。"},"finish_reason":"stop"}]}`)
		return true
	}
	r := f.accept(t)
	for round := 0; round < 2; round++ {
		f.work(t)
		s := f.snapshot(t, r.RunID)
		if s.Status != "paused_tool_steps" || searches.Load() != 1 || f.calls.Load() != int32(round+1) {
			t.Fatalf("research progress lost: %+v searches=%d", s, searches.Load())
		}
		if round == 0 && (!s.Research.Discovered || s.Research.Sources[0].Status != "candidate") {
			t.Fatal("first pause did not preserve search")
		}
		if round == 1 && s.Research.Sources[0].Status == "candidate" {
			t.Fatal("continue did not fetch next source")
		}
		kind := "resume"
		if round == 1 {
			kind = "stop"
		}
		if _, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	f.work(t)
	if f.calls.Load() != 2 {
		t.Fatal("stopped research dispatched another request")
	}
}

func TestPostgreSQLUsageOldCLIRequestAndInputTypes(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "完成"})
	})
	// workbench_study sends this exact legacy envelope to the mentor endpoint.
	raw := fmt.Sprintf(`{"operation_id":%q,"session_id":%q,"expected_version":1,"prompt":"讲解目标","save":true,"request_budget":1,"token_budget":1}`, uuid.NewString(), uuid.NewString())
	var c Create
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	r, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, f.goal, c)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if s := f.snapshot(t, r.RunID); s.Status != "succeeded" {
		t.Fatal(s)
	}
	for _, value := range []string{"-1", "1.5", `"1"`, "null", "9223372036854775808"} {
		for _, name := range []string{"request_budget", "token_budget"} {
			var c Create
			if json.Unmarshal([]byte(fmt.Sprintf(`{%q:%s}`, name, value)), &c) == nil {
				t.Errorf("accepted %s=%s", name, value)
			}
		}
	}
}

func TestPostgreSQLUsageToolPauseKeepsInteraction(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "ask", Type: "function", Function: modelclient.ToolFunction{Name: "ask_user", Arguments: `{"question":"需要例子吗？","choices":["需要"]}`}}}})
			return
		}
		if n == 2 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"还需要你确认是否需要例子。"},"finish_reason":"stop"}]}`)
			return
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "已给出例子"})
	})
	l := f.settings.View().Limits
	steps := 1
	l.MaxToolSteps = &steps
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &l}); err != nil {
		t.Fatal(err)
	}
	r := f.accept(t)
	f.work(t)
	s := f.snapshot(t, r.RunID)
	if s.Status != "paused_tool_steps" || s.Interaction == nil {
		t.Fatal(s)
	}
	ctx := context.Background()
	if _, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "resume"}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	s = f.snapshot(t, r.RunID)
	if s.Status != "waiting_input" || f.calls.Load() != 2 {
		t.Fatal("resume bypassed the user's answer", s)
	}
	if _, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "respond", InteractionID: s.Interaction.ID, Answer: "需要"}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if s = f.snapshot(t, r.RunID); s.Status != "succeeded" || f.calls.Load() != 3 {
		t.Fatal(s)
	}
}
