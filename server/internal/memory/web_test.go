package memory

import (
	"context"
	"testing"
	"time"
)

func TestWebCandidateRequiresReviewWithoutChangingLegacyAdmission(t *testing.T) {
	store := &recordingStore{}
	now := time.Now().UTC()
	service, err := NewService(store, ServiceOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	command := CreateCandidateCommand{OperationID: testOperation, Content: "我偏好简洁的分步解释", Reason: "明确偏好", Category: CategoryInteractionPreference, Sensitivity: SensitivityNonSensitive, Stability: StabilityStable, ValidUntil: now.Add(time.Hour)}
	if _, err = service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	legacyHash := store.plan.Operation.RequestHash
	if store.plan.AutomaticDecision == nil {
		t.Fatal("旧准入合同改变")
	}
	command.RequireReview = true
	if _, err = service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.plan.AutomaticDecision != nil || store.plan.DeliveryID != "" || store.plan.Candidate.Status != CandidatePending || store.plan.Operation.RequestHash == legacyHash {
		t.Fatal("Web 未确认已写记录，或幂等摘要没有绑定审阅策略")
	}
	command.Content = "参考答案：第一题选 A"
	if _, err = service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.plan.Candidate.Status != CandidateRejected {
		t.Fatal("强制审阅绕过禁止内容规则")
	}
}
