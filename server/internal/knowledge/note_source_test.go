package knowledge

import (
	"context"
	"github.com/google/uuid"
	"testing"
)

type noteGenerationStore struct{ *previewGenerationStore }

func (s *noteGenerationStore) SourceGeneration(context.Context) (int64, error) {
	return s.generation, nil
}
func TestNoteSourceSnapshotUsesSavedVersionAndReceipt(t *testing.T) {
	store := &noteGenerationStore{&previewGenerationStore{memoryCatalogStore: newMemoryCatalogStore(), generation: 1}}
	s, _ := NewService(store, NewCanonicalizer(), ServiceOptions{})
	id := uuid.NewString()
	read := NoteSourceReader(func(ctx context.Context, artifact string, version int64) (SavedNote, error) {
		if artifact != id || version != 2 {
			t.Fatal("lost requested saved identity")
		}
		return SavedNote{Title: "我的笔记", Markdown: "# 个人理解\n\n旧正文\n"}, nil
	})
	candidate, err := s.PrepareNoteSource(t.Context(), read, id, 2)
	if err != nil {
		t.Fatal(err)
	}
	c := ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: uuid.NewString(), Source: "note snapshot", Documents: []ImportDocument{candidate.Document}}
	p, err := s.PreviewImport(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: c, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	m := result.Revision.Documents[0].Revision.SourceMetadata
	if m == nil || m.Kind != "text" || m.Coverage != "user_note_snapshot" || m.DerivedFrom == nil || m.DerivedFrom.ArtifactID != id || m.DerivedFrom.Version != 2 || m.Fingerprint != SourceTextHash(candidate.Document.Markdown) {
		t.Fatalf("lost provenance: %+v", m)
	}
	c.Documents[0].Markdown += "forged"
	if _, err = s.PreviewImport(t.Context(), c); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("accepted modified saved note", err)
	}
	store.generation = 2
	c.Documents[0] = candidate.Document
	if _, err = s.PreviewImport(t.Context(), c); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("old generation resurrected", err)
	}
}
