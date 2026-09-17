package knowledge

import "github.com/google/uuid"

// ReferenceEntry 固定实际采用的版本；角色不改变来源的身份或正文。
type ReferenceEntry struct {
	ScopeEntry
	Role string `json:"role"`
}

type ReferenceSelection struct {
	SessionID string           `json:"session_id"`
	Entries   []ReferenceEntry `json:"entries"`
}

func (s ReferenceSelection) Validate() error {
	if s.SessionID != "" && uuid.Validate(s.SessionID) != nil || s.Entries == nil || len(s.Entries) > 100 {
		return &Error{Code: CodeInvalidRequest}
	}
	seen := map[ScopeEntry]bool{}
	for _, e := range s.Entries {
		if uuid.Validate(e.CollectionID) != nil || uuid.Validate(e.RevisionID) != nil || e.DocumentID != "" && uuid.Validate(e.DocumentID) != nil || e.NodeID != "" && (e.DocumentID == "" || uuid.Validate(e.NodeID) != nil) || seen[e.ScopeEntry] {
			return &Error{Code: CodeInvalidRequest}
		}
		if e.Role != "supplement" && e.Role != "prefer" && e.Role != "restrict" {
			return &Error{Code: CodeInvalidRequest}
		}
		seen[e.ScopeEntry] = true
	}
	return nil
}

type ReferenceState struct {
	Version         int64              `json:"version"`
	ContextID       string             `json:"context_id"`
	ScopeSnapshotID string             `json:"scope_snapshot_id"`
	Selection       ReferenceSelection `json:"selection"`
}

// ReferenceScope 在冻结之前执行限制，不把范围外正文交给检索器或模型。
func ReferenceScope(base []ScopeEntry, entries []ReferenceEntry) []ScopeEntry {
	restricted := false
	for _, e := range entries {
		restricted = restricted || e.Role == "restrict"
	}
	result := []ScopeEntry{}
	if !restricted {
		result = append(result, base...)
	}
	for _, e := range entries {
		if !restricted || e.Role == "restrict" {
			result = append(result, e.ScopeEntry)
		}
	}
	seen := map[ScopeEntry]bool{}
	unique := []ScopeEntry{}
	for _, e := range result {
		if !seen[e] {
			unique = append(unique, e)
			seen[e] = true
		}
	}
	return unique
}
