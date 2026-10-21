package mentorrun

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/edu-agent/edu-agent/server/internal/research"
	"golang.org/x/text/unicode/norm"
)

var (
	sourceResearchRFCHeader   = regexp.MustCompile(`(?i)\brequest\s+for\s+comments\s*:\s*([1-9][0-9]{0,5})(?:\s|$)`)
	sourceResearchRFCHeading  = regexp.MustCompile(`(?i)^\s*(?:info\s+)?rfc\s+([1-9][0-9]{0,5})(?:\s*[:\-]|\s|$)`)
	sourceResearchDOI         = regexp.MustCompile(`(?i)(?:\bdoi\s*:\s*(?:https?://(?:dx\.)?doi\.org/)?|\bhttps?://(?:dx\.)?doi\.org/)(10\.[0-9]{4,9}/[-._;()/:a-z0-9]*[a-z0-9][-._;()/:a-z0-9]*)(?:\s|$|[,\]])`)
	sourceResearchArXiv       = regexp.MustCompile(`(?i)(?:\barxiv\s*:\s*|\bhttps?://(?:www\.)?arxiv\.org/(?:abs|pdf)/)((?:[0-9]{2}(?:0[1-9]|1[0-2])\.[0-9]{4,5}|[a-z]+(?:-[a-z]+)?(?:\.[a-z]{2})?/[0-9]{2}(?:0[1-9]|1[0-2])[0-9]{3}))(?:v[1-9][0-9]*)?(?:\.pdf)?(?:\s|$|[,;)\]])`)
	sourceResearchISBN13      = regexp.MustCompile(`(?i)\bisbn(?:-13)?\s*:?\s*(97[89](?:[ -]?[0-9]){10})(?:\s|$|[,.;)])`)
	sourceResearchISBN10      = regexp.MustCompile(`(?i)\bisbn(?:-10)?\s*:?\s*([0-9](?:[ -]?[0-9]){8}[ -]?[0-9x])(?:\s|$|[,.;)])`)
	sourceResearchMetadataEnd = regexp.MustCompile(`(?im)(?:^|\n)\s*(?:abstract|summary|references|bibliography)\s*(?::|\n|$)`)
	sourceResearchRFCTitle    = regexp.MustCompile(`(?i)^\s*rfc\s+[1-9][0-9]{0,5}\s*[:\-–—]\s*`)
)

// Read only explicit front-matter labels in the first 2048 bytes, excluding
// bibliography/abstract sections. Bare numbers and in-prose RFC references
// are not identity evidence. Different namespaces are compared independently.
func sourceResearchFormalIdentifiers(text string) map[string]string {
	text = norm.NFKC.String(text)
	if end := sourceResearchMetadataEnd.FindStringIndex(text); end != nil {
		text = text[:end[0]]
	}
	ids := map[string]string{}
	for _, field := range []struct {
		kind    string
		pattern *regexp.Regexp
	}{
		{"rfc", sourceResearchRFCHeader}, {"rfc", sourceResearchRFCHeading},
		{"doi", sourceResearchDOI}, {"arxiv", sourceResearchArXiv},
		{"isbn", sourceResearchISBN13}, {"isbn", sourceResearchISBN10},
	} {
		match := field.pattern.FindStringSubmatchIndex(text)
		if match == nil || match[3] > 2048 || ids[field.kind] != "" {
			continue
		}
		value := strings.ToLower(text[match[2]:match[3]])
		switch field.kind {
		case "doi":
			value = strings.TrimRight(value, ".,;")
		case "arxiv":
			var sequence string
			if _, dated, old := strings.Cut(value, "/"); old {
				// Archive/YYMMNNN was used from July 1991 to March 2007.
				if dated[:4] > "0703" && dated[:4] < "9107" {
					continue
				}
				sequence = dated[4:]
			} else {
				// YYMM.NNNN began in April 2007; NNNNN in January 2015.
				month := value[:4]
				sequence = value[5:]
				if month < "0704" || month < "1501" && len(sequence) != 4 || month >= "1501" && len(sequence) != 5 {
					continue
				}
			}
			if strings.Trim(sequence, "0") == "" {
				continue
			}
		case "isbn":
			value = sourceResearchCanonicalISBN(value)
		}
		if value != "" {
			ids[field.kind] = value
		}
	}
	return ids
}

