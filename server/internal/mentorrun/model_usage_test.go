package mentorrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
)

type usageResponseTransport struct {
	wire, contentType string
}

func (t usageResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{t.contentType}},
		Body:       io.NopCloser(strings.NewReader(t.wire)),
	}, nil
}

func TestMentorResponsesUsageAfterRunRecovery(t *testing.T) {
	const response = `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}],"usage":{"input_tokens":11,"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":18}}`
	for _, tc := range []struct {
		name, contentType, wire, state string
		invalid                        bool
	}{
		{"json", "application/json", response, "received", false},
		{"stream", "text/event-stream", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n", "received", false},
		{"interrupted_stream", "text/event-stream", "data: {\"type\":\"response.in_progress\",\"response\":" + response + "}\n\n", "outcome_unknown", false},
		{"interrupted_json", "application/json", strings.TrimSuffix(response, "}"), "outcome_unknown", true},
		{"incomplete_stream", "text/event-stream", "data: {\"type\":\"response.incomplete\",\"response\":" + strings.Replace(response, "completed", "incomplete", 1) + "}\n\n", "received", false},
		{"failed_stream", "text/event-stream", "data: {\"type\":\"response.failed\",\"response\":" + strings.Replace(response, "completed", "failed", 1) + "}\n\n", "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t, func(http.ResponseWriter, *http.Request, int) { t.Error("unexpected HTTP dispatch") })
			receipt := f.accept(t)
			ctx := context.Background()
			owned, err := f.service.claim(ctx)
			if err != nil || owned == nil {
				t.Fatalf("claim: %v", err)
			}
			h := &executionHost{service: f.service, owned: *owned, body: owned.body}
			connection := f.settings.View().EffectiveMentor.Connection
			connection.APIStyle = "responses"
			transport := budgetTransport{host: h, connection: connection, stage: "study_answer", next: usageResponseTransport{wire: tc.wire, contentType: tc.contentType}}
			var pendingBody io.ReadCloser
			if tc.contentType == "application/json" {
				endpoint, err := url.Parse(connection.Endpoint)
				if err != nil {
					t.Fatal(err)
				}
				client, err := llm.New(llm.Options{BaseURL: endpoint, WireAPI: llm.Responses, Model: connection.Model, AllowNoKey: true, ContextWindow: 32768, MinimumContext: 1, Timeout: time.Second, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "test"}}})
				if (err != nil) != tc.invalid {
					t.Fatalf("Chat: %v", err)
				}
			} else {
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, connection.Endpoint+"/responses", strings.NewReader(`{}`))
				response, err := transport.RoundTrip(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				pendingBody = response.Body
				// Consume the protocol event without reading EOF or closing the body.
				if _, err = io.ReadFull(response.Body, make([]byte, len(tc.wire))); err != nil {
					t.Fatal(err)
				}
				if tc.state == "outcome_unknown" {
					if err = response.Body.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err = h.Complete(ctx, modelclient.Message{Role: "assistant", Content: "done"}); err != nil {
				t.Fatal(err)
			}
			if snapshot := f.snapshot(t, receipt.RunID); snapshot.Status != "succeeded" {
				t.Fatalf("run did not finish: %+v", snapshot)
			}
			// A later worker claim runs RecoverTx after the run released its lease.
			if next, err := f.service.claim(ctx); err != nil || next != nil {
				t.Fatalf("next claim: %+v %v", next, err)
			}
			assertUsage := func() {
				t.Helper()
				var state string
				var calls, input, output, reasoning, total int64
				err := f.pool.QueryRow(ctx, `SELECT min(dispatch_state),count(*),sum(input_tokens),sum(output_tokens),sum(reasoning_tokens),sum(total_tokens) FROM learning_model_calls`).Scan(&state, &calls, &input, &output, &reasoning, &total)
				if err != nil || state != tc.state || calls != 1 || input != 11 || output != 7 || reasoning != 3 || total != 18 {
					t.Fatalf("settlement after recovery: state=%s calls=%d tokens=%d/%d/%d/%d err=%v", state, calls, input, output, reasoning, total, err)
				}
			}
			assertUsage()
			if pendingBody != nil {
				for range 2 {
					if err = pendingBody.Close(); err != nil {
						t.Fatal(err)
					}
				}
				assertUsage()
			}
		})
	}
}

func assertTaskUsage(t *testing.T, f *runtimeFixture, origin, kind, stage string, count int) {
	t.Helper()
	var calls int
	var gotKind, gotStage, source, device, space string
	err := f.pool.QueryRow(context.Background(), `SELECT count(*),min(t.kind),min(c.stage),min(t.source),min(t.actor_device_id::text),min(t.space_id::text)
	 FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.origin_id=$1 AND c.started_at IS NOT NULL`, origin).Scan(&calls, &gotKind, &gotStage, &source, &device, &space)
	if err != nil || calls != count || gotKind != kind || gotStage != stage || source != "cli" || device != f.actor.Device.ID || space != learningspace.DefaultID {
		t.Fatalf("任务用量归属错误: calls=%d kind=%s stage=%s source=%s device=%s space=%s err=%v", calls, gotKind, gotStage, source, device, space, err)
	}
}

