package postgresstore

import (
	"reflect"
	"testing"
)

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
