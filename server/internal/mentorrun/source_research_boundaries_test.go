package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/transport/access"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSourceResearchSelectionRequiresExplicitItemChoice(t *testing.T) {
	var input SourceResearchSelectionCommand
	if err := json.Unmarshal([]byte(`{"operation_id":"a","expected_selection_version":1,"items":[{"candidate_id":"b"}]}`), &input); err == nil {
		t.Fatal("missing selected silently deselected a candidate")
	}
	if err := json.Unmarshal([]byte(`{"operation_id":"a","expected_selection_version":1,"items":[],"report_revision_id":null}`), &input); err != nil || !input.ReportChanged || input.ReportRevisionID != nil {
		t.Fatal("explicit report deselection was lost", err)
	}
}

func TestSourceResearchUnreadablePassEndsWithoutAnotherModelPlan(t *testing.T) {
	f := sourceResearchFixture(t)
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		t.Error("SSRF target dialed")
		return nil, research.ErrPolicy
	})
	job := completeSourceResearch(t, f)
	if job.Status != "completed" || job.Progress.ReadFailed != 2 || job.Progress.FetchHTTPRequests != 0 || f.calls.Load() != 1 {
		t.Fatalf("unreadable pass replanned: status=%s failed=%d HTTP=%d models=%d", job.Status, job.Progress.ReadFailed, job.Progress.FetchHTTPRequests, f.calls.Load())
	}
}

func TestSourceResearchSearchDispatchStaysUnknownUntilCursorCommit(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "检索恢复门禁"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := f.service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &executionHost{service: f.service, owned: *item, body: item.body, limits: f.settings.View().Limits}
	ctx = modelusage.WithTask(access.WithCredential(ctx, f.actor), modelusage.TaskContext{Kind: "source_research", OriginID: job.ID, Stage: "search_web", SpaceID: learningspace.DefaultID, Generation: item.Generation})
	if _, err = h.sourceResearchTool(ctx, "search_web", map[string]any{"query": "protocol basics", "count": 1}); err != nil {
		t.Fatal(err)
	}
	var dispatched bool
	if err = f.pool.QueryRow(ctx, `SELECT call_started FROM learning_mentor_runs WHERE id=$1`, item.RunID).Scan(&dispatched); err != nil || !dispatched {
		t.Fatal("paid search was settled before its result cursor committed", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, item.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil || after.Status != "outcome_unknown" || f.calls.Load() != 0 {
		t.Fatalf("unknown search automatically restarted: %+v %v", after, err)
	}
}

func TestSourceResearchPartialPDFReadSlotsAndPrivacy(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	if err := pdfsource.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	original := pdffixture.BuildWithOptions(false, "", "network protocol readable text", "")
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://example.org/book.pdf", 302)
		case "/book.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(original)
		default:
			w.WriteHeader(503)
		}
	}))
	defer page.Close()
	f.service.fetcher = research.NewFetcherWithNetwork(func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host == "private.example.org" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "协议基础 PDF"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := f.service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host := &executionHost{service: f.service, owned: *item, body: item.body, limits: f.settings.View().Limits}
	host.body.SourceResearch.Directions = []SourceResearchDirection{{ID: "direction-1", Title: "PDF", Query: "PDF"}}
	var doc research.WebDocument
	for i, url := range []string{"http://example.org/redirect", "http://example.org/failure", "http://private.example.org/private"} {
		resource, e := host.registerWebResource(ctx, url, "资料", "", "search")
		if e != nil {
			t.Fatal(e)
		}
		value, e := host.sourceResearchTool(ctx, "read_web", map[string]string{"resource_id": resource.ID})
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			doc = value.(research.WebDocument)
			if e = host.screenSourceResearch(ctx, doc.ID); e != nil {
				t.Fatal(e)
			}
		}
	}
	if doc.PDF == nil || doc.Coverage != "partial_pdf" || host.body.Web.Progress.ReadAttempted != 3 || host.body.Web.Progress.ReadFailed != 2 || host.body.Web.Progress.FetchHTTPRequests != 3 {
		t.Fatalf("PDF/redirect/failure counters: %+v %+v", doc.PDF, host.body.Web.Progress)
	}
	extra, err := host.registerWebResource(ctx, "http://example.org/extra", "超额", "", "search")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.sourceResearchTool(ctx, "read_web", map[string]string{"resource_id": extra.ID}); !errors.Is(err, ErrLimit) {
		t.Fatal("reading quota exceeded", err)
	}
	job, err = f.service.CommandSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchCommand{OperationID: uuid.NewString(), ExpectedVersion: job.Version, RoundID: job.RoundID, Kind: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: job.ResultSnapshotID, ExpectedSelectionVersion: job.SelectionVersion})
	if err != nil || len(preview.Items) != 1 || !preview.Items[0].RequiresPartialAccept {
		t.Fatalf("partial PDF lost from default selection: %+v %v", preview, err)
	}
	selected := preview.Items[0]
	command := WebImportRequest{OperationID: selected.OperationID, PreviewID: selected.WebPreviewID, ExpectedVersion: 1, RevisionID: selected.RevisionID, StorageConsent: true}
	if _, err = f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, command); !errors.Is(err, ErrConflict) {
		t.Fatal("partial PDF imported without acceptance", err)
	}
	command.AcceptPartial = true
	if _, err = f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, command); err != nil {
		t.Fatal(err)
	}
	eraseSourceResearch(t, f)
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_source_research_jobs)+(SELECT count(*) FROM learning_web_document_payloads)+(SELECT count(*) FROM learning_source_research_selections)+(SELECT count(*) FROM knowledge_source_metadata)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("privacy scrub retained PDF/selection/metadata", count, err)
	}
	if _, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID); err == nil {
		t.Fatal("old generation restored job")
	}
}
func eraseSourceResearch(t *testing.T, f *runtimeFixture) {
	t.Helper()
	ctx := context.Background()
	tutoring := tutoringdb.New(f.pool)
	store := privacydb.New(f.pool, privacydb.WithReadPermits(privacy.NewReadPermitManager()), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(knowledgedb.New(f.pool)), privacydb.WithLocalOwner(learningdb.New(f.pool, tutoring)), privacydb.WithLocalOwner(tutoring), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "智能搜集清除验收")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	barrier, err := store.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
}
func TestSourceResearchWindowRejectsBeforeModelDispatch(t *testing.T) {
	f := sourceResearchFixture(t)
	ctx := context.Background()
	limits := f.settings.View().Limits
	limits.ContextTokens = 4096
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: strings.Repeat("协议研究", 1000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil || job.Status != "failed" || f.calls.Load() != 0 {
		t.Fatalf("oversized context dispatched: %+v %v", job, err)
	}
}
