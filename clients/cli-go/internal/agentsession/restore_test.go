package agentsession

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

func archiveRestoreReceiptForTest(kind, outcome string) FileReceipt {
	e := fileeffects.New("restore_archive", ".edu-agent-archive/old-container/tree/item", "recovered/item", kind)
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	r := FileReceipt{ToolCallID: "restore-1", Effect: e, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: outcome}
	if outcome == NoticeOutcomeCompleted {
		r.StableCode = FilePublicationCompletedCode
	}
	return r
}

func TestArchiveRestorePayloadRoundTrip(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		for _, outcome := range []string{NoticeOutcomeCompleted, NoticeOutcomeUnknown} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				s := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
				defer s.Close()
				h, record, err := s.Create(t.Context(), CreateInput{Title: "restore", Checkpoint: []byte(`{"v":1}`)})
				if err != nil {
					t.Fatal(err)
				}
				defer h.Close()
				marker, err := h.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
				if err != nil {
					t.Fatal(err)
				}
				r := archiveRestoreReceiptForTest(kind, outcome)
				ahead := FileWriteAhead{ToolCallID: r.ToolCallID, Effect: r.Effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
				marker.MayHaveSideEffect, marker.File = true, &ahead
				marker, err = h.UpdateDirty(t.Context(), marker)
				if err != nil {
					t.Fatal(err)
				}
				plain, header := dirtyPayloadOnDiskForTest(t, s, h.dataKey, record)
				version, err := probeRecordPayloadSchema(plain, s.limits.DirtyMarkerBytes)
				if err != nil || version != dirtySchemaVersion || header.SchemaVersion != 1 {
					t.Fatal(version, header, err)
				}
				loaded, err := h.Load()
				if err != nil || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, marker) {
					t.Fatal(loaded, err)
				}
				marker.File, marker.FileJournal = nil, []FileJournalEntry{{WriteAhead: ahead, Result: &r}}
				marker, err = h.UpdateDirty(t.Context(), marker)
				if err != nil {
					t.Fatal(err)
				}
				record.FileReceipts, record.LastConsumedDirtyID = []FileReceipt{r}, marker.DirtyID
				saved, err := h.Save(t.Context(), record.RecordRevision, record)
				if err != nil {
					t.Fatal(err)
				}
				if version, header := recordPayloadVersionOnDiskForTest(t, s, h.dataKey, saved); version != recordPayloadSchemaVersion || header.SchemaVersion != 1 {
					t.Fatal(version, header)
				}
				loaded, err = h.Load()
				if err != nil || loaded.Interrupted != nil || !reflect.DeepEqual(loaded.Record.FileReceipts, []FileReceipt{r}) {
					t.Fatal(loaded, err)
				}
				bad := r
				bad.InvalidateObserved = false
				if validateFileReceipt(bad) == nil {
					t.Fatal("restore must invalidate both endpoints")
				}
				bad = r
				bad.Effect.Target.Version = bad.Effect.Source.Version
				if validateFileReceipt(bad) == nil {
					t.Fatal("source version reused as target version")
				}
			})
		}
	}
}

