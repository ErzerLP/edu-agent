package mentorrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
)

// Keep the real scheduler, reservations, persistence and model validation. Only
// the external search/model/page endpoints are deterministic.
func researchCoverageFixture(t *testing.T, directions, pages int, results func(string) []string, serve http.HandlerFunc) *runtimeFixture {
	t.Helper()
	f := sourceResearchFixture(t)
	scale := settings.SourceResearch{MaxSourcesPerRound: pages, MaxPagesPerRound: pages}
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, SourceResearch: &scale}); err != nil {
		t.Fatal(err)
	}
	f.modelReply = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		var req modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return true
		}
		if !strings.Contains(req.Messages[0].Content, "source_research_plan") {
			raw, _ := json.Marshal(req)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			return false
		}
		plan := []map[string]string{}
		for i := range directions {
			plan = append(plan, map[string]string{"title": fmt.Sprintf("方向%d", i), "purpose": "协议基础", "query": fmt.Sprintf("direction%d", i)})
		}
		raw, _ := json.Marshal(map[string]any{"directions": plan})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: string(raw)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 40, "total_tokens": 90}})
		return true
	}
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]string{}
		for _, u := range results(r.URL.Query().Get("q")) {
			items = append(items, map[string]string{"url": u, "title": u, "description": "协议基础"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": items}})
	}))
	t.Cleanup(search.Close)
	f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		var transport http.RoundTripper = f.service.usage.SearchTransport(http.DefaultTransport, "brave", "coverage")
		if wrap != nil {
			transport = wrap(transport)
		}
		return websearch.NewBrave(search.URL, "fixture", &http.Client{Transport: transport}), "coverage", nil
	}
	page := httptest.NewServer(serve)
	t.Cleanup(page.Close)
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	return f
}

func TestSourceResearchFailedReadsReserveLaterDirections(t *testing.T) {
	reads := map[string]int{}
	f := researchCoverageFixture(t, 4, 12, func(query string) []string {
		urls := []string{}
		for i := range 10 {
			urls = append(urls, fmt.Sprintf("http://site%d.org/%s/%d", i, query, i))
		}
		return urls
	}, func(w http.ResponseWriter, r *http.Request) {
		reads[strings.Split(r.URL.Path, "/")[1]]++
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	job := completeSourceResearch(t, f)
	if job.Status != "completed" || job.Progress.ReadAttempted != 12 || job.Progress.ReadFailed != 12 {
		t.Fatalf("failed reads lost their slots: %+v", job.Progress)
	}
	for i := range 4 {
		if got := reads[fmt.Sprintf("direction%d", i)]; got != 3 {
			t.Errorf("direction %d got %d attempts, want 3 including failures", i, got)
		}
	}
}

func TestSourceResearchEmptyDirectionReturnsAllowance(t *testing.T) {
	reads := map[string]int{}
	f := researchCoverageFixture(t, 3, 6, func(query string) []string {
		if query == "direction1" {
			return nil
		}
		urls := []string{}
		for i := range 10 {
			urls = append(urls, fmt.Sprintf("http://site%d.org/%s/%d", i, query, i))
		}
		return urls
	}, func(w http.ResponseWriter, r *http.Request) {
		reads[strings.Split(r.URL.Path, "/")[1]]++
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	job := completeSourceResearch(t, f)
	if job.Progress.ReadAttempted != 6 || reads["direction0"] != 2 || reads["direction2"] != 4 {
		t.Fatalf("empty direction stranded its slots: attempts=%d per_direction=%v", job.Progress.ReadAttempted, reads)
	}
}

func TestSourceResearchRejectedDomainIsNotRetried(t *testing.T) {
	for _, status := range []int{401, 403, 429, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			denied, good := 0, 0
			f := researchCoverageFixture(t, 2, 6, func(query string) []string {
				return []string{"http://blocked.org/" + query + "/1", "http://www.blocked.org/" + query + "/2", "http://good.org/" + query}
			}, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.Host, "blocked.org") {
					denied++
					if status == 200 {
						w.Header().Set("X-Robots-Tag", "noarchive")
						w.Header().Set("Content-Type", "text/plain")
					}
					w.WriteHeader(status)
					return
				}
				good++
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, "<main>协议基础：%s</main>", r.URL.Path)
			})
			job := completeSourceResearch(t, f)
			if denied != 1 || good != 2 || job.Progress.ReadAttempted != 3 || job.CandidateCount != 2 {
				t.Fatalf("denied domain retried or alternatives missed: denied=%d good=%d progress=%+v candidates=%d", denied, good, job.Progress, job.CandidateCount)
			}
		})
	}
}

