package mentorrun

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/edu-agent/edu-agent/server/internal/research"
	"golang.org/x/text/unicode/norm"
)

var sourceResearchRFCHeader = regexp.MustCompile(`(?i)\brequest\s+for\s+comments\s*:\s*([0-9]{1,6})\b`)

// Character shingles work for both spaced and unspaced languages, and ignore
// HTML anchor boundaries, line wrapping and punctuation. Require substantial
// distinct content and 90% coverage of BOTH snapshots: a shared abstract or a
// short excerpt of a longer work is not enough. The real 16 KB RFC 1122 pair
// scores 99.85%; the saved 3.6 KB excerpts score 99.19% (unrelated evidence
// in the same run scores below 1%). These are comparison keys, never citations.
func sourceResearchShingles(text string) map[string]struct{} {
	letters := make([]rune, 0, len(text))
	for _, r := range norm.NFKC.String(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			letters = append(letters, unicode.ToLower(r))
		}
	}
	shingles := map[string]struct{}{}
	for i := 0; i+12 <= len(letters); i++ {
		shingles[string(letters[i:i+12])] = struct{}{}
	}
	return shingles
}

func sourceResearchSameWork(a, b research.WebDocument) bool {
	if a.SHA256 != "" && a.SHA256 == b.SHA256 {
		return true
	}
	ua, ea := research.NormalizeURL(a.URL)
	ub, eb := research.NormalizeURL(b.URL)
	if ea == nil && eb == nil && ua == ub {
		return true
	}
	// A formal document header can veto a content match. Never treat a
	// referenced RFC number (such as the companion RFC in the summary) as
	// positive identity evidence, or merge different numbered specifications
	// simply because they share most of a template.
	headerNumber := func(text string) string {
		text = norm.NFKC.String(text)
		match := sourceResearchRFCHeader.FindStringSubmatchIndex(text)
		if match == nil || match[0] >= 2048 {
			return ""
		}
		return text[match[2]:match[3]]
	}
	if ra, rb := headerNumber(a.Text), headerNumber(b.Text); ra != "" && rb != "" && ra != rb {
		return false
	}
	sa, sb := sourceResearchShingles(a.Text), sourceResearchShingles(b.Text)
	if min(len(sa), len(sb)) < 300 {
		return false
	}
	shared := 0
	for s := range sa {
		if _, ok := sb[s]; ok {
			shared++
		}
	}
	return shared*100 >= 90*max(len(sa), len(sb))
}

// Use snapshot quality, not host reputation or unverified canonical tags.
// Ties retain the existing representative to keep candidate identity stable.
func sourceResearchPreferDocument(a, b research.WebDocument) bool {
	if (a.Coverage == "complete_text") != (b.Coverage == "complete_text") {
		return a.Coverage == "complete_text"
	}
	titled := func(d research.WebDocument) bool {
		title := strings.TrimSpace(d.Title)
		return title != "" && title != d.URL && title != d.RequestURL
	}
	if titled(a) != titled(b) {
		return titled(a)
	}
	return len(sourceResearchShingles(a.Text)) > len(sourceResearchShingles(b.Text))
}

// Keep group order (the query's relevance order), choosing a representative
// before the caller applies its evidence limit.
func sourceResearchUniqueDocuments(docs []research.WebDocument) []int {
	indices := []int{}
	for i, d := range docs {
		duplicate := false
		for j, prior := range indices {
			if sourceResearchSameWork(d, docs[prior]) {
				if sourceResearchPreferDocument(d, docs[prior]) {
					indices[j] = i
				}
				duplicate = true
				break
			}
		}
		if !duplicate {
			indices = append(indices, i)
		}
	}
	return indices
}

func (s *SourceResearchState) replaceDirectionDocument(from, to string) {
	for i, id := range s.DirectionDocuments {
		if id == from {
			s.DirectionDocuments[i] = to
		}
	}
}
