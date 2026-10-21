package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func TestSweepDiscardsUndecodableHistoryWithoutBlockingBatch(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "正常保存的回答"})
	})
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	badRuns := map[string]bool{}
	var badConversations []string
	for _, unknown := range []string{"requests", "tokens", "PRIVATE_HISTORY_FIELD"} {
		conversation := newHistory(t, f, true, f.goal)
		run := sendHistory(t, f, conversation, "旧版学习问题")
		f.work(t)
		snapshot := f.snapshot(t, run.RunID)
		// Removed policy fields and arbitrary unknown fields must remain
		// invalid under the strict history decoder.
		invalid := map[string]any{
			"study_exercise": map[string]any{"policy": map[string]any{
				"configuration": "saved-model", unknown: 8,
			}},
			"study":    map[string]any{"id": f.goal, "version": 1, "text": "已确认的学习方向"},
			"messages": []modelclient.Message{{Role: "user", Content: "旧版学习问题"}},
			"pending":  nil,
			"output":   "PRIVATE_HISTORY_BODY",
		}
		aad := historyAAD(conversation, snapshot.Generation, run.RunID)
		raw, err := sealHistory(f.service.aead, aad, invalid)
		if err != nil {
			t.Fatal(err)
		}
		if err = openHistory(f.service.aead, aad, raw, &Body{}); !errors.Is(err, ErrHistorySchema) {
			t.Fatalf("unknown field %q was not rejected: %v", unknown, err)
		}
		if _, err = f.pool.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=$2 WHERE run_id=$1`, run.RunID, raw); err != nil {
			t.Fatal(err)
		}
		if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=$2,state=jsonb_set(state,'{expires_at}',to_jsonb($2::timestamptz)) WHERE id=$1`, run.RunID, past); err != nil {
			t.Fatal(err)
		}
		badRuns[run.RunID] = true
		badConversations = append(badConversations, conversation)
	}
	goodConversation := newHistory(t, f, true, f.goal)
	good := sendHistory(t, f, goodConversation, "正常的问题")
	f.work(t)
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=$2,state=jsonb_set(state,'{expires_at}',to_jsonb($2::timestamptz)) WHERE id=$1`, good.RunID, past); err != nil {
		t.Fatal(err)
	}
	activeConversation := newHistory(t, f, true, f.goal)
	active := sendHistory(t, f, activeConversation, "尚未到期的问题")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	if count, err := f.service.Sweep(ctx); err != nil || count != 4 {
		t.Fatalf("invalid history blocked the cleanup batch: count=%d err=%v", count, err)
	}
	var remaining int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_mentor_runs WHERE expires_at<=clock_timestamp() AND ((state->>'body_available')::boolean OR checkpoint IS NOT NULL)`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("expired run bodies remain: count=%d err=%v", remaining, err)
	}
	for runID := range badRuns {
		var removed bool
		if err := f.pool.QueryRow(ctx, `SELECT ciphertext IS NULL FROM learning_tutor_turns WHERE run_id=$1`, runID).Scan(&removed); err != nil || !removed {
			t.Fatalf("invalid history ciphertext remains: removed=%v err=%v", removed, err)
		}
	}
	for _, conversation := range badConversations {
		page := historyPage(t, f, conversation, 0, 20)
		if len(page.Items) != 1 || page.Items[0].BodyAvailable || page.Items[0].Output != "" || len(page.Items[0].Messages) != 0 {
			t.Fatalf("invalid history is not an empty turn: %+v", page.Items)
		}
	}
	if page := historyPage(t, f, goodConversation, 0, 20); len(page.Items) != 1 || !page.Items[0].BodyAvailable || page.Items[0].Output != "正常保存的回答" {
		t.Fatalf("cleanup lost valid saved history: %+v", page.Items)
	}
	if snapshot := f.snapshot(t, active.RunID); !snapshot.BodyAvailable || snapshot.Status != "queued" {
		t.Fatalf("unexpired run was changed: %+v", snapshot.Meta)
	}
	if count, err := f.service.Sweep(ctx); err != nil || count != 0 {
		t.Fatalf("cleanup did not make lasting progress: count=%d err=%v", count, err)
	}
	if strings.Contains(logs.String(), "PRIVATE_HISTORY") || strings.Contains(logs.String(), "旧版学习问题") {
		t.Fatal("cleanup diagnostics leaked history content or an unknown field name")
	}
	decoder := json.NewDecoder(&logs)
	logged := map[string]bool{}
	for decoder.More() {
		var entry struct {
			Level    string `json:"level"`
			RunID    string `json:"run_id"`
			Category string `json:"error_category"`
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry.Level != "ERROR" || entry.Category != "tutor_history_unreadable" || !badRuns[entry.RunID] || logged[entry.RunID] {
			t.Fatalf("missing, repeated, or unsafe cleanup error classification: %+v", entry)
		}
		logged[entry.RunID] = true
	}
	if len(logged) != 3 {
		t.Fatalf("bad history was not individually reported: count=%d", len(logged))
	}
	var live bool
	if err := f.pool.QueryRow(ctx, `SELECT live_until>clock_timestamp() FROM learning_mentor_processes WHERE id=$1`, f.service.process).Scan(&live); err != nil || !live {
		t.Fatalf("cleanup heartbeat was not committed: live=%v err=%v", live, err)
	}
	if err := f.service.ReadConversation(ctx, f.actor, learningspace.DefaultID, activeConversation, 0, 1, func(TurnPage) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSweepWithoutHistoryKeyPreservesSavedHistory(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "正常保存的回答"})
	})
	ctx := context.Background()
	conversation := newHistory(t, f, true, f.goal)
	run := sendHistory(t, f, conversation, "正常的问题")
	f.work(t)
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	withoutKey, err := New(f.pool, f.settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := withoutKey.Sweep(ctx); count != 0 || !errors.Is(err, ErrStorage) {
		t.Fatalf("missing key was treated as corrupt history: count=%d err=%v", count, err)
	}
	if page := historyPage(t, f, conversation, 0, 20); len(page.Items) != 1 || !page.Items[0].BodyAvailable || page.Items[0].Output != "正常保存的回答" {
		t.Fatal("cleanup without a key discarded valid history")
	}
}

func TestSweepWithWrongHistoryKeyPreservesSavedHistory(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "正常保存的回答"})
	})
	ctx := context.Background()
	conversation := newHistory(t, f, true, f.goal)
	run := sendHistory(t, f, conversation, "正常的问题")
	f.work(t)
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	// A key mismatch cannot authenticate the body; it is not an unsupported shape.
	wrongKey, err := New(f.pool, f.settings, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongKey.Sweep(ctx); !errors.Is(err, ErrStorage) {
		t.Fatalf("wrong key was treated as unsupported history: %v", err)
	}
	if page := historyPage(t, f, conversation, 0, 20); len(page.Items) != 1 || !page.Items[0].BodyAvailable || page.Items[0].Output != "正常保存的回答" {
		t.Fatal("cleanup with a wrong key discarded valid history")
	}
}
