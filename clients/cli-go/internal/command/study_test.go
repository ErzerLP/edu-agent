package command

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func TestStudyResearchAliasAndStableJSON(t *testing.T) {
	goal := "10000000-0000-4000-8000-000000000001"
	session := "20000000-0000-4000-8000-000000000001"
	op := "30000000-0000-4000-8000-000000000001"
	run := "40000000-0000-4000-8000-000000000001"
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			writeJSONTest(w, 200, map[string]any{"schema_version": 1, "research": map[string]any{"available": true}})
			return
		}
		if r.URL.Path != "/v1/learning/goals/"+goal+"/runs" || r.Method != "POST" {
			t.Fatalf("不应读全局 current 或启动本地 Agent：%s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["operation_id"] != op || body["session_id"] != session {
			t.Error("脚本操作身份被替换")
		}
		posts++
		writeJSONTest(w, 202, map[string]any{"run_id": run, "session_id": session, "operation_id": op, "version": 1})
	}))
	defer s.Close()
	cfg, creds := pairedStores(s.URL, "fixture")
	a, out, errOut := newTestApp(cfg, creds, nil)
	file := filepath.Join(t.TempDir(), "研究.json")
	body := studyJSON(map[string]any{"operation_id": op, "session_id": session, "expected_version": 1, "save": true, "prompt": "Go", "request_budget": 10, "token_budget": 20000, "research": map[string]any{"topic": "Go", "external_consent": true, "auto_adopt": false, "policy": map[string]any{"mode": "supplement", "domains": []string{}}}})
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	if exit := a.Run(t.Context(), []string{"goal", "research", "--goal", goal, "--input", file}); exit != 0 {
		t.Fatal(exit, errOut.String())
	}
	var receipt api.StudyDocument
	if json.Unmarshal(out.Bytes(), &receipt) != nil || receipt.String("operation_id") != op || posts != 1 {
		t.Fatal(out.String(), posts)
	}
}

func TestStudyHelpNeedsNoPairing(t *testing.T) {
	for _, args := range [][]string{{"study", "help"}, {"goal", "research", "--help"}, {"goal", "start-learning", "--help"}} {
		a, out, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, nil)
		if exit := a.Run(t.Context(), args); exit != 0 || !strings.Contains(out.String(), "operation_id") {
			t.Fatal(args, exit, errOut.String())
		}
	}
}

type studyWorkbenchClient struct {
	APIClient
	request func(string, api.StudyQuery, json.RawMessage) (api.StudyDocument, error)
}

func (c studyWorkbenchClient) Study(_ context.Context, action string, q api.StudyQuery, b json.RawMessage) (api.StudyDocument, error) {
	return c.request(action, q, b)
}

func TestWorkbenchStudyApprovalKeepsDisplayedBasis(t *testing.T) {
	old := map[string]any{"session_id": "原会话", "expected_revision": 3, "hash": "原 hash", "interaction_id": "原交互"}
	writes := 0
	c := studyWorkbenchClient{request: func(action string, q api.StudyQuery, b json.RawMessage) (api.StudyDocument, error) {
		if q.Goal != "明确目标" || q.Change != "明确变更" {
			t.Fatal("归属被替换", q)
		}
		if action == "change" {
			var d api.StudyDocument
			_ = json.Unmarshal(studyJSON(map[string]any{"revision": 4, "hash": "新 hash", "interaction_id": "新交互", "session_id": "原会话"}), &d)
			return d, nil
		}
		if action != "change-command" {
			t.Fatal(action)
		}
		var body api.StudyDocument
		_ = json.Unmarshal(b, &body)
		if body.Number("expected_revision") != 3 || body.String("hash") != "原 hash" || body.String("interaction_id") != "原交互" || body.String("operation_id") != "原操作" {
			t.Fatal("刷新静默改变审批依据", string(b))
		}
		writes++
		return api.StudyDocument{}, nil
	}}
	a := &App{}
	p, err := a.workbenchStudy(t.Context(), c, workbench.Request{Page: "study-change", Resource: "明确目标/明确变更", Action: "approve", Basis: string(studyJSON(old)), Operation: "原操作"}, workbench.Page{})
	if err != nil || writes != 1 || p.Redirect != "study-change/明确目标/明确变更" {
		t.Fatal(p, err, writes)
	}
}

func TestWorkbenchStudyRunRestoresOriginalClassroom(t *testing.T) {
	c := studyWorkbenchClient{request: func(action string, q api.StudyQuery, b json.RawMessage) (api.StudyDocument, error) {
		if action != "run" || q.Run != "原运行" || len(b) != 0 {
			t.Fatal("读取运行触发其他操作")
		}
		var d api.StudyDocument
		_ = json.Unmarshal(studyJSON(map[string]any{"kind": "start_learning", "status": "succeeded", "version": 4, "body_available": true, "goal_id": "原目标", "start_learning": map[string]any{"result": map[string]any{"session_id": "原课堂", "artifact_id": "原正文"}}}), &d)
		return d, nil
	}}
	p, err := (&App{}).workbenchStudy(t.Context(), c, workbench.Request{Page: "study-run", Resource: "原运行"}, workbench.Page{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range p.Entries {
		if e.ID == "page:session/原课堂" {
			found = true
		}
	}
	if !found || !strings.Contains(p.Content, "原进程/标签页") {
		t.Fatal(p)
	}
}

type studyGoalClient struct {
	studyWorkbenchClient
	goalClient
}

func (studyGoalClient) Goal(context.Context, string) (api.GoalRevision, error) {
	return goalTestRevision(), nil
}

func TestWorkbenchStudyReusesDisplayedRunSession(t *testing.T) {
	writes, reads := 0, 0
	c := studyGoalClient{studyWorkbenchClient: studyWorkbenchClient{request: func(action string, q api.StudyQuery, raw json.RawMessage) (api.StudyDocument, error) {
		var d api.StudyDocument
		if action == "current" {
			reads++
			_ = json.Unmarshal(studyJSON(map[string]any{"run": map[string]any{"session_id": q.Kind + "原会话", "run_id": q.Kind + "原运行", "status": "succeeded"}}), &d)
			return d, nil
		}
		if action != "start" || reads != 2 {
			t.Fatal("提交时不应刷新或重新选择运行上下文", action, reads)
		}
		var body api.StudyDocument
		_ = json.Unmarshal(raw, &body)
		if body.String("session_id") != "start_learning原会话" || body.String("operation_id") != "原操作" || body.Number("expected_version") != 1 {
			t.Fatal("未复用展示时的运行归属", string(raw))
		}
		writes++
		_ = json.Unmarshal([]byte(`{"run_id":"新运行"}`), &d)
		return d, nil
	}}}
	a := &App{}
	p, err := a.workbenchStudy(t.Context(), c, workbench.Request{Page: "study-goal", Resource: "明确目标"}, workbench.Page{})
	if err != nil {
		t.Fatal(err)
	}
	request := workbench.Request{Page: "study-goal", Resource: "明确目标", Action: "start", Basis: p.Basis, Version: p.Version, Operation: "原操作", Entity: "不应创建的运行会话", Values: map[string]string{"requests": "10", "tokens": "20000", "prompt": "Go"}}
	for range 2 {
		next, err := a.workbenchStudy(t.Context(), c, request, workbench.Page{})
		if err != nil || next.Redirect != "study-run/新运行" {
			t.Fatal(next, err)
		}
	}
	if writes != 2 {
		t.Fatal(writes)
	}
}
