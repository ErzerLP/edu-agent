package agentsession

import "reflect"

func validateFileJournal(marker DirtyMarker, limit int) error {
	count := len(marker.FileJournal)
	if marker.File != nil {
		count++
	}
	if count > limit {
		return ErrStoreFull
	}
	seen := make(map[string]bool, count+1)
	if marker.Preference != nil {
		seen[marker.Preference.ToolCallID] = true
	}
	if marker.File != nil {
		if seen[marker.File.ToolCallID] {
			return ErrInvalid
		}
		seen[marker.File.ToolCallID] = true
	}
	for _, entry := range marker.FileJournal {
		if seen[entry.WriteAhead.ToolCallID] || validateFileWriteAhead(entry.WriteAhead) != nil || entry.Unchanged && entry.Result != nil {
			return ErrInvalid
		}
		seen[entry.WriteAhead.ToolCallID] = true
		if result := entry.Result; result != nil {
			if validateFileReceipt(*result) != nil || result.ToolCallID != entry.WriteAhead.ToolCallID || !entry.WriteAhead.Effect.SamePlan(result.Effect) || result.Effect.Directories.Created < entry.WriteAhead.Effect.Directories.Created {
				return ErrInvalid
			}
		}
	}
	return nil
}

// Existing evidence is append-only. An unresolved WAL may be settled once,
// never replaced with a new plan or removed. Raw-byte CAS in UpdateDirty still
// protects publication; these checks additionally reject stale logical updates.
func validateFileJournalTransition(current, candidate DirtyMarker) error {
	entries := make(map[string]FileJournalEntry, len(candidate.FileJournal)+1)
	for _, entry := range candidate.FileJournal {
		entries[entry.WriteAhead.ToolCallID] = entry
	}
	if candidate.File != nil {
		entries[candidate.File.ToolCallID] = FileJournalEntry{WriteAhead: *candidate.File}
	}
	previous := append([]FileJournalEntry(nil), current.FileJournal...)
	if current.File != nil {
		previous = append(previous, FileJournalEntry{WriteAhead: *current.File})
	}
	for _, old := range previous {
		next, ok := entries[old.WriteAhead.ToolCallID]
		if !ok || old.WriteAhead != next.WriteAhead || (old.Result != nil || old.Unchanged) && !reflect.DeepEqual(old, next) {
			return ErrCheckpointConflict
		}
	}
	return nil
}
