package api_test

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestStartLearningContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	context := knowledge.KnowledgeContextRevision{ID: id, ScopeSnapshotID: id, Policy: knowledge.KnowledgePolicy{ID: id, GoalRevisionID: id, Request: research.Request{Topic: "公开主题", ExternalConsent: true, AutoAdopt: true, Policy: research.Policy{Mode: "supplement", Domains: []string{}}}}, Concepts: []knowledge.ConceptRevision{{ConceptID: id, RevisionID: id, SemanticKey: "概率", Name: "概率入门", Support: []research.Citation{{SourceID: id, RevisionID: id, FragmentID: id, Quote: "真实片段"}}}}}
	state := learningstart.State{Request: learningstart.Request{NewSession: true, ModelConsent: true}, Result: &learningstart.Result{SessionID: id, ActivityID: id, ArtifactID: id, ArtifactVersion: 1, Context: context}}
	for name, value := range map[string]any{"KnowledgeContextRevision": context, "StartLearningState": state} {
		raw, _ := json.Marshal(value)
		var decoded any
		if err = json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if err = doc.Components.Schemas[name].Value.VisitJSON(decoded, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatal(name, err)
		}
	}
	for _, path := range []string{"/v1/learning/start/capabilities", "/v1/learning/goals/{goalID}/start", "/v1/tutoring/sessions/{sessionID}/knowledge-context"} {
		item := doc.Paths.Find(path)
		if item == nil || item.Get == nil || item.Get.Security == nil || item.Get.Extensions["x-required-scope"] == nil {
			t.Fatal("开学入口缺少合同或权限", path)
		}
	}
}
