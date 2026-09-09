package command

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func goalTestRevision() api.GoalRevision {
	return api.GoalRevision{GoalID: "10000000-0000-4000-8000-000000000001", GoalRevisionID: "20000000-0000-4000-8000-000000000001", SpaceID: api.DefaultLearningSpaceID, Revision: 1, Text: "学习 Go", Source: "go-cli-m1", ActorDeviceID: testDeviceID, CreatedAt: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), Management: &api.GoalManagement{Details: api.GoalDetails{Name: "Go", Priority: "normal"}, Status: "draft", CriteriaVerification: "unverified", ChangedFields: []string{}}}
}
func goalTestCapabilities(w http.ResponseWriter) {
	writeJSONTest(w, 200, api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "collections_v1", "learning": "goals_v1", "tutoring": "default_only", "memory": "default_only"}})
}
func TestGoalEditorRetainsMultilineInputAndRetryIdentity(t *testing.T) {
	g := goalTestRevision()
	requests := []api.LearningGoalRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/learning-spaces/capabilities":
			goalTestCapabilities(w)
		case r.Method == "GET" && r.URL.Path == "/v1/learning/goals/"+g.GoalID:
			writeJSONTest(w, 200, g)
		case r.Method == "PUT" && r.URL.Path == "/v1/learning/goals/"+g.GoalID:
			var request api.LearningGoalRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			requests = append(requests, request)
			if len(requests) <= 2 {
				writeJSONTest(w, 503, api.ErrorResponse{Error: api.ErrorBody{Code: "model_unavailable", Message: "模拟暂时故障", RequestID: "retry-goal"}})
				return
			}
			updated := g
			updated.Revision = 2
			updated.GoalRevisionID = "20000000-0000-4000-8000-000000000002"
			updated.Management = &api.GoalManagement{Details: *request.Details, Status: "draft", CriteriaVerification: "unverified", ChangedFields: []string{"scope"}, RouteAdjustmentNeeded: true}
			writeJSONTest(w, 201, api.GoalOperationResult{Status: "succeeded", AggregateType: "goal", AggregateID: g.GoalID, AggregateVersion: 2, FirstEventSeq: 2, LastEventSeq: 2, ProjectionAsOfEventSeq: 2, Result: updated})
		default:
			t.Errorf("目标编辑调用了其他入口：%s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	terminal := &fakeTerminal{lines: []string{"scope", "Channel", "Context", ".", "s", "s", "s"}}
	app, out, errOut := newTestApp(cfg, creds, terminal)
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if exit := app.Run(t.Context(), []string{"goal", "edit", "--id", g.GoalID}); exit != ExitOK {
		t.Fatalf("编辑失败：%d %s %s", exit, out, errOut)
	}
	if len(requests) != 3 || requests[0].Details.Scope != "Channel\nContext" {
		t.Fatalf("多行输入丢失：%+v", requests)
	}
	for _, request := range requests[1:] {
		if !reflect.DeepEqual(request, requests[0]) {
			t.Fatal("失败重试更换了内容或操作身份")
		}
	}
	if !strings.Contains(out.String(), "当前输入已保留") || cfg.saveCalls != 0 || creds.saveCalls != 0 {
		t.Fatal("保存失败没有保留编辑内容，或写入了本地个人数据")
	}
}

func TestGoalBrowserSearchStatusPaginationAndDetails(t *testing.T) {
	g := goalTestRevision()
	queries := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning-spaces/capabilities":
			goalTestCapabilities(w)
		case "/v1/learning/goals":
			queries = append(queries, r.URL.RawQuery)
			page := api.GoalPage{Items: []api.GoalRevision{g}}
			if r.URL.Query().Get("cursor") == "" {
				page.NextCursor = "next"
			}
			writeJSONTest(w, 200, page)
		case "/v1/learning/goals/" + g.GoalID:
			writeJSONTest(w, 200, g)
		default:
			t.Errorf("意外入口：%s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	term := &fakeTerminal{lines: []string{"s", "Go", "f", "draft", "p", "1", "q", "q"}}
	app, out, errOut := newTestApp(cfg, creds, term)
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if exit := app.Run(t.Context(), []string{"goal", "browse"}); exit != ExitOK {
		t.Fatalf("浏览失败：%d %s %s", exit, out, errOut)
	}
	if len(queries) < 4 || !strings.Contains(queries[3], "cursor=next") || !strings.Contains(queries[3], "status=draft") || !strings.Contains(queries[3], "search=Go") || !strings.Contains(out.String(), "资料缺口") {
		t.Fatalf("查询或详情失效：%v %s", queries, out)
	}
	if app.learningSpace != "" || term.confirmCalls != 0 {
		t.Fatal("查看目标修改了选择或目标状态")
	}
}

func TestGoalManagementReportsOldServerBeforeWriting(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/learning-spaces/capabilities" {
			writeJSONTest(w, 200, api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "default_only", "learning": "default_only", "tutoring": "default_only", "memory": "default_only"}})
			return
		}
		calls++
		w.WriteHeader(500)
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	app, _, errOut := newTestApp(cfg, creds, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"goal", "create", "学习 Go"}); exit != ExitUnavailable || calls != 0 || !strings.Contains(errOut.String(), "goal_management_unsupported") {
		t.Fatalf("旧服务端兼容错误：%d %d %s", exit, calls, errOut)
	}
}

