package tutormodel

import (
	"encoding/json"
	"slices"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type referenceQuoteCandidate struct {
	Reference struct {
		ID    string               `json:"node_revision_id"`
		Range learning.SourceRange `json:"range"`
		Hash  string               `json:"slice_sha256"`
	} `json:"reference"`
	Quote string               `json:"quote"`
	Range learning.SourceRange `json:"quote_range"`
	Hash  string               `json:"quote_sha256"`
}

func quotePrefix(ref learning.KnowledgeReference) (string, string) {
	quote, digest := ref.Slice, ref.SliceSHA256
	if len(quote) > 2048 {
		end := 2048
		for !utf8.RuneStart(quote[end]) {
			end--
		}
		quote = quote[:end]
		digest = learning.SHA256([]byte(quote))
	}
	return quote, digest
}

// Filter both support and the original reference arrays. An unselected node's
// text must not re-enter the model through the surrounding work item/retrieval.
// Other input fields remain data; this never repairs or canonicalizes output.
func quoteSupportInput(request learning.ProposalRequest) (learning.ProposalRequest, []referenceQuoteCandidate, error) {
	var input map[string]json.RawMessage
	if len(request.Input) == 0 {
		return request, []referenceQuoteCandidate{}, nil
	}
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return request, nil, err
	}
	refs := []learning.KnowledgeReference{}
	seen := map[string]bool{}
	filter := func(raw json.RawMessage) (json.RawMessage, error) {
		if len(raw) == 0 {
			return raw, nil
		}
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
		selected := []json.RawMessage{}
		for _, value := range values {
			var ref learning.KnowledgeReference
			if err := json.Unmarshal(value, &ref); err != nil {
				return nil, err
			}
			// Production requests have a nonempty, server-validated allow list.
			if len(request.NodeRevisionIDs) > 0 && !slices.Contains(request.NodeRevisionIDs, ref.NodeRevisionID) {
				continue
			}
			if request.KnowledgeRevisionID != "" && ref.KnowledgeRevisionID != "" && ref.KnowledgeRevisionID != request.KnowledgeRevisionID {
				continue
			}
			selected = append(selected, value)
			if !seen[ref.NodeRevisionID] && ref.Slice != "" {
				seen[ref.NodeRevisionID] = true
				refs = append(refs, ref)
			}
		}
		return json.Marshal(selected)
	}
	var work, activity, retrieval map[string]json.RawMessage
	if raw := input["work_item"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &work); err != nil {
			return request, nil, err
		}
		if raw := work["activity"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &activity); err != nil {
				return request, nil, err
			}
			if raw := activity["knowledge_references"]; len(raw) > 0 {
				filtered, err := filter(raw)
				if err != nil {
					return request, nil, err
				}
				activity["knowledge_references"] = filtered
				work["activity"], _ = json.Marshal(activity)
				input["work_item"], _ = json.Marshal(work)
			}
		}
	}
	if raw := input["retrieval"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &retrieval); err != nil {
			return request, nil, err
		}
		if raw := retrieval["hits"]; len(raw) > 0 {
			filtered, err := filter(raw)
			if err != nil {
				return request, nil, err
			}
			retrieval["hits"] = filtered
			input["retrieval"], _ = json.Marshal(retrieval)
		}
	}
	request.Input, _ = json.Marshal(input)
	result := []referenceQuoteCandidate{}
	for _, ref := range refs {
		quote, digest := quotePrefix(ref)
		candidate := referenceQuoteCandidate{Quote: quote, Range: learning.SourceRange{Start: 0, End: len(quote)}, Hash: digest}
		candidate.Reference.ID, candidate.Reference.Range, candidate.Reference.Hash = ref.NodeRevisionID, ref.Range, ref.SliceSHA256
		result = append(result, candidate)
	}
	return request, result, nil
}
