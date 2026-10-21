package mentorrun

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/research"
)

const (
	webAnswerMaxFragments = 6
	webAnswerTextBytes    = 7200
	webAnswerMaxCitations = 6
	webAnswerQuoteChars   = 240
)

// The answer needs provenance and read text, not the fetch/parser bookkeeping.
// Split read text into copyable quotes without changing its stored identity.
type webAnswerDocument struct {
	ID               string              `json:"document_id"`
	RevisionID       string              `json:"revision_id"`
	URL              string              `json:"url"`
	Title            string              `json:"title"`
	FetchedAt        time.Time           `json:"fetched_at"`
	Coverage         string              `json:"coverage"`
	FragmentsPartial bool                `json:"fragments_partial"`
	Fragments        []webAnswerFragment `json:"fragments"`
}

type webAnswerFragment struct {
	ID     string           `json:"id"`
	Page   int              `json:"page,omitempty"`
	Quotes []webAnswerQuote `json:"quotes"`
}

type webAnswerQuote struct {
	Candidate int    `json:"candidate"`
	Text      string `json:"text"`
}

type webAnswerCitation struct {
	Origin     string `json:"origin"`
	DocumentID string `json:"document_id"`
	RevisionID string `json:"revision_id"`
	FragmentID string `json:"fragment_id"`
	Quote      string `json:"quote"`
}

func selectWebAnswerDocuments(documents []research.WebDocument, supplied map[string][]string, question string, budget int) ([]webAnswerDocument, []webAnswerCitation) {
	type candidate struct{ document, fragment, score, rank int }
	candidates := []candidate{}
	for i, document := range documents {
		local := []candidate{}
		for j, fragment := range document.Fragments {
			if slices.Contains(supplied[document.ID], fragment.ID) {
				local = append(local, candidate{document: i, fragment: j, score: relevance(question, fragment.Text)})
			}
		}
		sort.SliceStable(local, func(i, j int) bool { return local[i].score > local[j].score })
		for rank, c := range local {
			c.rank = rank
			candidates = append(candidates, c)
		}
	}
	// Reserve breadth before taking more fragments from one page.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		return candidates[i].score > candidates[j].score
	})
	result := []webAnswerDocument{}
	count, textBytes := 0, 0
	for _, c := range candidates {
		if count == webAnswerMaxFragments {
			break
		}
		document := documents[c.document]
		fragment := document.Fragments[c.fragment]
		if textBytes+len(fragment.Text) > webAnswerTextBytes {
			continue
		}
		next := append([]webAnswerDocument(nil), result...)
		i := slices.IndexFunc(next, func(d webAnswerDocument) bool { return d.ID == document.ID })
		if i < 0 {
			i = len(next)
			next = append(next, webAnswerDocument{ID: document.ID, RevisionID: document.RevisionID, URL: document.URL, Title: document.Title, FetchedAt: document.FetchedAt, Coverage: document.Coverage})
		}
		quotes := []webAnswerQuote{}
		for _, text := range webAnswerQuotes(fragment.Text) {
			// Charge a conservative identifier width before final numbering.
			quotes = append(quotes, webAnswerQuote{Candidate: 999, Text: text})
		}
		next[i].Fragments = append(append([]webAnswerFragment(nil), next[i].Fragments...), webAnswerFragment{ID: fragment.ID, Page: fragment.Page, Quotes: quotes})
		next[i].FragmentsPartial = len(next[i].Fragments) < len(document.Fragments)
		raw, _ := json.Marshal(next)
		if messageCost([]modelclient.Message{{Role: "user", Content: string(raw)}}) > budget {
			continue
		}
		result = next
		count++
		textBytes += len(fragment.Text)
	}
	citations := []webAnswerCitation{}
	for i := range result {
		for j := range result[i].Fragments {
			fragment := &result[i].Fragments[j]
			for k := range fragment.Quotes {
				quote := &fragment.Quotes[k]
				quote.Candidate = len(citations) + 1
				citations = append(citations, webAnswerCitation{Origin: "web", DocumentID: result[i].ID, RevisionID: result[i].RevisionID, FragmentID: fragment.ID, Quote: quote.Text})
			}
		}
	}
	return result, citations
}

// Preserve every byte and never join fragments. Prefer sentence boundaries so
// the model can select evidence without reconstructing a sentence across IDs.
func webAnswerQuotes(text string) []string {
	quotes := []string{}
	for text != "" {
		runes := []rune(text)
		end := len(string(runes[:min(webAnswerQuoteChars, len(runes))]))
		if end < len(text) {
			prefix := text[:end]
			boundary := 0
			for _, separator := range []string{". ", "。", "\n", "! ", "? "} {
				if at := strings.LastIndex(prefix, separator); at >= 0 {
					boundary = max(boundary, at+len(separator))
				}
			}
			if boundary >= end/2 {
				end = boundary
			}
		}
		quotes = append(quotes, text[:end])
		text = text[end:]
	}
	return quotes
}
