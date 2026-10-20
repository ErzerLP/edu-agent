package modelgateway_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitypg "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningpg "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	memorypg "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/modelgateway"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	outboxpg "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypg "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	tutoringpg "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/testutil/dbtest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type gatewayFixture struct {
	pool          *pgxpool.Pool
	ids           *identity.Service
	usage         *modelusage.Store
	settings      *settings.Service
	service       *modelgateway.Service
	server        *httptest.Server
	credential    identity.IssuedCredential
	projection    modelgateway.Config
	session, turn string
	calls         atomic.Int32
	logs          lockedBuffer
	cancel        context.CancelFunc
}
type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedBuffer) text() string { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.String() }
func newGatewayFixture(t *testing.T, style string, handler func(http.ResponseWriter, *http.Request, int32)) *gatewayFixture {
	t.Helper()
	f := &gatewayFixture{pool: dbtest.Pool(t), session: uuid.NewString(), turn: uuid.NewString()}
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r, f.calls.Add(1)) }))
	t.Cleanup(u.Close)
	f.usage = modelusage.New(f.pool)
	var err error
	f.settings, err = settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{u.URL}, Teaching: settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: u.URL, Model: "fixture-model", AuthMode: "none", APIStyle: style}, ModelUsage: f.usage})
	if err != nil {
		t.Fatal(err)
	}
	f.ids, err = identity.NewService(identitypg.New(f.pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 3, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	f.credential = f.pair(t, identity.PairingProfileCLI)
	f.service, err = modelgateway.New(ctx, modelgateway.Options{Pool: f.pool, Settings: f.settings, Identity: f.ids, Usage: f.usage, Permits: privacy.NewReadPermitManager()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.service.Close)
	h, err := httpapi.New(httpapi.Options{ModelGateway: f.service, ModelUsage: f.usage, Identity: f.ids, Readiness: health.New(health.Options{}), Logger: slog.New(slog.NewJSONHandler(&f.logs, nil)), PairLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(h)
	t.Cleanup(f.server.Close)
	t.Cleanup(cancel)
	r := f.request(t, "GET", "/config", "", "")
	r.Header.Del("X-Learning-Generation")
	res := f.do(t, r)
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("config %d", res.StatusCode)
	}
	if err = json.NewDecoder(res.Body).Decode(&f.projection); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *gatewayFixture) pair(t *testing.T, profile identity.PairingProfile) identity.IssuedCredential {
	t.Helper()
	code, _, err := f.ids.CreatePairingCodeForProfile(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.ids.ExchangePairingCode(t.Context(), code, "生命周期测试设备")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func (f *gatewayFixture) request(t *testing.T, method, path, id, body string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+"/v1/model-gateway"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.credential.Token)
	r.Header.Set("X-Model-Gateway-Version", "1")
	r.Header.Set("X-Learning-Generation", fmt.Sprint(f.projection.PrivacyGeneration))
	if id != "" {
		r.Header.Set("X-CLI-Request-ID", id)
		r.Header.Set("X-CLI-Session-ID", f.session)
		r.Header.Set("X-CLI-Turn-ID", f.turn)
		r.Header.Set("X-CLI-Model-Stage", "foreground")
		r.Header.Set("X-Model-Configuration", f.projection.Models[0].ConfigurationRevision)
	}
	return r
}
func (f *gatewayFixture) do(t *testing.T, r *http.Request) *http.Response {
	t.Helper()
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

const streamBody = `{"model":"teaching","messages":[{"role":"user","content":"private-prompt-marker"}],"stream":true}`

func TestGatewayRevocationAndPrivacyStopOtherInstance(t *testing.T) {
	for _, operation := range []string{"revoke", "privacy", "cancel", "disconnect"} {
		t.Run(operation, func(t *testing.T) {
			closed := make(chan struct{})
			f := newGatewayFixture(t, "chat_completions", func(w http.ResponseWriter, r *http.Request, _ int32) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"private-answer-marker\"}}]}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(closed)
			})
			// Independent instance/permit manager exercises transactional NOTIFY.
			other, err := modelgateway.New(t.Context(), modelgateway.Options{Pool: f.pool, Settings: f.settings, Identity: f.ids, Usage: f.usage, Permits: privacy.NewReadPermitManager()})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			remote := httptest.NewServer(other)
			defer remote.Close()
			id := uuid.NewString()
			response := f.do(t, f.request(t, "POST", "/chat/completions", id, streamBody))
			reader := bufio.NewReader(response.Body)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(line, "private-answer-marker") {
					break
				}
			}
			busy := f.do(t, f.request(t, "POST", "/chat/completions", uuid.NewString(), streamBody))
			busy.Body.Close()
			if busy.StatusCode != 429 || busy.Header.Get("Retry-After") != "2" {
				t.Fatalf("device busy %d", busy.StatusCode)
			}
			var ref modelusage.CallRef
			if err = f.pool.QueryRow(t.Context(), `SELECT id::text,privacy_generation FROM learning_model_calls LIMIT 1`).Scan(&ref.ID, &ref.Generation); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "revoke":
				err = f.ids.RevokeDevice(t.Context(), f.credential.Device.ID)
			case "cancel":
				r := f.request(t, "POST", "/requests/"+id+"/cancel", "", "{}")
				r.URL.Scheme = "http"
				r.URL.Host = strings.TrimPrefix(remote.URL, "http://")
				res := f.do(t, r)
				res.Body.Close()
				if res.StatusCode != 200 {
					t.Fatalf("cancel %d", res.StatusCode)
				}
			case "disconnect":
				response.Body.Close()
			case "privacy":
				tutoring := tutoringpg.New(f.pool)
				store := privacypg.New(f.pool, privacypg.WithReadPermits(privacy.NewReadPermitManager()), privacypg.WithLocalOwner(identitypg.New(f.pool)), privacypg.WithLocalOwner(knowledgepg.New(f.pool)), privacypg.WithLocalOwner(learningpg.New(f.pool, tutoring)), privacypg.WithLocalOwner(tutoring), privacypg.WithLocalOwner(memorypg.New(f.pool)), privacypg.WithLocalOwner(outboxpg.New(f.pool)))
				now := time.Now()
				barrier, e := store.CommitBarrier(t.Context(), privacy.ErasureRequest{DeviceID: f.credential.Device.ID, ActorDeviceID: f.credential.Device.ID, OperationID: uuid.NewString(), ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(time.Hour), ExpectedCurrentLearnerGeneration: 1})
				if e != nil {
					t.Fatal(e)
				}
				if _, e = store.RunLocalScrub(t.Context(), barrier.ErasureID); e != nil {
					t.Fatal(e)
				}
				if e = f.usage.Finish(t.Context(), ref, "received", modelusage.ParseChatUsage([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":2}}`))); e == nil {
					t.Fatal("late usage revived")
				}
				var remaining int
				if e = f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM learning_cli_model_requests)+(SELECT count(*) FROM learning_cli_model_turns)+(SELECT count(*) FROM learning_model_execution_leases)+(SELECT count(*) FROM learning_model_calls)`).Scan(&remaining); e != nil || remaining != 0 {
					t.Fatalf("privacy residual %d %v", remaining, e)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream stayed open after cancellation")
			}
			rest, _ := io.ReadAll(reader)
			response.Body.Close()
			if strings.Contains(string(rest), "[DONE]") {
				t.Fatal("cancelled stream completed")
			}
			if f.calls.Load() != 1 {
				t.Fatal("cancellation retried upstream")
			}
			if strings.Contains(f.logs.text(), "private-") {
				t.Fatal("body entered HTTP audit log")
			}
		})
	}
}

func TestGatewayEarlyCancelScopeAndConfiguration(t *testing.T) {
	f := newGatewayFixture(t, "chat_completions", func(w http.ResponseWriter, r *http.Request, _ int32) { t.Error("unexpected paid call") })
	id := uuid.NewString()
	res := f.do(t, f.request(t, "POST", "/requests/"+id+"/cancel", "", "{}"))
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	res = f.do(t, f.request(t, "POST", "/chat/completions", id, streamBody))
	res.Body.Close()
	if res.StatusCode != 409 {
		t.Fatal(res.StatusCode)
	}
	for _, test := range []struct {
		header, value string
		status        int
	}{{"X-Model-Gateway-Version", "2", 426}, {"X-Learning-Generation", "99", 409}, {"X-Model-Configuration", "stale", 409}} {
		r := f.request(t, "POST", "/chat/completions", uuid.NewString(), streamBody)
		r.Header.Set(test.header, test.value)
		res = f.do(t, r)
		res.Body.Close()
		if res.StatusCode != test.status {
			t.Fatalf("%s: %d", test.header, res.StatusCode)
		}
	}
	user := f.pair(t, identity.PairingProfileUser)
	r := f.request(t, "POST", "/chat/completions", uuid.NewString(), streamBody)
	r.Header.Set("Authorization", "Bearer "+user.Token)
	res = f.do(t, r)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("user profile invoked")
	}
	r = f.request(t, "GET", "/config", "", "")
	r.Header.Del("Authorization")
	r.AddCookie(&http.Cookie{Name: "edu_web_dev", Value: "cookie"})
	res = f.do(t, r)
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("cookie authenticated")
	}
	v := f.settings.View()
	limits := v.Limits
	limits.Concurrency = 1
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: v.Revision, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	res = f.do(t, f.request(t, "GET", "/config", "", ""))
	var c modelgateway.Config
	_ = json.NewDecoder(res.Body).Decode(&c)
	res.Body.Close()
	if c.Available || c.Admission.CLICapacity != 0 || c.UnavailableReason == nil || *c.UnavailableReason != "gateway_capacity_disabled" {
		t.Fatalf("C=1 config %+v", c)
	}
	if f.calls.Load() != 0 {
		t.Fatal("rejected request reached upstream")
	}
}

func TestGatewayFallbackCountsEachAttemptAndInterruptedStreamNeverRetries(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			f := newGatewayFixture(t, "chat_completions", func(w http.ResponseWriter, r *http.Request, n int32) {
				_, _ = io.Copy(io.Discard, r.Body)
				if !broken && n == 1 {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"param":"stream_options","code":"unsupported_parameter","message":"private-upstream-error"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"回答\"}}]}\n\n")
				if !broken {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":11}}\n\ndata: [DONE]\n\n")
				}
			})
			id := uuid.NewString()
			res := f.do(t, f.request(t, "POST", "/chat/completions", id, streamBody))
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if broken && strings.Contains(string(raw), "[DONE]") || !broken && !strings.Contains(string(raw), "[DONE]") {
				t.Fatalf("invalid completion: %s", raw)
			}
			res = f.do(t, f.request(t, "GET", "/requests/"+id, "", ""))
			var status struct {
				State      string            `json:"state"`
				Usage      modelusage.Totals `json:"usage"`
				Dispatched bool              `json:"dispatched"`
			}
			if err := json.NewDecoder(res.Body).Decode(&status); err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			want := int64(2)
			if broken {
				want = 1
			}
			if status.Usage.Calls != want || int64(f.calls.Load()) != want || !status.Dispatched {
				t.Fatalf("attempt accounting: %+v calls=%d", status, f.calls.Load())
			}
			if !broken && (status.Usage.Input.KnownSum != 11 || status.Usage.Input.UnknownCalls != 1 || status.Usage.Output.UnknownCalls != 2) {
				t.Fatalf("nullable attempt totals %+v", status)
			}
			if strings.Contains(string(raw), "private-upstream-error") || strings.Contains(f.logs.text(), "private-upstream-error") {
				t.Fatal("upstream error leaked")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSharedChatClientConsumesBothGatewayProtocols(t *testing.T) {
	for _, style := range []string{"chat_completions", "responses"} {
		t.Run(style, func(t *testing.T) {
			f := newGatewayFixture(t, style, func(w http.ResponseWriter, r *http.Request, n int32) {
				var p map[string]any
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if style == "responses" {
					if p["store"] != false {
						t.Error("Responses stored content")
					}
					if n == 1 {
						for _, e := range []string{`{"type":"response.reasoning_summary_text.delta","delta":"检查文件"}`, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item","call_id":"call-1","name":"read_file","arguments":""}}`, `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`, `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"中文.txt\"}"}`, `{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"中文.txt\"}"}`, `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`} {
							fmt.Fprintf(w, "data: %s\n\n", e)
						}
					} else {
						fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"文件已检查🌍\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":6,\"total_tokens\":26}}}\n\n")
					}
					return
				}
				if n == 1 {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"检查文件\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"中文.txt\\\"}\"}}]}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"文件已检查🌍\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":6,\"total_tokens\":26}}\n\ndata: [DONE]\n\n")
				}
			})
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				template := f.request(t, "POST", "/chat/completions", uuid.NewString(), "")
				for k, v := range template.Header {
					r.Header[k] = v
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			client, err := modelclient.New(f.server.URL+"/v1/model-gateway", "teaching", "", time.Minute, &http.Client{Transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			request := modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "检查文件"}}, Tools: []modelclient.Tool{{Type: "function", Function: modelclient.ToolDefinition{Name: "read_file", Description: "读取", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}}}, MaxTokens: 100}
			var reasoning string
			first, err := client.Stream(t.Context(), request, func(e modelclient.StreamEvent) error {
				if e.Kind == modelclient.StreamEventReasoningDelta {
					reasoning += e.Text
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Message.ToolCalls) != 1 || first.Message.ToolCalls[0].Function.Arguments != `{"path":"中文.txt"}` || reasoning != "检查文件" {
				t.Fatalf("tool stream not consumable %+v %q", first, reasoning)
			}
			request.Messages = append(request.Messages, first.Message, modelclient.Message{Role: "tool", ToolCallID: "call-1", Content: "文件内容"})
			second, err := client.Stream(t.Context(), request, func(modelclient.StreamEvent) error { return nil })
			if err != nil || second.Message.Content != "文件已检查🌍" {
				t.Fatalf("second response %+v %v", second, err)
			}
			var tasks, calls, tokens int
			if err = f.pool.QueryRow(t.Context(), `SELECT count(DISTINCT task_id),count(*),sum(total_tokens) FROM learning_model_calls`).Scan(&tasks, &calls, &tokens); err != nil || tasks != 1 || calls != 2 || tokens != 41 {
				t.Fatalf("round accounting %d %d %d %v", tasks, calls, tokens, err)
			}
		})
	}
}

