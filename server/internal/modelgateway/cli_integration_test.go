//go:build linux

package modelgateway_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const cliFileContent = "GA122_LOCAL_FILE_正文只在本机和转发内存中🌍"
const cliPrompt = "GA122_PROMPT_请读取中文.txt并回答"
const cliAnswer = "GA122_ANSWER_文件已检查🌍"

type cliUpstream struct {
	calls  atomic.Int32
	closed chan struct{}
}

// Reject lost tool associations and return a result only after the real local
// read tool has supplied the file marker. No model or tool loop is replaced.
func (u *cliUpstream) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	u.calls.Add(1)
	var body struct {
		Model    string `json:"model"`
		Store    *bool  `json:"store"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		} `json:"input"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
		http.Error(w, "bad fixture input", 400)
		return
	}
	if r.Header.Get("Authorization") != "Bearer ga122-upstream-secret" || (body.Model != "actual-teaching" && body.Model != "actual-mentor") {
		t.Errorf("上游身份或实际模型未改写：model=%s", body.Model)
		http.Error(w, "bad fixture identity", 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if len(body.Messages) > 0 && strings.Contains(body.Messages[len(body.Messages)-1].Content, "GA122_BLOCK") {
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"GA122_流首帧已到达\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		u.closed <- struct{}{}
		return
	}
	toolResult := ""
	responses := r.URL.Path == "/v1/responses"
	if responses {
		if body.Store == nil || *body.Store || body.Model != "actual-teaching" {
			t.Error("Responses 必须禁用存储并使用所选 teaching")
		}
		for _, item := range body.Input {
			if item.Type == "function_call_output" {
				if item.CallID != "call-read" {
					t.Error("Responses 工具 call_id 丢失")
				}
				toolResult = item.Output
			}
		}
	} else {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("上游路径：%s", r.URL.Path)
		}
		for _, message := range body.Messages {
			if message.Role == "tool" {
				if message.ToolCallID != "call-read" {
					t.Error("Chat 工具 tool_call_id 丢失")
				}
				toolResult = message.Content
			}
		}
	}
	if !body.Stream || len(body.Tools) == 0 {
		t.Error("真实 CLI 未发出流式工具请求")
	}
	if toolResult != "" && !strings.Contains(toolResult, cliFileContent) {
		t.Errorf("本机工具未返回文件正文：%s", toolResult)
		http.Error(w, "missing local file result", 400)
		return
	}
	if responses {
		if toolResult == "" {
			for _, event := range []string{
				`{"type":"response.reasoning_summary_text.delta","delta":"准备检查文件"}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item-read","call_id":"call-read","name":"read","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"中文.txt\"}"}`,
				`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"中文.txt\"}"}`,
				`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"output_tokens_details":{"reasoning_tokens":2}}}}`,
			} {
				fmt.Fprintf(w, "data: %s\n\n", event)
			}
		} else {
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", cliAnswer)
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":6,\"total_tokens\":26,\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n")
		}
		return
	}
	if toolResult == "" {
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"准备检查文件\",\"tool_calls\":[{\"index\":0,\"id\":\"call-read\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"中文.txt\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n")
	} else {
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n", cliAnswer)
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":6,\"total_tokens\":26,\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\n")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestRealCLIModelGatewayIntegration(t *testing.T) {
	if os.Getenv("EDU_AGENT_INTEGRATION_CLI") == "" || os.Getenv("EDU_AGENT_INTEGRATION_SERVER") == "" {
		t.Skip("需要真实 CLI 和服务端二进制的环境变量")
	}
	u := &cliUpstream{closed: make(chan struct{}, 16)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { u.serve(t, w, r) }))
	t.Cleanup(upstream.Close)
	f := newCLIService(t, upstream.URL+"/v1")
	f.settings(t, "teaching", "responses", upstream.URL+"/v1", "actual-teaching", true)
	f.settings(t, "mentor", "chat_completions", upstream.URL+"/v1", "actual-mentor", true)

	for _, target := range []string{"teaching", "mentor"} {
		t.Run(target+"_local_tool_and_ledger", func(t *testing.T) {
			c := f.pair(t)
			c.selectModel(t, target, true)
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "中文.txt"), []byte(cliFileContent+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			before := u.calls.Load()
			p := c.terminal(t, "agent", "--no-save", "--workspace", root)
			p.wait(t, "Enter 发送")
			p.write(t, cliPrompt+"\r")
			p.wait(t, cliAnswer)
			p.wait(t, "已完成回答组织")
			p.write(t, "\x03")
			p.exit(t)
			if u.calls.Load()-before != 2 {
				t.Fatalf("本轮外发 %d 次，期望文字/工具两次", u.calls.Load()-before)
			}
			c.assertLedger(t, target)
		})
	}

	t.Run("reject_search_and_arbitrary_model", func(t *testing.T) {
		c := f.pair(t)
		c.selectModel(t, "mentor", true)
		before := u.calls.Load()
		for _, target := range []string{"search", "arbitrary-model", "actual-mentor"} {
			if output, err := c.command(t, "model", "select", target); err == nil || !strings.Contains(output, "model select teaching") {
				t.Fatalf("CLI 接受任意 target %s：%v %s", target, err, output)
			}
			res, raw := f.http(t, "POST", "/v1/model-gateway/chat/completions", c.token, map[string]any{"model": target, "messages": []any{map[string]string{"role": "user", "content": cliPrompt}}, "stream": false}, c.headers(t, uuid.NewString()))
			cliAssertError(t, res, raw, 403, "model_not_allowed")
		}
		if u.calls.Load() != before {
			t.Fatal("被拒绝的模型仍外发了请求")
		}
	})

	t.Run("busy_and_ctrl_c_cancel", func(t *testing.T) {
		// Observe the explicit cleanup POST through a transparent proxy. All
		// responses and admission decisions still come from the real server.
		destination, err := url.Parse(f.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(destination)
		type cancelResult struct {
			request *http.Request
			status  int
			body    string
		}
		cancels := make(chan cancelResult, 4)
		var cancelCalls atomic.Int32
		observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel") {
				cancelCalls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != "{}" {
					t.Errorf("cancel 请求体：%s %v", raw, err)
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				recorded := httptest.NewRecorder()
				proxy.ServeHTTP(recorded, r)
				for key, value := range recorded.Header() {
					w.Header()[key] = value
				}
				w.WriteHeader(recorded.Code)
				_, _ = w.Write(recorded.Body.Bytes())
				cancels <- cancelResult{r.Clone(r.Context()), recorded.Code, recorded.Body.String()}
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(observer.Close)
		c := f.pairAt(t, observer.URL)
		c.selectModel(t, "mentor", true)
		p := c.terminal(t, "agent", "--no-save", "--workspace", t.TempDir())
		p.wait(t, "Enter 发送")
		before := u.calls.Load()
		p.write(t, "GA122_BLOCK_取消测试\r")
		p.wait(t, "GA122_流首帧已到达")
		request := c.latestRequest(t)
		output, err := c.command(t, "model", "test")
		if err == nil || !strings.Contains(output, "gateway_busy") || !strings.Contains(output, "繁忙") {
			t.Fatalf("CLI 未提示设备繁忙：%v %s", err, output)
		}
		if u.calls.Load()-before != 1 {
			t.Fatal("繁忙请求仍然调用上游")
		}
		p.write(t, "\x03")
		p.exit(t)
		select {
		case result := <-cancels:
			cancel := result.request
			if result.status != 200 || cancelCalls.Load() != 1 || cancel.URL.Path != "/v1/model-gateway/requests/"+request+"/cancel" || cancel.Header.Get("X-Model-Gateway-Version") != "1" || cancel.Header.Get("X-Learning-Generation") != fmt.Sprint(c.config(t).PrivacyGeneration) {
				t.Fatalf("取消未沿用原请求身份、次数或响应有误：%d %s", result.status, result.body)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("CLI 退出前没有补发 cancel")
		}
		cliWaitClosed(t, u)
		c.assertRequestState(t, request, "cancelled")
	})

	t.Run("device_revocation_stops_stream", func(t *testing.T) {
		c := f.pair(t)
		c.selectModel(t, "mentor", true)
		p := c.terminal(t, "agent", "--no-save", "--workspace", t.TempDir())
		p.wait(t, "Enter 发送")
		before := u.calls.Load()
		p.write(t, "GA122_BLOCK_撤销测试\r")
		p.wait(t, "GA122_流首帧已到达")
		res, raw := f.http(t, "DELETE", "/v1/devices/"+c.device, c.token, nil, nil)
		if res.StatusCode != 204 {
			t.Fatalf("网页撤销设备：%d %s", res.StatusCode, raw)
		}
		p.wait(t, "device_revoked")
		p.wait(t, "重新配对")
		cliWaitClosed(t, u)
		p.write(t, "\x03")
		p.exit(t)
		output, err := c.command(t, "model", "test")
		if err == nil || !strings.Contains(output, "重新配对") || u.calls.Load()-before != 1 {
			t.Fatalf("撤销后仍可外发：%v %s", err, output)
		}
	})
	t.Run("nginx_proxy_smoke", func(t *testing.T) { cliNginxSmoke(t, f, u) })
	f.assertNoContent(t)
}

func cliWaitClosed(t *testing.T, u *cliUpstream) {
	t.Helper()
	select {
	case <-u.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("上游 HTTP context 未取消")
	}
}

func (c *realCLI) headers(t *testing.T, request string) http.Header {
	t.Helper()
	cfg := c.config(t)
	return http.Header{
		"X-Model-Gateway-Version": {"1"}, "X-Learning-Generation": {fmt.Sprint(cfg.PrivacyGeneration)},
		"X-Model-Configuration": {cfg.Models[1].ConfigurationRevision},
		"X-CLI-Session-ID":      {uuid.NewString()}, "X-CLI-Turn-ID": {uuid.NewString()},
		"X-CLI-Request-ID": {request}, "X-CLI-Model-Stage": {"foreground"},
	}
}

func (c *realCLI) latestRequest(t *testing.T) string {
	t.Helper()
	var request string
	if err := c.service.pool.QueryRow(t.Context(), `SELECT request_id::text FROM learning_cli_model_requests WHERE actor_device_id=$1 ORDER BY created_at DESC LIMIT 1`, c.device).Scan(&request); err != nil {
		t.Fatal(err)
	}
	return request
}

func (c *realCLI) assertRequestState(t *testing.T, id, want string) {
	t.Helper()
	res, raw := c.service.http(t, "GET", "/v1/model-gateway/requests/"+id, c.token, nil, c.headers(t, id))
	var state struct {
		State string `json:"state"`
	}
	cliDecode(t, raw, &state)
	if res.StatusCode != 200 || state.State != want {
		t.Fatalf("请求状态：%d %s，期望 %s", res.StatusCode, raw, want)
	}
}

func (c *realCLI) assertLedger(t *testing.T, target string) {
	t.Helper()
	pool := c.service.pool
	var task, source, device, session, turn string
	var calls, input, output, reasoning, total, received, succeeded int
	err := pool.QueryRow(t.Context(), `SELECT t.id::text,t.source,t.actor_device_id::text,tr.session_id::text,tr.turn_id::text,count(c.id),sum(c.input_tokens),sum(c.output_tokens),sum(c.reasoning_tokens),sum(c.total_tokens),count(*) FILTER(WHERE c.dispatch_state='received') FROM learning_model_tasks t JOIN learning_cli_model_turns tr ON tr.task_id=t.id JOIN learning_model_calls c ON c.task_id=t.id WHERE t.actor_device_id=$1 GROUP BY t.id,tr.session_id,tr.turn_id`, c.device).Scan(&task, &source, &device, &session, &turn, &calls, &input, &output, &reasoning, &total, &received)
	if err != nil || source != "cli" || device != c.device || calls != 2 || received != 2 || input != 30 || output != 11 || reasoning != 3 || total != 41 {
		t.Fatalf("PostgreSQL 用量归属/计数：task=%s source=%s device=%s calls=%d received=%d tokens=%d/%d/%d/%d err=%v", task, source, device, calls, received, input, output, reasoning, total, err)
	}
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM learning_cli_model_requests WHERE task_id=$1 AND state='succeeded' AND target=$2 AND dispatched`, task, target).Scan(&succeeded); err != nil || succeeded != 2 {
		t.Fatalf("两次请求未成功：%d %v", succeeded, err)
	}
	values := url.Values{"from": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}, "to": {time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}, "source": {"cli"}, "actor_device_id": {device}, "cli_session_id": {session}}
	res, raw := c.service.http(t, "GET", "/v1/learning/usage?"+values.Encode(), c.token, nil, nil)
	var summary modelusage.Summary
	cliDecode(t, raw, &summary)
	if res.StatusCode != 200 || summary.Totals.Calls != 2 || summary.Totals.Total.KnownSum != 41 || summary.Totals.Total.UnknownCalls != 0 {
		t.Fatalf("用量汇总：%d %s", res.StatusCode, raw)
	}
	res, raw = c.service.http(t, "GET", "/v1/learning/usage/tasks?"+values.Encode(), c.token, nil, nil)
	var tasks modelusage.Page[modelusage.Task]
	cliDecode(t, raw, &tasks)
	if res.StatusCode != 200 || len(tasks.Items) != 1 || tasks.Items[0].ID != task || tasks.Items[0].Source != "cli" || tasks.Items[0].ActorDeviceID == nil || *tasks.Items[0].ActorDeviceID != device || tasks.Items[0].CLITurnID == nil || *tasks.Items[0].CLITurnID != turn {
		t.Fatalf("用量任务筛选：%d %s", res.StatusCode, raw)
	}
	res, raw = c.service.http(t, "GET", "/v1/learning/usage/tasks/"+task+"/calls?"+values.Encode(), c.token, nil, nil)
	var page modelusage.Page[modelusage.Call]
	cliDecode(t, raw, &page)
	if res.StatusCode != 200 || len(page.Items) != 2 {
		t.Fatalf("调用明细：%d %s", res.StatusCode, raw)
	}
	wantStyle := "chat_completions"
	if target == "teaching" {
		wantStyle = "responses"
	}
	for _, call := range page.Items {
		if call.Model.Model != "actual-"+target || call.APIStyle != wantStyle || call.State != "received" {
			t.Fatalf("实际模型或协议归属丢失：%+v", call)
		}
	}
	for key, different := range map[string]string{"source": "web", "actor_device_id": uuid.NewString(), "cli_session_id": uuid.NewString()} {
		original := values.Get(key)
		values.Set(key, different)
		res, raw = c.service.http(t, "GET", "/v1/learning/usage/tasks?"+values.Encode(), c.token, nil, nil)
		cliDecode(t, raw, &tasks)
		if res.StatusCode != 200 || len(tasks.Items) != 0 {
			t.Fatalf("筛选 %s 未隔离本轮：%d %s", key, res.StatusCode, raw)
		}
		values.Set(key, original)
	}
	// The state endpoint uses aggregates, unlike the nullable Chat usage DTO.
	id := c.latestRequest(t)
	res, raw = c.service.http(t, "GET", "/v1/model-gateway/requests/"+id, c.token, nil, c.headers(t, id))
	var state struct {
		State string            `json:"state"`
		Usage modelusage.Totals `json:"usage"`
	}
	cliDecode(t, raw, &state)
	if res.StatusCode != 200 || state.State != "succeeded" || state.Usage.Calls != 1 || state.Usage.Total.KnownSum != 26 || state.Usage.Total.UnknownCalls != 0 || state.Usage.Reasoning.KnownSum != 1 {
		t.Fatalf("状态 usage 合同：%d %s", res.StatusCode, raw)
	}
	res, raw = c.service.http(t, "GET", "/v1/model-gateway/requests/"+uuid.NewString(), c.token, nil, c.headers(t, id))
	cliAssertError(t, res, raw, 404, "request_not_found")
}

func (f *cliService) assertNoContent(t *testing.T) {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT tablename FROM pg_tables WHERE schemaname=current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		var leaked bool
		err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{table}.Sanitize()+` r WHERE row_to_json(r)::text LIKE '%GA122_LOCAL_FILE_%' OR row_to_json(r)::text LIKE '%GA122_PROMPT_%' OR row_to_json(r)::text LIKE '%GA122_ANSWER_%')`).Scan(&leaked)
		if err != nil || leaked {
			t.Fatalf("正文进入数据库表 %s：leaked=%v err=%v", table, leaked, err)
		}
	}
	for _, marker := range []string{cliPrompt, cliFileContent, cliAnswer, "ga122-upstream-secret"} {
		if bytes.Contains([]byte(f.logs.text()), []byte(marker)) {
			t.Fatal("服务端日志包含正文或上游凭据")
		}
	}
}
