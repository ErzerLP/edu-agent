package blackbox

import (
	"net/http"
	"testing"
)

func TestBlackBoxStrictSchemaContinuation(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Understand the stable concept and its verification step")
	credential := h.pairCredential("", "严格教学协议验收")
	var capabilities struct {
		Compatible       bool `json:"compatible"`
		StructuredJSON   bool `json:"structured_json"`
		NativeJSONSchema bool `json:"native_json_schema"`
	}
	h.authenticatedJSON(http.MethodGet, h.serverURL+"/v1/model/capabilities", credential.Token, nil, http.StatusOK, &capabilities)
	if !capabilities.Compatible || !capabilities.StructuredJSON || !capabilities.NativeJSONSchema {
		t.Fatalf("教学连接探测未通过：%+v", capabilities)
	}

	// 显式继续原会话，与 issue 的生产入口一致。
	result := h.runCLI(h.primaryHome, standardTeachingInput().
		answer("accepted response").defaultHelp().acknowledgeFeedback().String(), "learn", "--session", sessionID)
	requireExit(t, result, 0, "严格 schema 续学")
	for _, marker := range []string{"Current: explanation (not scored)", "Question:", "disposition=accepted", "completed"} {
		requireContains(t, result.stdout, marker, "续学结果")
	}
	assertSessionCount(t, h, "原会话完成", `SELECT count(*) FROM tutoring_sessions WHERE id=$1 AND state='Completed'`, sessionID, 1)
	counts := h.fakeAuditCounts()
	for _, kind := range []string{"route", "explanation", "activity", "assessment"} {
		if counts[kind] != 1 {
			t.Fatalf("教学模型请求未成功：%s=%d", kind, counts[kind])
		}
	}
}
