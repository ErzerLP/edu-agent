package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/tutormodel"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/llmselector"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/transport/access"
	"github.com/google/uuid"
)

func TestServerTeachingKnowledgeAndProbeUsage(t *testing.T) {
	for _, style := range []string{"chat_completions", "responses"} {
		t.Run(style, func(t *testing.T) {
			pool := appIntegrationPool(t)
			usage := modelusage.New(pool)
			ctx := context.Background()
			actor := identity.Credential{Device: identity.Device{ID: uuid.NewString()}, TokenID: uuid.NewString(), Scopes: []string{"learning:read", "learning:write", "knowledge:read"}}
			hash := sha256.Sum256([]byte(actor.TokenID))
			if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'用量验收',now())`, actor.Device.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at) VALUES($1,$2,$3,$4,now())`, actor.TokenID, actor.Device.ID, hash[:], actor.Scopes); err != nil {
				t.Fatal(err)
			}
			ctx = access.WithCredential(ctx, actor)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
					Input []struct {
						Content string `json:"content"`
					} `json:"input"`
					Format struct {
						Type   string `json:"type"`
						Schema struct {
							Name string `json:"name"`
						} `json:"json_schema"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				messages := payload.Messages
				if style == "responses" {
					messages = payload.Input
				}
				content := `{"route":[]}`
				if len(messages) == 2 {
					var input map[string]any
					_ = json.Unmarshal([]byte(messages[1].Content), &input)
					data, _ := json.Marshal(map[string]any{"knowledge_revision_id": input["knowledge_revision_id"], "candidate_set_hash": input["candidate_set_hash"], "decisions": []any{}})
					content = string(data)
				} else if messages[len(messages)-1].Content == "Confirm the core profile." {
					if payload.Format.Type == "json_schema" {
						w.WriteHeader(400)
						fmt.Fprint(w, `{}`)
						return
					}
					content = `{"capability_probe":true}`
				}
				w.Header().Set("Content-Type", "application/json")
				if style == "responses" {
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": content}}}}, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18, "output_tokens_details": map[string]int{"reasoning_tokens": 3}}})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18, "completion_tokens_details": map[string]int{"reasoning_tokens": 3}}})
				}
			}))
			t.Cleanup(provider.Close)
			s, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{provider.URL}, Teaching: settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: provider.URL, Model: "usage-fixture", AuthMode: "none", APIStyle: style}, ModelUsage: usage})
			if err != nil {
				t.Fatal(err)
			}
			client, err := s.TeachingClient()
			if err != nil {
				t.Fatal(err)
			}
			request := learning.ProposalRequest{RequestID: uuid.NewString(), Type: learning.ProposalRoute}
			if _, err = tutormodel.New(client).Generate(ctx, request); err != nil {
				t.Fatal(err)
			}
			var kind, stage, source, device, space, firstCall string
			var calls, tokens int
			if err = pool.QueryRow(ctx, `SELECT t.kind,c.stage,t.source,t.actor_device_id::text,t.space_id::text,c.total_tokens,c.id::text FROM learning_model_tasks t JOIN learning_model_calls c ON c.task_id=t.id WHERE t.kind='teaching' AND t.actor_device_id=$1`, actor.Device.ID).Scan(&kind, &stage, &source, &device, &space, &tokens, &firstCall); err != nil || kind != "teaching" || stage != "route" || source != "cli" || device != actor.Device.ID || space != learningspace.DefaultID || tokens != 18 {
				t.Fatalf("教学归属错误: %s %s %s %s %s %d %v", kind, stage, source, device, space, tokens, err)
			}
			// Proposal/operation IDs are device-scoped. Retrying one proposal must
			// retain its task, while another device reusing its ID gets a new task.
			other := identity.Credential{Device: identity.Device{ID: uuid.NewString()}, TokenID: uuid.NewString(), Scopes: actor.Scopes}
			otherHash := sha256.Sum256([]byte(other.TokenID))
			if _, err = pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'第二台设备',now())`, other.Device.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at) VALUES($1,$2,$3,$4,now())`, other.TokenID, other.Device.ID, otherHash[:], other.Scopes); err != nil {
				t.Fatal(err)
			}
			for _, retryContext := range []context.Context{ctx, access.WithCredential(context.Background(), other)} {
				if _, err = tutormodel.New(client).Generate(retryContext, request); err != nil {
					t.Fatal(err)
				}
			}
			if err = pool.QueryRow(ctx, `SELECT count(DISTINCT t.id),count(*) FROM learning_model_tasks t JOIN learning_model_calls c ON t.id=c.task_id WHERE t.kind='teaching'`).Scan(&tokens, &calls); err != nil || tokens != 2 || calls != 3 {
				t.Fatalf("设备间提案归属合并或重试拆成新任务: tasks=%d calls=%d %v", tokens, calls, err)
			}
			ks, err := knowledge.NewService(knowledgepg.New(pool), knowledge.NewCanonicalizer(), knowledge.ServiceOptions{Selector: llmselector.New(client)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ks.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: actor.Device.ID, Source: "usage-test", Documents: []knowledge.ImportDocument{{Path: "alpha.md", Markdown: "# Alpha\n\n## Beta\n\nAlpha beta material.\n"}}}); err != nil {
				t.Fatal(err)
			}
			if _, err = ks.Retrieve(ctx, knowledge.RetrievalCommand{Query: "Alpha"}); err != nil {
				t.Fatal(err)
			}
			if err = pool.QueryRow(ctx, `SELECT count(*) FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.kind='knowledge' AND c.stage='directory_selection' AND t.source='cli' AND t.actor_device_id=$1 AND t.space_id=$2 AND c.total_tokens=18`, actor.Device.ID, learningspace.DefaultID).Scan(&calls); err != nil || calls != 1 {
				t.Fatalf("目录选择未记录: %d %v", calls, err)
			}
			view, err := s.Probe(ctx, settings.Teaching, s.View().Revision, true)
			if err != nil || view.Teaching.Status != "ready" {
				t.Fatalf("probe: %+v %v", view.Teaching, err)
			}
			wantProbe := 1
			if style == "chat_completions" {
				wantProbe = 2
			}
			if err = pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT t.id) FROM learning_model_tasks t JOIN learning_model_calls c ON c.task_id=t.id WHERE t.kind='model_probe' AND c.stage='teaching' AND t.space_id IS NULL AND t.source='cli' AND t.actor_device_id=$1 AND c.started_at IS NOT NULL`, actor.Device.ID).Scan(&calls, &tokens); err != nil || calls != wantProbe || tokens != 1 {
				t.Fatalf("探测/协议回退未按同一全局根任务计数: calls=%d tasks=%d %v", calls, tokens, err)
			}
			if err = usage.EnableFullCoverage(ctx); err != nil {
				t.Fatal(err)
			}
			q := modelusage.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Timezone: "UTC", Grain: "day", Limit: 20}
			summary, err := usage.Summary(ctx, actor, q)
			if err != nil || summary.Coverage != "server" || summary.FullCoverageSince == nil || summary.Totals.Calls != int64(4+wantProbe) {
				t.Fatalf("全覆盖累计错误: %+v %v", summary, err)
			}
			if err = modelusage.New(pool).EnableFullCoverage(ctx); err != nil {
				t.Fatal(err)
			}
			again, err := usage.Summary(ctx, actor, q)
			if err != nil || !again.FullCoverageSince.Equal(*summary.FullCoverageSince) {
				t.Fatal("重启改写全覆盖起点", err)
			}
			// Simulate a crash after dispatch, before its receipt settled. Recovery
			// must wait for the original HTTP deadline, even if its server is alive.
			if _, err = pool.Exec(ctx, `UPDATE learning_model_calls SET dispatch_state='dispatching',finished_at=NULL WHERE id=$1`, firstCall); err != nil {
				t.Fatal(err)
			}
			recoverCalls := func() {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if err = usage.RecoverTx(ctx, tx, summary.Generation); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			recoverCalls()
			var state string
			if err = pool.QueryRow(ctx, `SELECT dispatch_state FROM learning_model_calls WHERE id=$1`, firstCall).Scan(&state); err != nil || state != "dispatching" {
				t.Fatalf("活动进程的调用被恢复抢占: %s %v", state, err)
			}
			if _, err = pool.Exec(ctx, `UPDATE learning_model_calls SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, firstCall); err != nil {
				t.Fatal(err)
			}
			recoverCalls()
			recovered, err := usage.Summary(ctx, actor, q)
			if err != nil || recovered.Totals.Calls != int64(4+wantProbe) || recovered.Totals.OutcomeUnknownCalls != 1 {
				t.Fatalf("同步出口崩溃后丢失/重复尝试: %+v %v", recovered, err)
			}
		})
	}
}
