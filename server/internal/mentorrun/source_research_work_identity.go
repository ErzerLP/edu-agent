package mentorrun

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var (
	sourceResearchBylineEnd       = regexp.MustCompile(`(?im)(?:^|\n)\s*(?:abstract|introduction|summary|references|bibliography|keywords|1[. ]+introduction)\s*(?::|\n|$)`)
	sourceResearchYear            = regexp.MustCompile(`\b(?:19|20)[0-9]{2}\b`)
	sourceResearchRevision        = regexp.MustCompile(`(?i)\b(?:(?:very\s+)?(?:slightly\s+)?revised\s+version\s+of\s+(?:a|the)\s+paper\s+originally\s+(?:presented|published)|revision\s+of\s+(?:the|our)\s+(?:original|conference)\s+paper)\b`)
	sourceResearchAuthorSeparator = regexp.MustCompile(`\s+(?:and|&)\s+|[,;]`)
	sourceResearchNonAuthor       = regexp.MustCompile(`(?i)\b(?:university|laboratory|department|institute|school|computer|science|network|congestion|control|abstract|introduction|published|originally|berkeley|research|technical|report|editor|working|group|request|comments|contents|january|february|march|april|may|june|july|august|september|october|november|december)\b`)
)

type sourceResearchWork struct {
	title   string
	authors []string
	year    string
	revised bool
}

// Extract only a short front-matter byline, never title/author mentions in
// prose or references. Exact normalized titles avoid conflating related work.
func sourceResearchWorkIdentity(text string) sourceResearchWork {
	front := norm.NFKC.String(bounded(text, 4096))
	work := sourceResearchWork{revised: sourceResearchRevision.MatchString(front)}
	if end := sourceResearchBylineEnd.FindStringIndex(front); end != nil {
		front = front[:end[0]]
	}
	work.year = sourceResearchYear.FindString(front)
	lines := strings.Split(front, "\n")
	title := []string{}
	for i, line := range lines[:min(16, len(lines))] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i > 0 && len(title) > 0 {
			if authors := sourceResearchByline(line); len(authors) > 0 {
				work.authors = append(work.authors, authors...)
				continue
			}
		}
		if len(work.authors) > 0 {
			if sourceResearchNonAuthor.MatchString(line) || sourceResearchYear.MatchString(line) || strings.Contains(line, "@") {
				// A cover's publication note ends its byline. Do not interpret
				// another title, later body prose or references as more authors.
				if strings.Contains(strings.ToLower(line), "published") {
					break
				}
				continue
			}
			break
		}
		if len(title) >= 4 || sourceResearchNonAuthor.MatchString(line) && len(title) > 0 {
			return sourceResearchWork{}
		}
		title = append(title, line)
	}
	if len(work.authors) == 0 {
		return sourceResearchWork{}
	}
	work.title = sourceResearchWorkTitle(strings.Join(title, " "))
	if len(work.title) < 12 {
		return sourceResearchWork{}
	}
	return work
}

func sourceResearchWorkTitle(value string) string {
	var out strings.Builder
	for _, r := range norm.NFKC.String(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			out.WriteRune(unicode.ToLower(r))
		}
	}
	return out.String()
}

func sourceResearchAuthor(line string) string {
	if sourceResearchNonAuthor.MatchString(line) || strings.ContainsAny(line, "@:/()") {
		return ""
	}
	line = strings.TrimRightFunc(line, func(r rune) bool { return !unicode.IsLetter(r) && r != '.' })
	words := strings.Fields(line)
	if len(words) < 2 || len(words) > 4 {
		return ""
	}
	names := []string{}
	for _, word := range words {
		letters := []rune{}
		for _, r := range word {
			if unicode.IsLetter(r) {
				letters = append(letters, r)
			} else if !strings.ContainsRune(".'’−-", r) {
				return ""
			}
		}
		if len(letters) == 0 || !unicode.IsUpper(letters[0]) {
			return ""
		}
		names = append(names, strings.ToLower(string(letters)))
	}
	return strings.Join(names, " ")
}

func sourceResearchByline(line string) []string {
	authors := []string{}
	for _, part := range sourceResearchAuthorSeparator.Split(line, -1) {
		author := sourceResearchAuthor(strings.TrimSpace(part))
		if author == "" {
			return nil
		}
		authors = append(authors, author)
	}
	return authors
}

func sourceResearchAuthorsMatch(a, b string) bool {
	pa, pb := strings.Fields(a), strings.Fields(b)
	if len(pa) != len(pb) || pa[len(pa)-1] != pb[len(pb)-1] {
		return false
	}
	for i := range pa[:len(pa)-1] {
		if pa[i] != pb[i] && !(pa[i][0] == pb[i][0] && (len(pa[i]) == 1 || len(pb[i]) == 1)) {
			return false
		}
	}
	return true
}

// Different bylines/years veto a fuzzy body match. An author-set change is
// accepted only with an explicit revision statement, the same first author,
// a compatible subset of coauthors and the
// same known year; a generic same-title paper never gets this exception.
func sourceResearchCompareWorks(a, b sourceResearchWork) (same, conflict bool) {
	if a.title == "" || b.title == "" || a.title != b.title {
		return false, false
	}
	if a.year != "" && b.year != "" && a.year != b.year {
		return false, true
	}
	shared := 0
	for _, author := range a.authors {
		for _, other := range b.authors {
			if sourceResearchAuthorsMatch(author, other) {
				shared++
				break
			}
		}
	}
	if shared == len(a.authors) && shared == len(b.authors) {
		return true, false
	}
	if shared == min(len(a.authors), len(b.authors)) && sourceResearchAuthorsMatch(a.authors[0], b.authors[0]) && (a.revised || b.revised) && a.year != "" && a.year == b.year {
		return true, false
	}
	return false, true
}
