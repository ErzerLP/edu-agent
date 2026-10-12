package tutormodel

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

// A model must be able to copy existing digests and slice-relative byte ranges
// without calculating SHA-256 or confusing document offsets with quote offsets.
func TestAssessmentQuoteSupportPassesExistingValidation(t *testing.T) {
	const answerHash = "c1b3d526c12716961671c54f10281db2d17da91eef06160c274347624d23fb69"
	const sourceHash = "499c0fa2ddc93da9aed214e61383ecffc5350e45aa79f16071369d27ecce295b"
	activity := learning.Activity{
		ID: "activity", Revision: 1, SessionID: "session", Type: learning.ActivityOpen,
		KnowledgeRevisionID: "knowledge", TargetNodeID: "node", TargetNodeRevisionID: "node-revision",
		Rubric:     learning.Rubric{Items: []learning.RubricItem{{ID: "reflection", Criterion: "Name the device"}}},
		References: []learning.KnowledgeReference{{KnowledgeRevisionID: "knowledge", NodeID: "node", NodeRevisionID: "node-revision", Slice: "Coffee pot", SliceSHA256: sourceHash, Range: learning.SourceRange{Start: 100, End: 110}}},
	}
	attempt := learning.Attempt{ID: "attempt", ActivityID: "activity", ActivityRevision: 1, Answer: "咖啡壶", AnswerSHA256: answerHash, Help: learning.HelpNone}
	for _, test := range []struct {
		name        string
		inputHash   string
		tamperModel bool
		invalid     bool
		longSource  bool
	}{
		{name: "valid_quotes", inputHash: sourceHash},
		{name: "tampered_model_hash", inputHash: sourceHash, tamperModel: true, invalid: true},
		{name: "tampered_input_hash", inputHash: answerHash, invalid: true},
		{name: "long_nonascii_source", longSource: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			activity := activity
			activity.References = append([]learning.KnowledgeReference(nil), activity.References...)
			wantQuote, wantEnd, wantHash := "Coffee pot", 10, test.inputHash
			if test.longSource {
				activity.References[0].Slice = "Coffee pot" + strings.Repeat("咖啡壶", 230)
				activity.References[0].Range.End = 2180
				activity.References[0].SliceSHA256 = learning.SHA256([]byte(activity.References[0].Slice))
				wantQuote, wantEnd = "Coffee pot"+strings.Repeat("咖啡壶", 226)+"咖", 2047
				wantHash = "52d8a0a6809343d3007394806f8ad3accbdde892dcbf2064db8f977774e3e05b"
				test.inputHash = activity.References[0].SliceSHA256
			}
			inputActivity := activity
			inputActivity.References = append([]learning.KnowledgeReference(nil), activity.References...)
			inputActivity.References[0].SliceSHA256 = test.inputHash
			input, err := json.Marshal(map[string]any{"work_item": map[string]any{"activity": inputActivity, "attempt": attempt}})
			if err != nil {
				t.Fatal(err)
			}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []llm.Message `json:"messages"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("invalid model request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var prompt struct {
					Support struct {
						AnswerRange       learning.SourceRange `json:"answer_range"`
						AnswerQuoteSHA256 string               `json:"answer_quote_sha256"`
						Knowledge         []struct {
							ReferenceID string               `json:"knowledge_reference_id"`
							Range       learning.SourceRange `json:"knowledge_range"`
							SHA256      string               `json:"knowledge_quote_sha256"`
							Quote       string               `json:"knowledge_quote"`
						} `json:"knowledge"`
					} `json:"assessment_quote_support"`
				}
				if json.Unmarshal([]byte(request.Messages[len(request.Messages)-1].Content), &prompt) != nil ||
					prompt.Support.AnswerRange != (learning.SourceRange{Start: 0, End: 9}) || prompt.Support.AnswerQuoteSHA256 != answerHash ||
					len(prompt.Support.Knowledge) != 1 || prompt.Support.Knowledge[0].ReferenceID != "node-revision" ||
					prompt.Support.Knowledge[0].Range != (learning.SourceRange{Start: 0, End: wantEnd}) || prompt.Support.Knowledge[0].SHA256 != wantHash || prompt.Support.Knowledge[0].Quote != wantQuote {
					t.Error("model request lacks reusable digests and slice-relative UTF-8 byte ranges")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				item := learning.AssessmentItem{RubricItemID: "reflection", Conclusion: learning.ConclusionPass,
					AnswerQuote: attempt.Answer, AnswerRange: prompt.Support.AnswerRange, AnswerQuoteSHA256: prompt.Support.AnswerQuoteSHA256,
					KnowledgeReferenceID: prompt.Support.Knowledge[0].ReferenceID, KnowledgeQuote: prompt.Support.Knowledge[0].Quote,
					KnowledgeRange: prompt.Support.Knowledge[0].Range, KnowledgeQuoteSHA256: prompt.Support.Knowledge[0].SHA256}
				if test.tamperModel {
					item.KnowledgeQuoteSHA256 = answerHash
				}
				encoded, _ := json.Marshal(item)
				var fields map[string]any
				json.Unmarshal(encoded, &fields)
				fields["misconception_candidate"] = nil
				content, _ := json.Marshal(map[string]any{"assessment": map[string]any{"items": []any{fields}, "rubric_complete": true, "confidence": 950, "risk_flags": []string{}}})
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}}}})
			}))
			defer provider.Close()
			raw, err := New(newAdapterTestClient(t, provider.URL)).Generate(t.Context(), learning.ProposalRequest{Type: learning.ProposalAssessment, Input: input})
			if err != nil {
				t.Fatal(err)
			}
			var output struct {
				Assessment learning.AssessmentArtifact `json:"assessment"`
			}
			if err := json.Unmarshal(raw, &output); err != nil {
				t.Fatal(err)
			}
			output.Assessment.AttemptID = attempt.ID
			output.Assessment.ActivityID = activity.ID
			output.Assessment.ActivityRevision = activity.Revision
			acceptance, err := learning.EvaluateAssessment(activity, attempt, output.Assessment)
			if test.invalid {
				var failure *learning.Error
				if !errors.As(err, &failure) || failure.Reason != "invalid_knowledge_quote" {
					t.Fatalf("model hash escaped existing validation: %v", err)
				}
			} else if err != nil || acceptance.Disposition != learning.DispositionAccepted || acceptance.Outcome != learning.OutcomePass {
				t.Fatalf("reusable quote evidence rejected: %+v %v", acceptance, err)
			}
		})
	}
}
