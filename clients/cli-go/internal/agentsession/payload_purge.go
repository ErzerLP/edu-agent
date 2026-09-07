package agentsession

import (
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

// C9 wire shapes remain frozen: record v8/dirty v9 cannot contain purge facts.
// All nested effect, endpoint, directory and local-intent DTOs are historical.
type recordPayloadV8 struct {
	recordPayloadV1
	FileReceipts []fileReceiptV4 `json:"file_receipts,omitempty"`
}

type dirtyPayloadV9 struct{ dirtyPayloadV8 }

func validateFileEffectV3(v fileEffectV4) error {
	if v.SchemaVersion != 3 {
		return validateFileEffectV2(v)
	}
	parts := strings.SplitN(v.Source.Path, "/", 3)
	if v.Operation != "restore_archive" || !fileeffects.ValidPath(v.Source.Path, false) || len(parts) != 3 || parts[0] != fileeffects.ArchiveDirectory || !fileeffects.ValidPath(v.Target.Path, false) || fileeffects.Protected(v.Target.Path) || v.Source.Kind != v.Target.Kind || !strings.HasPrefix(v.Source.Version, "entry-v1:") || !fileeffects.ValidVersion(v.Source.Version) || v.Target.Version != "" || v.Directories != (directoryChainV4{}) {
		return ErrCorrupt
	}
	if v.Source.Kind == "file" && v.Scope == "entry" || v.Source.Kind == "directory" && v.Scope == "subtree" {
		return nil
	}
	return ErrCorrupt
}

func upcastEffectV8(v fileEffectV4) (fileeffects.Effect, error) {
	if validateFileEffectV3(v) != nil {
		return fileeffects.Effect{}, ErrCorrupt
	}
	return fileeffects.Effect{SchemaVersion: v.SchemaVersion, Operation: v.Operation, Source: fileeffects.Endpoint(v.Source), Target: fileeffects.Endpoint(v.Target), Scope: v.Scope, Directories: fileeffects.DirectoryChain(v.Directories)}, nil
}

func upcastReceiptV8(v fileReceiptV4) (FileReceipt, error) {
	e, err := upcastEffectV8(v.Effect)
	if err != nil {
		return FileReceipt{}, err
	}
	r := FileReceipt{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.Outcome}
	if validateFileReceipt(r) != nil {
		return FileReceipt{}, ErrCorrupt
	}
	return r, nil
}

func upcastWriteAheadV9(v fileWriteAheadV3) (FileWriteAhead, error) {
	e, err := upcastEffectV8(v.Effect)
	if err != nil {
		return FileWriteAhead{}, err
	}
	r := FileWriteAhead{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, PublicationOutcome: v.PublicationOutcome}
	if validateFileWriteAhead(r) != nil {
		return FileWriteAhead{}, ErrCorrupt
	}
	return r, nil
}

func upcastDirtyV9(v dirtyPayloadV9) (DirtyMarker, error) {
	if v.SchemaVersion != 9 {
		return DirtyMarker{}, ErrCorrupt
	}
	m := DirtyMarker{SchemaVersion: dirtySchemaVersion, DirtyID: v.DirtyID, SessionID: v.SessionID, StorageID: v.StorageID, BaseRevision: v.BaseRevision, TurnSequence: v.TurnSequence, OperationClass: v.OperationClass, MayHaveSideEffect: v.MayHaveSideEffect, StartedAt: v.StartedAt, Preference: v.Preference}
	if v.File != nil {
		ahead, err := upcastWriteAheadV9(*v.File)
		if err != nil {
			return DirtyMarker{}, err
		}
		m.File = &ahead
	}
	for _, entry := range v.FileJournal {
		ahead, err := upcastWriteAheadV9(entry.WriteAhead)
		if err != nil {
			return DirtyMarker{}, err
		}
		next := FileJournalEntry{WriteAhead: ahead, Unchanged: entry.Unchanged}
		if entry.Result != nil {
			r, err := upcastReceiptV8(*entry.Result)
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
