package httpapi

import (
	"bytes"
	"context"
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
	return learning.ProgressPage{Items: []learning.GoalProgress{}, Total: 0}, f.err
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
