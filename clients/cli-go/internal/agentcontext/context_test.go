package agentcontext

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const testSpace = "10000000-0000-4000-8000-000000000002"
const testGoal = "20000000-0000-4000-8000-000000000002"
const testScope = "30000000-0000-4000-8000-000000000002"
const testTeaching = "40000000-0000-4000-8000-000000000002"

func TestBoundClientNeverFallsBackToGlobalOrCurrent(t *testing.T) {
	var progress, reviews, retrieval, memory atomic.Int32
	var frozen atomic.Bool
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/learning-spaces/capabilities":
			json.NewEncoder(w).Encode(api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "collections_v1", "learning": "goals_v1", "tutoring": "sessions_v1", "memory": "default_only"}})
		case "/v1/learning-spaces/" + testSpace:
			json.NewEncoder(w).Encode(api.LearningSpace{ID: testSpace, Name: "Go", Status: "active", Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
		case "/v1/learning/goals/" + testGoal:
			json.NewEncoder(w).Encode(api.GoalRevision{GoalID: testGoal, SpaceID: testSpace, Revision: 1, Management: &api.GoalManagement{ChangedFields: []string{}, Details: api.GoalDetails{ScopeSnapshotID: testScope}}})
		case "/v1/tutoring/sessions":
			if r.URL.Query().Get("goal_id") != testGoal {
				t.Error("没有按目标校验教学归属")
			}
			json.NewEncoder(w).Encode(api.SessionPage{Items: []api.SessionSummary{{SessionID: testTeaching, LearningSpaceID: testSpace, GoalID: testGoal, GoalRevisionID: testGoal, ScopeSnapshotID: testTeaching}}})
		case "/v1/learning/progress", "/v1/learning/reviews":
			if r.Header.Get("X-Learning-Space-ID") != testSpace || r.URL.Query().Get("goal_id") != testGoal || r.URL.Query().Get("global") != "false" {
				t.Errorf("范围错误：%s %s", r.URL, r.Header.Get("X-Learning-Space-ID"))
			}
			metadata := api.ProjectionMetadata{Generation: testScope, ProjectionVersion: "v1", MasteryReducerVersion: "v1", AssessmentPolicyVersion: "v1", ReviewPolicyVersion: "v1", ReasonCodes: []string{}}
			if strings.HasSuffix(r.URL.Path, "progress") {
				progress.Add(1)
				json.NewEncoder(w).Encode(api.ProgressPage{Items: []api.GoalProgress{}, Metadata: metadata})
			} else {
				reviews.Add(1)
				json.NewEncoder(w).Encode(api.ReviewsPage{Items: []api.ReviewSchedule{}, Metadata: metadata})
			}
		case "/v1/knowledge/retrievals":
			retrieval.Add(1)
			var request api.KnowledgeRetrievalRequest
			json.NewDecoder(r.Body).Decode(&request)
			want := testScope
			if frozen.Load() {
				want = testTeaching
			}
			if request.ScopeSnapshotID != want || request.KnowledgeRevisionID != "" || r.Header.Get("X-Learning-Space-ID") != testSpace {
				t.Errorf("检索范围错误：%+v", request)
			}
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.ErrorBody{Code: "content_redacted", Message: "已清除", RequestID: "test"}})
		case "/v1/memory/export":
			memory.Add(1)
			if r.Header.Get("X-Learning-Space-ID") != api.DefaultLearningSpaceID {
				t.Error("全局偏好被错误分区")
			}
			json.NewEncoder(w).Encode(api.MemoryExportPage{Items: []api.MemoryExportItem{}, ReadGeneration: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}, ReasonCodes: []string{}})
		default:
			t.Errorf("不应调用：%s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer h.Close()
	c := New(api.NewClient(h.URL, "token", time.Second, nil), Binding{SpaceID: testSpace, GoalID: testGoal})
	if _, _, err := c.Validate(t.Context()); err != nil {
		t.Fatalf("归属校验：%v", err)
	}
	if _, err := c.Progress(t.Context(), api.ProgressQuery{Global: true, GoalID: "other", Limit: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ScopedReviews(t.Context(), api.ProgressQuery{Global: true, Limit: 20}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RetrieveKnowledge(t.Context(), api.KnowledgeRetrievalRequest{Query: "channel", QueryContextSchemaVersion: api.QueryContextSchemaVersion}); err == nil {
		t.Fatal("失效范围不应回退")
	}
	if _, err := c.CurrentSession(t.Context()); err == nil {
		t.Fatal("未绑定教学不应选择 current")
	}
	if _, err := c.ExportMemory(t.Context(), "", 1); err != nil {
		t.Fatal(err)
	}
	if progress.Load() != 1 || reviews.Load() != 1 || retrieval.Load() != 1 || memory.Load() != 1 {
		t.Fatal("发生隐式重试或全库回退")
	}
	frozen.Store(true)
	c = c.Rebind(Binding{SpaceID: testSpace, GoalID: testGoal, SessionID: testTeaching})
	if _, err := c.RetrieveKnowledge(t.Context(), api.KnowledgeRetrievalRequest{Query: "channel", QueryContextSchemaVersion: api.QueryContextSchemaVersion}); err == nil || retrieval.Load() != 2 {
		t.Fatal("没有使用教学冻结范围或发生全库回退")
	}
}

type countingModel struct{ calls int }

func (m *countingModel) Complete(context.Context, modelclient.Request) (modelclient.Response, error) {
	m.calls++
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "回答"}}, nil
}

func TestPrivacyGenerationChangeBlocksModelAndCompaction(t *testing.T) {
	var generation atomic.Int64
	generation.Store(1)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/learning-spaces/capabilities":
			json.NewEncoder(w).Encode(api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "collections_v1", "learning": "goals_v1", "tutoring": "sessions_v1", "memory": "default_only"}})
		case "/v1/learning-spaces/" + api.DefaultLearningSpaceID:
			json.NewEncoder(w).Encode(api.LearningSpace{ID: api.DefaultLearningSpaceID, Name: "默认", Status: "active", Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
		case "/v1/memory/export":
			json.NewEncoder(w).Encode(api.MemoryExportPage{Items: []api.MemoryExportItem{}, ReadGeneration: api.MemoryGenerationStamp{LearnerGeneration: generation.Load(), MemoryGeneration: 1}, ReasonCodes: []string{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer h.Close()
	base := &countingModel{}
	guard := &GuardedModel{Base: base, Client: New(api.NewClient(h.URL, "token", time.Second, nil), Binding{})}
	if _, err := guard.Complete(t.Context(), modelclient.Request{}); err != nil {
		t.Fatal(err)
	}
	generation.Store(2)
	if _, err := guard.Complete(t.Context(), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "旧正文"}}}); err == nil || base.calls != 1 {
		t.Fatalf("隐私清除后仍请求模型：%d %v", base.calls, err)
	}
}
