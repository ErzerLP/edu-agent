package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const studyGoal = "10000000-0000-4000-8000-000000000001"
const studySession = "20000000-0000-4000-8000-000000000001"
const studyRun = "30000000-0000-4000-8000-000000000001"
const studyArtifact = "40000000-0000-4000-8000-000000000001"
const studyOperation = "50000000-0000-4000-8000-000000000001"

func studyTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func studyContentFixture(kind string) map[string]any {
	return map[string]any{"protocol_version": 1, "artifact_id": studyArtifact, "version": 2, "committed_version": 2, "learning_space_id": DefaultLearningSpaceID, "goal_id": studyGoal, "session_id": studySession, "activity_id": studyRun, "status": "committed", "body": map[string]any{"blocks": []any{map[string]any{"kind": "future_diagram", "fallback": "文字替代", "text": "不能猜测的内容"}}, "interaction": map[string]any{"kind": kind}}}
}

func TestStudyContentNegotiationAndUnknownInteraction(t *testing.T) {
	posts := 0
	fixture := studyContentFixture("future_drag")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning/content/capabilities":
			studyTestJSON(w, map[string]any{"protocol_version": 1, "available": true})
		case "/v1/learning/content/" + studyArtifact:
			if r.Header.Get("X-Learning-Content-Version") != "1" || r.Header.Get("X-Learning-Space-ID") != DefaultLearningSpaceID {
				t.Error("未协商正文和区")
			}
			studyTestJSON(w, fixture)
		case "/v1/learning/content/" + studyArtifact + "/answers":
			posts++
			studyTestJSON(w, map[string]any{})
		default:
			t.Errorf("非预期路径 %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	c := NewClient(s.URL, "fixture", time.Second, nil)
	d, err := c.Study(t.Context(), "content", StudyQuery{Artifact: studyArtifact}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ContentText(d); !strings.Contains(got, "文字替代") || strings.Contains(got, "不能猜测") || !strings.Contains(got, "禁止提交") {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(map[string]any{"operation_id": studyOperation, "aggregate_id": studySession, "expected_version": 3, "content_version": 2, "action": "submit_attempt", "answer": "A"})
	_, err = c.Study(t.Context(), "answer", StudyQuery{Artifact: studyArtifact}, raw)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "learning_content_upgrade_required" || posts != 0 {
		t.Fatalf("未知规则发出答案：%v posts=%d", err, posts)
	}
	fixture["learning_space_id"] = studyGoal
	if _, err = c.Study(t.Context(), "content", StudyQuery{Artifact: studyArtifact}, nil); err == nil {
		t.Fatal("跨区内容未拒绝")
	}
}

func TestStudyWritesOnceAndKeepsOperationIdentity(t *testing.T) {
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			studyTestJSON(w, map[string]any{"schema_version": 1, "research": map[string]any{"available": true}})
			return
		}
		if r.URL.Path != "/v1/learning/goals/"+studyGoal+"/runs" || r.Header.Get("X-Learning-Space-ID") != DefaultLearningSpaceID {
			t.Error("运行归属错误")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["operation_id"] != studyOperation || body["session_id"] != studySession {
			t.Error("重写了操作身份")
		}
		posts++
		w.WriteHeader(502)
	}))
	defer s.Close()
	c := NewClient(s.URL, "fixture", time.Second, nil)
	raw, _ := json.Marshal(map[string]any{"operation_id": studyOperation, "session_id": studySession, "expected_version": 1, "prompt": "Go", "save": true, "research": map[string]any{"topic": "Go", "external_consent": true}})
	if _, err := c.Study(t.Context(), "research", StudyQuery{Goal: studyGoal}, raw); err == nil || posts != 1 {
		t.Fatalf("写操作被重试：posts=%d err=%v", posts, err)
	}
}

