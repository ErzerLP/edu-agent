package api_test

import (
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestSourceDiscoveryContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := doc.Paths.Find("/v1/knowledge/source-discovery")
	if path == nil || path.Post == nil {
		t.Fatal("missing discovery route")
	}
	if path.Post.Security == nil || path.Post.Extensions["x-required-scopes"] == nil {
		t.Fatal("missing authorization")
	}
	for name, value := range map[string]any{
		"SourceDiscoveryRequest": research.DiscoveryRequest{Keywords: "public", ExternalConsent: true, Policy: research.Policy{Mode: "supplement", Domains: []string{}}},
		"SourceDiscoveryResult":  research.DiscoveryResult{Items: []research.DiscoveryCandidate{{Title: "Public", URL: "https://example.org/", Summary: "summary", Status: "summary_only"}}, RequestBudget: 1, RequestsUsed: 1, CostEstimate: "unknown"},
	} {
		schema := doc.Components.Schemas[name]
		if schema == nil {
			t.Fatal("missing DTO", name)
		}
		raw, _ := json.Marshal(value)
		var decoded any
		_ = json.Unmarshal(raw, &decoded)
		if err := schema.Value.VisitJSON(decoded, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatal(name, err)
		}
		if name == "SourceDiscoveryRequest" {
			object := decoded.(map[string]any)
			object["goal_id"] = "private"
			if schema.Value.VisitJSON(object, openapi3.EnableJSONSchema2020()) == nil {
				t.Fatal("private context accepted")
			}
			delete(object, "goal_id")
			object["external_consent"] = false
			if schema.Value.VisitJSON(object, openapi3.EnableJSONSchema2020()) == nil {
				t.Fatal("unconfirmed request accepted")
			}
		}
	}
}
