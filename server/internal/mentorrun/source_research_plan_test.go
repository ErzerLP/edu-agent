package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

// Real planning responses have exceeded purpose's 80-character limit or copied
// the schema's additionalProperties keyword into the output object. A repair
// must identify the structural failure rather than ask for source citations.
func TestSourceResearchPlanRepairsSchemaFields(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bad        string
		field      string
		constraint string
	}{
		{"long_purpose", `{"directions":[{"title":"TCP","purpose":"` + strings.Repeat("p", 81) + `","query":"TCP paper PDF"}]}`, "directions[0].purpose", "maxLength"},
		{"schema_keyword_in_output", `{"directions":[{"title":"TCP","purpose":"原始论文","query":"TCP paper PDF"}],"additionalProperties":false}`, "$", "additionalProperties"},
		{"missing_directions", `{"title":"TCP","purpose":"原始论文","query":"TCP paper PDF"}`, "directions", "required"},
		{"wrong_query_type", `{"directions":[{"title":"TCP","purpose":"原始论文","query":["TCP paper PDF"]}]}`, "directions[0].query", "type"},
		{"untrusted_extra_key", `{"directions":[{"title":"TCP","purpose":"原始论文","query":"TCP paper PDF"}],"private-key-sentinel":true}`, "$", "additionalProperties"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sourceResearchFixture(t)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
				var request modelclient.Request
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				content := tc.bad
				if len(request.Messages) == 3 {
					var repair struct {
						Reason      string          `json:"rejection_reason"`
						Field       string          `json:"rejection_field"`
						Constraint  string          `json:"rejection_constraint"`
						Schema      json.RawMessage `json:"output_schema"`
						Instruction string          `json:"instruction"`
					}
					if err := json.Unmarshal([]byte(request.Messages[2].Content), &repair); err != nil {
						t.Error(err)
						return
					}
					if repair.Reason == "schema_mismatch" && repair.Field == tc.field && repair.Constraint == tc.constraint && len(repair.Schema) > 0 &&
						!strings.Contains(repair.Instruction, "引用") && !strings.Contains(repair.Instruction, "候选") {
						content = `{"directions":[{"title":"TCP","purpose":"原始论文","query":"TCP paper PDF"}]}`
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 40, "total_tokens": 90}})
			}
			h := sourceResearchPlanHost(t, f)
			if err := h.planSourceResearch(t.Context()); err != nil {
				t.Fatalf("planning did not repair %s: %v; diagnostics=%s", tc.field, err, logs.String())
			}
			if len(h.body.SourceResearch.Directions) != 1 || h.body.SourceResearch.Directions[0].Query != "TCP paper PDF" || f.calls.Load() != 2 {
				t.Fatal("repair did not produce a validated plan in two attempts")
			}
			if strings.Contains(logs.String(), "invalid_source_citation") || strings.Contains(logs.String(), `"rejection_field":"citations"`) ||
				!strings.Contains(logs.String(), `"error_category":"schema_mismatch"`) || !strings.Contains(logs.String(), `"rejection_field":"`+tc.field+`"`) {
				t.Fatalf("planning diagnostics misclassified a schema rejection: %s", logs.String())
			}
			if strings.Contains(logs.String(), "private-key-sentinel") || strings.Contains(logs.String(), strings.Repeat("p", 81)) {
				t.Fatal("schema diagnostics leaked model output")
			}
		})
	}
}

func TestSourceResearchPlanFailureKeepsSchemaDiagnostics(t *testing.T) {
	f := sourceResearchFixture(t)
	f.modelOverride = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"directions\":[]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":40,"total_tokens":90}}`))
	}
	h := sourceResearchPlanHost(t, f)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	err := h.planSourceResearch(t.Context())
	if err == nil || f.calls.Load() != 2 || len(h.body.SourceResearch.Directions) != 0 {
		t.Fatal("invalid plan bypassed validation or the two-attempt limit")
	}
	logRunFailure(t.Context(), h.owned, err)
	if finishErr := f.service.finish(t.Context(), h.owned, err); finishErr != nil {
		t.Fatal(finishErr)
	}
	var status, reason string
	if readErr := f.pool.QueryRow(t.Context(), `SELECT state->>'status',state->>'reason' FROM learning_mentor_runs WHERE id=$1`, h.owned.RunID).Scan(&status, &reason); readErr != nil || status != "failed" || reason != "schema_mismatch" {
		t.Fatalf("planning failure persisted as %s/%s: %v", status, reason, readErr)
	}
	entries := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var failure map[string]any
	if json.Unmarshal([]byte(entries[len(entries)-1]), &failure) != nil || failure["msg"] != "mentor_run_failed" ||
		failure["error_category"] != "schema_mismatch" || failure["rejection_reason"] != "schema_mismatch" || failure["rejection_field"] != "directions" {
		t.Fatalf("worker lost planning failure diagnostics: %s", logs.String())
	}
	if strings.Contains(logs.String(), "invalid_source_citation") || strings.Contains(logs.String(), "citations") {
		t.Fatal("planning failure was classified as a citation failure")
	}
}

func sourceResearchPlanHost(t *testing.T, f *runtimeFixture) *executionHost {
	t.Helper()
	if _, err := f.service.CreateSourceResearch(context.Background(), f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "找 TCP 经典论文 PDF 和练习"}); err != nil {
		t.Fatal(err)
	}
	owned, err := f.service.claim(t.Context())
	if err != nil || owned == nil {
		t.Fatalf("claim planner: %v", err)
	}
	return &executionHost{service: f.service, owned: *owned, body: owned.body, limits: f.settings.View().Limits}
}