func TestSourceResearchDomainDiversity(t *testing.T) {
	reads := map[string]int{}
	order := []string{}
	f := researchCoverageFixture(t, 1, 6, func(string) []string {
		return []string{"http://same.org/1", "http://www.same.org/2", "http://docs.same.org/3", "http://same.org/4", "http://same.org/5", "http://other.org/1", "http://third.org/1", "http://fourth.org/1"}
	}, func(w http.ResponseWriter, r *http.Request) {
		reads[r.Host]++
		order = append(order, r.Host)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<main>协议基础：%s %s</main>", r.Host, r.URL.Path)
	})
	job := completeSourceResearch(t, f)
	if job.Progress.ReadAttempted != 6 || reads["same.org"]+reads["www.same.org"]+reads["docs.same.org"] > 3 || reads["other.org"] != 1 || reads["third.org"] != 1 || reads["fourth.org"] != 1 {
		t.Fatalf("one domain crowded out independent sources: %v", order)
	}
	if len(order) < 4 || order[1] != "other.org" || order[2] != "third.org" || order[3] != "fourth.org" {
		t.Fatalf("repeated domain ranked before unseen domains: %v", order)
	}
}

func TestSourceResearchRejectedRedirectDomainIsNotRetried(t *testing.T) {
	denied := 0
	f := researchCoverageFixture(t, 2, 6, func(query string) []string {
		first := "http://entry.org/redirect"
		if query == "direction1" {
			first = "http://www.blocked.org/direct"
		}
		return []string{first, "http://good.org/" + query}
	}, func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "entry.org":
			http.Redirect(w, r, "http://blocked.org/denied", http.StatusFound)
		case "blocked.org", "www.blocked.org":
			denied++
			w.WriteHeader(http.StatusForbidden)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<main>协议基础：%s</main>", r.URL.Path)
		}
	})
	job := completeSourceResearch(t, f)
	if denied != 1 || job.Progress.ReadAttempted != 3 || job.CandidateCount != 2 {
		t.Fatalf("failed final redirect host was forgotten: denied=%d reads=%d candidates=%d", denied, job.Progress.ReadAttempted, job.CandidateCount)
	}
}

func TestSourceResearchKnownDeniedDomainIsNotReachedByAnotherRedirect(t *testing.T) {
	denied := 0
	f := researchCoverageFixture(t, 2, 6, func(query string) []string {
		return []string{"http://" + query + ".org/redirect", "http://good.org/" + query}
	}, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "direction") {
			http.Redirect(w, r, "http://blocked.org/denied", http.StatusFound)
		} else if r.Host == "blocked.org" {
			denied++
			w.WriteHeader(http.StatusForbidden)
		} else {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<main>协议基础：%s</main>", r.URL.Path)
		}
	})
	job := completeSourceResearch(t, f)
	if denied != 1 || job.Progress.ReadAttempted != 4 || job.Progress.FetchHTTPRequests != 5 || job.CandidateCount != 2 {
		t.Fatalf("redirect retried a denied destination: denied=%d reads=%d HTTP=%d candidates=%d", denied, job.Progress.ReadAttempted, job.Progress.FetchHTTPRequests, job.CandidateCount)
	}
}

