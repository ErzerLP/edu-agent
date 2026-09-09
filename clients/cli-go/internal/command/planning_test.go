package command

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDiagnosticRequiresExplicitRouteConfirmation(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tutoring/sessions/" + commandSessionID:
			writeJSONTest(w, 200, commandSessionView("Diagnostic", "open", "", false, false))
		case "/v1/knowledge/revisions/head":
			writeJSONTest(w, 200, api.HeadResponse{Revision: testRevision()})
		case "/v1/knowledge/retrievals":
			writeJSONTest(w, 200, commandRetrieval(false, false))
		case "/v1/tutoring/proposals":
			var request api.TutoringProposalRequest
			if e := json.NewDecoder(r.Body).Decode(&request); e != nil {
				t.Error(e)
			}
			proposal := commandProposal(request, "route", "open")
			proposal.Route = []api.RouteProposalStep{{NodeRevisionID: commandNodeRevision, TeachingIntent: "学习并发", CompletionCondition: "解释并发"}}
			writeJSONTest(w, 201, proposal)
		default:
			actions.Add(1)
			http.Error(w, "不应应用路线", 500)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(cfg, creds, &fakeTerminal{})
	exit := app.Run(t.Context(), []string{"learn", "--session", commandSessionID})
	if exit != ExitInput || actions.Load() != 0 || !strings.Contains(out.String(), "proposed route") || !strings.Contains(errOut.String(), "planning_confirmation_required") {
		t.Fatalf("未确认发生写入：%d %d %s %s", exit, actions.Load(), out, errOut)
	}
}

type planningUIFixture struct {
	draft      api.PlanningDraft
	actions    []string
	failedEdit string
	retried    bool
}

func (f *planningUIFixture) PlanningList(context.Context, string) ([]api.PlanningDraft, error) {
	return []api.PlanningDraft{}, nil
}
func (f *planningUIFixture) Planning(context.Context, string, string) (api.PlanningDraft, error) {
	return f.draft, nil
}
func (f *planningUIFixture) ChangePlanning(_ context.Context, goal, id string, c api.PlanningCommand) (api.PlanningDraft, error) {
	if c.Action == "edit" && !f.retried {
		if f.failedEdit == "" {
			f.failedEdit = c.OperationID
			return api.PlanningDraft{}, fmt.Errorf("模拟响应丢失")
		}
		if f.failedEdit != c.OperationID {
			return api.PlanningDraft{}, fmt.Errorf("重试操作身份改变")
		}
		f.retried = true
	}
	f.actions = append(f.actions, c.Action)
	if c.Action == "create" {
		f.draft = api.PlanningDraft{ID: id, Goal: api.GoalRevision{GoalID: goal, Text: "学习并发"}, State: "draft", Content: api.PlanningContent{Details: api.GoalDetails{Name: "并发"}}, ModelSource: "服务端教学模型配置"}
	}
	if c.Action == "generate" {
		suggestion := f.draft.Content
		suggestion.Details.ExpectedOutcome = "可以解释并发"
		f.draft.Suggestion = &suggestion
	}
	if c.Action == "edit" {
		f.draft.Content = *c.Content
	}
	if c.Action == "confirm" {
		if c.Target != "new_session" {
			return f.draft, fmt.Errorf("应用用途错误")
		}
		f.draft.State = "applied"
		f.draft.AppliedSessionID = commandSessionID
	}
	f.draft.Version = c.ExpectedVersion + 1
	return f.draft, nil
}
func TestPlanningUIReviewRetryConfirmAndCancel(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			lines := []string{"n", "g", "u", "r", "c", "2"}
			if cancel {
				lines = []string{"n", "g", "q"}
			}
			cfg, creds := pairedStores("http://127.0.0.1", "token")
			app, out, _ := newTestApp(cfg, creds, &fakeTerminal{lines: lines, confirmed: true})
			fixture := &planningUIFixture{}
			if err := app.browsePlanning(t.Context(), fixture, commandGoalRevision, "", ""); err != nil {
				t.Fatal(err)
			}
			if cancel {
				if fixture.draft.State != "draft" || len(fixture.actions) != 2 {
					t.Fatal("退出错误应用草稿")
				}
			} else {
				if !fixture.retried || fixture.draft.State != "applied" || fixture.draft.Content.Details.ExpectedOutcome != "可以解释并发" {
					t.Fatal("编辑重试或确认失败")
				}
			}
			if !strings.Contains(out.String(), "AI 建议") || !strings.Contains(out.String(), "用户原文") {
				t.Fatal("未区分原文与建议")
			}
		})
	}
}
func TestPlanningReorderPreservesDependencies(t *testing.T) {
	steps := []api.PlanningStep{{Name: "基础"}, {Name: "练习", Prerequisites: []int{0}}, {Name: "可选"}}
	moved := reorderPlanningSteps(steps, 2, 0)
	if moved[2].Name != "练习" || len(moved[2].Prerequisites) != 1 || moved[2].Prerequisites[0] != 1 {
		t.Fatal("重排改变依赖目标")
	}
}
