package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func responsesClient(t *testing.T, base string) *Client {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{BaseURL: u, WireAPI: "responses", Model: "test-model", APIKey: "test-key", ContextWindow: 8192, MinimumContext: 4096, Timeout: time.Second, MaxOutputTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResponsesStructuredOutputAndUsage(t *testing.T) {
	for _, tc := range []struct {
		base, path string
		native     bool
	}{
		{"", "/v1/responses", true},
		{"/gateway/v1/", "/gateway/v1/responses", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			messages := []Message{{Role: RoleSystem, Content: "Return JSON."}, {Role: RoleAssistant, Content: "OK."}, {Role: RoleUser, Content: "Confirm."}}
			schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != tc.path || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("wrong request destination or credentials")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				var input []Message
				if json.Unmarshal(body["input"], &input) != nil || !reflect.DeepEqual(input, messages) {
					t.Error("input messages lost")
				}
				if string(body["model"]) != `"test-model"` || string(body["max_output_tokens"]) != "2000" || body["messages"] != nil || body["response_format"] != nil || body["max_tokens"] != nil {
					t.Error("incorrect Responses payload")
				}
				var text struct {
					Format struct {
						Type, Name string
						Strict     bool
						Schema     json.RawMessage
					}
				}
				if err := json.Unmarshal(body["text"], &text); err != nil {
					t.Error(err)
					return
				}
				if tc.native {
					var got, want any
					json.Unmarshal(text.Format.Schema, &got)
					json.Unmarshal(schema, &want)
					if text.Format.Type != "json_schema" || text.Format.Name != "test_schema" || !text.Format.Strict || !reflect.DeepEqual(got, want) {
						t.Error("strict schema was not sent")
					}
				} else if text.Format.Type != "json_object" {
					t.Error("JSON mode was not sent")
				}
				fmt.Fprint(w, `{"status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","content":[{"type":"output_text","text":"{\"ok\":"},{"type":"output_text","text":"true}"}]}],"usage":{"input_tokens":42,"output_tokens":665,"output_tokens_details":{"reasoning_tokens":620},"total_tokens":707}}`)
			}))
			defer s.Close()
			result, err := responsesClient(t, s.URL+tc.base).Chat(t.Context(), ChatRequest{Messages: messages, Schema: schema, SchemaName: "test_schema", UseNativeJSONSchema: tc.native})
			if err != nil {
				t.Fatal(err)
			}
			if string(result.JSON) != `{"ok":true}` || result.Usage.InputTokens != 42 || result.Usage.OutputTokens != 665 || result.Usage.ReasoningTokens != 620 || result.Usage.TotalTokens != 707 {
				t.Fatalf("incorrect result: %+v", result)
			}
		})
	}
}

func TestResponsesRejectsIncompleteAndInvalidOutput(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		category   ErrorCategory
	}{
		{"budget exhausted even with valid text", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`, "output_budget_exceeded"},
		{"other incomplete", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`, "response_not_completed"},
		{"failed", `{"status":"failed","error":{"message":"private-provider-body"}}`, "response_not_completed"},
		{"in progress", `{"status":"in_progress"}`, "response_not_completed"},
		{"missing status", `{"output":[]}`, "response_not_completed"},
		{"missing text", `{"status":"completed","output":[{"content":[{"type":"refusal","refusal":"private-provider-body"}]}]}`, "invalid_response"},
		{"malformed JSON", `{"status":"completed","output":[{"content":[{"type":"output_text","text":"broken"}]}]}`, "invalid_response"},
		{"schema mismatch", `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{\"ok\":\"yes\"}"}]}]}`, "schema_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer s.Close()
			result, err := responsesClient(t, s.URL).Chat(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "Return JSON."}}, Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)})
			if err == nil || Category(err) != tc.category || len(result.JSON) != 0 {
				t.Fatalf("expected %s without successful JSON, got %+v %v", tc.category, result, err)
			}
		})
	}
}

func TestResponsesProbeUsesSelectedAPI(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/responses" {
					t.Error("probe used wrong API")
				}
				var body struct {
					Text struct {
						Format struct {
							Type, Name string
							Strict     bool
						}
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if calls == 1 {
					if body.Text.Format.Type != "json_schema" || body.Text.Format.Name != "capability_probe" || !body.Text.Format.Strict {
						t.Error("probe did not request native schema")
					}
					if !native {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				} else if body.Text.Format.Type != "json_object" {
					t.Error("fallback did not use JSON mode")
				}
				fmt.Fprint(w, `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{\"capability_probe\":true}"}]}]}`)
			}))
			defer s.Close()
			capabilities := responsesClient(t, s.URL).Probe(t.Context())
			wantCalls := 1
			if !native {
				wantCalls = 2
			}
			if !capabilities.Compatible || !capabilities.StructuredJSON || capabilities.NativeJSONSchema != native || capabilities.Profile != "openai-responses-v1" || calls != wantCalls {
				t.Fatalf("wrong capability result: %+v, calls %d", capabilities, calls)
			}
		})
	}
}

func TestResponsesSendsNullableSchemasAsAnyOfAndValidatesOriginal(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":["object","null"],"properties":{"names":{"type":["array","null"],"items":{"type":"string"}}},"required":["names"],"additionalProperties":false}},"required":["value"],"additionalProperties":false}`)
	for _, tc := range []struct {
		output string
		valid  bool
	}{
		{`{"value":null}`, true},
		{`{"value":{"names":["one"]}}`, true},
		{`{"value":{"names":false}}`, false},
		{`{"value":{"names":[1]}}`, false},
	} {
		t.Run(tc.output, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Text struct {
						Format struct{ Schema map[string]any }
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				value := request.Text.Format.Schema["properties"].(map[string]any)["value"].(map[string]any)
				branches, ok := value["anyOf"].([]any)
				if !ok || len(branches) != 2 {
					t.Error("nullable object was not represented as anyOf")
					w.WriteHeader(400)
					return
				}
				object := branches[0].(map[string]any)
				if object["type"] != "object" || object["additionalProperties"] != false || !reflect.DeepEqual(object["required"], []any{"names"}) || branches[1].(map[string]any)["type"] != "null" {
					t.Error("object/null constraints changed")
				}
				names := object["properties"].(map[string]any)["names"].(map[string]any)
				items, ok := names["anyOf"].([]any)
				if !ok || len(items) != 2 || items[0].(map[string]any)["type"] != "array" || items[0].(map[string]any)["items"].(map[string]any)["type"] != "string" || items[1].(map[string]any)["type"] != "null" {
					t.Error("nested nullable array constraints changed")
				}
				fmt.Fprintf(w, `{"status":"completed","output":[{"content":[{"type":"output_text","text":%q}]}]}`, tc.output)
			}))
			defer s.Close()
			_, err := responsesClient(t, s.URL).Chat(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "Return JSON."}}, Schema: schema, UseNativeJSONSchema: true})
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			if !tc.valid && (err == nil || Category(err) != ErrorSchemaMismatch) {
				t.Fatalf("original schema validation lost: %v", err)
			}
		})
	}
}
