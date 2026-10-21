package knowledge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
)

// NoteSourceReader is the formal learningcontent read port. Only the server wires
// this port; callers cannot submit their own text as a saved note revision.
type SavedNote struct{ Title, Markdown string }
type NoteSourceReader func(context.Context, string, int64) (SavedNote, error)
type NoteSourceCandidate struct {
	DerivedFrom SourceDerivation `json:"derived_from"`
	Title       string           `json:"title"`
	Document    ImportDocument   `json:"document"`
	SHA256      string           `json:"sha256"`
}

func (s *Service) PrepareNoteSource(ctx context.Context, read NoteSourceReader, id string, version int64) (NoteSourceCandidate, error) {
	if !validUUID(id) || version < 1 || read == nil {
		return NoteSourceCandidate{}, &Error{Code: CodeInvalidRequest}
	}
	// Capture the destination generation before reading the source, so a clear
	// between the owners cannot issue a receipt for old text in a new generation.
	store, ok := s.store.(interface {
		SourceGeneration(context.Context) (int64, error)
	})
	if !ok {
		return NoteSourceCandidate{}, &Error{Code: CodeNotFound}
	}
	generation, err := store.SourceGeneration(ctx)
	if err != nil {
		return NoteSourceCandidate{}, err
	}
	note, err := read(ctx, id, version)
	if err != nil {
		return NoteSourceCandidate{}, err
	}
	if strings.TrimSpace(note.Markdown) == "" {
		return NoteSourceCandidate{}, &Error{Code: CodeInvalidRequest}
	}
	derivation := SourceDerivation{ArtifactID: id, Version: version}
	hash := SourceTextHash(note.Markdown)
	metadata := SourceMetadata{Kind: "text", MediaType: "text/markdown", Parser: ParserVersion, Coverage: "user_note_snapshot", Fingerprint: hash, Status: "parsed", DerivedFrom: &derivation}
	raw, _ := json.Marshal(fetchedReceipt{space.Scope(ctx), generation, hash, metadata})
	receipt := base64.RawURLEncoding.EncodeToString(raw) + "." + sourceSignature(raw)
	return NoteSourceCandidate{DerivedFrom: derivation, Title: note.Title, SHA256: hash, Document: ImportDocument{Path: fmt.Sprintf("note-%s-v%d.md", id, version), Markdown: note.Markdown, SourceReceipt: receipt}}, nil
}
