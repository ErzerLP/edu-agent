package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func TestMCPTutoringScopeIsExplicitAndNeverFallsBack(t *testing.T) {
	handler, _, _, service, _, _ := newProtocolFixture(t, []string{"learning:read", "learning:write"})
	const space = "10000000-0000-4000-8000-000000000001"
	const id = "20000000-0000-4000-8000-000000000001"
	service.err = &learning.Error{Code: learning.CodeNotFound}
	for _, name := range []string{"tutoring.create_session", "tutoring.propose", "tutoring.apply_action"} {
		args := map[string]any{"learning_space_id": space, "aggregate_type": "session", "aggregate_id": id}
		if name == "tutoring.propose" {
			args["request_id"], args["proposal_type"], args["aggregate_version"] = id, "route", 1
			args["knowledge_revision_id"], args["input"] = id, map[string]any{}
			args["node_revision_ids"] = []string{id}
		} else {
			args["operation_id"], args["payload_schema_version"], args["expected_version"] = id, 1, 1
			if name == "tutoring.create_session" {
				args["goal_revision_id"] = id
				args["expected_version"] = 0
			} else {
				args["session_id"], args["action"] = id, "start_diagnostic"
			}
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		before := service.calls
		response := directMCPRequest(handler, "POST", string(body), testToken)
		if service.calls != before+1 || service.lastTeachingSpace != space || !strings.Contains(response.Body.String(), "not_found") {
			t.Fatalf("%s 未保留来源或回退：%s", name, response.Body)
		}
		response = directMCPRequest(handler, "POST", strings.Replace(string(body), space, "invalid", 1), testToken)
		if service.calls != before+1 {
			t.Fatalf("%s 无效区调用了应用服务：%s", name, response.Body)
		}
	}
	for _, test := range []struct{ uri, want string }{
		{"edu-agent://spaces/" + space + "/tutoring/sessions/" + id, space},
		{"edu-agent://tutoring/sessions/" + id, learningspace.DefaultID},
	} {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "resources/read", "params": map[string]any{"uri": test.uri}})
		response := directMCPRequest(handler, "POST", string(body), testToken)
		if service.lastTeachingSpace != test.want || !strings.Contains(response.Body.String(), "not_found") {
			t.Fatalf("读取会话丢失范围：%s", response.Body)
		}
	}
}
