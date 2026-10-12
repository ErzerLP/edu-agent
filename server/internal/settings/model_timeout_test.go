package settings

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
)

// A reasoning provider may send no headers until its complete JSON is ready.
// Both execution clients must honor the configured timeout beyond 15 seconds.
func TestExecutionModelsWaitForConfiguredTimeout(t *testing.T) {
	for _, kind := range []string{"mentor", "teaching", "teaching_responses"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(16 * time.Second):
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if kind == "teaching_responses" {
					w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ready\":true}"}]}]}`))
					return
				}
				w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"ready\":true}"}}]}`))
			}))
			defer provider.Close()
			limits := DefaultLimits()
			limits.IdleTimeoutSeconds = 60
			connection := Connection{Enabled: true, Provider: "openai_compatible", Endpoint: provider.URL, Model: "reasoning-provider", AuthMode: "none"}
			if kind == "teaching_responses" {
				connection.APIStyle = "responses"
			}
			s, err := Open(Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{provider.URL}, Limits: &limits, Teaching: connection})
			if err != nil {
				t.Fatal(err)
			}
			yes := true
			if _, err := s.Update(Update{ExpectedRevision: 0, Target: Mentor, MentorUsesTeaching: &yes}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if kind == "mentor" {
				client, _, _, err := s.MentorClient()
				if err != nil {
					t.Fatal(err)
				}
				result, err := client.Complete(ctx, modelclient.Request{MaxTokens: 64, Messages: []modelclient.Message{{Role: "user", Content: "Return public JSON."}}})
				if err != nil || result.Message.Content != `{"ready":true}` {
					t.Fatalf("configured mentor wait interrupted: %v", err)
				}
			} else {
				client, err := s.TeachingClient()
				if err != nil {
					t.Fatal(err)
				}
				result, err := client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "Return public JSON."}}})
				if err != nil || string(result.JSON) != `{"ready":true}` {
					t.Fatalf("configured teaching wait interrupted: %v", err)
				}
			}
		})
	}
}

// Removing the header deadline must not remove the configured request deadline.
func TestExecutionModelsEnforceConfiguredTimeout(t *testing.T) {
	for _, kind := range []string{"mentor", "teaching", "teaching_responses"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(7 * time.Second):
				}
			}))
			defer provider.Close()
			limits := DefaultLimits()
			limits.IdleTimeoutSeconds = 5
			connection := Connection{Enabled: true, Provider: "openai_compatible", Endpoint: provider.URL, Model: "reasoning-provider", AuthMode: "none"}
			if kind == "teaching_responses" {
				connection.APIStyle = "responses"
			}
			s, err := Open(Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{provider.URL}, Limits: &limits, Teaching: connection})
			if err != nil {
				t.Fatal(err)
			}
			yes := true
			if _, err := s.Update(Update{ExpectedRevision: 0, Target: Mentor, MentorUsesTeaching: &yes}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			started := time.Now()
			if kind == "mentor" {
				client, _, _, err := s.MentorClient()
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Complete(ctx, modelclient.Request{MaxTokens: 64, Messages: []modelclient.Message{{Role: "user", Content: "Return public JSON."}}})
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("configured mentor timeout not enforced: %v", err)
				}
			} else {
				client, err := s.TeachingClient()
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "Return public JSON."}}})
				if llm.Category(err) != llm.ErrorTimeout {
					t.Fatalf("configured teaching timeout not enforced: %v", err)
				}
			}
			if elapsed := time.Since(started); elapsed < 5*time.Second || elapsed > 6*time.Second {
				t.Fatalf("request did not stop at the configured 5s timeout: %s", elapsed)
			}
		})
	}
}
