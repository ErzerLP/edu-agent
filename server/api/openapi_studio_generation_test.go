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
	for _, path := range []string{"/v1/learning/studio/generations", "/v1/learning/studio/artifacts/{artifactID}/sources/{citationID}", "/v1/learning/studio/artifacts/{artifactID}/quiz", "/v1/learning/studio/artifacts/{artifactID}/quiz/start", "/v1/learning/studio/quiz-operations/{operationID}"} {
		if doc.Paths.Find(path) == nil {
			t.Fatal("missing Studio generation route", path)
		}
	}
	for _, name := range []string{"StudioGenerate", "StudioGuide", "StudioBrief", "StudioClaim", "StudioState", "StudioQuiz", "StudioQuestion", "StudioQuizStart", "StudioQuizState"} {
		if doc.Components.Schemas[name] == nil {
			t.Fatal("missing schema", name)
		}
	}
}
