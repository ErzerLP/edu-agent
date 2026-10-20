package api_test

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/getkin/kin-openapi/openapi3"
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

func TestStudioExerciseContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/learning/studio/exercises/{exerciseID}", "/v1/learning/studio/exercises/{exerciseID}/help", "/v1/learning/studio/exercises/{exerciseID}/help/{operationID}", "/v1/learning/studio/exercises/{exerciseID}/assessment/resume"} {
		if doc.Paths.Find(path) == nil {
			t.Fatal("missing Studio exercise route", path)
		}
	}
	for _, name := range []string{"StudioExercise", "StudioQuizDirection"} {
		if doc.Components.Schemas[name] == nil {
			t.Fatal("missing schema", name)
		}
	}
	id := "10000000-0000-4000-8000-000000000001"
	v := mentorrun.StudioExerciseView{StudioArtifactID: id, StudioVersion: 1, Question: 1, PracticeView: mentorrun.PracticeView{ID: id, SpaceID: id, Generation: 1, SessionID: id, ActivityID: id, ArtifactID: id, ContentVersion: 1, SessionVersion: 4, Prompt: "解释偶数", AllowedHelp: []learning.HelpLevel{learning.HelpHint}}}
	raw, _ := json.Marshal(v)
	var wire any
	_ = json.Unmarshal(raw, &wire)
	if err = doc.Components.Schemas["StudioExercise"].Value.VisitJSON(wire); err != nil {
		t.Fatal("Studio card DTO diverges", err)
	}
}

func TestStudioDirectionRequestContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	id := "10000000-0000-4000-8000-000000000001"
	v := map[string]any{"operation_id": id, "privacy_generation": 1, "kind": "quiz", "goal_id": nil, "goal_version": 0, "mode": "direction", "direction_version": 1, "artifact_id": nil, "expected_version": 0, "source_selection": map[string]any{"mode": "items", "items": []any{map[string]any{"collection_id": id, "revision_id": id, "document_id": id, "document_revision_id": id}}}}
	schema := doc.Components.Schemas["StudioGenerate"].Value
	visit := func(v map[string]any) error {
		raw, _ := json.Marshal(v)
		var wire any
		_ = json.Unmarshal(raw, &wire)
		return schema.VisitJSON(wire)
	}

	if err = visit(v); err != nil {
		t.Fatal("direction request invalid", err)
	}
	v["goal_id"], v["goal_version"] = id, 1
	if visit(v) == nil {
		t.Fatal("contract admits mixed goal and direction")
	}
	delete(v, "mode")
	delete(v, "direction_version")
	if err = visit(v); err != nil {
		t.Fatal("legacy request no longer valid", err)
	}
}