func TestStudyDisabledStillReadsAndClears(t *testing.T) {
	clears := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			studyTestJSON(w, map[string]any{"schema_version": 1, "research": map[string]any{"available": false}})
		case "/v1/learning/runs/" + studyRun:
			studyTestJSON(w, map[string]any{"run_id": studyRun, "session_id": studySession, "goal_id": studyGoal, "space_id": DefaultLearningSpaceID, "version": 3, "status": "succeeded"})
		case "/v1/learning/runs/" + studyRun + "/commands":
			clears++
			studyTestJSON(w, map[string]any{"run_id": studyRun, "session_id": studySession, "operation_id": studyOperation, "version": 4})
		default:
			t.Errorf("功能关闭仍发起 %s", r.URL.Path)
		}
	}))
	defer s.Close()
	c := NewClient(s.URL, "fixture", time.Second, nil)
	if _, err := c.Study(t.Context(), "run", StudyQuery{Run: studyRun}, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"operation_id": studyOperation, "expected_version": 3, "kind": "clear"})
	if _, err := c.Study(t.Context(), "run-command", StudyQuery{Run: studyRun}, raw); err != nil || clears != 1 {
		t.Fatal(err, clears)
	}
	raw, _ = json.Marshal(map[string]any{"operation_id": studyOperation, "session_id": studySession, "expected_version": 1, "save": true, "research": map[string]any{"topic": "Go", "external_consent": true}})
	_, err := c.Study(t.Context(), "research", StudyQuery{Goal: studyGoal}, raw)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "learning_services_disabled" {
		t.Fatal(err)
	}
}

func TestStudyOldServerAndFutureSchemaExplainUpgrade(t *testing.T) {
	for _, version := range []int{0, 2} {
		t.Run(strconvForStudy(version), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if version == 0 {
					w.WriteHeader(404)
				} else {
					studyTestJSON(w, map[string]any{"schema_version": version})
				}
			}))
			defer s.Close()
			_, err := NewClient(s.URL, "fixture", time.Second, nil).Study(context.Background(), "runs", StudyQuery{}, nil)
			var e *APIError
			if !errors.As(err, &e) || e.Code != "learning_services_upgrade_required" {
				t.Fatal(err)
			}
		})
	}
}
func strconvForStudy(n int) string {
	if n == 0 {
		return "旧服务"
	}
	return "未来协议"
}

func TestStudyCurrentSelectsExplicitGoalAndKind(t *testing.T) {
	goal := studyGoal
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			studyTestJSON(w, map[string]any{"schema_version": 1})
			return
		}
		if r.URL.Path != "/v1/learning/goals/"+studyGoal+"/start" || r.Header.Get("X-Learning-Space-ID") != DefaultLearningSpaceID {
			t.Error("未明确选择目标、类型或学习区", r.URL.Path)
		}
		studyTestJSON(w, map[string]any{"run": map[string]any{"run_id": studyRun, "session_id": studySession, "goal_id": goal, "space_id": DefaultLearningSpaceID, "version": 3, "kind": "start_learning"}})
	}))
	defer s.Close()
	c := NewClient(s.URL, "fixture", time.Second, nil)
	q := StudyQuery{Goal: studyGoal, Kind: "start_learning"}
	d, err := c.Study(t.Context(), "current", q, nil)
	if err != nil || d.Object("run").String("session_id") != studySession {
		t.Fatal(d, err)
	}
	goal = studyArtifact
	if _, err := c.Study(t.Context(), "current", q, nil); err == nil {
		t.Fatal("未拒绝其他目标的运行")
	}
}

func TestStudyChangeDecisionRequiresExactReviewedIdentity(t *testing.T) {
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			studyTestJSON(w, map[string]any{"protocol_version": 1, "available": true})
			return
		}
		if r.Header.Get("X-Learning-Change-Version") != "1" || r.Header.Get("X-Learning-Space-ID") != DefaultLearningSpaceID {
			t.Error("变更未明确协商")
		}
		var b StudyDocument
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Number("expected_revision") != 7 || b.String("hash") != strings.Repeat("a", 64) || b.String("interaction_id") != studyOperation {
			t.Error("未保留审阅版本")
		}
		posts++
		studyTestJSON(w, map[string]any{"id": studyArtifact, "goal_id": studyGoal, "session_id": studySession, "learning_space_id": DefaultLearningSpaceID, "revision": 7, "status": "queued_for_boundary"})
	}))
	defer s.Close()
	c := NewClient(s.URL, "fixture", time.Second, nil)
	body := map[string]any{"operation_id": studyOperation, "session_id": studySession, "action": "approve", "expected_revision": 7, "hash": strings.Repeat("a", 64), "interaction_id": studyOperation, "immediate": false}
	raw, _ := json.Marshal(body)
	d, err := c.Study(t.Context(), "change-command", StudyQuery{Goal: studyGoal, Change: studyArtifact}, raw)
	if err != nil || d.String("status") != "queued_for_boundary" || posts != 1 {
		t.Fatal(d, err, posts)
	}
	delete(body, "hash")
	raw, _ = json.Marshal(body)
	if _, err = c.Study(t.Context(), "change-command", StudyQuery{Goal: studyGoal, Change: studyArtifact}, raw); err == nil || posts != 1 {
		t.Fatal("无具体 hash 仍提交")
	}
}
