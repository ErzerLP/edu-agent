package modelgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitypg "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/modelgateway"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/testutil/dbtest"
	"github.com/google/uuid"
)

func TestGatewayHTTPAccountingDeduplicationAndMetadata(t *testing.T) {
	for _, style := range []string{"chat_completions", "responses"} {
		t.Run(style, func(t *testing.T) {
			pool := dbtest.Pool(t)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != "actual-model" {
					t.Error("target was not resolved")
				}
				if style == "responses" {
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer-private-marker"}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`)
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"answer-private-marker"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
				}
			}))
			defer upstream.Close()
			usage := modelusage.New(pool)
			config, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{upstream.URL}, Teaching: settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: upstream.URL, Model: "actual-model", AuthMode: "none", APIStyle: style}, ModelUsage: usage})
			if err != nil {
				t.Fatal(err)
			}
			ids, err := identity.NewService(identitypg.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 3, LastUsedTouchInterval: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			code, _, err := ids.CreatePairingCodeForProfile(t.Context(), identity.PairingProfileCLI)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := ids.ExchangePairingCode(t.Context(), code, "测试 CLI")
			if err != nil {
				t.Fatal(err)
			}
			gateway, err := modelgateway.New(t.Context(), modelgateway.Options{Pool: pool, Settings: config, Identity: ids, Usage: usage, Permits: privacy.NewReadPermitManager()})
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			server := httptest.NewServer(gateway)
			defer server.Close()
			get, _ := http.NewRequest("GET", server.URL+"/v1/model-gateway/config", nil)
			get.Header.Set("Authorization", "Bearer "+credential.Token)
			response, err := http.DefaultClient.Do(get)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || strings.Contains(string(raw), upstream.URL) {
				t.Fatalf("bad config %d %s", response.StatusCode, raw)
			}
			var projection modelgateway.Config
			if err = json.Unmarshal(raw, &projection); err != nil {
				t.Fatal(err)
			}
			session, turn, request := uuid.NewString(), uuid.NewString(), uuid.NewString()
			invoke := func(target, id string) *http.Response {
				req, _ := http.NewRequest("POST", server.URL+"/v1/model-gateway/chat/completions", bytes.NewBufferString(`{"model":"`+target+`","messages":[{"role":"user","content":"prompt-private-marker"}],"stream":false}`))
				req.Header.Set("Authorization", "Bearer "+credential.Token)
				req.Header.Set("X-Model-Gateway-Version", "1")
				req.Header.Set("X-Learning-Generation", fmt.Sprint(projection.PrivacyGeneration))
				req.Header.Set("X-Model-Configuration", projection.Models[0].ConfigurationRevision)
				req.Header.Set("X-CLI-Session-ID", session)
				req.Header.Set("X-CLI-Turn-ID", turn)
				req.Header.Set("X-CLI-Request-ID", id)
				req.Header.Set("X-CLI-Model-Stage", "foreground")
				res, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				return res
			}
			for _, target := range []string{"search", "arbitrary"} {
				r := invoke(target, uuid.NewString())
				b, _ := io.ReadAll(r.Body)
				r.Body.Close()
				if r.StatusCode != 403 {
					t.Fatalf("accepted %s: %d %s", target, r.StatusCode, b)
				}
			}
			response = invoke("teaching", request)
			raw, _ = io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || !strings.Contains(string(raw), "answer-private-marker") {
				t.Fatalf("request %d %s", response.StatusCode, raw)
			}
			for i := 0; i < 2; i++ {
				r := invoke("teaching", request)
				r.Body.Close()
				if r.StatusCode != 409 {
					t.Fatalf("duplicate: %d", r.StatusCode)
				}
			}
			response = invoke("teaching", uuid.NewString())
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("second same-turn call %d", response.StatusCode)
			}
			if calls.Load() != 2 {
				t.Fatalf("unexpected paid attempts %d", calls.Load())
			}
			var tasks, count, total int
			var source, device, kind string
			if err = pool.QueryRow(context.Background(), `SELECT count(DISTINCT t.id),count(c.id),sum(c.total_tokens),min(t.source),min(t.actor_device_id::text),min(t.kind) FROM learning_model_tasks t JOIN learning_model_calls c ON c.task_id=t.id`).Scan(&tasks, &count, &total, &source, &device, &kind); err != nil {
				t.Fatal(err)
			}
			if tasks != 1 || count != 2 || total != 36 || source != "cli" || device != credential.Device.ID || kind != "cli_gateway" {
				t.Fatalf("incorrect attribution: %d %d %d %s %s %s", tasks, count, total, source, device, kind)
			}
			actor, err := ids.Authenticate(t.Context(), credential.Token, "learning:read")
			if err != nil {
				t.Fatal(err)
			}
			q := modelusage.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Timezone: "UTC", Grain: "day", Limit: 10, Source: "cli", ActorDeviceID: credential.Device.ID, CLISessionID: session}
			summary, err := usage.Summary(t.Context(), actor, q)
			if err != nil || summary.Totals.Calls != 2 {
				t.Fatalf("session filter: %+v %v", summary, err)
			}
			page, err := usage.Tasks(t.Context(), actor, q)
			if err != nil || len(page.Items) != 1 || page.Items[0].CLISessionID == nil || *page.Items[0].CLISessionID != session || page.Items[0].ActorDeviceID == nil || *page.Items[0].ActorDeviceID != credential.Device.ID {
				t.Fatalf("task attribution: %+v %v", page, err)
			}
			q.Source = "web"
			summary, err = usage.Summary(t.Context(), actor, q)
			if err != nil || summary.Totals.Calls != 0 {
				t.Fatalf("source filter: %+v %v", summary, err)
			}
			for _, table := range []string{"learning_cli_model_requests", "learning_cli_model_turns", "learning_model_calls", "learning_model_tasks"} {
				var leaked bool
				if err = pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM `+table+` x WHERE row_to_json(x)::text LIKE '%private-marker%')`).Scan(&leaked); err != nil || leaked {
					t.Fatalf("body persisted in %s: %v", table, err)
				}
			}
		})
	}
}
