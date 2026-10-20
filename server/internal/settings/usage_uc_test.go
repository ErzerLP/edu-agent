package settings

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestToolStepSettings(t *testing.T) {
	s, _ := testService(t)
	for _, value := range []string{"0", "-1", "1.5", `"2"`, "9223372036854775808"} {
		var u Update
		err := json.Unmarshal([]byte(fmt.Sprintf(`{"expected_revision":%d,"limits":{"max_tool_steps":%s}}`, s.View().Revision, value)), &u)
		if err == nil {
			_, err = s.Update(u)
		}
		if err == nil {
			t.Errorf("accepted invalid max_tool_steps %s", value)
		}
	}
	for _, limits := range []string{`{"max_tool_steps":2}`, `{"output_tokens":4096}`, `{"max_tool_steps":null}`} {
		var u Update
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"expected_revision":%d,"limits":%s}`, s.View().Revision, limits)), &u); err != nil {
			t.Fatal(err)
		}
		v, err := s.Update(u)
		if err != nil {
			t.Fatalf("partial settings %s: %v", limits, err)
		}
		raw, _ := json.Marshal(v.Limits)
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		if limits != `{"max_tool_steps":null}` && fields["max_tool_steps"] != float64(2) {
			t.Fatalf("omission erased tool limit: %s", raw)
		}
		if limits == `{"max_tool_steps":null}` && fields["max_tool_steps"] != nil {
			t.Fatalf("null did not remove limit: %s", raw)
		}
	}
}

func TestRemovedLimitFieldsAreUnknown(t *testing.T) {
	for _, name := range []string{"research_requests", "research_tokens"} {
		for _, value := range []string{"null", "20", "100000"} {
			var update Update
			if json.Unmarshal([]byte(fmt.Sprintf(`{"limits":{%q:%s}}`, name, value)), &update) == nil {
				t.Errorf("accepted removed field %s=%s", name, value)
			}
		}
	}
}
