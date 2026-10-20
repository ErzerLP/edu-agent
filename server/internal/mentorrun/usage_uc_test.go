package mentorrun

import (
	"context"
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

func TestPostgreSQLUsageHasNoCumulativeLimits(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 25 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"goal-%d\",\"type\":\"function\",\"function\":{\"name\":\"read_goal\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", n)
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"已完成\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4000,\"completion_tokens\":20,\"total_tokens\":4020}}\n\ndata: [DONE]\n\n")
	})
	r := f.accept(t)
	f.work(t)
	s := f.snapshot(t, r.RunID)
	if s.Status != "succeeded" || s.RequestsUsed != 26 || f.calls.Load() != 26 {
		t.Fatalf("cumulative requests blocked execution: %+v calls=%d", s, f.calls.Load())
	}
	var total int64
	if err := f.pool.QueryRow(context.Background(), `SELECT sum(c.total_tokens) FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.origin_id=$1`, r.RunID).Scan(&total); err != nil || total != 104520 {
		t.Fatalf("cumulative token usage missing: %d %v", total, err)
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

func TestPostgreSQLUsageReceiptUsesCurrentPayload(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "完成"})
	})
	r := f.accept(t)
	ctx := context.Background()
	if replay, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); err != nil || replay != r {
		t.Fatalf("receipt replay: %+v %v", replay, err)
	}
	changed := f.create
	changed.Prompt = "不同的问题"
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, changed); !errors.Is(err, ErrOperation) {
		t.Fatalf("changed payload accepted: %v", err)
	}
	var obsoleteColumn bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='learning_mentor_operations' AND column_name='hash_version')`).Scan(&obsoleteColumn); err != nil || obsoleteColumn {
		t.Fatalf("budget-only hash version remains: %v %v", obsoleteColumn, err)
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
