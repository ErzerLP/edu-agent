package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

// A stopped/unknown execution must resume its checkpoint, not renew page slots
// or automatically replay an external dispatch when its lease expires.
func TestSourceResearchResumeStoppedAndUnknown(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "unknown"}[unknown], func(t *testing.T) {
			f := sourceResearchFixture(t)
			ctx := context.Background()
			job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "恢复原轮"})
			if err != nil {
				t.Fatal(err)
			}
			owned, err := f.service.claim(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if unknown {
				_, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET call_started=true,lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, owned.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.service.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				job, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
				if err != nil || job.Status != "outcome_unknown" || f.calls.Load() != 0 {
					t.Fatalf("unknown dispatched again: %+v %v", job, err)
				}
				if _, err = f.service.RunOnce(ctx); err != nil || f.calls.Load() != 0 {
					t.Fatal("unknown automatically replayed", err)
				}
			} else {
				job, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "stop"})
				if err != nil {
					t.Fatal(err)
				}
			}
			scale := settings.SourceResearch{MaxSourcesPerRound: 1, MaxPagesPerRound: 1}
			if _, err = f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, SourceResearch: &scale}); err != nil {
				t.Fatal(err)
			}
			resumed, err := f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "resume"})
			if err != nil {
				t.Fatal("original round cannot resume", err)
			}
			if resumed.RoundID != job.RoundID || resumed.RunID != job.RunID || resumed.Limits != job.Limits {
				t.Fatal("resume replaced round or limits")
			}
			if _, err = f.service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			resumed, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
			if err != nil || resumed.Status != "completed" {
				t.Fatalf("resume: %+v %v", resumed, err)
			}
		})
	}
}

func TestSourceResearchMoreConcurrentPreservesHistory(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	job := completeSourceResearch(t, f)
	oldReport, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	oldCandidates, err := f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	job, err = f.service.SelectSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchSelectionCommand{OperationID: uuid.NewString(), ExpectedVersion: job.SelectionVersion, Items: []SourceResearchSelectionItem{{CandidateID: oldCandidates.Items[0].ID, Selected: false}}, ReportRevisionID: &job.ReportRevisionID})
	if err != nil {
		t.Fatal(err)
	}
	scale := settings.SourceResearch{MaxSourcesPerRound: 1, MaxPagesPerRound: 2}
	if _, err = f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, SourceResearch: &scale}); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		job SourceResearchJob
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			next, e := f.service.MoreSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchMore{OperationID: uuid.NewString(), ExpectedVersion: job.Version, AdditionalRequest: "补充实践"})
			results <- outcome{next, e}
		}()
	}
	close(start)
	var next SourceResearchJob
	success, conflict := 0, 0
	for range 2 {
		r := <-results
		if r.err == nil {
			success++
			next = r.job
		} else if errors.Is(r.err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(r.err)
		}
	}
	if success != 1 || conflict != 1 || next.RoundID == job.RoundID || next.Limits != scale {
		t.Fatalf("concurrent more: success=%d conflict=%d %+v", success, conflict, next)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	next, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil || next.Status != "completed" || next.Progress.ReadAttempted != 0 || next.CandidateCount != 0 {
		t.Fatalf("old URLs read again: %+v %v", next, err)
	}
	after, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil || !reflect.DeepEqual(oldReport, after) || next.ReportRevisionID == job.ReportRevisionID || next.SelectedReportRevisionID != job.ReportRevisionID {
		t.Fatal("published report/selection changed", err)
	}
	candidates, err := f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil || len(candidates.Items) != 2 {
		t.Fatalf("old results lost: %+v %v", candidates, err)
	}
	for _, c := range candidates.Items {
		if c.ID == oldCandidates.Items[0].ID && c.Selected {
			t.Fatal("old cancellation reset")
		}
	}
	var rounds, roots int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_source_research_rounds WHERE job_id=$1`, job.ID).Scan(&rounds); err != nil || rounds != 2 {
		t.Fatal(rounds, err)
	}
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_model_tasks WHERE origin_id=$1`, job.ID).Scan(&roots); err != nil || roots != 1 {
		t.Fatal("split usage root", roots, err)
	}
	same := SourceResearchMore{OperationID: uuid.NewString(), ExpectedVersion: next.Version}
	start = make(chan struct{})
	for range 2 {
		go func() {
			<-start
			value, e := f.service.MoreSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, same)
			results <- outcome{value, e}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.job.RoundID != b.job.RoundID {
		t.Fatalf("concurrent operation replay: %v %v", a.err, b.err)
	}
}

