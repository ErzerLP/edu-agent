package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

func planCandidate(topic string, chapter int) string {
	raw, _ := json.Marshal(map[string]any{"title": "资料学习大纲", "coverage": "partial", "uncovered": []string{}, "explanation": "已按要求调整顺序与重点。", "sections": []any{
		map[string]any{"id": "first", "topic": topic, "objective": "理解资料中这一部分", "chapters": []int{chapter}},
		map[string]any{"id": "second", "topic": "练习前回顾", "objective": "回顾所选资料", "chapters": []int{1}},
		map[string]any{"id": "third", "topic": "复习提纲", "objective": "组织复习顺序", "chapters": []int{1}},
	}})
	return string(raw)
}

// A lost receipt, stale tab or new Service instance must not replace a plan,
// forget sent edits, create artifacts, or attribute edits to separate usage roots.
func TestPostgreSQLStudioPlanLifecycle(t *testing.T) {
	topic := "初始重点"
	f, legacy := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		raw, _ := io.ReadAll(r.Body)
		for _, secret := range []string{"UNSELECTED_SECRET_SOURCE", "BODY_ONLY_NEVER_SEND", "真实绑定目标", "RAW_CHAT_NEVER_SEND"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("unexpected body/history sent: %s", secret)
			}
		}
		if !bytes.Contains(raw, []byte("directory")) {
			t.Error("directory preflight absent")
		}
		studioReply(w, planCandidate(topic, 1), "stop")
	})
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	// An actual saved tutor conversation in this same space must never be read
	// into planning; only learning records are eligible context.
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		stream(w, modelclient.Message{Role: "assistant", Content: "RAW_CHAT_NEVER_SEND"})
	}
	conversation := newHistory(t, f, true, f.goal)
	sendHistory(t, f, conversation, "RAW_CHAT_NEVER_SEND")
	f.work(t)
	f.modelOverride = nil
	selected := selectedDocument(t, f, "目录名称\n\nBODY_ONLY_NEVER_SEND")
	legacy.SourceSelection.Items = []knowledge.SourceIdentity{selected}
	c := StudioJobCreate{OperationID: uuid.NewString(), Generation: 1, Kind: "guide", SourceSelection: legacy.SourceSelection, Focus: "先理解再复习"}
	j, err := f.service.CreateStudioJob(ctx, f.actor, c)
	if err != nil {
		t.Fatal(err)
	}
	if replay, e := f.service.CreateStudioJob(ctx, f.actor, c); e != nil || replay.ID != j.ID {
		t.Fatal("create replay", e)
	}
	f.work(t)
	j, err = f.service.StudioJob(ctx, f.actor, j.ID)
	if err != nil || j.Status != "awaiting_confirmation" || j.Plan == nil || len(j.Plan.Sections) != 3 {
		t.Fatalf("initial plan: %+v %v", j, err)
	}
	for _, next := range []string{"先看子网", "再做复习"} {
		topic = next
		command := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, Text: next}
		pending, e := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "message", command)
		if e != nil {
			t.Fatal(e)
		}
		if replay, e := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "message", command); e != nil || replay.RunID != pending.RunID {
			t.Fatal("message replay", e)
		}
		f.work(t)
		j, err = f.service.StudioJob(ctx, f.actor, j.ID)
		if err != nil || j.Plan.Sections[0].Topic != next {
			t.Fatal("revision lost", err)
		}
	}
	stale := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version - 1, PlanRevision: j.ActivePlanRevision, DeleteIDs: []string{"third"}}
	if _, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "patch", stale); !errors.Is(err, learningcontent.ErrConflict) {
		t.Fatalf("stale tab: %v", err)
	}
	edit := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, DeleteIDs: []string{"third"}, OrderedIDs: []string{"second", "first"}}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "patch", edit)
	if err != nil || len(j.Plan.Sections) != 2 || j.Plan.Sections[0].ID != "second" {
		t.Fatal("atomic delete/reorder", err)
	}
	confirm := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "confirm", confirm)
	if err != nil || j.Status != "queued" || j.ConfirmedPlanRevision != j.ActivePlanRevision {
		t.Fatal("confirmation", err)
	}
	if replay, e := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "confirm", confirm); e != nil || replay.Version != j.Version {
		t.Fatal("confirmation replay", e)
	}
	reloaded, e := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if e != nil {
		t.Fatal(e)
	}
	reloaded.ConfigureStudio(f.service.studio)
	restored, e := reloaded.StudioJob(ctx, f.actor, j.ID)
	if e != nil || restored.Status != "queued" || len(restored.Messages) != 2 || restored.Messages[1].Text != "再做复习" || restored.Plan.Sections[0].ID != "second" {
		t.Fatalf("restore: %+v %v", restored, e)
	}
	var artifacts, roots, calls int
	if e = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_studio_artifacts),(SELECT count(*) FROM learning_model_tasks WHERE kind='studio'),(SELECT count(*) FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.kind='studio')`).Scan(&artifacts, &roots, &calls); e != nil {
		t.Fatal(e)
	}
	if artifacts != 0 || roots != 1 || calls != 3 {
		t.Fatalf("artifacts=%d roots=%d calls=%d", artifacts, roots, calls)
	}
	// A normal key rotation must preserve the same saved plan and edit history.
	newKey := bytes.Repeat([]byte{43}, 32)
	rewrap, e := keyrotation.New(bytes.Repeat([]byte{42}, 32), newKey)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := f.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `LOCK TABLE learning_mentor_runs,learning_tutor_conversations,learning_tutor_turns,learning_content_revisions IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); e != nil {
		t.Fatal(e)
	}
	if e = RotateKeyTx(ctx, tx, rewrap); e != nil {
		t.Fatal(e)
	}
	if e = learningcontent.RotateKeyTx(ctx, tx, rewrap); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	rotated, e := New(f.pool, f.settings, newKey)
	if e != nil {
		t.Fatal(e)
	}
	content, e := learningcontent.New(f.pool, nil, newKey)
	if e != nil {
		t.Fatal(e)
	}
	rotated.ConfigureStudio(content)
	restored, e = rotated.StudioJob(ctx, f.actor, j.ID)
	if e != nil || restored.Plan.Sections[0].ID != "second" || len(restored.Messages) != 2 {
		t.Fatal("rotation lost planning state", e)
	}
	deleted, e := rotated.ChangeStudioPlan(ctx, f.actor, j.ID, "delete", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: restored.Version, PlanRevision: restored.ActivePlanRevision, Kind: "delete"})
	if e != nil || deleted.Status != "deleted" {
		t.Fatal("delete failed", e)
	}
	if _, e = rotated.StudioJob(ctx, f.actor, j.ID); !errors.Is(e, learningcontent.ErrNotFound) {
		t.Fatal("deleted job restored", e)
	}
	var remains int
	if e = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_studio_plans)+(SELECT count(*) FROM learning_studio_plan_messages)+(SELECT count(*) FROM learning_studio_job_operations)+(SELECT count(*) FROM learning_mentor_runs WHERE studio_job_id IS NOT NULL)`).Scan(&remains); e != nil || remains != 0 {
		t.Fatal("deleted planning inputs survived", remains, e)
	}
}

func TestPostgreSQLStudioPlanPagesLargeDirectory(t *testing.T) {
	f, legacy := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if agentcore.NewTokenEstimator().EstimateRequest(request)+request.MaxTokens+410 > 8192 {
			t.Error("planning request exceeds configured context")
		}
		var input struct {
			Directory []knowledge.SourceDirectoryEntry `json:"directory"`
			Total     int                              `json:"available_chapters"`
		}
		if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
			t.Fatal(err)
		}
		if input.Total < 90 || len(input.Directory) >= 90 {
			t.Fatalf("directory was not paged/bounded: %d/%d", len(input.Directory), input.Total)
		}
		index := 0
		for i, n := range input.Directory {
			if n.Title == "末页子网重点" {
				index = i + 1
			}
		}
		if index == 0 {
			t.Fatal("relevant chapter beyond first directory page was omitted")
		}
		studioReply(w, planCandidate("子网重点", index), "stop")
	})
	var update settings.Update
	_ = json.Unmarshal([]byte(`{"limits":{"context_tokens":8192}}`), &update)
	update.ExpectedRevision = f.settings.View().Revision
	if _, err := f.settings.Update(update); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	text.WriteString("目录\n\n首段\n")
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&text, "\n## 普通章节 %d\n\n%s\n", i, strings.Repeat("不应外发的大段正文。", 50))
	}
	text.WriteString("\n## 末页子网重点\n\nBODY_ONLY_NEVER_SEND\n")
	selected := selectedDocument(t, f, text.String())
	legacy.SourceSelection.Items = []knowledge.SourceIdentity{selected}
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	j, err := f.service.CreateStudioJob(ctx, f.actor, StudioJobCreate{OperationID: uuid.NewString(), Generation: 1, Kind: "brief", Focus: "末页子网重点", SourceSelection: legacy.SourceSelection})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	j, err = f.service.StudioJob(ctx, f.actor, j.ID)
	if err != nil || j.Plan == nil || j.Plan.Coverage != "partial" || j.Plan.Sections[0].Chapters[0].Title != "末页子网重点" {
		t.Fatalf("large plan: %+v %v", j, err)
	}
}

func TestPostgreSQLStudioPlanRejectsInventedChapter(t *testing.T) {
	f, legacy := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		studioReply(w, planCandidate("伪造章节", 999), "stop")
	})
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	j, err := f.service.CreateStudioJob(ctx, f.actor, StudioJobCreate{OperationID: uuid.NewString(), Generation: 1, Kind: "note", SourceSelection: legacy.SourceSelection})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	j, err = f.service.StudioJob(ctx, f.actor, j.ID)
	if err != nil || j.Status != "failed" || j.Plan != nil {
		t.Fatalf("invented chapter saved: %+v %v", j, err)
	}
}
