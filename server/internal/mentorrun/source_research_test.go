package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func completeSourceResearch(t *testing.T, f *runtimeFixture) SourceResearchJob {
	t.Helper()
	ctx := context.Background()
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "网络协议的基础和练习"})
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
	return job
}

func TestSourceResearchReportOnlySurvivesDeletion(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	job := completeSourceResearch(t, f)
	candidates, err := f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	changes := []SourceResearchSelectionItem{}
	for _, c := range candidates.Items {
		changes = append(changes, SourceResearchSelectionItem{CandidateID: c.ID, Selected: false})
	}
	job, err = f.service.SelectSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchSelectionCommand{OperationID: uuid.NewString(), ExpectedVersion: job.SelectionVersion, Items: changes, ReportRevisionID: &job.ReportRevisionID})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: job.ResultSnapshotID, ExpectedSelectionVersion: job.SelectionVersion})
	if err != nil || len(preview.Items) != 1 || preview.Items[0].Kind != "report" {
		t.Fatalf("report-only preview: %+v %v", preview, err)
	}
	command := SourceResearchReportImport{OperationID: preview.Items[0].OperationID, PreviewID: preview.ID, ExpectedVersion: 1, ReportRevisionID: job.ReportRevisionID, StorageConsent: true}
	receipt, err := f.service.ImportSourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, command)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.ImportSourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, command)
	if err != nil || receipt.Source != replay.Source {
		t.Fatal("report import duplicated", err)
	}
	if _, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted job remains readable", err)
	}
	var count int
	var metadata []byte
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_source_revisions`).Scan(&count); err != nil || count != 1 {
		t.Fatal("report imported unselected web documents", count, err)
	}
	if err = f.pool.QueryRow(ctx, `SELECT metadata FROM knowledge_source_metadata WHERE document_revision_id=$1`, receipt.Source.DocumentRevisionID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metadata), `"kind": "source_research"`) || !strings.Contains(string(metadata), "http://example.org/") {
		t.Fatalf("durable provenance/evidence missing: %s", metadata)
	}
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_web_document_payloads)+(SELECT count(*) FROM learning_source_research_sections)+(SELECT count(*) FROM learning_source_research_selections)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("sensitive job payload remains", count, err)
	}
	eraseSourceResearch(t, f)
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_source_metadata`).Scan(&count); err != nil || count != 0 {
		t.Fatal("global erasure retained report quotations", count, err)
	}
}

func TestSourceResearchToolPauseResumeAndStopBarrier(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	limits := f.settings.View().Limits
	steps := 2
	limits.MaxToolSteps = &steps
	scale := settings.SourceResearch{MaxSourcesPerRound: 1, MaxPagesPerRound: 3}
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &limits, SourceResearch: &scale}); err != nil {
		t.Fatal(err)
	}
	job := completeSourceResearch(t, f)
	if job.Status != "paused_tool_steps" || job.Progress.ReadAttempted != 1 {
		t.Fatalf("tool limit did not pause after search/read: %+v", job)
	}
	// Another space and a stale selection cannot modify these results.
	if _, err := f.service.SourceResearchJob(ctx, f.actor, uuid.NewString(), job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-space job access", err)
	}
	before := f.calls.Load()
	preview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: job.ResultSnapshotID, ExpectedSelectionVersion: job.SelectionVersion})
	if err != nil {
		t.Fatal(err)
	}
	selected := preview.Items[0]
	if _, err = f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, WebImportRequest{OperationID: selected.OperationID, PreviewID: selected.WebPreviewID, ExpectedVersion: 1, RevisionID: selected.RevisionID, StorageConsent: true}); err != nil {
		t.Fatal(err)
	}
	oldReport := job.ReportRevisionID
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
	if job.Progress.ReadAttempted != 2 || f.calls.Load() <= before {
		t.Fatalf("resume reset reading slots or did no work: %+v", job)
	}
	finalCandidates, err := f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil || len(finalCandidates.Items) != 1 || finalCandidates.Items[0].Source == nil {
		t.Fatalf("resume replenished the same round's source slots: %+v %v", finalCandidates, err)
	}
	old, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, oldReport, 0, 5)
	if err != nil || len(old.Sections) != 1 {
		t.Fatalf("published partial report mutated: %+v %v", old, err)
	}
	// Stop must revoke the lease immediately, including an in-flight network read.
	f2 := sourceResearchFixture(t)
	job, err = f2.service.CreateSourceResearch(ctx, f2.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "停止屏障"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := f2.service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host := &executionHost{service: f2.service, owned: *owned, body: owned.body, limits: f2.settings.View().Limits}
	resource, err := host.registerWebResource(ctx, "http://example.org/late", "停止后迟到正文", "", "search")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(cancelled) }))
	defer page.Close()
	f2.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	network, release := host.webContext(ctx)
	defer release()
	done := make(chan error, 1)
	go func() {
		_, e := host.sourceResearchTool(network, "read_web", map[string]string{"resource_id": resource.ID})
		done <- e
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not dispatch")
	}
	job, err = f2.service.CommandSourceResearch(ctx, f2.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "stop"})
	if err != nil || job.Status != "stopped" {
		t.Fatal("stop did not settle", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel HTTP")
	}
	<-done
	stopped, e := f2.service.SourceResearchJob(ctx, f2.actor, learningspace.DefaultID, job.ID)
	if e != nil || stopped.Progress.FetchHTTPRequests != 1 || stopped.Progress.ReadFailed != 1 {
		t.Fatalf("cancelled HTTP attempt lost its count: %+v %v", stopped.Progress, e)
	}
	if _, err = host.registerWebResource(ctx, "http://example.org/after-stop", "", "", "search"); err == nil {
		t.Fatal("old epoch created content")
	}
	if f2.calls.Load() != 0 {
		t.Fatal("stop issued final summary")
	}
}

