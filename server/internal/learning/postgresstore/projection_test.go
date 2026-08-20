package postgresstore

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestSessionViewReadsGenerationStatsAndDefaultsMissingStats(t *testing.T) {
	session := learning.SessionProjection{Session: tutoring.Session{ID: "50000000-0000-4000-8000-000000000001", State: tutoring.StateGoalReady}}
	rawSession, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	last := first.Add(5 * time.Minute)
	estimate := learning.ActiveTimeEstimate{DurationSeconds: 300, Estimated: true, AlgorithmVersion: learning.ActiveTimePolicyVersion, SampleCount: 2, FirstReceivedAt: &first, LastReceivedAt: &last}
	rawStats, err := json.Marshal(estimate)
	if err != nil {
		t.Fatal(err)
	}
	metadata := learning.ProjectionMetadata{GenerationID: "generation"}
	view, err := sessionViewFromProjection(metadata, rawSession, rawStats)
	if err != nil || !reflect.DeepEqual(view.Estimate, estimate) {
		t.Fatalf("generation stats view=%+v err=%v", view, err)
	}
	fallback, err := sessionViewFromProjection(metadata, rawSession, []byte("null"))
	if err != nil || !fallback.Estimate.Estimated || fallback.Estimate.AlgorithmVersion != learning.ActiveTimePolicyVersion || fallback.Estimate.SampleCount != 0 {
		t.Fatalf("missing stats fallback=%+v err=%v", fallback, err)
	}
}

func TestOptionalUUIDFilterForPostgreSQLArgument(t *testing.T) {
	if got := optionalUUIDFilter(""); got != nil {
		t.Fatalf("empty optional UUID argument=%v, want nil", got)
	}
	const nodeRevisionID = "41000000-0000-4000-8000-000000000004"
	if got := optionalUUIDFilter(nodeRevisionID); got != nodeRevisionID {
		t.Fatalf("non-empty optional UUID argument=%v, want %s", got, nodeRevisionID)
	}
}

func TestNormalizeTextArrayForPostgreSQLArgument(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{name: "nil", input: nil, want: []string{}},
		{name: "empty", input: []string{}, want: []string{}},
		{name: "reasons", input: []string{"rebuild_failed", "checkpoint_lag"}, want: []string{"rebuild_failed", "checkpoint_lag"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeTextArray(test.input)
			if got == nil {
				t.Fatal("normalized PostgreSQL text[] argument is nil")
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalized text[]=%v want=%v", got, test.want)
			}
			if len(got) > 0 {
				got[0] = "changed"
				if test.input[0] == "changed" {
					t.Fatal("normalizer aliases its input")
				}
			}
		})
	}
}
