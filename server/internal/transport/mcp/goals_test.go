package mcp

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestMCPGoalCreationKeepsScopeAndRejectsLifecycleArguments(t *testing.T) {
	handler, _, _, service, _, _ := newProtocolFixture(t, []string{"learning:write"})
	service.err = &learning.Error{Code: learning.CodeNotFound}
	const spaceID = "10000000-0000-4000-8000-000000000001"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"learning.create_goal","arguments":{"operation_id":"20000000-0000-4000-8000-000000000001","payload_schema_version":1,"aggregate_type":"goal","aggregate_id":"30000000-0000-4000-8000-000000000001","expected_version":0,"text":"学习 Go","source":"mcp","learning_space_id":"` + spaceID + `"}}}`
	rec := directMCPRequest(handler, "POST", body, testToken)
	if service.calls != 1 || service.lastGoalSpace != spaceID || service.actor != testDeviceID || !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("目标创建未转发范围：%s", rec.Body)
	}
	for _, invalid := range []string{strings.Replace(body, spaceID, "invalid", 1), strings.Replace(body, `"source":"mcp"`, `"source":"mcp","action":"complete"`, 1)} {
		rec = directMCPRequest(handler, "POST", invalid, testToken)
		if service.calls != 1 {
			t.Fatalf("新增无授权状态入口或范围绕过：%s", rec.Body)
		}
	}
}
