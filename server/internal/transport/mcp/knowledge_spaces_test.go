package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func TestMCPKnowledgeScopeIsForwardedAndNeverFallsBack(t *testing.T) {
	handler, _, service, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
	const spaceID = "10000000-0000-4000-8000-000000000001"
	const scopeID = "20000000-0000-4000-8000-000000000001"
	calls := 0
	service.retrieveFn = func(ctx context.Context, c knowledge.RetrievalCommand) (knowledge.RetrievalResult, error) {
		calls++
		if learningspace.Scope(ctx) != spaceID || c.ScopeSnapshotID == nil || *c.ScopeSnapshotID != scopeID {
			t.Errorf("MCP 丢失资料范围: %+v", c)
		}
		return knowledge.RetrievalResult{}, &knowledge.Error{Code: knowledge.CodeNotFound}
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge.retrieve","arguments":{"query":"channel","learning_space_id":"` + spaceID + `","scope_snapshot_id":"` + scopeID + `"}}}`
	response := directMCPRequest(handler, "POST", body, testToken)
	if calls != 1 || !strings.Contains(response.Body.String(), "not_found") {
		t.Fatalf("范围错误被忽略: calls=%d %s", calls, response.Body)
	}
	response = directMCPRequest(handler, "POST", strings.Replace(body, spaceID, "invalid", 1), testToken)
	if calls != 1 {
		t.Fatalf("错误区 ID 调用了资料服务: %s", response.Body)
	}
}
