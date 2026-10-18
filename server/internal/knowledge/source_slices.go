package knowledge

// Schema 2 identifies an exact subrange of a node's original local body.
// Offsets are half-open canonical UTF-8 bytes; line numbers are one-based.
type SourceSliceProof struct {
	Schema        int         `json:"schema"`
	OriginalRange SourceRange `json:"original_range"`
	Range         SourceRange `json:"range"`
	SHA256        string      `json:"sha256"`
}
type SourceSliceRequest struct {
	SourceIdentity
	NodeID         string `json:"node_id"`
	NodeRevisionID string `json:"node_revision_id"`
	Start          int    `json:"start"`
	End            int    `json:"end"`
}
type SourceDirectoryEntry struct {
	SourceIdentity
	NodeID         string      `json:"node_id"`
	NodeRevisionID string      `json:"node_revision_id"`
	Title          string      `json:"title"`
	Range          SourceRange `json:"range"`
}
type SourceDirectoryPage struct {
	Items      []SourceDirectoryEntry `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

func (s SourceItem) Identity() SourceIdentity {
	return SourceIdentity{s.CollectionID, s.RevisionID, s.DocumentID, s.DocumentRevisionID}
}

// current must be reconstructed by the owner reader from the exact immutable
// source. A model-supplied hash or text never serves as the comparison source.
func VerifySourceFragment(stored, current SourceFragment, hash string) bool {
	if stored.NodeID != current.NodeID || stored.NodeRevisionID != current.NodeRevisionID || stored.Range != current.Range || stored.Text != current.Text || SourceTextHash(current.Text) != hash {
		return false
	}
	if stored.Proof == nil {
		return current.Proof == nil
	} // Original whole-fragment schema.
	if stored.Proof.Schema != 2 || current.Proof == nil || current.Proof.Schema != 2 {
		return false
	}
	return *stored.Proof == *current.Proof && stored.Proof.Range == stored.Range && stored.Proof.SHA256 == hash
}

func SourceDocumentHash(m SourceManifest, id SourceIdentity) string {
	for _, e := range m.Entries {
		if e.SourceIdentity == id {
			return e.SHA256
		}
	}
	return ""
}