func TestSourceResearchResumeReservedLastPage(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	scale := settings.SourceResearch{MaxSourcesPerRound: 1, MaxPagesPerRound: 1}
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, SourceResearch: &scale}); err != nil {
		t.Fatal(err)
	}
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "恢复最后一个阅读名额"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := f.service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &executionHost{service: f.service, owned: *owned, body: owned.body, limits: f.settings.View().Limits}
	resource, err := h.registerWebResource(ctx, "http://example.org/reserved", "保留页面", "", "search")
	if err != nil {
		t.Fatal(err)
	}
	state := h.body.SourceResearch
	state.Directions = []SourceResearchDirection{{ID: "direction-1", Title: "基础", Purpose: "基础", Query: "basics", State: "reading"}}
	state.Searched = true
	state.Queue = []string{resource.ID}
	state.PendingResource = resource.ID
	state.DirectionAllowance = 1
	state.DirectionCandidateGoal = 1
	if err = h.reserveSourceResearchURL(ctx, resource.URL); err != nil {
		t.Fatal(err)
	}
	h.body.Web.Progress.ReadAttempted = 1
	if err = h.checkpoint(ctx, "reserved"); err != nil {
		t.Fatal(err)
	}
	job, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	job, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil || job.Status != "completed" || job.Progress.ReadAttempted != 1 || job.Progress.ReadSucceeded != 1 || job.Progress.ReadFailed != 0 || job.CandidateCount != 1 {
		t.Fatalf("reserved last page lost or counted twice: %+v %v", job, err)
	}
}