func TestGatewayResponsesLargeTerminalStillRecordsUsage(t *testing.T) {
	f := newGatewayFixture(t, "responses", func(w http.ResponseWriter, r *http.Request, _ int32) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 2; i++ {
			event, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": strings.Repeat("a", 40<<10)})
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": strings.Repeat("a", 80<<10)}}}}, "usage": map[string]int{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}}})
		fmt.Fprintf(w, "data: %s\n\n", terminal)
	})
	res := f.do(t, f.request(t, "POST", "/chat/completions", uuid.NewString(), streamBody))
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(raw), "[DONE]") {
		t.Fatal("valid response failed")
	}
	var total *int64
	if err := f.pool.QueryRow(t.Context(), `SELECT total_tokens FROM learning_model_calls LIMIT 1`).Scan(&total); err != nil || total == nil || *total != 18 {
		t.Fatalf("large terminal lost real usage %v %v", total, err)
	}
}

func TestGatewayTruncatedResponseIsNotSuccessfulRequest(t *testing.T) {
	f := newGatewayFixture(t, "responses", func(w http.ResponseWriter, r *http.Request, _ int32) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"部分回答"}]}]}`)
	})
	id := uuid.NewString()
	res := f.do(t, f.request(t, "POST", "/chat/completions", id, strings.Replace(streamBody, `"stream":true`, `"stream":false`, 1)))
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(raw), `"finish_reason":"length"`) {
		t.Fatalf("lost truncation: %d %s", res.StatusCode, raw)
	}
	res = f.do(t, f.request(t, "GET", "/requests/"+id, "", ""))
	var value struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(res.Body).Decode(&value)
	res.Body.Close()
	if value.State != "failed" {
		t.Fatalf("truncated request marked %s", value.State)
	}
}

func TestGatewayRecoveryDoesNotReplayAndPreservesUnknownAttempts(t *testing.T) {
	f := newGatewayFixture(t, "chat_completions", func(w http.ResponseWriter, r *http.Request, _ int32) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	id := uuid.NewString()
	res := f.do(t, f.request(t, "POST", "/chat/completions", id, strings.Replace(streamBody, `"stream":true`, `"stream":false`, 1)))
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	var origin, task string
	if err := f.pool.QueryRow(t.Context(), `SELECT origin_id::text,id::text FROM learning_model_tasks`).Scan(&origin, &task); err != nil {
		t.Fatal(err)
	}
	lease := uuid.NewString()
	past := time.Now().Add(-time.Minute)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	ref, err := f.usage.BeginTx(t.Context(), tx, modelusage.BeginRequest{Generation: 1, Source: "cli", ActorDeviceID: f.credential.Device.ID, Kind: "cli_gateway", OriginID: origin, Stage: "foreground", AttemptKey: uuid.NewString(), LeaseID: lease, LeaseUntil: &past, Provider: "fixture", Model: "fixture-model", APIStyle: "chat_completions"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `UPDATE learning_cli_model_requests SET state='running',finished_at=NULL,lease_id=$1,lease_until=$2 WHERE request_id=$3`, lease, past, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	var callState, state string
	if err = f.pool.QueryRow(t.Context(), `SELECT dispatch_state FROM learning_model_calls WHERE id=$1`, ref.ID).Scan(&callState); err != nil {
		t.Fatal(err)
	}
	if callState != "not_dispatched" {
		t.Fatalf("prepared recovery %s", callState)
	}
	if _, err = f.pool.Exec(t.Context(), `UPDATE learning_model_calls SET dispatch_state='dispatching',finished_at=NULL,started_at=clock_timestamp(),lease_until=$2 WHERE id=$1`, ref.ID, past); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT dispatch_state FROM learning_model_calls WHERE id=$1`, ref.ID).Scan(&callState); err != nil {
		t.Fatal(err)
	}
	if callState != "outcome_unknown" {
		t.Fatalf("dispatched recovery %s", callState)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT state FROM learning_cli_model_requests WHERE request_id=$1`, id).Scan(&state); err != nil || state != "outcome_unknown" {
		t.Fatalf("request recovery %s %v", state, err)
	}
	res = f.do(t, f.request(t, "POST", "/chat/completions", id, streamBody))
	res.Body.Close()
	if res.StatusCode != 409 || f.calls.Load() != 1 {
		t.Fatal("recovered request replayed")
	}
}

func TestAuthorizedPrivacyBarrierUsesSameGateOrderAsGateway(t *testing.T) {
	f := newGatewayFixture(t, "chat_completions", func(http.ResponseWriter, *http.Request, int32) { t.Error("unexpected upstream") })
	tutoring := tutoringpg.New(f.pool)
	store := privacypg.New(f.pool, privacypg.WithReadPermits(privacy.NewReadPermitManager()), privacypg.WithLocalOwner(identitypg.New(f.pool)), privacypg.WithLocalOwner(knowledgepg.New(f.pool)), privacypg.WithLocalOwner(learningpg.New(f.pool, tutoring)), privacypg.WithLocalOwner(tutoring), privacypg.WithLocalOwner(memorypg.New(f.pool)), privacypg.WithLocalOwner(outboxpg.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacypg.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(t.Context(), f.credential.Device.ID, "网关清除门禁测试")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	reader, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(context.Background())
	var pid int
	var g int64
	if err = reader.QueryRow(ctx, `SELECT pg_backend_pid(),privacy_lock_owner_gate('identity','read',NULL)`).Scan(&pid, &g); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		now := time.Now()
		_, e := store.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.credential.Device.ID, ActorDeviceID: f.credential.Device.ID, OperationID: uuid.NewString(), ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.credential.Device.ID, grant.Token))
		done <- e
	}()
	for {
		var blocked bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("barrier did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	var device string
	readErr := reader.QueryRow(ctx, `SELECT id::text FROM devices WHERE id=$1 FOR SHARE`, f.credential.Device.ID).Scan(&device)
	_ = reader.Rollback(context.Background())
	barrierErr := <-done
	if readErr != nil || barrierErr != nil {
		t.Fatalf("privacy/device lock inversion: read=%v barrier=%v", readErr, barrierErr)
	}
}

func TestSynchronousTeachingUsesSamePoolAndReservedSlot(t *testing.T) {
	syncStarted := make(chan struct{})
	f := newGatewayFixture(t, "chat_completions", func(w http.ResponseWriter, r *http.Request, _ int32) {
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"开始\"}}]}\n\n")
			w.(http.Flusher).Flush()
		} else {
			close(syncStarted)
		}
		<-r.Context().Done()
	})
	v := f.settings.View()
	limits := v.Limits
	limits.Concurrency = 2
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: v.Revision, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	res := f.do(t, f.request(t, "GET", "/config", "", ""))
	_ = json.NewDecoder(res.Body).Decode(&f.projection)
	res.Body.Close()
	res = f.do(t, f.request(t, "POST", "/chat/completions", uuid.NewString(), streamBody))
	defer res.Body.Close()
	client, err := f.settings.TeachingClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Chat(modelusage.WithTask(ctx, modelusage.TaskContext{Kind: "teaching", OriginID: uuid.NewString(), Stage: "route"}), llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "同步模型"}}})
		done <- err
	}()
	select {
	case <-syncStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("reserved slot unavailable to synchronous model")
	}
	_, err = client.Chat(modelusage.WithTask(t.Context(), modelusage.TaskContext{Kind: "teaching", OriginID: uuid.NewString(), Stage: "route"}), llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "超额"}}})
	if err == nil || f.calls.Load() != 2 {
		t.Fatal("synchronous exit bypassed shared pool")
	}
	cancel()
	<-done
}

func TestStaleNotificationDoesNotCancelCurrentGeneration(t *testing.T) {
	closed := make(chan struct{})
	f := newGatewayFixture(t, "chat_completions", func(w http.ResponseWriter, r *http.Request, _ int32) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"回答中\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	})
	id := uuid.NewString()
	response := f.do(t, f.request(t, "POST", "/chat/completions", id, streamBody))
	defer response.Body.Close()
	var schema string
	if err := f.pool.QueryRow(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []map[string]any{{"schema": schema, "before_generation": 1, "reason": "privacy_generation_changed"}, {"schema": schema, "generation": 2, "device": f.credential.Device.ID, "request": id, "reason": "request_cancelled"}} {
		raw, _ := json.Marshal(payload)
		if _, err := f.pool.Exec(t.Context(), `SELECT pg_notify('model_gateway_cancel',$1)`, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-closed:
		t.Fatal("notification crossed generation")
	case <-time.After(200 * time.Millisecond):
	}
	res := f.do(t, f.request(t, "POST", "/requests/"+id+"/cancel", "", "{}"))
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("current cancellation was not delivered")
	}
}

func TestGatewayAuditDoesNotCaptureInvalidPathContent(t *testing.T) {
	f := newGatewayFixture(t, "chat_completions", func(http.ResponseWriter, *http.Request, int32) { t.Error("unexpected upstream") })
	res := f.do(t, f.request(t, "GET", "/requests/private-path-marker", "", ""))
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal(res.StatusCode)
	}
	if strings.Contains(f.logs.text(), "private-path-marker") {
		t.Fatal("invalid gateway path leaked into audit log")
	}
}
