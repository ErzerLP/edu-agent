package tutormodel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestReferenceSupportCopiesDocumentRangeAndExcludesUnselectedText(t *testing.T) {
	const hash = "499c0fa2ddc93da9aed214e61383ecffc5350e45aa79f16071369d27ecce295b"
	selected := learning.KnowledgeReference{KnowledgeRevisionID: "knowledge", NodeRevisionID: "selected", Slice: "Coffee pot", SliceSHA256: hash, Range: learning.SourceRange{Start: 100, End: 110}}
	unselected := selected
	unselected.NodeRevisionID = "unselected"
	unselected.Slice = "private-unselected-body"
	for _, kind := range []learning.ProposalType{learning.ProposalActivity, learning.ProposalFreeAnswer, learning.ProposalExplanation, learning.ProposalAssessment} {
		t.Run(string(kind), func(t *testing.T) {
			input, _ := json.Marshal(map[string]any{"work_item": map[string]any{"activity": learning.Activity{References: []learning.KnowledgeReference{selected, unselected}}}, "retrieval": map[string]any{"hits": []learning.KnowledgeReference{selected, unselected}}})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []llm.Message `json:"messages"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("bad wire request")
					return
				}
				raw := body.Messages[len(body.Messages)-1].Content
				var modelInput struct {
					Support []struct {
						Reference struct {
							NodeRevisionID string               `json:"node_revision_id"`
							Range          learning.SourceRange `json:"range"`
							Hash           string               `json:"slice_sha256"`
						} `json:"reference"`
						Quote      string               `json:"quote"`
						QuoteRange learning.SourceRange `json:"quote_range"`
						QuoteHash  string               `json:"quote_sha256"`
					} `json:"reference_quote_support"`
				}
				if json.Unmarshal([]byte(raw), &modelInput) != nil || len(modelInput.Support) != 1 || strings.Contains(raw, "unselected") {
					t.Error("missing candidates or unselected material sent")
					w.WriteHeader(500)
					return
				}
				c := modelInput.Support[0]
				if c.Reference.NodeRevisionID != "selected" || c.Reference.Range != (learning.SourceRange{Start: 100, End: 110}) || c.Reference.Hash != hash || c.Quote != "Coffee pot" || c.QuoteRange != (learning.SourceRange{Start: 0, End: 10}) || c.QuoteHash != hash {
					t.Error("document and slice ranges confused")
				}
				outputs := map[learning.ProposalType]string{
					learning.ProposalActivity:    `{"activity":{"prompt":"Question","type":"open","rubric":{"rubric_revision":"r1","items":[{"rubric_item_id":"i1","criterion":"correct","required_reference_ids":null}],"objective_rule":null},"difficulty":1,"allowed_help":["none"],"knowledge_references":[{"node_revision_id":"selected","slice_sha256":"` + hash + `","range":{"start":100,"end":110}}]}}`,
					learning.ProposalAssessment:  `{"assessment":{"items":[],"rubric_complete":false,"confidence":0,"risk_flags":[]}}`,
					learning.ProposalFreeAnswer:  `{"text":{"text":"Hint","knowledge_references":[{"node_revision_id":"selected","slice_sha256":"` + hash + `","range":{"start":100,"end":110}}]}}`,
					learning.ProposalExplanation: `{"text":{"text":"Explanation","knowledge_references":[{"node_revision_id":"selected","slice_sha256":"` + hash + `","range":{"start":100,"end":110}}]}}`,
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": outputs[kind]}}}})
			}))
			defer provider.Close()
			if _, err := New(newAdapterTestClient(t, provider.URL)).Generate(t.Context(), learning.ProposalRequest{Type: kind, KnowledgeRevisionID: "knowledge", NodeRevisionIDs: []string{"selected"}, Input: input}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
