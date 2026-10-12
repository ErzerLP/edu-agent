package research

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"
)

type CitationCandidate struct {
	Citation Citation `json:"citation"`
	Range    struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"range"`
	SHA256 string `json:"quote_sha256"`
}

// CopyableCitations derives hints only from the fragments supplied to this call.
// Returned citations still require the full original validation.
func CopyableCitations(sources []Source) []CitationCandidate {
	result := []CitationCandidate{}
	for _, source := range sources {
		for _, fragment := range source.Fragments {
			end := min(384, len(fragment.Text))
			for end < len(fragment.Text) && !utf8.RuneStart(fragment.Text[end]) {
				end--
			}
			quote := fragment.Text[:end]
			if quote == "" {
				continue
			}
			sum := sha256.Sum256([]byte(quote))
			candidate := CitationCandidate{Citation: Citation{SourceID: source.ID, RevisionID: source.RevisionID, FragmentID: fragment.ID, Quote: quote}, SHA256: hex.EncodeToString(sum[:])}
			candidate.Range.Start, candidate.Range.End = fragment.Start, fragment.Start+end
			result = append(result, candidate)
		}
	}
	return result
}
