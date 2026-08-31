package agentloop

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestServerReferenceNormalizesNestedTypedValuesAndStrictGeneration(t *testing.T) {
	due := time.Date(2026, 9, 3, 4, 5, 6, 0, time.FixedZone("test", 8*60*60))
	sessionReference := serverReferenceForToolResult("get_learning_progress", map[string]any{
		"active": true,
		"session": api.SessionView{
			Metadata: api.ProjectionMetadata{Generation: "12"},
			Session:  api.TutoringSession{SessionID: "session-typed", AggregateVersion: 7},
		},
	})
	if sessionReference.EntityID != "session-typed" || sessionReference.Version != 7 || sessionReference.Generation != 12 {
		t.Fatalf("nested session reference=%+v", sessionReference)
	}

	preferenceReference := serverReferenceForToolResult("list_long_term_preferences", map[string]any{
		"items": []map[string]any{{"revision": int64(2)}, {"revision": "9"}},
		"read_generation": map[string]any{
			"learner_generation": "7",
			"memory_generation":  "13",
		},
	})
	if preferenceReference.Version != 9 || preferenceReference.Generation != 13 ||
		preferenceReference.LearnerGeneration != 7 || preferenceReference.MemoryGeneration != 13 {
		t.Fatalf("typed preference reference=%+v", preferenceReference)
	}
	olderPreference := &ServerReference{Tool: "list_long_term_preferences", Entity: "long_term_preferences", EntityID: "current_user", Generation: 12, LearnerGeneration: 7, MemoryGeneration: 12}
	if !serverReferenceNewer(preferenceReference, olderPreference) || serverReferenceStale(preferenceReference, olderPreference) ||
		!serverReferenceStale(olderPreference, preferenceReference) {
		t.Fatalf("preference generation comparison current=%+v older=%+v", preferenceReference, olderPreference)
	}

	reviewReference := serverReferenceForToolResult("get_due_reviews", map[string]any{
		"due_before": due,
		"generation": "14",
	})
	if reviewReference.Revision != due.Format(time.RFC3339) || reviewReference.Generation != 14 {
		t.Fatalf("typed due reference=%+v", reviewReference)
	}

	for _, test := range []struct {
		value any
		want  int64
	}{
		{value: "0", want: 0},
		{value: "0015", want: 15},
		{value: "+15", want: 0},
		{value: "-1", want: 0},
		{value: "1.5", want: 0},
		{value: "9223372036854775808", want: 0},
		{value: 3.5, want: 0},
	} {
		if got := projectionInt64(test.value); got != test.want {
			t.Fatalf("projectionInt64(%v)=%d want=%d", test.value, got, test.want)
		}
	}
}

func TestCompactProjectionValueSelectsMapFieldsDeterministically(t *testing.T) {
	value := make(map[string]any, 30)
	for index := 29; index >= 0; index-- {
		value[fmt.Sprintf("field-%02d", index)] = index
	}

	first, err := json.Marshal(compactProjectionValue(value, 0, 5, 64))
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 20; iteration++ {
		current, marshalErr := json.Marshal(compactProjectionValue(value, 0, 5, 64))
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if string(current) != string(first) {
			t.Fatalf("projection changed between runs:\nfirst=%s\ncurrent=%s", first, current)
		}
	}

	var projected map[string]any
	if err := json.Unmarshal(first, &projected); err != nil {
		t.Fatal(err)
	}
	if _, ok := projected["field-23"]; !ok {
		t.Fatalf("expected earliest sorted fields to be retained: %s", first)
	}
	if _, ok := projected["field-24"]; ok {
		t.Fatalf("projection retained fields beyond the deterministic limit: %s", first)
	}
	if projected["truncated"] != true || projected["degraded"] != true {
		t.Fatalf("projection did not report deterministic truncation: %s", first)
	}
}
