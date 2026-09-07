package agentsession

import (
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

// C8 record v7/dirty v8 contain only effect v1/v2 and the original local
// intents. All nested wire shapes are frozen, not aliases to live DTOs.
type recordPayloadV7 struct {
	recordPayloadV1
	FileReceipts []fileReceiptV4 `json:"file_receipts,omitempty"`
}

type dirtyPayloadV8 struct {
	dirtyPayloadV7
}

func validateFileEffectV2(v fileEffectV4) error {
	if v.SchemaVersion == 1 {
		return validateFileEffectV1(v)
	}
	if v.SchemaVersion != 2 || v.Operation != "copy" || v.Source.Kind != "directory" || v.Target.Kind != "directory" || v.Scope != "subtree" || !fileeffects.ValidPath(v.Source.Path, false) || !fileeffects.ValidPath(v.Target.Path, false) || fileeffects.Protected(v.Source.Path) || fileeffects.Protected(v.Target.Path) || !strings.HasPrefix(v.Source.Version, "entry-v1:") || !fileeffects.ValidVersion(v.Source.Version) || v.Target.Version != "" || v.Directories != (directoryChainV4{}) {
		return ErrCorrupt
	}
	source, target := strings.Split(v.Source.Path, "/"), strings.Split(v.Target.Path, "/")
	if len(target) >= len(source) {
		prefix := true
		for i := range source {
			prefix = prefix && strings.EqualFold(source[i], target[i])
		}
		if prefix {
			return ErrCorrupt
		}
	}
	return nil
}

func upcastEffectV7(v fileEffectV4) (fileeffects.Effect, error) {
	if validateFileEffectV2(v) != nil {
		return fileeffects.Effect{}, ErrCorrupt
	}
	return fileeffects.Effect{SchemaVersion: v.SchemaVersion, Operation: v.Operation,
		Source: fileeffects.Endpoint(v.Source), Target: fileeffects.Endpoint(v.Target),
		Scope: v.Scope, Directories: fileeffects.DirectoryChain(v.Directories)}, nil
}

func upcastReceiptV7(v fileReceiptV4) (FileReceipt, error) {
	e, err := upcastEffectV7(v.Effect)
	if err != nil {
		return FileReceipt{}, err
	}
	r := FileReceipt{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.Outcome}
	if validateFileReceipt(r) != nil {
		return FileReceipt{}, ErrCorrupt
	}
	return r, nil
}

func upcastWriteAheadV8(v fileWriteAheadV3) (FileWriteAhead, error) {
	e, err := upcastEffectV7(v.Effect)
	if err != nil {
		return FileWriteAhead{}, err
	}
	r := FileWriteAhead{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, PublicationOutcome: v.PublicationOutcome}
	if validateFileWriteAhead(r) != nil {
		return FileWriteAhead{}, ErrCorrupt
	}
	return r, nil
}

func upcastDirtyV8(v dirtyPayloadV8) (DirtyMarker, error) {
	if v.SchemaVersion != 8 {
		return DirtyMarker{}, ErrCorrupt
	}
	m := DirtyMarker{
		SchemaVersion: dirtySchemaVersion, DirtyID: v.DirtyID, SessionID: v.SessionID,
		StorageID: v.StorageID, BaseRevision: v.BaseRevision, TurnSequence: v.TurnSequence,
		OperationClass: v.OperationClass, MayHaveSideEffect: v.MayHaveSideEffect,
		StartedAt: v.StartedAt, Preference: v.Preference,
	}
	if v.File != nil {
		ahead, err := upcastWriteAheadV8(*v.File)
		if err != nil {
			return DirtyMarker{}, err
		}
		m.File = &ahead
	}
	for _, entry := range v.FileJournal {
		ahead, err := upcastWriteAheadV8(entry.WriteAhead)
		if err != nil {
			return DirtyMarker{}, err
		}
		next := FileJournalEntry{WriteAhead: ahead, Unchanged: entry.Unchanged}
		if entry.Result != nil {
			r, err := upcastReceiptV7(*entry.Result)
			if err != nil || !ahead.Effect.SamePlan(r.Effect) {
				return DirtyMarker{}, ErrCorrupt
			}
			next.Result = &r
		}
		m.FileJournal = append(m.FileJournal, next)
	}
	var err error
	m.LocalEffects, err = upcastLocalEffectsV7(v.LocalEffects)
	if err != nil || validateDirtyMarker(m) != nil {
		return DirtyMarker{}, ErrCorrupt
	}
	return m, nil
}

func upcastLocalEffectsV7(v []localEffectIntentV7) ([]LocalEffectIntent, error) {
	if v == nil {
		return nil, nil
	}
	result := make([]LocalEffectIntent, len(v))
	for i, old := range v {
		switch old.Operation {
		case "shell":
			if old.TaskID != "" {
				return nil, ErrCorrupt
			}
		case "task_input", "task_close_input", "task_stop":
			if !validLocalTaskID(old.TaskID) {
				return nil, ErrCorrupt
			}
		default:
			return nil, ErrCorrupt
		}
		result[i] = LocalEffectIntent{ToolCallID: old.ToolCallID, Operation: old.Operation, TaskID: old.TaskID}
	}
	return result, nil
}
