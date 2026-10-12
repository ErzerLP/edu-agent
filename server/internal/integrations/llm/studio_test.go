package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestStudioChatRequiresCompletedResponse(t *testing.T) {
	for _, tc := range []struct {
		finish   string
		category ErrorCategory
	}{{"length", ErrorOutputBudgetExceeded}, {"", ErrorResponseNotCompleted}, {"stop", ""}} {
		t.Run(tc.finish, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"choices":[{"finish_reason":%q,"message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":12,"completion_tokens":30,"total_tokens":42}}`, tc.finish)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			c, err := New(Options{BaseURL: u, Model: "test", AllowNoKey: true, ContextWindow: 4096, MinimumContext: 4096, Timeout: time.Second, RequireCompletion: true})
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "JSON"}}})
			if tc.category != "" {
				if err == nil || Category(err) != tc.category {
					t.Fatalf("got %v", err)
				}
			} else if err != nil || result.Usage.TotalTokens != 42 {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}
