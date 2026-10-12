package api_test

import (
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestStudioGenerationContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/learning/studio/generations", "/v1/learning/studio/artifacts/{artifactID}/sources/{citationID}"} {
		if doc.Paths.Find(path) == nil {
			t.Fatal("missing Studio generation route", path)
		}
	}
	for _, name := range []string{"StudioGenerate", "StudioGuide", "StudioBrief", "StudioClaim", "StudioState"} {
		if doc.Components.Schemas[name] == nil {
			t.Fatal("missing schema", name)
		}
	}
}
