package api_test

import (
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestStudyDirectionContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema := doc.Paths.Find("/v1/learning/conversations").Post.RequestBody.Value.Content["application/json"].Schema.Value
	id := "10000000-0000-4000-8000-000000000001"
	for _, body := range []any{map[string]any{"id": id, "saved": false}, map[string]any{"id": id, "mode": "study"}} {
		if err = schema.VisitJSON(body, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []any{map[string]any{"id": id}, map[string]any{"id": id, "mode": "invalid", "saved": true}} {
		if schema.VisitJSON(body, openapi3.EnableJSONSchema2020()) == nil {
			t.Fatal("accepted invalid conversation", body)
		}
	}
	space := doc.Components.Schemas["LearningSpaceCommand"].Value
	body := map[string]any{"operation_id": id, "expected_version": 0, "name": "代数", "description": "", "status": "active", "direction": "宽泛方向"}
	if err = space.VisitJSON(body, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatal(err)
	}
	body["direction"] = nil
	if space.VisitJSON(body, openapi3.EnableJSONSchema2020()) == nil {
		t.Fatal("null direction accepted")
	}
}