func TestMentorModelUsageHTTP(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"回答\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"completion_tokens_details\":{\"reasoning_tokens\":3},\"total_tokens\":18}}\n\ndata: [DONE]\n\n")
	})
	r := f.accept(t)
	f.work(t)
	var input, output, reasoning, total, count int64
	err := f.pool.QueryRow(context.Background(), `SELECT count(*),sum(input_tokens),sum(output_tokens),sum(reasoning_tokens),sum(total_tokens) FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.origin_id=$1 AND c.dispatch_state='received'`, r.RunID).Scan(&count, &input, &output, &reasoning, &total)
	if err != nil || count != 1 || input != 11 || output != 7 || reasoning != 3 || total != 18 {
		t.Fatalf("真实导师出口未记录一次真实用量: %d %d %d %d %d %v", count, input, output, reasoning, total, err)
	}
	ctx := context.Background()
	var ref modelusage.CallRef
	if err = f.pool.QueryRow(ctx, `SELECT id::text,privacy_generation FROM learning_model_calls`).Scan(&ref.ID, &ref.Generation); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = f.service.usage.Finish(ctx, ref, "received", modelusage.ParseChatUsage([]byte(`{"usage":{"prompt_tokens":90,"completion_tokens":90}}`))); err != nil {
			t.Fatal(err)
		}
	}
	q := modelusage.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Timezone: "Asia/Shanghai", Grain: "day", Limit: 20}
	for _, scope := range []string{"", learningspace.DefaultID} {
		q.SpaceID = scope
		summary, e := f.service.usage.Summary(ctx, f.actor, q)
		if e != nil || summary.Totals.Calls != 1 || summary.Totals.Total.KnownSum != 18 || summary.Totals.Total.UnknownCalls != 0 || len(summary.Buckets) != 1 {
			t.Fatalf("重复回调/聚合有误: %+v %v", summary, e)
		}
		page, e := f.service.usage.Tasks(ctx, f.actor, q)
		if e != nil || len(page.Items) != 1 || page.Items[0].Totals.Total.KnownSum != 18 {
			t.Fatalf("任务累计有误: %+v %v", page, e)
		}
		calls, e := f.service.usage.Calls(ctx, f.actor, q, page.Items[0].ID)
		if e != nil || len(calls.Items) != 1 || calls.Items[0].Input == nil || *calls.Items[0].Input != 11 {
			t.Fatalf("明细有误: %+v %v", calls, e)
		}
	}
}

func TestMentorMissingUsageStaysUnknown(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "未报告用量"})
	})
	f.accept(t)
	f.work(t)
	var state string
	var input, output, reasoning, total *int64
	err := f.pool.QueryRow(context.Background(), `SELECT dispatch_state,input_tokens,output_tokens,reasoning_tokens,total_tokens FROM learning_model_calls`).Scan(&state, &input, &output, &reasoning, &total)
	if err != nil || state != "received" || input != nil || output != nil || reasoning != nil || total != nil {
		t.Fatalf("缺用量被当作零: %s %v", state, err)
	}
}

func TestMentorTimeoutPreservesAttempt(t *testing.T) {
	started := make(chan struct{})
	lifetime, stop := context.WithCancel(context.Background())
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-lifetime.Done():
		}
	})
	t.Cleanup(stop)
	f.accept(t)
	// The shared client's idle timer uses CancelCause(DeadlineExceeded).
	ctx, cancel := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(300*time.Millisecond, func() { cancel(context.DeadlineExceeded) })
	defer timer.Stop()
	defer cancel(context.Canceled)
	if _, err := f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	default:
		t.Fatal("未执行真实 HTTP")
	}
	var state string
	var count int
	err := f.pool.QueryRow(context.Background(), `SELECT min(dispatch_state),count(*) FROM learning_model_calls WHERE input_tokens IS NULL AND output_tokens IS NULL`).Scan(&state, &count)
	if err != nil || state != "outcome_unknown" || count != 1 {
		t.Fatalf("超时尝试丢失: %s %d %v", state, count, err)
	}
}

func TestMentorRestartKeepsUnknownWithoutRedispatch(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "未完成结算"})
	})
	r := f.accept(t)
	ctx := context.Background()
	owned, err := f.service.claim(ctx)
	if err != nil || owned == nil {
		t.Fatalf("claim: %v", err)
	}
	h := &executionHost{service: f.service, owned: *owned, body: owned.body, reserve: 100}
	connection := f.settings.View().EffectiveMentor.Connection
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, connection.Endpoint+"/chat/completions", bytes.NewBufferString(`{}`))
	response, err := (budgetTransport{host: h, next: http.DefaultTransport, connection: connection}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, r.RunID); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = f.pool.QueryRow(ctx, `SELECT dispatch_state FROM learning_model_calls`).Scan(&state); err != nil || state != "outcome_unknown" || f.calls.Load() != 1 {
		t.Fatalf("重启重复外发或丢失未知记录: %s %v", state, err)
	}
}
