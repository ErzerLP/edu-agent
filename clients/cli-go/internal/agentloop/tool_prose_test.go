package agentloop

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestCompactToolProsePreservesConstraintsAndQuestion(t *testing.T) {
	const schema = `{"type":"object","description":"docs","properties":{"description":{"type":"string","description":"field docs","minLength":3}},"required":["description"],"additionalProperties":false,"const":{"description":"literal"},"enum":[{"description":"literal"}],"$defs":{"description":{"type":"string","pattern":"^safe$","description":"docs"}}}`
	const expected = `{"type":"object","properties":{"description":{"type":"string","minLength":3}},"required":["description"],"additionalProperties":false,"const":{"description":"literal"},"enum":[{"description":"literal"}],"$defs":{"description":{"type":"string","pattern":"^safe$"}}}`
	original := []modelclient.Tool{tool("example", "prose", schema), tool("ask_user_question", "question safety", schema)}
	projected := compactToolProse(original)
	var actual, want any
	if err := json.Unmarshal(projected[0].Function.Parameters, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, want) || projected[0].Function.Name != "example" || projected[0].Type != "function" {
		t.Fatalf("constraints changed: %+v", projected)
	}
	if !reflect.DeepEqual(projected[1], original[1]) || string(original[0].Function.Parameters) != schema || original[0].Function.Description != "prose" {
		t.Fatal("question/source modified")
	}
	definitions := append(Tools(), workspace.Definitions()...)
	compact := compactToolProse(definitions)
	if len(compact) != len(definitions) {
		t.Fatal("tools hidden")
	}
	for i := range definitions {
		if compact[i].Function.Name != definitions[i].Function.Name {
			t.Fatal("tool order/name changed")
		}
	}
}
