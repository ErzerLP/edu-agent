package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func (f *testLearning) Progress(ctx context.Context, q learning.ProgressQuery) (learning.ProgressPage, error) {
	f.calls++
	f.lastGoalSpace = learningspace.Scope(ctx)
	f.progressQuery = q
	return learning.ProgressPage{Items: []learning.GoalProgress{}}, f.err
}

func TestMCPProgressPreservesScopeAndPagination(t *testing.T) {
	h, _, _, svc, _, _ := newProtocolFixture(t, []string{"learning:read"})
	const space = "10000000-0000-4000-8000-000000000001"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"learning.progress","arguments":{"learning_space_id":"` + space + `","global":true,"status":"paused","limit":2,"cursor":"opaque"}}}`
	w := directMCPRequest(h, "POST", body, testToken)
	if svc.calls != 1 || svc.lastGoalSpace != space || !svc.progressQuery.Global || svc.progressQuery.Limit != 2 || svc.progressQuery.Cursor != "opaque" || !strings.Contains(w.Body.String(), "items") {
		t.Fatalf("MCP 范围分页丢失：%+v %s", svc.progressQuery, w.Body)
	}
}
