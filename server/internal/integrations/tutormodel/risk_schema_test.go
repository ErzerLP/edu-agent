package tutormodel

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestAssessmentModelSchemaRejectsUnknownRiskFlag(t *testing.T) {
	for _, risk := range []struct {
		value    string
		rejected bool
	}{{"insufficient_knowledge_support", false}, {"invented_model_risk", true}} {
		t.Run(risk.value, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"assessment":{"items":[],"rubric_complete":false,"confidence":500,"risk_flags":["` + risk.value + `"]}}`}}}})
			}))
			defer provider.Close()
			_, err := New(newAdapterTestClient(t, provider.URL)).Generate(t.Context(), learning.ProposalRequest{Type: learning.ProposalAssessment})
			var failed learning.ModelFailure
			if risk.rejected {
				if !errors.As(err, &failed) || failed.ModelCategory() != "schema_mismatch" {
					t.Fatalf("unknown risk was released as a candidate: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
