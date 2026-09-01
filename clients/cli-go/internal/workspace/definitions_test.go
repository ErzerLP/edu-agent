package workspace

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestWorkspaceDefinitionsExposeStrictSchemas(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != 5 {
		t.Fatalf("definitions=%d want=5", len(definitions))
	}
	wantRequired := map[string][]string{
		ToolList: {}, ToolRead: {"path"}, ToolSearch: {"query"},
		ToolWrite: {"path", "mode", "content"}, ToolEdit: {"path", "expected_hash", "edits"},
	}
	seen := map[string]bool{}
	for _, definition := range definitions {
		name := definition.Function.Name
		if _, ok := wantRequired[name]; !ok || seen[name] {
			t.Fatalf("unexpected or duplicate tool %q", name)
		}
		seen[name] = true
		var schema map[string]any
		if err := json.Unmarshal(definition.Function.Parameters, &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("%s root schema=%+v", name, schema)
		}
		if got := stringSlice(schema["required"]); !reflect.DeepEqual(got, wantRequired[name]) {
			t.Fatalf("%s required=%v want=%v", name, got, wantRequired[name])
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s properties=%T", name, schema["properties"])
		}
		assertWorkspaceSchemaDetails(t, name, properties)
	}
}

func assertWorkspaceSchemaDetails(t *testing.T, name string, properties map[string]any) {
	t.Helper()
	property := func(key string) map[string]any {
		value, ok := properties[key].(map[string]any)
		if !ok {
			t.Fatalf("%s property %s=%T", name, key, properties[key])
		}
		return value
	}
	switch name {
	case ToolList:
		if property("offset")["minimum"] != float64(0) || property("offset")["maximum"] != float64(2000) {
			t.Fatalf("list offset=%+v", property("offset"))
		}
	case ToolRead:
		if property("limit")["maximum"] != float64(200) || property("byte_offset")["maximum"] != float64(1048576) || property("expected_hash")["pattern"] != "^sha256:[0-9a-f]{64}$" {
			t.Fatalf("read bounds=%+v", properties)
		}
	case ToolSearch:
		if !reflect.DeepEqual(stringSlice(property("mode")["enum"]), []string{"literal", "regex"}) || property("include")["maxItems"] != float64(16) {
			t.Fatalf("search enums/bounds=%+v", properties)
		}
	case ToolWrite:
		if !reflect.DeepEqual(stringSlice(property("mode")["enum"]), []string{"create", "replace"}) || property("content")["maxLength"] != float64(8192) || property("expected_hash")["pattern"] != "^sha256:[0-9a-f]{64}$" {
			t.Fatalf("write schema=%+v", properties)
		}
	case ToolEdit:
		edits := property("edits")
		if edits["minItems"] != float64(1) || edits["maxItems"] != float64(32) {
			t.Fatalf("edit bounds=%+v", edits)
		}
		items, _ := edits["items"].(map[string]any)
		if items["additionalProperties"] != false || !reflect.DeepEqual(stringSlice(items["required"]), []string{"old_text", "new_text"}) {
			t.Fatalf("edit item=%+v", items)
		}
	}
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		text, _ := item.(string)
		result = append(result, text)
	}
	return result
}