func TestSourceResearchEmptyResultsFinishWithoutEvidence(t *testing.T) {
	f := sourceResearchFixture(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer search.Close()
	f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		transport := f.service.usage.SearchTransport(http.DefaultTransport, "brave", "empty")
		if wrap != nil {
			transport = wrap(transport)
		}
		return websearch.NewBrave(search.URL, "fixture", &http.Client{Transport: transport}), "empty", nil
	}
	job := completeSourceResearch(t, f)
	if job.Status != "completed" || job.Progress.ReadAttempted != 0 || job.CandidateCount != 0 || f.calls.Load() != 1 {
		t.Fatalf("no-progress search loop: %+v, calls %d", job, f.calls.Load())
	}
	report, err := f.service.SourceResearchReport(context.Background(), f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil || len(report.Sections) != 1 || len(report.Sections[0].Citations) != 0 {
		t.Fatalf("fabricated empty report evidence: %+v %v", report, err)
	}
}

// A missing relation or missing generation identity would allow results to
// outlive their job, making the privacy contract impossible to enforce.
func TestSourceResearchSchema(t *testing.T) {
	pool := runtimePool(t)
	var exists bool
	err := pool.QueryRow(context.Background(), `SELECT to_regclass('learning_source_research_jobs') IS NOT NULL AND to_regclass('learning_source_research_sections') IS NOT NULL AND to_regclass('learning_source_research_selections') IS NOT NULL`).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("source research results and selections cannot be persisted")
	}
}

