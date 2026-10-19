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
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func contentPlan(count int) string {
	sections := []any{}
	for i := 1; i <= count; i++ {
		sections = append(sections, map[string]any{"id": fmt.Sprintf("s%d", i), "topic": fmt.Sprintf("第%d部分", i), "objective": "理解来源", "chapters": []int{1}})
	}
	raw, _ := json.Marshal(map[string]any{"title": "分段学习", "coverage": "partial", "sections": sections, "uncovered": []string{}, "explanation": "按资料组织。"})
	return string(raw)
}
func contentJob(t *testing.T, f *runtimeFixture, c StudioGenerate) (context.Context, learningcontent.StudioJob) {
	t.Helper()
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	create := StudioJobCreate{OperationID: uuid.NewString(), Generation: 1, Kind: "note", SourceSelection: c.SourceSelection}
	if c.ArtifactID != nil {
		create.ArtifactID = *c.ArtifactID
		create.ExpectedArtifactVersion = c.ExpectedVersion
	}
	j, err := f.service.CreateStudioJob(ctx, f.actor, create)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	j, err = f.service.StudioJob(ctx, f.actor, j.ID)
	if err != nil || j.Status != "awaiting_confirmation" {
		t.Fatalf("plan: %+v %v", j, err)
	}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "confirm", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, j
}

func contentSuccess(w http.ResponseWriter, r *http.Request, _ int) {
	raw, _ := io.ReadAll(r.Body)
	if bytes.Contains(raw, []byte("directory")) {
		studioReply(w, contentPlan(1), "stop")
		return
	}
	if bytes.Contains(raw, []byte("allowed_claims")) {
		studioReply(w, `{"overview":["s1-1:1"],"transitions":[],"uncovered":[]}`, "stop")
		return
	}
	studioReply(w, `{"title":"笔记","coverage":"partial","claims":[{"text":"资料事实","citations":[{"source":1,"fragment":1}]}]}`, "stop")
}

func TestPostgreSQLStudioContentResumesSAQueuedJob(t *testing.T) {
	f, c := studioFixture(t, contentSuccess)
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	j, err := f.service.CreateStudioJob(ctx, f.actor, StudioJobCreate{OperationID: uuid.NewString(), Generation: 1, Kind: "note", SourceSelection: c.SourceSelection})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	tx, g, err := f.service.studioJobBegin(ctx, f.actor, true)
	if err != nil {
		t.Fatal(err)
	}
	j, err = f.service.studio.StudioJobTx(ctx, tx, j.ID, g, true)
	if err != nil {
		t.Fatal(err)
	}
	// S-A confirmation persisted only this state; its planning run is complete.
	j.ConfirmedPlanRevision = j.ActivePlanRevision
	j.Status = "queued"
	j.Version++
	j.Input.Configuration = ""
	j.Input.Destination = ""
	if err = f.service.studio.SaveStudioJobTx(ctx, tx, &j); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	f.work(t)
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "completed" || j.TotalUnits != 1 || j.CompletedUnits != 1 {
		t.Fatalf("S-A queued job not resumed: %s %d/%d", j.Status, j.CompletedUnits, j.TotalUnits)
	}
}

