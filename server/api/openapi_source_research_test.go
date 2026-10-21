package api_test

import (
	"context"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestSourceResearchContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/capabilities", "/jobs", "/jobs/{jobID}", "/jobs/{jobID}/events", "/jobs/{jobID}/candidates", "/jobs/{jobID}/reports/{revisionID}", "/jobs/{jobID}/selection", "/jobs/{jobID}/commands", "/jobs/{jobID}/import-previews", "/jobs/{jobID}/report-imports", "/jobs/{jobID}/imports"} {
		item := doc.Paths.Find("/v1/learning/source-research" + suffix)
		if item == nil {
			t.Fatalf("missing source research route: %s", suffix)
		}
		for _, op := range item.Operations() {
			if op.Security == nil || op.Extensions["x-required-scope"] == nil {
				t.Fatalf("missing auth contract: %s", suffix)
			}
		}
	}
	if doc.Paths.Find("/v1/knowledge/source-discovery") != nil {
		t.Fatal("removed discovery route remains public")
	}
	scale := doc.Components.Schemas["SourceResearchLimits"].Value
	if err = scale.VisitJSON(map[string]any{"max_sources_per_round": float64(3), "max_pages_per_round": float64(8)}, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatal(err)
	}
	if err = scale.VisitJSON(map[string]any{"max_sources_per_round": float64(21), "max_pages_per_round": float64(8)}, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("unbounded research scale")
	}
	if err = doc.Components.Schemas["SourceResearchJob"].Value.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
