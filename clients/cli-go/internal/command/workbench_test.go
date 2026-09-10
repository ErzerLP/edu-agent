package command

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func workbenchSpaceHTTP(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/v1/learning-spaces/capabilities" {
		writeJSONTest(w, 200, api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "default_only", "learning": "default_only", "tutoring": "default_only", "memory": "default_only"}})
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/v1/learning-spaces/") {
		writeJSONTest(w, 200, api.LearningSpace{ID: strings.TrimPrefix(r.URL.Path, "/v1/learning-spaces/"), Name: "测试学习区", Status: "active", Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
		return true
	}
	return false
}

func TestWorkbenchAnswerUsesCLIApplicationAction(t *testing.T) {
	var submitted api.ActionAttemptRequest
	var completed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if workbenchSpaceHTTP(w, r) {
			return
		}
		if r.Header.Get("X-Learning-Space-ID") != api.DefaultLearningSpaceID {
			t.Error("业务请求未绑定学习区")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/tutoring/sessions/current":
			if completed.Load() {
				writeJSONTest(w, 404, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "no active session", RequestID: "current"}})
				return
			}
			writeJSONTest(w, 200, commandSessionView("AwaitingResponse", "open", "", false, false))
		case r.Method == "GET" && r.URL.Path == "/v1/tutoring/sessions/"+commandSessionID:
			state := "AwaitingResponse"
			if completed.Load() {
				state = "Completed"
			}
			writeJSONTest(w, 200, commandSessionView(state, "open", "", false, false))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/actions"):
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Error(err)
			}
			completed.Store(true)
			writeJSONTest(w, 201, commandOperationResult())
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(cfg, creds, &fakeTerminal{})
	service := workbenchService{app: *app}
	req := workbench.Request{Space: api.DefaultLearningSpaceID, Page: "learn", Session: commandSessionID}
	page, err := service.Load(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Content, "题目") || page.Stage != "AwaitingResponse" {
		t.Fatalf("缺少权威题目：%+v", page)
	}
	req.Session, req.Version, req.Action, req.Text = page.Session, page.Version, "submit_attempt:hint", "中文答案\n第二行"
	page, err = service.Load(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if page.Stage != "Completed" || submitted.Answer != req.Text || submitted.Help != "hint" || submitted.ExpectedVersion != req.Version {
		t.Fatalf("未复用正式作答契约：%+v %+v", page, submitted)
	}
	if out.Len() != 0 || errOut.Len() != 0 || cfg.saveCalls != 0 || creds.saveCalls != 0 {
		t.Fatal("工作台输出或草稿泄漏到外部终端/磁盘")
	}
}

func TestWorkbenchRejectsStaleContextAndNonDefaultFallback(t *testing.T) {
	var reads, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if workbenchSpaceHTTP(w, r) {
			return
		}
		if r.Method != "GET" {
			writes.Add(1)
			http.Error(w, "unexpected", 500)
			return
		}
		reads.Add(1)
		writeJSONTest(w, 200, commandSessionView("AwaitingResponse", "open", "", false, false))
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	app, _, _ := newTestApp(cfg, creds, &fakeTerminal{})
	service := workbenchService{app: *app}
	_, err := service.Load(t.Context(), workbench.Request{Space: api.DefaultLearningSpaceID, Page: "learn", Action: "complete_session", Session: commandSessionID, Version: -1})
	if err == nil || !strings.Contains(err.Error(), "version_conflict") || writes.Load() != 0 {
		t.Fatalf("旧上下文执行了写入：%v", err)
	}
	previous := reads.Load()
	page, err := service.Load(t.Context(), workbench.Request{Space: "10000000-0000-4000-8000-000000000002", Page: "learn"})
	if err != nil || !strings.Contains(page.Content, "仅支持默认") || reads.Load() != previous {
		t.Fatal("未支持学习区落入默认区业务")
	}
}

