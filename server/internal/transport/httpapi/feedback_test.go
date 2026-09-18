package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

func (f *fakeLearning) ListFeedback(_ context.Context, q learning.FeedbackQuery) (learning.FeedbackPage, error) {
	f.called("feedback_list", "")
	f.feedbackQuery = q
	return learning.FeedbackPage{Items: []learning.FeedbackSummary{}}, f.err
}
func (f *fakeLearning) Feedback(ctx context.Context, id string, byAssessment bool) (learning.FeedbackView, error) {
	f.called("feedback", "")
	f.feedbackID = id
	f.feedbackByAssessment = byAssessment
	if f.feedbackFn != nil {
		return f.feedbackFn(ctx)
	}
	return learning.FeedbackView{}, f.err
}

func TestFeedbackPrivacyBarrierDiscardsLateBody(t *testing.T) {
	const secret = "清除后不得返回的原答案"
	for _, owner := range []privacy.OwnerKind{privacy.OwnerKnowledge, privacy.OwnerLearning, privacy.OwnerTutoring} {
		t.Run(string(owner), func(t *testing.T) {
			manager := privacy.NewReadPermitManager()
			started := make(chan struct{})
			service := &fakeLearning{feedbackFn: func(ctx context.Context) (learning.FeedbackView, error) {
				close(started)
				<-ctx.Done()
				return learning.FeedbackView{Attempt: learning.Attempt{Answer: secret}}, nil
			}}
			var logs bytes.Buffer
			handler := newLearningTestAPIWithPermits(t, []string{"learning:read"}, service, manager, &logs)
			responses := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				responses <- learningRequest(t, handler, http.MethodGet, "/v1/learning/attempts/"+testRelatedID+"/feedback", "")
			}()
			<-started
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := manager.CloseAndDrain(ctx, 2, owner); err != nil {
				t.Fatal(err)
			}
			response := <-responses
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
				t.Fatalf("清除竞态泄漏原答案：status=%d", response.Code)
			}
		})
	}
}

func TestFeedbackHTTPPermissionsIdentityAndStrictQueries(t *testing.T) {
	var logs bytes.Buffer
	service := &fakeLearning{}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	for _, path := range []string{"/v1/learning/assessments?status=provisional&limit=10", "/v1/learning/assessments/" + testAssessment, "/v1/learning/attempts/" + testRelatedID + "/feedback"} {
		response := learningRequest(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("反馈查询失败：%s %d %s", path, response.Code, response.Body.String())
		}
	}
	if service.feedbackQuery.Status != "provisional" || service.feedbackQuery.Page.Limit != 10 || service.feedbackID != testRelatedID || service.feedbackByAssessment {
		t.Fatal("未绑定原答案或筛选")
	}
	before := service.calls
	for _, path := range []string{"/v1/learning/assessments?current=true", "/v1/learning/assessments?limit=unknown", "/v1/learning/attempts/not-a-uuid/feedback"} {
		if response := learningRequest(t, handler, http.MethodGet, path, ""); response.Code != 400 || service.calls != before {
			t.Fatalf("无效查询进入服务：%s", path)
		}
	}
	handler = newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
	if response := learningRequest(t, handler, http.MethodGet, "/v1/learning/assessments", ""); response.Code != 403 || service.calls != before {
		t.Fatal("无读取权限仍能查询反馈")
	}
	handler = newLearningTestAPI(t, []string{"learning:write", "learning:approve"}, service, &logs)
	body := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"aggregate_type":"session","aggregate_id":"` + testAggregateID + `","expected_version":7,"kind":"confirm","expected_disposition_version":1,"reason":"核对原答案与来源后确认"}`
	if response := learningRequest(t, handler, http.MethodPost, "/v1/learning/assessments/"+testAssessment+"/decisions", body); response.Code != 201 || service.decision.Reason != "核对原答案与来源后确认" {
		t.Fatalf("复核原因未到达正式 owner：%d", response.Code)
	}
}
