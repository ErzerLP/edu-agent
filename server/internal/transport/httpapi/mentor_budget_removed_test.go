package httpapi

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
)

func TestMentorRequestsRejectRemovedBudgets(t *testing.T) {
	for _, target := range []any{&mentorrun.Create{}, &mentorrun.Command{}, &mentorrun.SubmitTurn{}, &mentorrun.StudioGenerate{}, &mentorrun.StudyAssessmentResume{}} {
		for _, name := range []string{"request_budget", "token_budget"} {
			if err := decodeJSONData([]byte(fmt.Sprintf(`{%q:1}`, name)), target); err == nil {
				t.Errorf("%T accepted removed field %s", target, name)
			}
		}
	}
}

func TestMentorSnapshotOmitsBudgetBalances(t *testing.T) {
	raw, err := json.Marshal(mentorrun.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"requests_left", "tokens_left"} {
		if _, exists := fields[name]; exists {
			t.Errorf("snapshot exposes removed balance %s", name)
		}
	}
}