func TestPostgreSQLStudioContentVersionConflict(t *testing.T) {
	f, c := studioFixture(t, contentSuccess)
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	original, err := f.service.studio.StudioSave(ctx, f.actor, "", learningcontent.StudioSave{OperationID: uuid.NewString(), Generation: 1, Kind: "note", GoalID: &f.goal, Title: "原笔记", Markdown: "原内容"})
	if err != nil {
		t.Fatal(err)
	}
	c.ArtifactID = &original.ArtifactID
	c.ExpectedVersion = 1
	ctx, j := contentJob(t, f, c)
	updated, err := f.service.studio.StudioSave(ctx, f.actor, original.ArtifactID, learningcontent.StudioSave{OperationID: uuid.NewString(), Generation: 1, Kind: "note", GoalID: &f.goal, ExpectedVersion: 1, Title: "用户新版本", Markdown: "不能覆盖"})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	f.work(t)
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "failed" || j.Reason != "studio_version_conflict" || j.CompletedUnits != 1 {
		t.Fatalf("conflict lost progress: %+v", j)
	}
	j = contentCommand(t, f, ctx, j, "save_as_new")
	f.work(t)
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "completed" || j.Result.ArtifactID == original.ArtifactID {
		t.Fatalf("save as new: %+v", j)
	}
	for _, v := range []int64{1, 2} {
		r, e := f.service.studio.StudioGet(ctx, f.actor, original.ArtifactID, v)
		if e != nil || r.Body.Schema != 1 {
			t.Fatal("legacy version", e)
		}
	}
	current, err := f.service.studio.StudioGet(ctx, f.actor, original.ArtifactID, 0)
	if err != nil || current.Version != updated.Version || current.Body.Markdown != "不能覆盖" {
		t.Fatal("overwrote target", err)
	}
	c.ExpectedVersion = 2
	ctx, regenerated := contentJob(t, f, c)
	f.work(t)
	f.work(t)
	regenerated, _ = contentSnapshot(t, f, ctx, regenerated.ID)
	if regenerated.Status != "completed" || regenerated.Result.ArtifactID != original.ArtifactID || regenerated.Result.Version != 3 {
		t.Fatalf("legacy target regeneration: %+v", regenerated)
	}
	r, err := f.service.studio.StudioGet(ctx, f.actor, original.ArtifactID, 3)
	if err != nil || r.GoalID == nil || *r.GoalID != f.goal {
		t.Fatal("lost legacy goal binding", err)
	}
}

