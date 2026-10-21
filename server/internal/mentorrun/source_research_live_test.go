package mentorrun

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Explicit live acceptance only. The local relay enforces the user's hard
// request cap while forwarding every allowed request to the real model.
func TestSourceResearchLiveSmall(t *testing.T) {
	if os.Getenv("SOURCE_RESEARCH_LIVE") != "1" {
		t.Skip("explicit real model and Brave acceptance opt-in required")
	}
	target, err := url.Parse(os.Getenv("EDU_ACC_MODEL_BASE_URL"))
	if err != nil || target.Host == "" || os.Getenv("EDU_ACC_MODEL_API_KEY") == "" || os.Getenv("EDU_ACC_BRAVE_API_KEY") == "" {
		t.Fatal("acceptance environment missing")
	}
	var modelCalls, searchCalls atomic.Int32
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1")
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.Host = target.Host
		r.URL.Path = strings.TrimRight(target.Path, "/") + path
		r.URL.RawPath = ""
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "upstream unavailable", 502) }
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if modelCalls.Add(1) > 16 {
			http.Error(w, "acceptance request cap", 429)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	view := f.settings.View()
	connection := view.EffectiveMentor.Connection
	connection.Model = os.Getenv("EDU_ACC_MODEL_ID")
	if connection.Model == "" {
		connection.Model = "deepseek-v4-flash"
	}
	connection.APIStyle = "responses"
	connection.AuthMode = "bearer"
	limits := settings.DefaultLimits()
	limits.OutputTokens = 2048
	limits.IdleTimeoutSeconds = 180
	config, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "live", "settings.json"), ModelEndpoints: []string{connection.Endpoint}, ModelUsage: f.service.usage})
	if err != nil {
		t.Fatal("live settings unavailable")
	}
	key := os.Getenv("EDU_ACC_MODEL_API_KEY")
	useTeaching := true
	scale := settings.SourceResearch{MaxSourcesPerRound: 3, MaxPagesPerRound: 8}
	if _, err = config.Update(settings.Update{ExpectedRevision: 0, Target: settings.Teaching, MentorUsesTeaching: &useTeaching, Connection: &connection, NewKey: &key, Limits: &limits, SourceResearch: &scale}); err != nil {
		t.Fatal("model configuration failed")
	}
	brave := os.Getenv("EDU_ACC_BRAVE_API_KEY")
	if _, err = config.Update(settings.Update{ExpectedRevision: config.View().Revision, Target: settings.Search, Connection: &settings.Connection{Enabled: true, Provider: "brave", Endpoint: "https://api.search.brave.com/res/v1/web/search", AuthMode: "bearer"}, NewKey: &brave}); err != nil {
		t.Fatal("search configuration failed")
	}
	f.settings = config
	f.service.settings = config
	f.service.fetcher = research.NewFetcher()
	f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		return config.SearchClient(func(next http.RoundTripper) http.RoundTripper {
			guard := sourceResearchLiveTransport{next: next, calls: &searchCalls}
			if wrap != nil {
				return wrap(guard)
			}
			return guard
		})
	}
	f.actor.Scopes = append(f.actor.Scopes, "knowledge:read", "knowledge:write", "research:adopt")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if _, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=$2 WHERE id=$1`, f.actor.TokenID, f.actor.Scopes); err != nil {
		t.Fatal(err)
	}
	job, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "找适合初学者学习 Go context 取消和超时的官方资料，覆盖概念、实践示例和 API 参考。优先 go.dev 与 pkg.go.dev，只需要三份有用资料。"})
	if err != nil {
		t.Fatal("live job creation failed")
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal("live worker failed")
	}
	job, err = f.service.SourceResearchJob(ctx, f.actor, learningspace.DefaultID, job.ID)
	if err != nil {
		t.Fatal("live result unavailable")
	}
	rows, err := f.pool.Query(ctx, `SELECT stage,array_agg(output_tokens ORDER BY attempt_no),count(*) FILTER(WHERE output_tokens IS NULL) FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.origin_id=$1 GROUP BY stage ORDER BY stage`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var stage string
		var outputs []*int64
		var unknown int
		if err = rows.Scan(&stage, &outputs, &unknown); err != nil {
			t.Fatal(err)
		}
		values := []int64{}
		for _, n := range outputs {
			if n != nil {
				values = append(values, *n)
			}
		}
		t.Logf("stage=%s output_tokens=%v unknown=%d", stage, values, unknown)
	}
	rows.Close()
	t.Logf("live status=%s model_calls=%d search_calls=%d read_attempted=%d read_succeeded=%d candidates=%d", job.Status, modelCalls.Load(), searchCalls.Load(), job.Progress.ReadAttempted, job.Progress.ReadSucceeded, job.CandidateCount)
	if job.Status != "completed" || job.CandidateCount < 2 || job.ReportRevisionID == "" || modelCalls.Load() > 25 || searchCalls.Load() > 15 {
		t.Fatal("live acceptance did not complete within the request cap")
	}
	report, err := f.service.SourceResearchReport(ctx, f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil || len(report.Sections) == 0 {
		t.Fatal("live cited report missing")
	}
	for _, section := range report.Sections {
		t.Logf("report_section=%s citations=%d body=%s", section.Title, len(section.Citations), section.Body)
	}
}

type sourceResearchLiveTransport struct {
	next  http.RoundTripper
	calls *atomic.Int32
}

func (t sourceResearchLiveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.calls.Add(1) > 13 {
		return nil, ErrLimit
	}
	return t.next.RoundTrip(r)
}
