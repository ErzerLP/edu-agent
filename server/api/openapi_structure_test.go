package api_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestKnowledgeStructureOpenAPI(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/knowledge/structure", "/v1/knowledge/structure/capabilities", "/v1/knowledge/structure/concepts/{conceptID}", "/v1/knowledge/structure/proposals", "/v1/knowledge/structure/proposals/{proposalID}"} {
		if item := doc.Paths.Find(path); item == nil || item.Get == nil || item.Get.Extensions["x-required-scope"] != "knowledge:read" {
			t.Fatalf("知识读取合同缺失: %s", path)
		}
	}
	if doc.Paths.Find("/v1/knowledge/structure/proposals/{proposalID}/decisions").Post.Extensions["x-required-scope"] != "knowledge:approve" {
		t.Fatal("知识审批权限不独立")
	}
	content := knowledge.ConceptContent{SourceStatus: "candidate", Sources: []knowledge.ConceptSource{}, Claims: []knowledge.ConceptClaim{}, Relations: []knowledge.ConceptRelation{}, ReplacedBy: []string{}}
	node := knowledge.StructureNode{ConceptRevision: knowledge.ConceptRevision{ConceptID: uuid.NewString(), RevisionID: uuid.NewString(), Name: "真实节点", Support: []research.Citation{}}, Content: content, LearningState: "unseen"}
	page := knowledge.StructurePage{Version: 1, Generation: 1, Items: []knowledge.StructureNode{node}, Edges: []knowledge.StructureEdge{}, Notice: "当前授权范围"}
	raw, _ := json.Marshal(page)
	var wire any
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if err = doc.Components.Schemas["KnowledgeStructurePage"].Value.VisitJSON(wire, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatal("生产领域响应与 OpenAPI 不一致", err)
	}
	c := knowledge.StructureCommand{OperationID: uuid.NewString(), Generation: 1, Kind: "edit", Reason: "独立身份", Edits: []knowledge.StructureEdit{{ConceptID: node.ConceptID, Name: node.Name, Content: content}}}
	raw, _ = json.Marshal(c)
	var request map[string]any
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	request["actor_device_id"] = uuid.NewString()
	if err = doc.Components.Schemas["KnowledgeStructureCommand"].Value.VisitJSON(request, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("允许客户端伪造审阅身份")
	}
}