func TestPostgreSQLStudioContentStopPublishRace(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	block := true
	f, c := studioFixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("allowed_claims")) && block {
			close(entered)
			<-release
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		contentSuccess(w, r, n)
	})
	ctx, j := contentJob(t, f, c)
	f.work(t)
	done := make(chan error, 1)
	go func() { _, e := f.service.RunOnce(ctx); done <- e }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("summary not dispatched")
	}
	if _, err := f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var activeCalls int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_model_calls WHERE stage='studio_summary' AND dispatch_state='dispatching'`).Scan(&activeCalls); err != nil || activeCalls != 1 {
		close(release)
		<-done
		t.Fatalf("active job usage recovered as lost: %d %v", activeCalls, err)
	}
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	j = contentCommand(t, f, ctx, j, "stop")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "stopped" || j.Result != nil || j.CompletedUnits != 1 {
		t.Fatalf("late publish after stop: %+v", j)
	}
	block = false
	j = contentCommand(t, f, ctx, j, "resume")
	stale := j
	f.work(t)
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "completed" {
		t.Fatalf("resume summary: %+v", j)
	}
	result := contentCommand(t, f, ctx, stale, "stop")
	if result.Status != "completed" || result.Result.ArtifactID != j.Result.ArtifactID {
		t.Fatal("stop after publish lost result")
	}
}

// A worker may enqueue its next run while a command waits for the previous run.
// The command must lock that new run before returning with the job locked.
func TestPostgreSQLStudioContentCommandLocksRunAfterProgress(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f, c := studioFixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte("directory")) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		contentSuccess(w, r, n)
	})
	ctx, observed := contentJob(t, f, c)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	worker := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(ctx); worker <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("section not dispatched")
	}
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var pid int
	if err = blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM learning_studio_jobs WHERE id=$1 FOR UPDATE`, observed.ID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	waitBlocked := func(want int) {
		t.Helper()
		for {
			var count int
			if err := f.pool.QueryRow(ctx, `WITH RECURSIVE waiting(pid) AS (
 SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))
 UNION SELECT a.pid FROM pg_stat_activity a JOIN waiting w ON w.pid=ANY(pg_blocking_pids(a.pid))
) SELECT count(*) FROM waiting`, pid).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitBlocked(1)
	command, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Rollback(context.Background())
	lockedJob := make(chan learningcontent.StudioJob, 1)
	commandError := make(chan error, 1)
	go func() {
		j, err := f.service.studioCommandJobTx(ctx, command, observed.ID, observed.Generation, "stop")
		lockedJob <- j
		commandError <- err
	}()
	waitBlocked(2)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	j := <-lockedJob
	if err = <-commandError; err != nil {
		t.Fatal(err)
	}
	if err = <-worker; err != nil {
		t.Fatal(err)
	}
	if j.RunID == observed.RunID || j.CompletedUnits != 1 {
		t.Fatal("command did not observe the worker's next run")
	}
	_, err = f.pool.Exec(ctx, `SELECT id FROM learning_mentor_runs WHERE id=$1 FOR UPDATE NOWAIT`, j.RunID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("current run was not locked before the job: %v", err)
	}
}

func TestPostgreSQLStudioContentLargeSourceAndChapter(t *testing.T) {
	sectionCalls := 0
	windowRejected := false
	f, c := studioFixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Error(err)
		}
		if len(payload.Messages) > 1 && strings.Contains(payload.Messages[0].Content, "当前部分") {
			sectionCalls++
			var input struct {
				Sources []knowledge.SourceDetail `json:"sources"`
			}
			if err := json.Unmarshal([]byte(payload.Messages[1].Content), &input); err != nil {
				t.Error(err)
			}
			if len(input.Sources) != 1 || len(input.Sources[0].Fragments) != 1 || len(input.Sources[0].Fragments[0].Text) > 4096 {
				t.Errorf("unbounded section request")
			}
			if bytes.Contains(raw, []byte("UNSELECTED_SECRET_SOURCE")) {
				t.Error("source leak")
			}
			if !windowRejected && len(input.Sources[0].Fragments[0].Text) > 2048 {
				windowRejected = true
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"code":"context_length_exceeded"}}`))
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		contentSuccess(w, r, n)
	})
	// One real chapter alone exceeds the configured model window; another
	// selected document increases total source size without entering this unit.
	a := selectedDocument(t, f, "长章节\n\n"+strings.Repeat("来源事实。", 3000))
	b := selectedDocument(t, f, "其他章节\n\n"+strings.Repeat("其他事实。", 3000))
	c.SourceSelection.Items = []knowledge.SourceIdentity{a, b}
	ctx, j := contentJob(t, f, c)
	if j.TotalUnits < 8 {
		t.Fatalf("large chapter not split: %d", j.TotalUnits)
	}
	for i := 0; i < j.TotalUnits+1; i++ {
		f.work(t)
	}
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "completed" || sectionCalls != j.TotalUnits+1 || !windowRejected {
		t.Fatalf("large source: %s %d/%d", j.Status, sectionCalls, j.TotalUnits)
	}
}
func contentSnapshot(t *testing.T, f *runtimeFixture, ctx context.Context, id string) (learningcontent.StudioJob, map[string]any) {
	t.Helper()
	j, err := f.service.StudioJob(ctx, f.actor, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(j)
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return j, result
}
func contentCommand(t *testing.T, f *runtimeFixture, ctx context.Context, j learningcontent.StudioJob, kind string) learningcontent.StudioJob {
	t.Helper()
	next, err := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, kind, StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, Kind: kind})
	if err != nil {
		t.Fatal(kind, err)
	}
	return next
}

// Progress must not invalidate controls, lose saved units, or replay side effects.
func TestPostgreSQLStudioContentStopResumeWithStaleVersion(t *testing.T) {
	calls := 0
	f, c := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("studio_plan")) || bytes.Contains(raw, []byte("directory")) {
			studioReply(w, contentPlan(8), "stop")
			return
		}
		calls++
		if bytes.Contains(raw, []byte("allowed_claims")) {
			studioReply(w, `{"overview":["s1-1:1"],"transitions":[],"uncovered":["未纳入的资料范围"]}`, "stop")
			return
		}
		studioReply(w, `{"title":"本段笔记","coverage":"partial","claims":[{"text":"偶数可被2整除。","citations":[{"source":1,"fragment":1}]}]}`, "stop")
	})
	ctx, j := contentJob(t, f, c)
	observed := j
	for i := 0; i < 3; i++ {
		f.work(t)
	}
	j, snapshot := contentSnapshot(t, f, ctx, j.ID)
	if snapshot["completed_units"] != float64(3) || snapshot["total_units"] != float64(8) {
		t.Fatalf("expected persisted 3/8, got %+v", snapshot)
	}
	if j.Version <= observed.Version {
		t.Fatal("generation did not advance the job version")
	}
	staleEdit := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: observed.Version, PlanRevision: observed.ActivePlanRevision, DeleteIDs: []string{"s8"}}
	if _, err := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "patch", staleEdit); !errors.Is(err, learningcontent.ErrConflict) {
		t.Fatalf("stale plan edit: %v", err)
	}
	for _, invalid := range []StudioPlanCommand{
		{OperationID: uuid.NewString(), ExpectedVersion: j.Version + 1, PlanRevision: j.ActivePlanRevision, Kind: "stop"},
		{OperationID: uuid.NewString(), ExpectedVersion: observed.Version, PlanRevision: j.ActivePlanRevision - 1, Kind: "stop"},
		{OperationID: uuid.NewString(), ExpectedVersion: observed.Version, PlanRevision: j.ActivePlanRevision, Kind: "resume"},
	} {
		if _, err := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, invalid.Kind, invalid); !errors.Is(err, learningcontent.ErrConflict) {
			t.Fatalf("invalid control (%s): %v", invalid.Kind, err)
		}
	}
	stop := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: observed.Version, PlanRevision: observed.ActivePlanRevision, Kind: "stop"}
	j, err := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "stop", stop)
	if err != nil {
		t.Fatalf("stop after progress with observed version %d: %v", observed.Version, err)
	}
	if j.Status != "stopped" {
		t.Fatalf("stop: %s", j.Status)
	}
	stopped := j
	if again := contentCommand(t, f, ctx, observed, "stop"); again.Version != stopped.Version {
		t.Fatal("repeated stop changed the job version")
	}
	if n, err := f.service.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("dispatched after stop: %d %v", n, err)
	}
	var artifacts int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_studio_artifacts`).Scan(&artifacts); err != nil || artifacts != 0 {
		t.Fatalf("partial publish: %d %v", artifacts, err)
	}
	reloaded, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	reloaded.ConfigureStudio(f.service.studio)
	f.service = reloaded
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	resume := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: observed.Version, PlanRevision: observed.ActivePlanRevision, Kind: "resume"}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "resume", resume)
	if err != nil {
		t.Fatalf("resume with stale version: %v", err)
	}
	if replay, err := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "stop", stop); err != nil || replay.Status != "stopped" || replay.Version != stopped.Version {
		t.Fatalf("stop receipt replay: status=%s version=%d err=%v", replay.Status, replay.Version, err)
	}
	changed := stop
	changed.ExpectedVersion = j.Version
	if _, err := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "stop", changed); !errors.Is(err, learningcontent.ErrConflict) {
		t.Fatalf("changed request reused operation ID: %v", err)
	}
	f.work(t)
	if replay, err := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "resume", resume); err != nil || replay.RunID != j.RunID || replay.Version != j.Version {
		t.Fatalf("resume receipt replay: run=%s version=%d err=%v", replay.RunID, replay.Version, err)
	}
	for i := 0; i < 5; i++ {
		f.work(t)
	}
	j, snapshot = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "completed" || snapshot["completed_units"] != float64(8) || calls != 9 {
		t.Fatalf("resume: %+v calls=%d", snapshot, calls)
	}
	result := snapshot["result"].(map[string]any)
	artifact, err := f.service.studio.StudioGet(ctx, f.actor, result["artifact_id"].(string), 1)
	if err != nil || artifact.Body.Schema != 2 {
		t.Fatalf("published: %+v %v", artifact, err)
	}
	sections, err := f.service.studio.StudioSections(ctx, f.actor, artifact.ArtifactID, 1, 0, 2)
	if err != nil || len(sections.Items) != 2 || sections.NextCursor != "2" {
		t.Fatalf("sections: %+v %v", sections, err)
	}
	edit := learningcontent.StudioSectionSave{OperationID: uuid.NewString(), Generation: 1, ExpectedVersion: 1, Markdown: "我的理解：需要再读资料。"}
	edited, err := f.service.studio.StudioSaveSection(ctx, f.actor, artifact.ArtifactID, sections.Items[0].ID, edit)
	if err != nil || edited.Version != 2 {
		t.Fatal("section edit", err)
	}
	replay, err := f.service.studio.StudioSaveSection(ctx, f.actor, artifact.ArtifactID, sections.Items[0].ID, edit)
	if err != nil || replay.Version != 2 {
		t.Fatal("edit replay", err)
	}
	latest, err := f.service.studio.StudioSections(ctx, f.actor, artifact.ArtifactID, 2, 0, 2)
	if err != nil || !latest.Items[0].Body.UserAuthored || len(latest.Items[0].Body.Claims) != 0 || latest.Items[1].Body.Claims[0].Text != sections.Items[1].Body.Claims[0].Text {
		t.Fatal("edited provenance or unchanged section", err)
	}
	old, err := f.service.studio.StudioSections(ctx, f.actor, artifact.ArtifactID, 1, 0, 2)
	if err != nil || len(old.Items[0].Body.Claims) != 1 {
		t.Fatal("old version modified", err)
	}
	note, err := f.service.studio.StudioNoteSource(ctx, f.actor, artifact.ArtifactID, 2)
	if err != nil || !strings.Contains(note.Markdown, edit.Markdown) || strings.Count(note.Markdown, "偶数可被2整除") != 7 {
		t.Fatal("note source projection", err)
	}
}

