package command

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type sessionRefreshClient struct {
	APIClient
	source        api.SessionView
	currentCalls  int
	sessionCalls  int
	proposalError error
	actionError   error
	actionCalls   int
}

func (c *sessionRefreshClient) CurrentSession(context.Context) (api.SessionView, error) {
	c.currentCalls++
	b := c.source
	b.Session.SessionID = "11000000-0000-4000-8000-000000000002"
	return b, nil
}
func (c *sessionRefreshClient) Session(_ context.Context, id string) (api.SessionView, error) {
	c.sessionCalls++
	if id != c.source.Session.SessionID {
		return api.SessionView{}, &api.ProtocolError{Category: "wrong_source"}
	}
	return c.source, nil
}
func (c *sessionRefreshClient) CreateProposal(context.Context, api.TutoringProposalRequest) (api.TutoringProposal, error) {
	return api.TutoringProposal{}, c.proposalError
}
func (c *sessionRefreshClient) ApplySessionAction(context.Context, string, api.TutoringAction) (api.SessionOperationResult, error) {
	c.actionCalls++
	return api.SessionOperationResult{}, c.actionError
}

func TestProposalRefreshKeepsOriginSession(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "成功", true: "过期"}[stale], func(t *testing.T) {
			c := &sessionRefreshClient{source: commandSessionView("Diagnostic", "open", "", false, false)}
			if stale {
				c.proposalError = &api.APIError{Code: "stale_proposal"}
			}
			a := &App{Err: io.Discard}
			_, got, _, err := a.createProposalAndRefetch(t.Context(), c, c.source, api.TutoringProposalRequest{})
			if err != nil || got.Session.SessionID != c.source.Session.SessionID || c.currentCalls != 0 || c.sessionCalls != 1 {
				t.Fatalf("来源=%s 返回=%s current调用=%d 指定读取=%d 错误=%v", c.source.Session.SessionID, got.Session.SessionID, c.currentCalls, c.sessionCalls, err)
			}
		})
	}
}

func TestOriginClientKeepsScopeAfterUISelectionChanges(t *testing.T) {
	const origin = "10000000-0000-4000-8000-000000000001"
	const selected = "10000000-0000-4000-8000-000000000002"
	view := commandSessionView("AwaitingResponse", "open", "", false, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/learning-spaces/capabilities" {
			writeJSONTest(w, http.StatusOK, api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "collections_v1", "learning": "goals_v1", "tutoring": "sessions_v1", "memory": "default_only"}})
			return
		}
		if r.Header.Get("X-Learning-Space-ID") != origin {
			t.Errorf("重试跟随当前页面改变归属：%s", r.Header.Get("X-Learning-Space-ID"))
		}
		writeJSONTest(w, http.StatusOK, view)
	}))
	defer server.Close()
	a := &App{learningSpace: selected, NewClient: func(server, token string, timeout time.Duration) APIClient {
		return api.NewClient(server, token, timeout, nil)
	}}
	client := a.clientInSpace(server.URL, "token", time.Second, origin)
	if _, err := refetchSession(t.Context(), client, view.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	if a.learningSpace != selected {
		t.Fatal("来源请求改变了 UI 选择")
	}
}

func TestLostAnswerResponseQueriesOnlyOriginalSession(t *testing.T) {
	for _, failure := range []error{&api.TransportError{Category: "connection_reset"}, &api.APIError{Code: "dependency_unavailable", Status: 504}, &api.ProtocolError{Category: "malformed_success_response"}} {
		original := commandSessionView("AwaitingResponse", "open", "", false, false)
		client := &sessionRefreshClient{source: commandSessionView("Evaluating", "open", "", false, false), actionError: failure}
		a := &App{Err: io.Discard}
		got, recovered, err := a.applyAndRefetch(t.Context(), client, original, api.ActionAttemptRequest{})
		if err != nil || !recovered || got.Session.State != "Evaluating" || client.actionCalls != 1 || client.currentCalls != 0 || client.sessionCalls != 1 {
			t.Fatalf("丢响应恢复错误：%+v action=%d current=%d session=%d err=%v", got.Session, client.actionCalls, client.currentCalls, client.sessionCalls, err)
		}
	}
}