func TestGoalEditorConflictRefreshRequiresConfirmationAndRetainsInput(t *testing.T) {
	g := goalTestRevision()
	requests := []api.LearningGoalRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/learning-spaces/capabilities":
			goalTestCapabilities(w)
		case r.Method == "GET":
			writeJSONTest(w, 200, g)
		case r.Method == "PUT":
			var request api.LearningGoalRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			requests = append(requests, request)
			if len(requests) == 1 {
				g.Revision = 2
				g.GoalRevisionID = "20000000-0000-4000-8000-000000000002"
				g.Management.Details.Scope = "远端并发修订"
				writeJSONTest(w, 409, api.ErrorResponse{Error: api.ErrorBody{Code: "version_conflict", Message: "远端已修改", RequestID: "conflict"}, Conflict: &api.LearningConflict{AggregateType: "goal", AggregateID: g.GoalID, ExpectedVersion: 1, CurrentVersion: 2, AsOfEventSeq: 2}})
				return
			}
			g.Revision = 3
			g.Management.Details = *request.Details
			writeJSONTest(w, 201, api.GoalOperationResult{Status: "succeeded", AggregateType: "goal", AggregateID: g.GoalID, AggregateVersion: 3, FirstEventSeq: 3, LastEventSeq: 3, ProjectionAsOfEventSeq: 3, Result: g})
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	term := &fakeTerminal{lines: []string{"scope", "保留我的输入", ".", "s", "r", "s"}, confirmed: true}
	app, out, errOut := newTestApp(cfg, creds, term)
	app.NewUUID = uuidSequence(t, "70000000-0000-4000-8000-000000000001", "70000000-0000-4000-8000-000000000002")
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if exit := app.Run(t.Context(), []string{"goal", "edit", "--id", g.GoalID}); exit != ExitOK {
		t.Fatalf("冲突恢复失败：%d %s %s", exit, out, errOut)
	}
	if len(requests) != 2 || requests[1].ExpectedVersion != 2 || requests[1].PreviousRevisionID != "20000000-0000-4000-8000-000000000002" || requests[1].Details.Scope != "保留我的输入" || requests[1].OperationID == requests[0].OperationID || term.confirmCalls != 1 || !strings.Contains(out.String(), "远端并发修订") {
		t.Fatalf("没有明确确认版本或保留输入：%+v %s", requests, out)
	}
}

func TestGoalRejectsContentFlagsOnLifecycleCommands(t *testing.T) {
	for _, args := range [][]string{{"goal", "pause", "--id", goalTestRevision().GoalID, "--scope", "不得丢弃"}, {"goal", "edit", "--id", goalTestRevision().GoalID, "--reason", "错误参数"}} {
		app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
		if exit := app.Run(t.Context(), args); exit != ExitInput || !strings.Contains(errOut.String(), "usage") {
			t.Fatalf("未拒绝混合参数：%d %s", exit, errOut)
		}
	}
}

func TestGoalEditorSelectsFrozenChapterAndRetriesWithoutLosingSelection(t *testing.T) {
	g := goalTestRevision()
	collection := "30000000-0000-4000-8000-000000000001"
	revision := "30000000-0000-4000-8000-000000000002"
	document := "30000000-0000-4000-8000-000000000003"
	node := "30000000-0000-4000-8000-000000000004"
	freezes := []api.KnowledgeScopeSnapshot{}
	var saved api.LearningGoalRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/learning-spaces/capabilities":
			goalTestCapabilities(w)
		case r.Method == "GET" && r.URL.Path == "/v1/learning/goals/"+g.GoalID:
			writeJSONTest(w, 200, g)
		case r.URL.Path == "/v1/knowledge/collections":
			writeJSONTest(w, 200, map[string]any{"items": []api.KnowledgeCollection{{ID: collection, Name: "Go 教材", HeadRevisionID: &revision}}})
		case r.URL.Path == "/v1/knowledge/revisions/"+revision+"/tree":
			writeJSONTest(w, 200, map[string]any{"revision": map[string]any{"documents": []any{map[string]any{"path": "go.md", "document": map[string]any{"document_id": document, "nodes": []any{map[string]any{"node_id": node, "title": "并发章节", "heading_level": 1}}}}}}})
		case r.Method == "POST" && r.URL.Path == "/v1/knowledge/scopes":
			var request api.KnowledgeScopeSnapshot
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			freezes = append(freezes, request)
			if len(freezes) == 1 {
				writeJSONTest(w, 500, api.ErrorResponse{Error: api.ErrorBody{Code: "internal_error", Message: "冻结失败", RequestID: "freeze-retry"}})
				return
			}
			writeJSONTest(w, 200, request)
		case r.Method == "PUT":
			if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
				t.Error(err)
			}
			g.Revision = 2
			g.Management.Details = *saved.Details
			writeJSONTest(w, 201, api.GoalOperationResult{Status: "succeeded", AggregateType: "goal", AggregateID: g.GoalID, AggregateVersion: 2, FirstEventSeq: 2, LastEventSeq: 2, ProjectionAsOfEventSeq: 2, Result: g})
		default:
			t.Errorf("意外调用：%s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	term := &fakeTerminal{lines: []string{"m", "1", "1", "1", "s", "r", "s"}}
	app, out, errOut := newTestApp(cfg, creds, term)
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if exit := app.Run(t.Context(), []string{"goal", "edit", "--id", g.GoalID}); exit != ExitOK {
		t.Fatalf("资料选择失败：%d %s %s", exit, out, errOut)
	}
	want := api.KnowledgeScopeEntry{CollectionID: collection, RevisionID: revision, DocumentID: document, NodeID: node}
	if len(freezes) != 2 || !reflect.DeepEqual(freezes[0], freezes[1]) || len(freezes[0].Entries) != 1 || freezes[0].Entries[0] != want || saved.Details == nil || saved.Details.ScopeSnapshotID != freezes[0].ID || !strings.Contains(out.String(), "并发章节") {
		t.Fatalf("冻结范围或重试身份丢失：%+v %+v", freezes, saved)
	}
}
