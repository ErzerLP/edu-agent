package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
)

func TestStudioUsesSharedResponsesAndConfiguredBudget(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			t.Fatal("bad request")
		}
		if r.URL.Path != "/v1/responses" || p["max_output_tokens"] != float64(8192) || p["text"].(map[string]any)["format"].(map[string]any)["type"] != "json_schema" {
			t.Errorf("incorrect Studio protocol/budget")
		}
		w.Write([]byte(`{"status":"completed","output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`))
	}))
	defer provider.Close()
	s, _ := testService(t, provider.URL)
	limits := DefaultLimits()
	limits.OutputTokens = 8192
	_, err := s.Update(Update{ExpectedRevision: 0, Target: Teaching, Connection: &Connection{Enabled: true, Provider: "openai_compatible", Endpoint: provider.URL, Model: "deepseek-v4-flash", AuthMode: "none", APIStyle: "responses"}, Limits: &limits})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	_, err = s.Update(Update{ExpectedRevision: s.View().Revision, MentorUsesTeaching: &yes})
	if err != nil {
		t.Fatal(err)
	}
	c, model, native, err := s.StudioClient(nil)
	if err != nil || c == nil || !native || model != "deepseek-v4-flash" {
		t.Fatal("shared Studio connection missing", err)
	}
	_, err = c.Chat(context.Background(), llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "JSON"}}, Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["ok"],"properties":{"ok":{"type":"boolean"}}}`), UseNativeJSONSchema: native})
	if err != nil {
		t.Fatal(err)
	}
	if s.View().EffectiveMentor.APIStyle != "chat_completions" {
		t.Fatal("legacy mentor protocol changed")
	}
}
