package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func TestIssue30KnowledgeStructureCapabilities(t *testing.T) {
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"knowledge:read"}}}
	handler := newKnowledgeMaintenanceHTTP(t, id, &fakeKnowledge{}, 4096)
	r := maintenanceRequest(handler, http.MethodGet, "/v1/knowledge/structure/capabilities", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"protocol_version":1`) {
		t.Fatalf("缺少可查询的知识结构能力入口: status=%d body=%s", r.Code, r.Body.String())
	}
}

type fakeStructureKnowledge struct {
	fakeKnowledge
	knowledgeStructure
	command  knowledge.StructureCommand
	decision knowledge.StructureDecision
	space    string
}

func (f *fakeStructureKnowledge) SupportsStructure() bool { return true }
func (f *fakeStructureKnowledge) ReadStructure(ctx context.Context, _ knowledge.StructureQuery) (knowledge.StructurePage, error) {
	f.space = learningspace.Scope(ctx)
	return knowledge.StructurePage{Items: []knowledge.StructureNode{}, Edges: []knowledge.StructureEdge{}}, nil
}
func (f *fakeStructureKnowledge) CreateStructureProposal(ctx context.Context, c knowledge.StructureCommand) (knowledge.StructureProposal, error) {
	f.space, f.command = learningspace.Scope(ctx), c
	return knowledge.StructureProposal{ID: maintenanceProposalID, Status: "open"}, nil
}
func (f *fakeStructureKnowledge) DecideStructureProposal(_ context.Context, _ string, c knowledge.StructureDecision) (knowledge.StructureProposal, error) {
	f.decision = c
	return knowledge.StructureProposal{ID: maintenanceProposalID, Status: "applied"}, nil
}

func TestIssue30KnowledgeStructureHTTPAuthorityAndClosedInputs(t *testing.T) {
	service := &fakeStructureKnowledge{}
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"knowledge:read", "knowledge:write", "learning:approve"}}}
	handler := newKnowledgeMaintenanceHTTP(t, id, service, 4096)
	body := `{"operation_id":"a0000000-0000-4000-8000-000000000004","kind":"edit","reason":"人工审阅","base_version":0,"generation":1,"edits":[]}`
	r := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/structure/proposals", body)
	if r.Code != http.StatusOK || service.command.ActorDeviceID != maintenanceDeviceID || service.space != learningspace.DefaultID || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("未注入可信身份或未禁止缓存: status=%d command=%+v space=%s headers=%v", r.Code, service.command, service.space, r.Header())
	}
	spoof := strings.TrimSuffix(body, "}") + `,"actor_device_id":"a0000000-0000-4000-8000-000000000099"}`
	if r = maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/structure/proposals", spoof); r.Code != http.StatusBadRequest {
		t.Fatalf("接受客户端伪造 actor: %d %s", r.Code, r.Body.String())
	}
	if r = maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/structure/proposals", strings.Replace(body, `"edit"`, `"compensate"`, 1)); r.Code != http.StatusForbidden {
		t.Fatalf("教学批准权限越权执行知识补偿: %d %s", r.Code, r.Body.String())
	}
	decision := `{"operation_id":"a0000000-0000-4000-8000-000000000005","hash":"reviewed","decision":"approve","reason":"核对完成"}`
	path := "/v1/knowledge/structure/proposals/" + maintenanceProposalID + "/decisions"
	if r = maintenanceRequest(handler, http.MethodPost, path, decision); r.Code != http.StatusForbidden || service.decision.OperationID != "" {
		t.Fatalf("缺少知识批准权限仍调用审批: %d %s", r.Code, r.Body.String())
	}
	id.auth.Scopes = append(id.auth.Scopes, "knowledge:approve")
	if r = maintenanceRequest(handler, http.MethodPost, path, decision); r.Code != http.StatusOK || service.decision.ActorDeviceID != maintenanceDeviceID {
		t.Fatalf("知识批准未注入可信身份: %d %s", r.Code, r.Body.String())
	}
	for _, query := range []string{"limit=1&limit=2", "limit=not-a-number", "unexpected=true"} {
		if r = maintenanceRequest(handler, http.MethodGet, "/v1/knowledge/structure?"+query, ""); r.Code != http.StatusBadRequest {
			t.Fatalf("未拒绝非合同查询 %q: %d %s", query, r.Code, r.Body.String())
		}
	}
}