// Invalid citations never count as committed; summary retries cannot regenerate sections.
func TestPostgreSQLStudioContentInvalidCitationAndSummaryRetry(t *testing.T) {
	invalid, summaryFail := true, true
	sectionCalls, summaryCalls := 0, 0
	f, c := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("directory")) {
			studioReply(w, contentPlan(1), "stop")
			return
		}
		if bytes.Contains(raw, []byte("allowed_claims")) {
			summaryCalls++
			if summaryFail {
				http.Error(w, "unavailable", http.StatusBadRequest)
				return
			}
			studioReply(w, `{"overview":["s1-1:1"],"transitions":[],"uncovered":[]}`, "stop")
			return
		}
		sectionCalls++
		ref := 1
		if invalid {
			ref = 999
		}
		studioReply(w, fmt.Sprintf(`{"title":"笔记","coverage":"partial","claims":[{"text":"资料事实","citations":[{"source":%d,"fragment":1}]}]}`, ref), "stop")
	})
	ctx, j := contentJob(t, f, c)
	f.work(t)
	j, snapshot := contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "failed" || snapshot["completed_units"] != float64(0) || sectionCalls != 2 {
		t.Fatalf("invalid result persisted or repair unbounded: %+v calls=%d", snapshot, sectionCalls)
	}
	invalid = false
	j = contentCommand(t, f, ctx, j, "retry")
	f.work(t)
	f.work(t)
	j, snapshot = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "failed" || snapshot["completed_units"] != float64(1) {
		t.Fatalf("summary failure lost sections: %+v", snapshot)
	}
	summaryFail = false
	j = contentCommand(t, f, ctx, j, "retry")
	f.work(t)
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "completed" || sectionCalls != 3 || summaryCalls != 2 {
		t.Fatalf("summary retry: %s section=%d summary=%d", j.Status, sectionCalls, summaryCalls)
	}
}
