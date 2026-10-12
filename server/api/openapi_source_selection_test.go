package api_test

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestSourceSelectionContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ref := doc.Components.Schemas["SourceSelection"]
	if ref == nil {
		t.Fatal("missing SourceSelection")
	}
	for _, value := range []any{map[string]any{"mode": "none", "items": []any{}}} {
		if err := ref.Value.VisitJSON(value, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []any{nil, map[string]any{}, map[string]any{"mode": "items", "items": []any{}}, map[string]any{"mode": "none", "items": nil}, map[string]any{"mode": "none", "items": []any{}, "all": true}} {
		if ref.Value.VisitJSON(value, openapi3.EnableJSONSchema2020()) == nil {
			t.Fatalf("accepted invalid selection: %#v", value)
		}
	}
	for _, path := range []string{"/v1/learning/goals/{goalID}/runs", "/v1/learning/conversations/{conversationID}/turns"} {
		schema := doc.Paths.Find(path).Post.RequestBody.Value.Content["application/json"].Schema.Value
		if schema.Properties["source_selection"] == nil {
			t.Fatal("missing source selection", path)
		}
	}
	path := doc.Paths.Find("/v1/learning/runs/{runID}/citations/{citationID}")
	if path == nil || path.Get == nil || path.Get.Extensions["x-required-scope"] == nil {
		t.Fatal("missing citation route")
	}
}
