package mentorrun

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/research"
)

func TestPreparedSchemaUsesOnlySuppliedQuoteCandidates(t *testing.T) {
	sources := []research.Source{{ID: "s", RevisionID: "r", Text: "actual", Fragments: []research.Fragment{{ID: "f", Start: 0, End: 6, Text: "actual"}}}}
	var schema map[string]any
	if err := json.Unmarshal(preparedSchemaFor(sources), &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	quote := properties["citations"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["quote"].(map[string]any)
	values, _ := quote["enum"].([]any)
	if len(values) != 1 || values[0] != "actual" {
		t.Fatalf("model must copy actual supplied quote: %v", values)
	}
	if string(preparedSchema) == string(preparedSchemaFor(sources)) {
		t.Fatal("shared schema was mutated or quote constraint omitted")
	}
}

func TestSynthesisSchemaUsesOnlySuppliedQuoteCandidates(t *testing.T) {
	sources := []research.Source{{ID: "s", RevisionID: "r", Text: "actual", Fragments: []research.Fragment{{ID: "f", Start: 0, End: 6, Text: "actual"}}}}
	var schema map[string]any
	if err := json.Unmarshal(synthesisSchemaFor(sources), &schema); err != nil {
		t.Fatal(err)
	}
	quote := schema["properties"].(map[string]any)["points"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["citations"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["quote"].(map[string]any)
	values, _ := quote["enum"].([]any)
	if len(values) != 1 || values[0] != "actual" {
		t.Fatalf("model quote is unconstrained: %v", values)
	}
}
