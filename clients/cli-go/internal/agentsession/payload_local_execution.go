package agentsession

import (
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

// Dirty v6 allowed file journals, but no local execution intents. Keep the
// nested DTOs frozen so future effects cannot be smuggled into old payloads.
type dirtyPayloadV6 struct {
	dirtyPayloadV1
	File        *fileWriteAheadV3    `json:"file,omitempty"`
	FileJournal []fileJournalEntryV6 `json:"file_journal,omitempty"`
}

type fileJournalEntryV6 struct {
	WriteAhead fileWriteAheadV3 `json:"write_ahead"`
	Result     *fileReceiptV4   `json:"result,omitempty"`
	Unchanged  bool             `json:"unchanged,omitempty"`
}

func upcastDirtyV6(v dirtyPayloadV6) (DirtyMarker, error) {
	if v.SchemaVersion != 6 {
		return DirtyMarker{}, ErrCorrupt
	}
	m := DirtyMarker{
		SchemaVersion: dirtySchemaVersion, DirtyID: v.DirtyID, SessionID: v.SessionID,
		StorageID: v.StorageID, BaseRevision: v.BaseRevision, TurnSequence: v.TurnSequence,
		OperationClass: v.OperationClass, MayHaveSideEffect: v.MayHaveSideEffect,
		StartedAt: v.StartedAt, Preference: v.Preference,
	}
	if v.File != nil {
		ahead, err := upcastWriteAheadV5(*v.File)
		if err != nil {
			return DirtyMarker{}, err
		}
		m.File = &ahead
	}
	for _, entry := range v.FileJournal {
		ahead, err := upcastWriteAheadV5(entry.WriteAhead)
		if err != nil {
			return DirtyMarker{}, err
		}
		next := FileJournalEntry{WriteAhead: ahead, Unchanged: entry.Unchanged}
		if old := entry.Result; old != nil {
			effect, err := upcastEffectV6(old.Effect)
			if err != nil {
				return DirtyMarker{}, err
			}
			r := FileReceipt{ToolCallID: old.ToolCallID, Effect: effect,
				InvalidateObserved: old.InvalidateObserved, StableCode: old.StableCode, Outcome: old.Outcome}
			if validateFileReceipt(r) != nil || !ahead.Effect.SamePlan(r.Effect) {
				return DirtyMarker{}, ErrCorrupt
			}
			next.Result = &r
		}
		m.FileJournal = append(m.FileJournal, next)
	}
	if validateDirtyMarker(m) != nil {
		return DirtyMarker{}, ErrCorrupt
	}
	return m, nil
}

func upcastEffectV6(v fileEffectV4) (fileeffects.Effect, error) {
	switch v.Operation {
	case "write_create", "write_replace", "edit", "archive", "mkdir", "copy", "move":
	default:
		return fileeffects.Effect{}, ErrCorrupt
	}
	e := fileeffects.Effect{SchemaVersion: v.SchemaVersion, Operation: v.Operation,
		Source: fileeffects.Endpoint(v.Source), Target: fileeffects.Endpoint(v.Target),
		Scope: v.Scope, Directories: fileeffects.DirectoryChain(v.Directories)}
	if e.Validate() != nil {
		return fileeffects.Effect{}, ErrCorrupt
	}
	return e, nil
}

func validateLocalEffects(marker DirtyMarker, limit int) error {
	if len(marker.LocalEffects) > limit {
		return ErrStoreFull
	}
	seen := make(map[string]bool, len(marker.LocalEffects))
	if marker.Preference != nil {
		seen[marker.Preference.ToolCallID] = true
	}
	if marker.File != nil {
		seen[marker.File.ToolCallID] = true
	}
	for _, entry := range marker.FileJournal {
		seen[entry.WriteAhead.ToolCallID] = true
	}
	for _, effect := range marker.LocalEffects {
		if effect.ToolCallID == "" || strings.TrimSpace(effect.ToolCallID) != effect.ToolCallID || !safeText(effect.ToolCallID, 256) || seen[effect.ToolCallID] {
			return ErrInvalid
		}
		seen[effect.ToolCallID] = true
		switch effect.Operation {
		case "shell":
			if effect.TaskID != "" {
				return ErrInvalid
			}
		case "task_input", "task_close_input", "task_stop":
			if !validLocalTaskID(effect.TaskID) {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	return nil
}

func validLocalTaskID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c != '_' && c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func validateLocalEffectTransition(current, candidate DirtyMarker) error {
	if len(candidate.LocalEffects) < len(current.LocalEffects) {
		return ErrCheckpointConflict
	}
	for i, old := range current.LocalEffects {
		if old != candidate.LocalEffects[i] {
			return ErrCheckpointConflict
		}
	}
	return nil
}
