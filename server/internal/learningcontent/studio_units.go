package learningcontent

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

type StudioSectionInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UnitCount int    `json:"unit_count"`
}
type StudioSummaryClaim struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}
type StudioSummary struct {
	Overview    []StudioSummaryClaim `json:"overview"`
	Transitions []StudioSummaryClaim `json:"transitions"`
	Uncovered   []string             `json:"uncovered"`
}
type StudioUnit struct {
	ID               string                       `json:"id"`
	SectionID        string                       `json:"section_id"`
	Ordinal          int                          `json:"ordinal"`
	Status           string                       `json:"status"`
	Attempt          int                          `json:"attempt"`
	RunID            string                       `json:"run_id,omitempty"`
	Topic            string                       `json:"topic"`
	Objective        string                       `json:"objective"`
	Slice            knowledge.SourceSliceRequest `json:"-"`
	InputFingerprint string                       `json:"input_fingerprint"`
	Body             *StudioBody                  `json:"body,omitempty"`
}
type studioUnitPayload struct {
	SectionID, Topic, Objective, InputFingerprint string
	Slice                                         knowledge.SourceSliceRequest
	Body                                          *StudioBody
}
type StudioUnitPage struct {
	Items      []StudioUnit `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// Sections use one closed claim shape for all document kinds. The kind and
// confirmed objective determine presentation, not the provenance contract.
func StudioSectionSchema() json.RawMessage {
	var schema map[string]any
	_ = json.Unmarshal(StudioSchema("brief"), &schema)
	properties := schema["properties"].(map[string]any)
	properties["claims"] = properties["overview"]
	delete(properties, "overview")
	delete(properties, "differences")
	delete(properties, "gaps")
	schema["required"] = []string{"title", "coverage", "claims"}
	raw, _ := json.Marshal(schema)
	return raw
}
func DecodeStudioSection(raw []byte, manifest *knowledge.SourceManifest, sources []knowledge.SourceDetail) (StudioBody, error) {
	var candidate struct {
		Title    string                 `json:"title"`
		Coverage string                 `json:"coverage"`
		Claims   []studioCandidateClaim `json:"claims"`
	}
	if agentcore.DecodeArguments(string(raw), &candidate) != nil {
		return StudioBody{}, ErrInvalid
	}
	// Reuse the citation boundary parser; no second interpretation of refs.
	adapted, _ := json.Marshal(map[string]any{"title": candidate.Title, "coverage": candidate.Coverage, "overview": candidate.Claims, "differences": candidate.Claims, "gaps": candidate.Claims})
	body, err := DecodeStudioCandidate("brief", adapted, manifest, sources)
	if err != nil {
		return body, err
	}
	body.Claims, body.Brief = body.Brief.Overview, nil
	body.Schema = 2
	var markdown strings.Builder
	for _, claim := range body.Claims {
		markdown.WriteString(claim.Text)
		for _, id := range claim.Citations {
			markdown.WriteString(" [" + id + "]")
		}
		markdown.WriteString("\n\n")
	}
	body.Markdown = strings.TrimSpace(markdown.String())
	return body, nil
}
func StudioClaimID(unit string, index int) string { return unit + ":" + strconv.Itoa(index+1) }
