package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningdirection"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/google/uuid"
)

func TestPostgreSQLStudyModeIsExplicitAndSaved(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		t.Error("opening a conversation called the model")
	})
	var request NewConversation
	if err := json.Unmarshal([]byte(`{"id":"`+uuid.NewString()+`","mode":"study"}`), &request); err != nil {
		t.Fatal(err)
	}
	id, err := f.service.NewConversation(context.Background(), f.actor, learningspace.DefaultID, request)
	if err != nil {
		t.Fatal(err)
	}
	page := historyPage(t, f, id, 0, 10)
	raw, _ := json.Marshal(page.Conversation)
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	if c["mode"] != "study" || !page.Conversation.Saved || page.Conversation.GoalID != "" {
		t.Fatalf("study semantics absent: %s", raw)
	}
	for _, request := range []NewConversation{{ID: uuid.NewString(), Mode: "other"}, {ID: uuid.NewString(), Mode: "study", GoalID: f.goal}} {
		if _, err = f.service.NewConversation(context.Background(), f.actor, learningspace.DefaultID, request); err == nil {
			t.Fatal("invalid study mode/context accepted")
		}
	}
	if _, err = f.service.Submit(context.Background(), f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, Prompt: "must not guess direction", RequestBudget: 1, TokenBudget: 20000}); err != ErrInvalid {
		t.Fatal("missing direction/source accepted", err)
	}
	var count int
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_direction_goals`).Scan(&count); err != nil || count != 0 {
		t.Fatal("opening conversation created managed goal", err)
	}
}

func TestPostgreSQLStudySourceAndDirectionIsolationReopen(t *testing.T) {
	inputs := []string{}
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		raw, _ := io.ReadAll(r.Body)
		inputs = append(inputs, string(raw))
		// The real structured adapter is exercised; only the remote model is replaced.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": fmt.Sprintf(`{"answer":"讲解 %d [1]","citations":[{"source":1,"fragment":1}],"exercise":null}`, n)}}}})
	})
	ctx := context.Background()
	spaces := spacepg.New(f.pool, f.service.directions)
	direction := "DIRECTION_ALPHA"
	_, err := spaces.Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "代数", Description: "PRIVATE_DESCRIPTION", Status: "active", Direction: &direction})
	if err != nil {
		t.Fatal(err)
	}
	a, b := selectedDocument(t, f, "SOURCE_ALPHA"), selectedDocument(t, f, "SOURCE_BETA")
	id, err := f.service.NewConversation(ctx, f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), Mode: "study"})
	if err != nil {
		t.Fatal(err)
	}
	send := func(source knowledge.SourceIdentity, version int64, prompt string) Receipt {
		c := historyPage(t, f, id, 0, 10).Conversation
		req := SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: c.Version, DirectionVersion: version, SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}}, Prompt: prompt, RequestBudget: 2, TokenBudget: 50000}
		receipt, e := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, req)
		if e != nil {
			t.Fatal(e)
		}
		f.work(t)
		snapshot := f.snapshot(t, receipt.RunID)
		if snapshot.Status != "succeeded" || len(snapshot.Citations) != 1 {
			t.Fatalf("study failed: %+v", snapshot)
		}
		replay, e := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, req)
		if e != nil || replay.RunID != receipt.RunID {
			t.Fatal("replay", e)
		}
		return receipt
	}
	send(a, 1, "QUESTION_ALPHA")
	send(b, 1, "QUESTION_BETA")
	direction = "DIRECTION_BETA"
	_, err = spaces.Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 2, Name: "代数", Description: "PRIVATE_DESCRIPTION", Status: "active", Direction: &direction})
	if err != nil {
		t.Fatal(err)
	}
	send(b, 2, "QUESTION_GAMMA")
	if len(inputs) != 3 {
		t.Fatalf("unexpected calls %d", len(inputs))
	}
	for i, input := range inputs {
		if strings.Contains(input, "PRIVATE_DESCRIPTION") || strings.Contains(input, "真实绑定目标") {
			t.Fatalf("private context leaked at %d", i)
		}
	}
	if strings.Contains(inputs[1], "SOURCE_ALPHA") || strings.Contains(inputs[1], "QUESTION_ALPHA") || !strings.Contains(inputs[1], "SOURCE_BETA") {
		t.Fatal("A to B history isolation failed")
	}
	if strings.Contains(inputs[2], "DIRECTION_ALPHA") || strings.Contains(inputs[2], "QUESTION_BETA") || !strings.Contains(inputs[2], "DIRECTION_BETA") {
		t.Fatal("direction history isolation failed")
	}
	// The same maintenance transaction rewraps direction and saved study turns.
	rewrap, _ := keyrotation.New(bytes.Repeat([]byte{42}, 32), bytes.Repeat([]byte{43}, 32))
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_mentor_runs,learning_tutor_conversations,learning_tutor_turns IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = learningdirection.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.pool, f.settings, bytes.Repeat([]byte{43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	page := historyPage(t, f, id, 0, 10)
	if len(page.Items) != 3 || page.Items[0].Output != "讲解 1 [1]" {
		t.Fatal("saved study cannot reopen")
	}
	if _, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'knowledge:read') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	page = historyPage(t, f, id, 0, 10)
	for _, turn := range page.Items {
		if turn.BodyAvailable || turn.Output != "" {
			t.Fatal("revoked content resurfaced")
		}
	}
	var practice int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tutoring_sessions)+(SELECT count(*) FROM learning_attempts)`).Scan(&practice); err != nil || practice != 0 {
		t.Fatal("explanation created practice facts", err)
	}
}

func TestPostgreSQLStudyCitationRetriesStayBounded(t *testing.T) {
	for _, scenario := range []string{"repair", "invalid", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
				fragment := 99
				if scenario == "repair" && n == 2 {
					fragment = 1
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": fmt.Sprintf(`{"answer":"说明 [1]","citations":[{"source":1,"fragment":%d}],"exercise":null}`, fragment)}}}})
			})
			ctx := context.Background()
			direction := "从资料理解概念"
			if _, err := spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "重试", Direction: &direction, Status: "active"}); err != nil {
				t.Fatal(err)
			}
			source := selectedDocument(t, f, "可以核验的原文")
			id, err := f.service.NewConversation(ctx, f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), Mode: "study"})
			if err != nil {
				t.Fatal(err)
			}
			budget := 2
			if scenario == "budget" {
				budget = 1
			}
			receipt, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, DirectionVersion: 1, Prompt: "讲解", RequestBudget: budget, TokenBudget: 30000, SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}}})
			if err != nil {
				t.Fatal(err)
			}
			f.work(t)
			snapshot := f.snapshot(t, receipt.RunID)
			if scenario == "repair" {
				if snapshot.Status != "succeeded" || len(snapshot.Citations) != 1 || f.calls.Load() != 2 {
					t.Fatal("repair failed or exceeded bounds")
				}
			} else {
				if snapshot.Output != "" || len(snapshot.Citations) != 0 {
					t.Fatal("unverified answer published")
				}
				if scenario == "invalid" && (snapshot.Status != "failed" || f.calls.Load() != 2) {
					t.Fatal("invalid citations exceeded retry bound")
				}
				if scenario == "budget" && (snapshot.Status != "paused_budget" || f.calls.Load() != 1) {
					t.Fatal("retry bypassed budget")
				}
			}
		})
	}
}
