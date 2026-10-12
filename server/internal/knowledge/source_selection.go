package knowledge

import (
	"encoding/json"
	"sort"
)

// SourceSelection is request intent, never a reference policy or a collection head.
type SourceSelection struct {
	Mode  string           `json:"mode"`
	Items []SourceIdentity `json:"items"`
}

type SourceIdentity struct {
	CollectionID       string `json:"collection_id"`
	RevisionID         string `json:"revision_id"`
	DocumentID         string `json:"document_id"`
	DocumentRevisionID string `json:"document_revision_id"`
}

type SourceManifestEntry struct {
	SourceIdentity
	SHA256 string `json:"sha256"`
}

// Schema 1 is additive to existing encrypted mentor body/history schemas.
type SourceManifest struct {
	Schema     int                   `json:"schema"`
	SpaceID    string                `json:"space_id"`
	Generation int64                 `json:"generation"`
	Entries    []SourceManifestEntry `json:"entries"`
	SHA256     string                `json:"sha256"`
}

func (s SourceSelection) Validate() error {
	if s.Items == nil || len(s.Items) > 100 || (s.Mode != "none" && s.Mode != "items") || (s.Mode == "none") != (len(s.Items) == 0) {
		return &Error{Code: CodeInvalidRequest}
	}
	seen := map[string]bool{}
	for _, item := range s.Items {
		key := item.CollectionID + ":" + item.DocumentID
		if !validUUID(item.CollectionID) || !validUUID(item.RevisionID) || !validUUID(item.DocumentID) || !validUUID(item.DocumentRevisionID) || seen[key] {
			return &Error{Code: CodeInvalidRequest}
		}
		seen[key] = true
	}
	return nil
}

func SourceTextHash(text string) string { return sha256Hex([]byte(text)) }

func (m SourceManifest) Hash() string {
	m.SHA256 = ""
	raw, _ := json.Marshal(m)
	return sha256Hex(raw)
}

func SortSourceEntries(entries []SourceManifestEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		return a.CollectionID+":"+a.DocumentID < b.CollectionID+":"+b.DocumentID
	})
}
