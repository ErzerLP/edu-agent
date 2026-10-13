package api_test

import (
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestStudyExerciseContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/learning/exercises/{exerciseID}", "/v1/learning/exercises/{exerciseID}/commands", "/v1/learning/exercises/{exerciseID}/help", "/v1/learning/exercises/{exerciseID}/help/{operationID}", "/v1/learning/exercises/{exerciseID}/assessment/resume", "/v1/learning/exercises/{exerciseID}/citations/{referenceID}"} {
		if doc.Paths.Find(path) == nil {
			t.Fatalf("missing exercise contract: %s", path)
		}
	}
	schema := doc.Components.Schemas["StudyExerciseCommand"].Value
	base := map[string]any{"operation_id": "10000000-0000-4000-8000-000000000001", "kind": "defer"}
	if err = schema.VisitJSON(base); err != nil {
		t.Fatal(err)
	}
	base["kind"] = "complete"
	if schema.VisitJSON(base) == nil {
		t.Fatal("defer contract allowed completion")
	}
	base["kind"] = "resume"
	base["device_id"] = "forged"
	if schema.VisitJSON(base) == nil {
		t.Fatal("caller identity accepted")
	}
	id := "10000000-0000-4000-8000-000000000001"
	view := mentorrun.StudyExerciseView{ID: id, ConversationID: id, RunID: id, SpaceID: id, Generation: 1, SessionID: id, ActivityID: id, ArtifactID: id, ContentVersion: 1, SessionVersion: 4, Prompt: "解释偶数", AllowedHelp: []learning.HelpLevel{learning.HelpNone, learning.HelpHint}}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var wire any
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if err = doc.Components.Schemas["StudyExercise"].Value.VisitJSON(wire); err != nil {
		t.Fatal("public card projection diverges from contract", err)
	}
}
