package command

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func TestWorkbenchLibraryGoalAndExplicitSessionFlow(t *testing.T) {
	collectionID := "10000000-0000-4000-8000-000000000009"
	scopeID := "10000000-0000-4000-8000-000000000008"
	g := goalTestRevision()
	var guard sync.Mutex
	var sessionWrites, currentReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guard.Lock()
		defer guard.Unlock()
		if r.URL.Path == "/v1/learning-spaces/capabilities" {
			goalTestCapabilities(w)
			return
		}
		if workbenchSpaceHTTP(w, r) {
			return
		}
		if r.Header.Get("X-Learning-Space-ID") != api.DefaultLearningSpaceID {
			t.Error("缺失业务学习区身份")
		}
		switch {
		case r.URL.Path == "/v1/knowledge/collections":
			writeJSONTest(w, 200, map[string]any{"items": []api.KnowledgeCollection{{ID: collectionID, Name: "并发教材", Source: "测试", Version: 1, HeadRevisionID: stringPointer(commandKnowledgeID)}}})
		case r.URL.Path == "/v1/knowledge/revisions/"+commandKnowledgeID+"/tree":
			writeJSONTest(w, 200, map[string]any{"revision": api.KnowledgeRevision{Documents: []api.SnapshotDocument{{Path: "并发.md", Document: api.DocumentRevision{DocumentID: testDocID, Nodes: []api.NodeRevision{{NodeID: commandNodeID, Title: "并发基础", HeadingLevel: 1}}}}}}})
		case r.URL.Path == "/v1/knowledge/revisions/"+commandKnowledgeID+"/export":
			writeJSONTest(w, 200, map[string]any{"documents": []map[string]string{{"path": "并发.md", "markdown": "# 并发基础\n中文正文"}}})
		case r.URL.Path == "/v1/knowledge/scopes":
			var scope api.KnowledgeScopeSnapshot
			if err := json.NewDecoder(r.Body).Decode(&scope); err != nil {
				t.Error(err)
			}
			if len(scope.Entries) != 1 || scope.Entries[0].NodeID != commandNodeID || scope.Entries[0].RevisionID != commandKnowledgeID {
				t.Errorf("冻结范围错误：%+v", scope)
			}
			scope.ID = scopeID
			writeJSONTest(w, 200, scope)
		case r.URL.Path == "/v1/learning/goals" && r.Method == "POST":
			var req api.LearningGoalRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			g.GoalID = req.AggregateID
			g.Text = req.Text
			g.Management.Details = *req.Details
			writeJSONTest(w, 201, api.GoalOperationResult{Status: "succeeded", AggregateType: "goal", AggregateID: g.GoalID, AggregateVersion: 1, FirstEventSeq: 1, LastEventSeq: 1, ProjectionAsOfEventSeq: 1, Result: g})
		case r.URL.Path == "/v1/learning/goals/"+g.GoalID:
			writeJSONTest(w, 200, g)
		case r.URL.Path == "/v1/tutoring/sessions" && r.Method == "POST":
			sessionWrites.Add(1)
			result := commandOperationResult()
			result.Result.Focus.GoalRevisionID = g.GoalRevisionID
			writeJSONTest(w, 201, result)
		case r.URL.Path == "/v1/tutoring/sessions/"+commandSessionID:
			writeJSONTest(w, 200, commandSessionView("AwaitingResponse", "open", "", false, false))
		case r.URL.Path == "/v1/tutoring/sessions/current":
			currentReads.Add(1)
			http.Error(w, "禁止全局回退", 500)
		default:
			t.Errorf("意外请求：%s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(cfg, creds, &fakeTerminal{})
	service := workbenchService{app: *app}
	load := func(r workbench.Request) workbench.Page {
		t.Helper()
		r.Space = api.DefaultLearningSpaceID
		p, e := service.Load(t.Context(), r)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	p := load(workbench.Request{Page: "materials"})
	if !strings.Contains(p.Content, "并发教材") {
		t.Fatal("资料集合未展示")
	}
	p = load(workbench.Request{Page: "collection", Resource: collectionID + "/" + testDocID + "/" + commandNodeID})
	if !strings.Contains(p.Content, "中文正文") {
		t.Fatal("章节正文不可读")
	}
	p = load(workbench.Request{Page: "collection", Resource: collectionID + "/" + testDocID + "/" + commandNodeID, Action: "freeze", Basis: p.Basis, Entity: scopeID})
	if p.Scope != scopeID {
		t.Fatal("未返回冻结范围")
	}
	op := "70000000-0000-4000-8000-000000000009"
	p = load(workbench.Request{Page: "new-goal", Scope: scopeID, Action: "save", Operation: op, Entity: goalTestRevision().GoalID, Values: map[string]string{"name": "并发目标", "text": "中文多行\n学习意图", "priority": "normal"}})
	if p.Redirect != "goal/"+goalTestRevision().GoalID || sessionWrites.Load() != 0 || currentReads.Load() != 0 {
		t.Fatal("保存目标触碰教学状态")
	}
	p = load(workbench.Request{Page: "goal", Resource: goalTestRevision().GoalID})
	if !strings.Contains(p.Content, scopeID) || !strings.Contains(p.Content, "中文多行\n学习意图") {
		t.Fatal("目标未保留结构化信息和资料")
	}
	p = load(workbench.Request{Page: "goal", Resource: goalTestRevision().GoalID, Action: "new-session", Version: p.Version, Entity: commandSessionID, Operation: op})
	if p.Redirect != "session/"+commandSessionID || sessionWrites.Load() != 1 {
		t.Fatal("显式开始教学未接入")
	}
	p = load(workbench.Request{Page: "session", Resource: commandSessionID})
	if p.Stage != "AwaitingResponse" || currentReads.Load() != 0 {
		t.Fatal("教学未按指定会话恢复")
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatal("工作台依赖了命令输出")
	}
}

func stringPointer(v string) *string { return &v }