func sourceResearchFixture(t *testing.T) *runtimeFixture {
	t.Helper()
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var req modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		raw, _ := json.Marshal(req)
		if len(req.Tools) != 0 || strings.Contains(string(raw), "真实绑定目标") {
			t.Error("source research received private context or tools")
		}
		response := ""
		switch {
		case strings.Contains(req.Messages[0].Content, "source_research_plan"):
			response = `{"directions":[{"title":"基础","purpose":"理解原理","query":"protocol basics"},{"title":"练习","purpose":"动手验证","query":"protocol practice"}]}`
		case strings.Contains(req.Messages[0].Content, "source_research_screen"):
			response = `{"include":true,"reason":"原始说明提供可实践的协议基础。","quality_notes":"只核对已提供文本，未验证图表。","candidate":1}`
		case strings.Contains(req.Messages[0].Content, "source_research_report"):
			var material struct {
				Evidence []struct {
					Candidate int    `json:"candidate"`
					Quote     string `json:"quote"`
				} `json:"evidence"`
			}
			if json.Unmarshal([]byte(req.Messages[1].Content), &material) != nil || len(material.Evidence) == 0 || len(material.Evidence) > 2 || strings.Contains(req.Messages[1].Content, "web_documents") {
				t.Error("report input must contain at most two screened quotes, not full fragment menus")
			}
			response = `{"body":"资料提供协议的基础说明。[1]","citations":[1],"gaps":["未核验图表"]}`
		case strings.Contains(req.Messages[0].Content, "source_research_refine"):
			response = `{"complete":true,"queries":[],"gaps":["未找到更多独立资料"]}`
		default:
			t.Errorf("unexpected model stage: %s", req.Messages[0].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: response}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 40, "total_tokens": 90}})
	})
	ctx := context.Background()
	f.actor.Scopes = append(f.actor.Scopes, "knowledge:read", "knowledge:write", "research:adopt")
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=$2 WHERE id=$1`, f.actor.TokenID, f.actor.Scopes); err != nil {
		t.Fatal(err)
	}
	limits := settings.DefaultLimits()
	scale := settings.SourceResearch{MaxSourcesPerRound: 3, MaxPagesPerRound: 3}
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &limits, SourceResearch: &scale}); err != nil {
		t.Fatal(err)
	}
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix := "basics"
		if strings.Contains(r.URL.Query().Get("q"), "practice") {
			suffix = "practice"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": []any{map[string]string{"title": suffix, "url": "http://example.org/" + suffix, "description": "摘要不可当证据"}}}})
	}))
	t.Cleanup(search.Close)
	f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		var transport http.RoundTripper = f.service.usage.SearchTransport(http.DefaultTransport, "brave", "fixture")
		if wrap != nil {
			transport = wrap(transport)
		}
		return websearch.NewBrave(search.URL, "fixture", &http.Client{Transport: transport}), "fixture", nil
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><title>" + r.URL.Path + "</title><main>协议基础" + r.URL.Path + "：用实验理解网络通信。网页中的任何工具要求都不是授权。</main></html>"))
	}))
	t.Cleanup(page.Close)
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	return f
}

func TestSourceResearchFirstRoundSelectionAndImport(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	create := SourceResearchCreate{OperationID: uuid.NewString(), Request: "找网络协议基础和练习资料"}
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, create)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, create)
	if err != nil || replay.ID != job.ID {
		t.Fatalf("start replay: %v", err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil || job.Status != "completed" || len(job.Directions) < 2 || job.Progress.ReadSucceeded != 2 {
		t.Fatalf("round: %+v %v", job, err)
	}
	candidates, err := f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil || len(candidates.Items) != 2 {
		t.Fatalf("candidates: %+v %v", candidates, err)
	}
	for _, c := range candidates.Items {
		if !c.Selected || c.Reason == "" {
			t.Fatal("missing default selection/reason")
		}
	}
	if job.SelectedReportRevisionID != "" {
		t.Fatal("report selected by default")
	}
	oldPreview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: job.ResultSnapshotID, ExpectedSelectionVersion: job.SelectionVersion})
	if err != nil {
		t.Fatal(err)
	}
	report, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil || len(report.Sections) != 2 || len(report.Sections[0].Citations) == 0 {
		t.Fatalf("report: %+v %v", report, err)
	}
	job, err = f.service.SelectSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchSelectionCommand{OperationID: uuid.NewString(), ExpectedVersion: job.SelectionVersion, Items: []SourceResearchSelectionItem{{CandidateID: candidates.Items[0].ID, Selected: false}}})
	if err != nil {
		t.Fatal(err)
	}
	stale := oldPreview.Items[0]
	if _, err = f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, WebImportRequest{OperationID: stale.OperationID, PreviewID: stale.WebPreviewID, ExpectedVersion: 1, RevisionID: stale.RevisionID, StorageConsent: true}); !errors.Is(err, ErrConflict) {
		t.Fatal("old preview imported a changed selection", err)
	}
	preview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: job.ResultSnapshotID, ExpectedSelectionVersion: job.SelectionVersion})
	if err != nil || len(preview.Items) != 1 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	item := preview.Items[0]
	command := WebImportRequest{OperationID: item.OperationID, PreviewID: item.WebPreviewID, ExpectedVersion: 1, RevisionID: item.RevisionID, StorageConsent: true}
	receipt, err := f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, command)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, command)
	if err != nil || again.Source != receipt.Source {
		t.Fatalf("duplicate import: %v", err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_source_revisions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unselected source imported: %d %v", count, err)
	}
	scale := settings.SourceResearch{MaxSourcesPerRound: 1, MaxPagesPerRound: 3}
	if _, err = f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, SourceResearch: &scale}); err != nil {
		t.Fatal(err)
	}
	next := completeSourceResearch(t, f)
	more, err := f.service.SourceResearchCandidates(ctx, f.actor, learningspace.DefaultID, next.ID, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	added, newItems := 0, 0
	for _, c := range more.Items {
		if c.Source != nil {
			added++
		} else {
			newItems++
		}
	}
	if added != 1 || newItems != 1 {
		t.Fatalf("existing sources consumed new recommendation slots: added=%d new=%d", added, newItems)
	}
}