func TestArchiveRestoreFrozenC8Migration(t *testing.T) {
	s, h, record, marker := journalStore(t)
	marker = journalSettled(marker)
	copyReceipt := directoryCopyReceiptForTest(NoticeOutcomeCompleted)
	copyAhead := FileWriteAhead{ToolCallID: copyReceipt.ToolCallID, Effect: copyReceipt.Effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
	marker.File = &copyAhead
	marker.LocalEffects = []LocalEffectIntent{{ToolCallID: "local-shell", Operation: "shell"}, {ToolCallID: "local-input", Operation: "task_input", TaskID: "task_1"}}
	marker.SchemaVersion = 8
	plain, err := encodeStrict(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeDirtyPayloadForTest(t, s, h.dataKey, record, plain)
	record.FileReceipts = []FileReceipt{*marker.FileJournal[0].Result, copyReceipt}
	writeRecordPayloadForTest(t, s, h.dataKey, record, 7, nil)
	if version, _ := recordPayloadVersionOnDiskForTest(t, s, h.dataKey, record); version != 7 {
		t.Fatal("fixture is not the old record payload", version)
	}
	oldDirty := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
	marker.SchemaVersion = dirtySchemaVersion
	loaded, err := h.Load()
	if err != nil || loaded.Record.SchemaVersion != recordPayloadSchemaVersion || !recordsEqual(loaded.Record, record) || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, marker) {
		t.Fatalf("lost C8 facts: %+v %v", loaded, err)
	}
	// Compatible records use the existing atomic migration publication; the
	// interrupted WAL stays untouched until explicit checkpoint consumption.
	if version, _ := recordPayloadVersionOnDiskForTest(t, s, h.dataKey, loaded.Record); version != recordPayloadSchemaVersion {
		t.Fatal("compatible record was not migrated", version)
	}
	if !bytes.Equal(oldDirty, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
		t.Fatal("migration changed interrupted WAL evidence")
	}
	for _, point := range []string{`"file_journal":[{`, `"write_ahead":{`, `"effect":{`, `"source":{`, `"target":{`, `"directories":{`, `"local_effects":[{`} {
		bad := bytes.ReplaceAll(plain, []byte(point), []byte(point+`"future":null,`))
		if bytes.Equal(bad, plain) {
			t.Fatal("fixture missing", point)
		}
		if _, err := decodeDirtyPayload(bad, 1<<20); !errors.Is(err, ErrCorrupt) {
			t.Fatal(point, err)
		}
	}
	record.SchemaVersion = 7
	recordPlain, _ := encodeStrict(record)
	for _, point := range []string{`"file_receipts":[{`, `"effect":{`, `"source":{`, `"target":{`, `"directories":{`} {
		bad := bytes.ReplaceAll(recordPlain, []byte(point), []byte(point+`"future":null,`))
		if bytes.Equal(bad, recordPlain) {
			t.Fatal("fixture missing", point)
		}
		if _, _, err := decodeRecordPayload(bad, 1<<20); !errors.Is(err, ErrCorrupt) {
			t.Fatal(point, err)
		}
	}
}

func TestArchiveRestoreCannotBeSmuggledIntoLegacyPayloads(t *testing.T) {
	_, _, record, marker := journalStore(t)
	for _, effectVersion := range []int{1, 2, 3} {
		r := archiveRestoreReceiptForTest("directory", NoticeOutcomeUnknown)
		r.Effect.SchemaVersion = effectVersion
		for version := 1; version <= 7; version++ {
			record.SchemaVersion, record.FileReceipts = version, []FileReceipt{r}
			plain, _ := encodeStrict(record)
			if _, _, err := decodeRecordPayload(plain, 1<<20); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("record v%d effect v%d: %v", version, effectVersion, err)
			}
		}
		for version := 1; version <= 8; version++ {
			for _, slot := range []string{"file", "journal-wal", "journal-result"} {
				m := marker
				m.SchemaVersion, m.File, m.FileJournal = version, nil, nil
				ahead := FileWriteAhead{ToolCallID: r.ToolCallID, Effect: r.Effect, InvalidateObserved: true, StableCode: r.StableCode, PublicationOutcome: r.Outcome}
				switch slot {
				case "file":
					m.File = &ahead
				case "journal-wal":
					m.FileJournal = []FileJournalEntry{{WriteAhead: ahead}}
				case "journal-result":
					m.FileJournal = []FileJournalEntry{{WriteAhead: journalAhead("legacy"), Result: &r}}
				}
				plain, _ := encodeStrict(m)
				if _, err := decodeDirtyPayload(plain, 1<<20); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("dirty v%d effect v%d %s: %v", version, effectVersion, slot, err)
				}
			}
		}
	}
}
