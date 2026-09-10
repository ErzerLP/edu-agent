package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func (f *fakeLearning) SupportsProgress() bool { return true }
func (f *fakeLearning) Progress(_ context.Context, q learning.ProgressQuery) (learning.ProgressPage, error) {
	f.calls++
	f.progressQuery = q
	if f.progressPage != nil {
		return *f.progressPage, f.err
	}
	return learning.ProgressPage{Items: []learning.GoalProgress{}, Total: 0}, f.err
}

func TestProgressNormalizesRealReducerOutput(t *testing.T) {
	node := learning.ReduceNode("10000000-0000-4000-8000-000000000001", nil, nil, nil)
	f := &fakeLearning{progressPage: &learning.ProgressPage{Items: []learning.GoalProgress{{Nodes: []learning.NodeReduction{node}}}, Total: 1}}
	h := newLearningTestAPI(t, []string{"learning:read"}, f, &bytes.Buffer{})
	r := httptest.NewRequest(http.MethodGet, "/v1/learning/progress", nil)
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("X-Request-ID", "issue17-progress")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("X-Request-ID") != "issue17-progress" {
		t.Fatal("进度响应缺少成功状态或关联请求标识")
	}
	var page learning.ProgressPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	n := page.Items[0].Nodes[0]
	if n.Mastery.UncertaintyReasons == nil || n.Misconceptions == nil || n.Mastery.ValidEvidenceCount != 0 || n.Mastery.State != learning.MasteryUnseen {
		t.Fatal("真实 reducer 的空集合未规范化，或学习事实被修改")
	}
}

func TestProgressHTTPForwardsScopeAndRejectsAmbiguousInput(t *testing.T) {
	f := &fakeLearning{}
	h := newLearningTestAPI(t, []string{"learning:read"}, f, &bytes.Buffer{})
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("/v1/learning/progress?global=true&status=paused&limit=2&cursor=opaque")
	if w.Code != 200 || !f.progressQuery.Global || f.progressQuery.Status != "paused" || f.progressQuery.Limit != 2 || f.progressQuery.Cursor != "opaque" || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("范围转发错误：%+v %d %s", f.progressQuery, w.Code, w.Body)
	}
	before := f.calls
	w = request("/v1/learning/progress?global=true&global=false")
	if w.Code != 400 || f.calls != before {
		t.Fatal("歧义参数调用了服务")
	}
	f.err = &learning.Error{Code: learning.CodeProjectionUnavailable}
	w = request("/v1/learning/progress")
	if w.Code == 200 || strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("故障变成空列表：%s", w.Body)
	}
}