func TestSourceResearchDeferredQueuesStayBounded(t *testing.T) {
	f := researchCoverageFixture(t, 4, 12, func(query string) []string {
		urls := []string{}
		for i := range 10 {
			urls = append(urls, fmt.Sprintf("http://site%d.org/%s/%d", i, query, i))
		}
		return urls
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<main>协议基础：%s %s</main>", r.Host, r.URL.Path)
	})
	limits := f.settings.View().Limits
	steps := 7
	limits.MaxToolSteps = &steps
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	job := completeSourceResearch(t, f)
	if job.Status != "paused_tool_steps" || job.Progress.ReadAttempted != 5 {
		t.Fatalf("did not reach the queued-leads checkpoint: %+v", job)
	}
	ctx := context.Background()
	tx, generation, err := f.service.sourceResearchTx(ctx, f.actor, learningspace.DefaultID, job.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	item, _, _, _, _, err := f.service.sourceResearchRowTx(ctx, tx, f.actor, learningspace.DefaultID, job.ID, generation, false)
	if err != nil {
		t.Fatal(err)
	}
	state := item.body.SourceResearch
	queued := len(state.Queue)
	for _, queue := range state.DeferredQueues {
		queued += len(queue)
	}
	if queued > 14 {
		t.Fatalf("queued %d leads with seven reading attempts left; want at most 14", queued)
	}
}

func TestSourceResearchScreensRelevantBodyBeyondFirstPage(t *testing.T) {
	f := sourceResearchFixture(t)
	f.modelReply = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		var req modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return true
		}
		if !strings.Contains(req.Messages[0].Content, "source_research_screen") {
			raw, _ := json.Marshal(req)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			return false
		}
		var material struct {
			Documents []webAnswerDocument `json:"web_documents"`
		}
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &material); err != nil {
			t.Error(err)
		}
		proof := 0
		for _, d := range material.Documents {
			for _, fragment := range d.Fragments {
				for _, quote := range fragment.Quotes {
					if strings.Contains(quote.Text, "练习题：网络协议的基础") {
						proof = quote.Candidate
					}
				}
			}
		}
		raw, _ := json.Marshal(map[string]any{"include": proof > 0, "reason": "有可核对的网络协议基础练习题", "quality_notes": "只核对有界片段", "candidate": proof})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: string(raw)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 40, "total_tokens": 90}})
		return true
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<main>%s练习题：网络协议的基础，比较 TCP/IP 分层与封装。%s</main>", strings.Repeat("目录说明。", 300), r.URL.Path)
	}))
	t.Cleanup(page.Close)
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	job := completeSourceResearch(t, f)
	if job.Status != "completed" || job.CandidateCount != 2 {
		t.Fatalf("relevant exercises after the initial fragments were never screened: status=%s candidates=%d", job.Status, job.CandidateCount)
	}
}

func TestSourceResearchScreenCanExcludeMirrorsOfExistingMaterials(t *testing.T) {
	f := sourceResearchFixture(t)
	f.modelReply = func(w http.ResponseWriter, r *http.Request, _ int) bool {
		var req modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return true
		}
		if !strings.Contains(req.Messages[0].Content, "source_research_screen") {
			raw, _ := json.Marshal(req)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			return false
		}
		var input struct {
			Existing []string `json:"existing_titles"`
		}
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &input); err != nil {
			t.Error(err)
		}
		duplicate := false
		for _, title := range input.Existing {
			duplicate = duplicate || strings.Contains(title, "RFC 1122")
		}
		proof := 1
		if duplicate {
			proof = 0
		}
		raw, _ := json.Marshal(map[string]any{"include": !duplicate, "reason": "RFC 1122 官方规范正文", "quality_notes": "不将同一文献的镜像重复推荐", "candidate": proof})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: string(raw)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 40, "total_tokens": 90}})
		return true
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Different wrappers create distinct text hashes for the same work.
		fmt.Fprintf(w, "<title>RFC 1122 - Communication Layers</title><main>网络协议基础，链路、IP与传输层主机要求。镜像说明：%s</main>", r.URL.Path)
	}))
	t.Cleanup(page.Close)
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	job := completeSourceResearch(t, f)
	if job.Status != "completed" || job.CandidateCount != 1 || job.Progress.ReadSucceeded != 2 {
		t.Fatalf("screening lacked the existing work needed to exclude mirrors: candidates=%d reads=%d status=%s", job.CandidateCount, job.Progress.ReadSucceeded, job.Status)
	}
}