// Validate check digits and compare equivalent ISBN-10/ISBN-13 spellings.
func sourceResearchCanonicalISBN(value string) string {
	value = strings.NewReplacer("-", "", " ", "").Replace(value)
	if len(value) == 10 {
		sum := 0
		for i, r := range value {
			digit := int(r - '0')
			if r == 'x' {
				digit = 10
			}
			sum += (10 - i) * digit
		}
		if sum%11 != 0 {
			return ""
		}
		value = "978" + value[:9]
	}
	sum := 0
	for i := 0; i < 12; i++ {
		sum += int(value[i]-'0') * (1 + 2*(i%2))
	}
	check := byte('0' + (10-sum%10)%10)
	if len(value) == 13 && value[12] != check {
		return ""
	}
	return value[:12] + string(check)
}

func sourceResearchSimilarTitles(a, b research.WebDocument) bool {
	title := func(d research.WebDocument) string {
		value := strings.TrimSpace(norm.NFKC.String(d.Title))
		if value == d.URL || value == d.RequestURL || strings.HasPrefix(strings.ToLower(value), "http://") || strings.HasPrefix(strings.ToLower(value), "https://") {
			return ""
		}
		value, _, _ = strings.Cut(value, "|")
		return sourceResearchRFCTitle.ReplaceAllString(value, "")
	}
	// Character trigrams tolerate punctuation, casing and minor title edits in
	// both spaced and unspaced languages. Require 80% Dice similarity.
	sa, sb := sourceResearchCharacterShingles(title(a), 3), sourceResearchCharacterShingles(title(b), 3)
	if min(len(sa), len(sb)) == 0 {
		return false
	}
	shared := 0
	for s := range sa {
		if _, ok := sb[s]; ok {
			shared++
		}
	}
	return 200*shared >= 80*(len(sa)+len(sb))
}

// Character shingles work for both spaced and unspaced languages, and ignore
// HTML anchor boundaries, line wrapping and punctuation. Require substantial
// distinct content and 90% coverage of the smaller shingle set. Partial HTML
// and TXT snapshots have different text budgets after whitespace extraction:
// the real RFC 1180 pair scores 100%/73.64%, RFC 1122 99.87%/99.85%.
// These are comparison keys, never citations.
func sourceResearchShingles(text string) map[string]struct{} {
	return sourceResearchCharacterShingles(text, 12)
}

func sourceResearchCharacterShingles(text string, size int) map[string]struct{} {
	letters := make([]rune, 0, len(text))
	for _, r := range norm.NFKC.String(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			letters = append(letters, unicode.ToLower(r))
		}
	}
	shingles := map[string]struct{}{}
	for i := 0; i+size <= len(letters); i++ {
		shingles[string(letters[i:i+size])] = struct{}{}
	}
	return shingles
}

func sourceResearchSameWork(a, b research.WebDocument) bool {
	ia, ib := sourceResearchFormalIdentifiers(a.Text), sourceResearchFormalIdentifiers(b.Text)
	sameIdentifier := false
	for kind, value := range ia {
		if other := ib[kind]; other != "" {
			if other != value {
				return false
			}
			sameIdentifier = true
		}
	}
	if a.SHA256 != "" && a.SHA256 == b.SHA256 {
		return true
	}
	ua, ea := research.NormalizeURL(a.URL)
	ub, eb := research.NormalizeURL(b.URL)
	if ea == nil && eb == nil && ua == ub {
		return true
	}
	if same, conflict := sourceResearchCompareWorks(sourceResearchWorkIdentity(a.Text), sourceResearchWorkIdentity(b.Text)); same || conflict {
		return same
	}
	if sameIdentifier && sourceResearchSimilarTitles(a, b) {
		return true
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
	return shared*100 >= 90*min(len(sa), len(sb))
}

// Use snapshot quality and explicit front-matter provenance, not host
// reputation or unverified canonical tags.
// Ties retain the existing representative to keep candidate identity stable.
func sourceResearchPreferDocument(a, b research.WebDocument) bool {
	if (a.Coverage == "complete_text") != (b.Coverage == "complete_text") {
		return a.Coverage == "complete_text"
	}
	wa, wb := sourceResearchWorkIdentity(a.Text), sourceResearchWorkIdentity(b.Text)
	if same, _ := sourceResearchCompareWorks(wa, wb); same && wa.revised != wb.revised {
		return wa.revised
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

func sourceResearchVersionReason(reason, alternateURL string) string {
	alternateURL = bounded(alternateURL, 240)
	if alternateURL == "" || strings.Contains(reason, alternateURL) {
		return reason
	}
	return bounded(reason, 360) + "\n另有版本：" + alternateURL
}
