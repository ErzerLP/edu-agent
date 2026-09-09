package llm

import "testing"

func TestNullablePlanningFieldsRemainTypeChecked(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"minutes":{"type":["integer","null"]}},"required":["minutes"],"additionalProperties":false}`)
	for _, value := range []string{`{"minutes":30}`, `{"minutes":null}`} {
		if err := validateJSONSchema(schema, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{`{"minutes":"30"}`, `{"minutes":true}`, `{"minutes":1.5}`, `{}`, `{"minutes":30,"mastery":"retained"}`} {
		if err := validateJSONSchema(schema, []byte(value)); err == nil {
			t.Fatalf("未拒绝非法规划字段：%s", value)
		}
	}
}