func TestSourceResearchMoreDeduplicatesAndKeepsImports(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	var more atomic.Bool
	prose := ""
	for i := 0; i < 90; i++ {
		prose += fmt.Sprintf("Protocol exercise %d explains network packets and independently measures latency at node %d. ", i, i*i)
	}
	pages := map[string]string{
		"/basics":   "<title>RFC 1234 - Network Example</title><main>Request for Comments: 1234\nThis original protocol description explains the packet header and operational design.</main>",
		"/practice": "<title>Network exercises</title><main>" + prose + "</main>",
		"/formal":   "<title>Network Example</title><main>RFC 1234\nA different format of the same numbered specification with a substantially different extracted excerpt.</main>",
		"/hash":     "<title>Network exercises</title><main>" + prose + "</main>",
		"/near":     "<title>Exercises mirror</title><main>Publisher introduction. " + prose + "</main>",
		"/new":      "<title>New independent lab</title><main>独立实验使用全新的实验数据分析拥塞窗口，提供故障排查的实际操作步骤。</main>",
	}
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urls := []string{"http://example.org/basics"}
		if strings.Contains(r.URL.Query().Get("q"), "practice") {
			urls = []string{"http://example.org/practice"}
		}
		if more.Load() {
			urls = []string{"http://example.org/basics?utm_source=again", "http://formal.example.net/formal", "http://hash.example.com/hash", "http://near.example.edu/near", "http://new.example.info/new"}
		}
		items := []map[string]string{}
		for _, u := range urls {
			items = append(items, map[string]string{"title": "公开线索", "url": u})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": items}})
	}))
	defer search.Close()
	f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		var transport http.RoundTripper = f.service.usage.SearchTransport(http.DefaultTransport, "brave", "fixture")
		if wrap != nil {
			transport = wrap(transport)
		}
		return websearch.NewBrave(search.URL, "fixture", &http.Client{Transport: transport}), "fixture", nil
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, pages[r.URL.Path])
	}))
	defer page.Close()
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	job := completeSourceResearch(t, f)
	candidates, err := f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil || len(candidates.Items) != 2 {
		t.Fatalf("first candidates %+v %v", candidates, err)
	}
	cancelled := candidates.Items[0].ID
	job, err = f.service.SelectSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchSelectionCommand{OperationID: uuid.NewString(), ExpectedVersion: job.SelectionVersion, Items: []SourceResearchSelectionItem{{CandidateID: cancelled, Selected: false}}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: job.ResultSnapshotID, ExpectedSelectionVersion: job.SelectionVersion})
	if err != nil {
		t.Fatal(err)
	}
	oldReport, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	scale := settings.SourceResearch{MaxSourcesPerRound: 1, MaxPagesPerRound: 8}
	if _, err = f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, SourceResearch: &scale}); err != nil {
		t.Fatal(err)
	}
	more.Store(true)
	command := SourceResearchMore{OperationID: uuid.NewString(), ExpectedVersion: job.Version}
	next, err := f.service.MoreSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, command)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.MoreSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, command)
	if err != nil || replay.RoundID != next.RoundID {
		t.Fatal("more replay changed round", err)
	}
	// An old, exact preview remains importable while the next round runs.
	entry := preview.Items[0]
	importCommand := WebImportRequest{OperationID: entry.OperationID, PreviewID: entry.WebPreviewID, ExpectedVersion: 1, RevisionID: entry.RevisionID, StorageConsent: true}
	receipt, err := f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, importCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	next, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil || next.Status != "completed" || next.CandidateCount != 1 {
		t.Fatalf("supplement %+v %v", next, err)
	}
	candidates, err = f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil || len(candidates.Items) != 3 {
		t.Fatalf("cross-round duplicates delivered: %+v %v", candidates, err)
	}
	for _, c := range candidates.Items {
		if c.ID == cancelled && c.Selected {
			t.Fatal("cancelled candidate selected")
		}
		if c.RoundID == next.RoundID && (!c.Selected || c.Title != "New independent lab") {
			t.Fatalf("new candidate %+v", c)
		}
	}
	again, err := f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, importCommand)
	if err != nil || again.Source != receipt.Source {
		t.Fatal("old import duplicated", err)
	}
	saved, err := f.service.SourceResearchImports(ctx, f.actor, learningspace.DefaultID, job.ID, preview.ID)
	if err != nil || saved.Items[0].Status != "imported" {
		t.Fatalf("old receipt lost %+v %v", saved, err)
	}
	old, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil || !reflect.DeepEqual(old, oldReport) {
		t.Fatal("old report mutated", err)
	}
	newPreview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: next.ResultSnapshotID, ExpectedSelectionVersion: next.SelectionVersion})
	if err != nil || len(newPreview.Items) != 1 {
		t.Fatalf("new preview %+v %v", newPreview, err)
	}
	entry = newPreview.Items[0]
	if _, err = f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, WebImportRequest{OperationID: entry.OperationID, PreviewID: entry.WebPreviewID, ExpectedVersion: 1, RevisionID: entry.RevisionID, StorageConsent: true}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_source_revisions`).Scan(&count); err != nil || count != 2 {
		t.Fatal("duplicate source", count, err)
	}
	if _, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: next.Version, RoundID: next.RoundID, Kind: "delete"}); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_mentor_runs WHERE source_research_job_id=$1 AND checkpoint IS NOT NULL`, job.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted job retains old round payload", count, err)
	}
}

func TestSourceResearchResumeDoesNotRegenerateCommittedSection(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "保留已提交章节"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := f.service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &executionHost{service: f.service, owned: *owned, body: owned.body, limits: f.settings.View().Limits}
	state := h.body.SourceResearch
	state.Directions = []SourceResearchDirection{{ID: "direction-1", Title: "基础", Purpose: "原理", Query: "basics", State: "reading"}}
	state.Searched = true
	state.DirectionAllowance = 3
	state.DirectionCandidateGoal = 3
	r, err := h.registerWebResource(ctx, "http://example.org/committed", "已读正文", "", "search")
	if err != nil {
		t.Fatal(err)
	}
	value, err := h.sourceResearchTool(ctx, "read_web", map[string]string{"resource_id": r.ID})
	if err != nil {
		t.Fatal(err)
	}
	doc := value.(research.WebDocument)
	state.DirectionDocuments = []string{doc.ID}
	if err = h.screenSourceResearch(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.reportSourceResearch(ctx, state.Directions[0], state.DirectionDocuments); err != nil {
		t.Fatal(err)
	}
	// Deliberately stop after the section transaction and before advancing the
	// direction cursor, exactly the durable interruption boundary under test.
	job, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	before := f.calls.Load()
	original, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	job, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil || len(report.Sections) != 1 || !reflect.DeepEqual(report.Sections, original.Sections) || f.calls.Load() != before {
		t.Fatalf("committed report regenerated: %+v calls=%d/%d %v", report, f.calls.Load(), before, err)
	}
}
