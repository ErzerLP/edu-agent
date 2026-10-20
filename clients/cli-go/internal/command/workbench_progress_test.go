package command

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func TestWorkbenchProgressPreservesParagraphs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if workbenchSpaceHTTP(w, r) {
			return
		}
		switch r.URL.Path {
		case "/v1/learning/progress":
			writeJSONTest(w, 200, api.ProgressPage{Metadata: commandSessionView("GoalReady", "", "", false, false).Metadata, Items: []api.GoalProgress{}})
		case "/v1/learning/reviews":
			writeJSONTest(w, 200, api.ReviewsPage{Metadata: commandSessionView("GoalReady", "", "", false, false).Metadata, Items: []api.ReviewSchedule{}})
		default:
			t.Errorf("意外请求：%s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	app, _, _ := newTestApp(cfg, creds, &fakeTerminal{})
	for _, name := range []string{"overview", "reviews"} {
		page, err := (workbenchService{app: *app}).Load(t.Context(), workbench.Request{Space: api.DefaultLearningSpaceID, Page: name})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(page.Content, "\n按优先级") || strings.Contains(page.Content, `\n`) {
			t.Fatalf("%s 正文换行被提前转义：%q", name, page.Content)
		}
	}
}
