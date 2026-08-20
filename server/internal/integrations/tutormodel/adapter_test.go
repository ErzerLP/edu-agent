package tutormodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestAdapterSendsStrictRecursiveSchemasAndThreeRoles(t *testing.T) {
	outputs := map[learning.ProposalType]string{
		learning.ProposalRoute:      `{"route":[{"node_revision_id":"node","teaching_intent":"teach","completion_condition":"pass"}]}`,
		learning.ProposalActivity:   `{"activity":{"prompt":"Question","type":"open","rubric":{"rubric_revision":"r1","items":[{"rubric_item_id":"i1","criterion":"correct"}]},"difficulty":1,"allowed_help":["none"],"knowledge_references":[{"node_revision_id":"node"}]}}`,
		learning.ProposalAssessment: `{"assessment":{"items":[],"rubric_complete":false,"confidence":0,"risk_flags":[]}}`,
		learning.ProposalFreeAnswer: `{"text":{"text":"Answer","knowledge_references":[{"node_revision_id":"node"}]}}`,
	}
	for kind, output := range outputs {
		t.Run(string(kind), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages       []llm.Message `json:"messages"`
					ResponseFormat struct {
						Type       string `json:"type"`
						JSONSchema struct {
							Strict bool           `json:"strict"`
							Schema map[string]any `json:"schema"`
						} `json:"json_schema"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if body.ResponseFormat.Type != "json_schema" || !body.ResponseFormat.JSONSchema.Strict {
					t.Errorf("response format is not strict: %#v", body.ResponseFormat)
				}
				roles := map[llm.Role]bool{}
				for _, message := range body.Messages {
					roles[message.Role] = true
				}
				if !roles[llm.RoleSystem] || !roles[llm.RoleAssistant] || !roles[llm.RoleUser] {
					t.Errorf("missing role: %#v", roles)
				}
				schemaJSON, _ := json.Marshal(body.ResponseFormat.JSONSchema.Schema)
				if !strings.Contains(string(schemaJSON), `"additionalProperties":false`) || !strings.Contains(string(schemaJSON), `"required"`) {
					t.Errorf("schema is not closed/required: %s", schemaJSON)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": output}}}})
			}))
			defer server.Close()
			client := newAdapterTestClient(t, server.URL)
			adapter := New(client)
			result, err := adapter.Generate(context.Background(), learning.ProposalRequest{Type: kind, Input: json.RawMessage(`{"context":true}`)})
			if err != nil || !json.Valid(result) {
				t.Fatalf("Generate = %s, %v", result, err)
			}
		})
	}
}

func TestAdapterPreservesModelFailureCategory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer server.Close()
	_, err := New(newAdapterTestClient(t, server.URL)).Generate(context.Background(), learning.ProposalRequest{Type: learning.ProposalRoute, Input: json.RawMessage(`{}`)})
	failure, ok := err.(interface{ ModelCategory() string })
	if !ok || failure.ModelCategory() != "rate_limited" {
		t.Fatalf("category = %T %v", err, err)
	}
}

func newAdapterTestClient(t *testing.T, rawURL string) *llm.Client {
	t.Helper()
	base, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := llm.New(llm.Options{BaseURL: base, Model: "strict-fake", APIKey: "test-key", ContextWindow: 8192, MinimumContext: 4096, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
