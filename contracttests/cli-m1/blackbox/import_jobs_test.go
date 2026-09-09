package blackbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBlackBoxImportJobsProcessRestartAndLostResponse(t *testing.T) {
	staging := t.TempDir()
	if err := os.Chmod(staging, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IMPORT_JOB_STAGING_DIR", staging)
	h := newHarnessWithOptions(t, harnessOptions{withoutModel: true})
	var drop atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
			return
		}
		r.Body.Close()
		upstream, e := http.NewRequestWithContext(r.Context(), r.Method, h.serverURL+r.URL.RequestURI(), bytes.NewReader(raw))
		if e != nil {
			t.Error(e)
			return
		}
		upstream.Header = r.Header.Clone()
		response, e := http.DefaultTransport.RoundTrip(upstream)
		if e != nil {
			http.Error(w, "upstream", 502)
			return
		}
		defer response.Body.Close()
		body, e := io.ReadAll(response.Body)
		if e != nil {
			t.Error(e)
			return
		}
		if strings.Contains(string(raw), `"action":"continue"`) && response.StatusCode == 200 && drop.CompareAndSwap(true, false) {
			connection, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			connection.Close()
			return
		}
		for k, v := range response.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	h.primaryHome = h.newCLIHome("import-jobs-user")
	h.pair(h.primaryHome, proxy.URL, "持久导入验收")
	run := func(args ...string) commandResult {
		t.Helper()
		r := h.runCLI(h.primaryHome, "", args...)
		requireExit(t, r, 0, "持久导入")
		if strings.Contains(string(r.stdout), "\x1b[") {
			t.Fatal("非TTY输出全屏控制序列")
		}
		return r
	}
	var space struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(run("space", "create", "--name", "恢复验收区").stdout, &space); e != nil {
		t.Fatal(e)
	}
	collection := randomUUID(t)
	run("--space", space.ID, "knowledge", "library", "create", "--id", collection, "--name", "恢复资料", "--source", "local")
	root := t.TempDir()
	for name, body := range map[string]string{"a.md": "# Alpha\nfirst persistent source", "b.txt": "second source kept verbatim"} {
		if e := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	type jobState struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		PlanVersion int64  `json:"plan_version"`
		Batches     []struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		} `json:"batches"`
	}
	jobID := randomUUID(t)
	args := func(action string, extra ...string) []string {
		return append([]string{"--space", space.ID, "knowledge", "import", "jobs", action, "--collection", collection, "--id", jobID}, extra...)
	}
	decode := func(r commandResult) jobState {
		t.Helper()
		var j jobState
		if e := json.Unmarshal(r.stdout, &j); e != nil {
			t.Fatal(e)
		}
		return j
	}
	decode(run(args("new", root)...))
	h.serverProcess.stop(t)
	h.serverProcess = startProcess(t, "edu-agentd-job-restart", serverBin, []string{"serve"}, h.serverEnv, openProcessLog(t, "edu-agentd-job-restart"))
	waitHTTPStatus(t, h.serverURL+"/livez", http.StatusOK, nil)
	run(args("resume", root)...)
	p := decode(run(args("preview")...))
	if p.Status != "ready" {
		t.Fatalf("预览未就绪: %s", p.Status)
	}
	if count := h.scalarInt("预览不发布", `SELECT count(*) FROM knowledge_revisions WHERE collection_id=$1`, collection); count != 0 {
		t.Fatal("预览已发布")
	}
	run(args("confirm", "--plan-version", strconv.FormatInt(p.PlanVersion, 10))...)
	drop.Store(true)
	lost := h.runCLI(h.primaryHome, "", args("continue")...)
	requireNonZero(t, lost, "提交响应丢失")
	j := decode(run(args("show")...))
	if j.Batches[0].Status != "completed" {
		t.Fatal("未查询到原提交事实")
	}
	h.serverProcess.stop(t)
	h.serverProcess = startProcess(t, "edu-agentd-job-confirmed-restart", serverBin, []string{"serve"}, h.serverEnv, openProcessLog(t, "edu-agentd-job-confirmed-restart"))
	waitHTTPStatus(t, h.serverURL+"/livez", http.StatusOK, nil)
	j = decode(run(args("continue")...))
	if j.Status != "completed" || len(j.Batches) != 2 {
		t.Fatalf("重启继续结果: %+v", j)
	}
	if count := h.scalarInt("每批只有一个提交", `SELECT count(*) FROM knowledge_import_operations WHERE summary->>'collection_id'=$1`, collection); count != 2 {
		t.Fatalf("重复提交: %d", count)
	}
	run(args("list")...)
	// 已批准的身份决定也要跨真实进程重启恢复，不能依赖进程内签名密钥。
	if e := os.WriteFile(filepath.Join(root, "a.md"), []byte("# Alpha\nfirst persistent source updated"), 0600); e != nil {
		t.Fatal(e)
	}
	jobID = randomUUID(t)
	run(args("new", root)...)
	p = decode(run(args("preview")...))
	if p.Status != "review" {
		t.Fatalf("改写未要求审阅: %s", p.Status)
	}
	decisionFile := filepath.Join(t.TempDir(), "decisions.json")
	type candidate struct {
		StableID   string `json:"stable_id"`
		RevisionID string `json:"revision_id"`
	}
	type reviewItem struct {
		Locator    string      `json:"locator"`
		Candidates []candidate `json:"candidates"`
	}
	for attempt := 0; p.Status == "review" && attempt < 4; attempt++ {
		var reviewed struct {
			Preview struct {
				Review struct {
					Documents []reviewItem `json:"document_reviews"`
					Nodes     []reviewItem `json:"node_reviews"`
				} `json:"identity_review"`
			} `json:"preview"`
		}
		if e := json.Unmarshal(run(args("batch", "--batch", "0")...).stdout, &reviewed); e != nil {
			t.Fatal(e)
		}
		r := reviewed.Preview.Review
		if len(r.Documents)+len(r.Nodes) == 0 {
			t.Fatal("审阅候选缺失")
		}
		documents, nodes := []map[string]any{}, []map[string]any{}
		for _, d := range r.Documents {
			if len(d.Candidates) == 0 {
				t.Fatal("文档候选缺失")
			}
			documents = append(documents, map[string]any{"locator": d.Locator, "action": "preserve", "document_id": d.Candidates[0].StableID, "reason": "明确保留原文档身份"})
		}
		for _, n := range r.Nodes {
			if len(n.Candidates) == 0 {
				t.Fatal("章节候选缺失")
			}
			nodes = append(nodes, map[string]any{"locator": n.Locator, "action": "rewrite", "source_node_revision_ids": []string{n.Candidates[0].RevisionID}, "reason": "明确更新原资料并保留身份"})
		}
		raw, _ := json.Marshal(map[string]any{"batch": 0, "document_resolutions": documents, "node_resolutions": nodes})
		if e := os.WriteFile(decisionFile, raw, 0600); e != nil {
			t.Fatal(e)
		}
		p = decode(run(args("resolve", "--request", decisionFile)...))
	}
	if p.Status != "ready" {
		t.Fatalf("身份决定后未就绪: %s", p.Status)
	}
	run(args("confirm", "--plan-version", strconv.FormatInt(p.PlanVersion, 10))...)
	h.serverProcess.stop(t)
	h.serverProcess = startProcess(t, "edu-agentd-job-review-restart", serverBin, []string{"serve"}, h.serverEnv, openProcessLog(t, "edu-agentd-job-review-restart"))
	waitHTTPStatus(t, h.serverURL+"/livez", http.StatusOK, nil)
	j = decode(run(args("continue", "--all")...))
	if j.Status != "completed" {
		t.Fatalf("重启丢失已确认审阅: %+v", j)
	}
	run("space", "archive", "--id", space.ID)
	run(args("show")...)
	run(args("cancel")...)
	if files, e := os.ReadDir(staging); e != nil || len(files) != 0 {
		t.Fatalf("完成未清理: %d %v", len(files), e)
	}
}
