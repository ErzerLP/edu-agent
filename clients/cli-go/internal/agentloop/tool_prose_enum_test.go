package agentloop

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestCompactToolProseEnumsKeepValidationAndLiteralData(t *testing.T) {
	const source = `{"type":"object","properties":{"choice":{"type":"string","enum":["a","b"],"default":"a","minLength":1},"mixed":{"type":"string","enum":["a",1]},"empty":{"type":"string","enum":[]},"union":{"type":["string","null"],"enum":["a",null]},"data":{"const":{"default":"literal","description":"literal"}}},"required":["choice"],"additionalProperties":false}`
	tools := []modelclient.Tool{tool("example", "docs", source)}
	projected := compactToolProse(tools)
	var got map[string]any
	if err := json.Unmarshal(projected[0].Function.Parameters, &got); err != nil {
		t.Fatal(err)
	}
	properties := got["properties"].(map[string]any)
	choice := properties["choice"].(map[string]any)
	if _, exists := choice["type"]; exists {
		t.Fatal("redundant string type retained")
	}
	if _, exists := choice["default"]; exists {
		t.Fatal("annotation retained")
	}
	if !reflect.DeepEqual(choice["enum"], []any{"a", "b"}) || choice["minLength"] != float64(1) || got["additionalProperties"] != false || !reflect.DeepEqual(got["required"], []any{"choice"}) {
		t.Fatal("validation changed", got)
	}
	for _, name := range []string{"mixed", "empty", "union"} {
		if properties[name].(map[string]any)["type"] == nil {
			t.Fatal("non-redundant type dropped", name)
		}
	}
	literal := properties["data"].(map[string]any)["const"].(map[string]any)
	if literal["default"] != "literal" || literal["description"] != "literal" || string(tools[0].Function.Parameters) != source {
		t.Fatal("literal data or original schema changed")
	}
	// The retained enumeration rejects all former non-string/unknown choices.
	for _, value := range []any{nil, true, float64(1), "", "c", map[string]any{}, []any{}} {
		for _, allowed := range choice["enum"].([]any) {
			if reflect.DeepEqual(value, allowed) {
				t.Fatal("enum widened", value)
			}
		}
	}
}
