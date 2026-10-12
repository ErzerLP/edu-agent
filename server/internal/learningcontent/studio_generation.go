package learningcontent

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

var ErrStudioCitation = errors.New("invalid_studio_citation")

type StudioClaim struct {
	Text      string   `json:"text"`
	Citations []string `json:"citations"`
}
type StudioGuide struct {
	KeyPoints      []StudioClaim `json:"key_points"`
	Terms          []StudioClaim `json:"terms"`
	LearningOrder  []StudioClaim `json:"learning_order"`
	Misconceptions []StudioClaim `json:"misconceptions"`
}
type StudioBrief struct {
	Overview    []StudioClaim `json:"overview"`
	Differences []StudioClaim `json:"differences"`
	Gaps        []StudioClaim `json:"gaps"`
}
type studioReference struct {
	Source   int `json:"source"`
	Fragment int `json:"fragment"`
}
type studioCandidateClaim struct {
	Text      string            `json:"text"`
	Citations []studioReference `json:"citations"`
}

// StudioSchema describes two separate, closed output shapes. It is also sent
// in the prompt for providers which only support json_object.
func StudioSchema(kind string) json.RawMessage {
	fields := studioFields(kind)
	if fields == nil {
		return nil
	}
	ref := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"source", "fragment"}, "properties": map[string]any{"source": map[string]any{"type": "integer", "minimum": 1}, "fragment": map[string]any{"type": "integer", "minimum": 1}}}
	claim := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"text", "citations"}, "properties": map[string]any{"text": map[string]any{"type": "string", "minLength": 1, "maxLength": 1500}, "citations": map[string]any{"type": "array", "minItems": 1, "maxItems": 10, "items": ref}}}
	properties := map[string]any{"title": map[string]any{"type": "string", "minLength": 1, "maxLength": 200}, "coverage": map[string]any{"type": "string", "enum": []string{"complete", "partial"}}}
	for _, name := range fields {
		properties[name] = map[string]any{"type": "array", "minItems": 1, "maxItems": 8, "items": claim}
	}
	raw, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": append([]string{"title", "coverage"}, fields...), "properties": properties})
	return raw
}
func studioFields(kind string) []string {
	switch kind {
	case "guide":
		return []string{"key_points", "terms", "learning_order", "misconceptions"}
	case "brief":
		return []string{"overview", "differences", "gaps"}
	}
	return nil
}
func studioText(s string, max int) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, 0)
}

func DecodeStudioCandidate(kind string, raw []byte, manifest *knowledge.SourceManifest, sources []knowledge.SourceDetail) (StudioBody, error) {
	body := StudioBody{Schema: 1, SourceManifest: manifest, Citations: []StudioCitation{}}
	fields := studioFields(kind)
	if fields == nil || manifest == nil || len(sources) == 0 || len(manifest.Entries) != len(sources) {
		return body, ErrInvalid
	}
	var values map[string]json.RawMessage
	if agentcore.DecodeArguments(string(raw), &values) != nil || len(values) != len(fields)+2 || json.Unmarshal(values["title"], &body.Title) != nil || json.Unmarshal(values["coverage"], &body.Coverage) != nil || !studioText(body.Title, 200) || strings.ContainsAny(body.Title, "\r\n") || body.Coverage != "complete" && body.Coverage != "partial" {
		return body, ErrInvalid
	}
	seen := map[studioReference]string{}
	groups := map[string][]StudioClaim{}
	for _, field := range fields {
		var claims []studioCandidateClaim
		if agentcore.DecodeArguments(`{"claims":`+string(values[field])+`}`, &struct {
			Claims *[]studioCandidateClaim `json:"claims"`
		}{&claims}) != nil || len(claims) < 1 || len(claims) > 8 {
			return body, ErrInvalid
		}
		for _, claim := range claims {
			if !studioText(claim.Text, 1500) || len(claim.Citations) < 1 || len(claim.Citations) > 10 {
				return body, ErrInvalid
			}
			stored := StudioClaim{Text: claim.Text, Citations: []string{}}
			local := map[studioReference]bool{}
			for _, ref := range claim.Citations {
				if ref.Source < 1 || ref.Source > len(sources) || ref.Fragment < 1 || ref.Fragment > len(sources[ref.Source-1].Fragments) || local[ref] {
					return body, ErrStudioCitation
				}
				local[ref] = true
				id, ok := seen[ref]
				if !ok {
					if len(body.Citations) >= 100 {
						return body, ErrInvalid
					}
					id = strconv.Itoa(len(body.Citations) + 1)
					seen[ref] = id
					source := sources[ref.Source-1]
					fragment := source.Fragments[ref.Fragment-1]
					body.Citations = append(body.Citations, StudioCitation{ID: id, Source: source.SourceItem, Fragment: fragment, DocumentSHA256: manifest.Entries[ref.Source-1].SHA256, SliceSHA256: knowledge.SourceTextHash(fragment.Text)})
				}
				stored.Citations = append(stored.Citations, id)
			}
			groups[field] = append(groups[field], stored)
		}
	}
	if kind == "guide" {
		body.Guide = &StudioGuide{groups["key_points"], groups["terms"], groups["learning_order"], groups["misconceptions"]}
	} else {
		body.Brief = &StudioBrief{groups["overview"], groups["differences"], groups["gaps"]}
	}
	// A model cannot upgrade known extraction gaps or bounded fragments to complete.
	for _, source := range sources {
		if len(source.Fragments) == 0 || source.Metadata != nil && source.Metadata.Status == "partial" {
			body.Coverage = "partial"
		}
		for _, fragment := range source.Fragments {
			if fragment.Truncated {
				body.Coverage = "partial"
			}
		}
	}
	return body, nil
}

// VerifyStudioSources is called on publication and on every later read. It
// verifies exact historical identity, range, text and hashes, never a new head.
func VerifyStudioSources(body StudioBody, sources []knowledge.SourceDetail) error {
	if body.SourceManifest == nil || len(body.SourceManifest.Entries) != len(sources) || len(sources) == 0 || len(body.Citations) == 0 {
		return ErrInvalid
	}
	for i, c := range body.Citations {
		if c.ID != strconv.Itoa(i+1) {
			return ErrInvalid
		}
		found := false
		for j, source := range sources {
			if source.CollectionID != c.Source.CollectionID || source.RevisionID != c.Source.RevisionID || source.DocumentID != c.Source.DocumentID || source.DocumentRevisionID != c.Source.DocumentRevisionID {
				continue
			}
			if body.SourceManifest.Entries[j].SHA256 != c.DocumentSHA256 {
				return ErrInvalid
			}
			for _, fragment := range source.Fragments {
				if fragment.NodeID == c.Fragment.NodeID && fragment.NodeRevisionID == c.Fragment.NodeRevisionID && fragment.Range == c.Fragment.Range && fragment.Text == c.Fragment.Text && knowledge.SourceTextHash(fragment.Text) == c.SliceSHA256 {
					found = true
					break
				}
			}
		}
		if !found {
			return ErrInvalid
		}
	}
	return nil
}
