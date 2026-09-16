package api_test

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestResearchContractMatchesSourceAndPolicies(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"ResearchSource":  research.Source{SpaceID: uuid.NewString(), GoalID: uuid.NewString(), Purpose: "goal_reference", ID: uuid.NewString(), Status: "candidate", Fragments: []research.Fragment{}},
		"ResearchRequest": research.Request{Topic: "公开主题", ExternalConsent: true, Policy: research.Policy{Mode: "supplement", Domains: []string{}}},
	} {
		raw, _ := json.Marshal(value)
		var decoded any
		_ = json.Unmarshal(raw, &decoded)
		if err = doc.Components.Schemas[name].Value.VisitJSON(decoded, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("%s 合同不一致：%v", name, err)
		}
	}
	for _, path := range []string{"/v1/learning/goals/{goalID}/research", "/v1/learning/runs/{runID}/sources", "/v1/learning/runs/{runID}/sources/{sourceID}", "/v1/learning/runs/{runID}/sources/{sourceID}/decisions"} {
		item := doc.Paths.Find(path)
		if item == nil {
			t.Fatalf("缺少研究接口 %s", path)
		}
		for _, operation := range item.Operations() {
			if operation.Security == nil || operation.Extensions["x-required-scope"] == nil {
				t.Fatalf("缺少范围与身份 %s", path)
			}
		}
	}
}
