package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestRejectedOutputRetainsUsageWithoutJSON(t *testing.T) {
	for _, test := range []struct {
		name     string
		wire     WireAPI
		body     string
		category ErrorCategory
	}{
		{"responses_truncated", Responses, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":101,"output_tokens":8192,"output_tokens_details":{"reasoning_tokens":8192},"total_tokens":8293}}`, ErrorOutputBudgetExceeded},
		{"chat_truncated", ChatCompletions, `{"choices":[{"finish_reason":"length","message":{"content":"{}"}}],"usage":{"prompt_tokens":101,"completion_tokens":8192,"completion_tokens_details":{"reasoning_tokens":8192},"total_tokens":8293}}`, ErrorOutputBudgetExceeded},
		{"responses_schema", Responses, `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{\"ok\":\"wrong\"}"}]}],"usage":{"input_tokens":101,"output_tokens":8192,"output_tokens_details":{"reasoning_tokens":8192},"total_tokens":8293}}`, ErrorSchemaMismatch},
		{"responses_tool_with_json", Responses, `{"status":"completed","output":[{"type":"function_call","name":"invented_tool","arguments":"{}"},{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}],"usage":{"input_tokens":101,"output_tokens":8192,"output_tokens_details":{"reasoning_tokens":8192},"total_tokens":8293}}`, ErrorResponseNotCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, test.body) }))
			defer provider.Close()
			u, _ := url.Parse(provider.URL)
			client, err := New(Options{BaseURL: u, WireAPI: test.wire, Model: "fixture", AllowNoKey: true, ContextWindow: 32768, MinimumContext: 4096, Timeout: time.Second, MaxOutputTokens: 8192, RequireCompletion: true})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Chat(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "Return JSON"}}, Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)})
			if err == nil || Category(err) != test.category || len(result.JSON) != 0 || result.Usage != (TokenUsage{InputTokens: 101, OutputTokens: 8192, ReasoningTokens: 8192, TotalTokens: 8293}) {
				t.Fatalf("rejection discarded usage or released invalid JSON: %+v %v", result, err)
			}
		})
	}
}