func TestWorkbenchDisplaysRealActionNames(t *testing.T) {
	for _, action := range []string{"issue_activity", "present_activity", "submit_attempt", "record_assessment", "resume_focus"} {
		view := api.SessionView{WorkItem: &api.SessionWorkItem{AllowedActions: []string{action}, Activity: &api.Activity{AllowedHelp: []string{"none"}}}}
		if len(learningActions(view)) != 1 {
			t.Fatalf("遗漏正式教学动作 %s", action)
		}
	}
}

func TestWorkbenchRoutePreviewRequiresVisibleConfirmation(t *testing.T) {
	var applied atomic.Bool
	var proposals atomic.Int32
	var proposalID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if workbenchSpaceHTTP(w, r) {
			return
		}
		switch r.URL.Path {
		case "/v1/tutoring/sessions/" + commandSessionID:
			state := "Diagnostic"
			if applied.Load() {
				state = "RouteActive"
			}
			writeJSONTest(w, 200, commandSessionView(state, "open", "", false, false))
		case "/v1/knowledge/revisions/head":
			writeJSONTest(w, 200, api.HeadResponse{Revision: testRevision()})
		case "/v1/knowledge/retrievals":
			writeJSONTest(w, 200, commandRetrieval(false, false))
		case "/v1/tutoring/proposals":
			var request api.TutoringProposalRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			proposal := commandProposal(request, "route", "open")
			proposal.Route = []api.RouteProposalStep{{NodeRevisionID: commandNodeRevision, TeachingIntent: "学习并发", CompletionCondition: "解释并发"}}
			proposalID = proposal.ProposalID
			proposals.Add(1)
			writeJSONTest(w, 201, proposal)
		case "/v1/tutoring/sessions/" + commandSessionID + "/actions":
			var request api.ActionProposalRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.ProposalID != proposalID || request.Action != "apply_route" {
				t.Error("确认没有使用已展示的原路线")
			}
			applied.Store(true)
			writeJSONTest(w, 201, commandOperationResult())
		default:
			t.Errorf("意外请求：%s %s", r.Method, r.URL.Path)
			http.Error(w, "意外请求", 500)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	terminal := &fakeTerminal{}
	app, out, _ := newTestApp(cfg, creds, terminal)
	service := workbenchService{app: *app}
	req := workbench.Request{Space: api.DefaultLearningSpaceID, Page: "session", Resource: commandSessionID}
	page, err := service.Load(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Version, req.Action = page.Version, "apply_route"
	page, err = service.Load(t.Context(), req)
	if err != nil || terminal.confirmCalls != 0 || !strings.Contains(page.Content, "学习并发") || !strings.Contains(page.Content, "解释并发") || applied.Load() {
		t.Fatalf("路线未在工作台展示，或借用了终端确认：错误=%v，终端确认=%d，页面=%s", err, terminal.confirmCalls, page.Content)
	}
	var confirm workbench.Action
	for _, action := range page.Actions {
		if action.ID == "confirm_route" {
			confirm = action
		}
	}
	if confirm.Confirmation == "" || page.Basis != proposalID {
		t.Fatal("路线缺少明确确认或原提案身份")
	}
	// 返回读取原会话相当于取消预览，不能应用或再次生成路线。
	req.Action = ""
	if _, err = service.Load(t.Context(), req); err != nil || applied.Load() || proposals.Load() != 1 {
		t.Fatal("取消预览产生了路线副作用")
	}
	req.Action, req.Basis, req.Version = confirm.ID, page.Basis, page.Version-1
	if _, err = service.Load(t.Context(), req); err == nil || applied.Load() {
		t.Fatal("过期确认应用了路线")
	}
	req.Version = page.Version
	page, err = service.Load(t.Context(), req)
	if err != nil || !applied.Load() || page.Stage != "RouteActive" || proposals.Load() != 1 || out.Len() != 0 || terminal.confirmCalls != 0 {
		t.Fatalf("明确确认没有复用原路线：%v，阶段=%s", err, page.Stage)
	}
}
