package agentsession

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

func TestMkdirFileEffectPayloadRoundTrip(t *testing.T) {
	for _, created := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(created), func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer store.Close()
			handle, record, err := store.Create(t.Context(), CreateInput{Title: "mkdir", Checkpoint: []byte(`{"v":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
			if err != nil {
				t.Fatal(err)
			}
			effect := fileeffects.New("mkdir", "", "a/b", "directory")
			effect.Directories = fileeffects.DirectoryChain{Anchor: ".", Count: 2}
			marker.MayHaveSideEffect = true
			marker.File = &FileWriteAhead{ToolCallID: "mkdir", Effect: effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
			marker, err = handle.UpdateDirty(t.Context(), marker)
			if err != nil {
				t.Fatal(err)
			}
			plain, header := dirtyPayloadOnDiskForTest(t, store, handle.dataKey, record)
			v, err := probeRecordPayloadSchema(plain, store.limits.DirtyMarkerBytes)
			if err != nil || v != dirtySchemaVersion || header.SchemaVersion != 1 {
				t.Fatalf("dirty=%d header=%d err=%v", v, header.SchemaVersion, err)
			}
			effect.Directories.Created = created
			outcome, code := NoticeOutcomeUnknown, FilePublicationUnknownCode
			if created == 2 {
				outcome, code = NoticeOutcomeCompleted, FilePublicationCompletedCode
			}
			receipt := FileReceipt{ToolCallID: "mkdir", Effect: effect, InvalidateObserved: true, StableCode: code, Outcome: outcome}
			record.FileReceipts = []FileReceipt{receipt}
			record.LastConsumedDirtyID = marker.DirtyID
			saved, err := handle.Save(t.Context(), record.RecordRevision, record)
			if err != nil {
				t.Fatal(err)
			}
			if v, h := recordPayloadVersionOnDiskForTest(t, store, handle.dataKey, saved); v != recordPayloadSchemaVersion || h.SchemaVersion != 1 {
				t.Fatalf("record=%d container=%d", v, h.SchemaVersion)
			}
			loaded, err := handle.Load()
			if err != nil || loaded.Interrupted != nil || !reflect.DeepEqual(loaded.Record.FileReceipts, []FileReceipt{receipt}) {
				t.Fatalf("loaded=%+v err=%v", loaded, err)
			}
			if bytes.Contains(plain, []byte("archive_path")) || bytes.Contains(plain, []byte("identity")) || bytes.Contains(plain, []byte("workspace_root")) {
				t.Fatalf("unsafe new WAL: %s", plain)
			}
			bad := receipt
			bad.Outcome = NoticeOutcomeCompleted
			bad.StableCode = FilePublicationCompletedCode
			bad.Effect.Directories.Created = 1
			if !errors.Is(validateFileReceipt(bad), ErrInvalid) {
				t.Fatal("partial accepted as completed")
			}
		})
	}
}
func TestFileEffectLegacyV3AndDirtyV2RemainFrozen(t *testing.T) {
	if validateLegacyV1Receipts([]fileReceiptV1{{ToolCallID: "legacy", Operation: "write_create", Path: " leading", Kind: "file", StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted}}) == nil {
		t.Fatal("relaxed legacy path validation")
	}
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "legacy-v3", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	legacyArchive := archiveReceiptForTest("directory", NoticeOutcomeUnknown)
	record.FileReceipts = []FileReceipt{upcastFileReceipt(legacyArchive), upcastFileReceipt(fileReceiptV3{ToolCallID: "write", Operation: "write_replace", Path: "notes.md", Kind: "file", ContentHash: "sha256:" + strings.Repeat("a", 64), StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted})}
	marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
	if err != nil {
		t.Fatal(err)
	}
	legacy := dirtyPayloadV2{dirtyPayloadV1: dirtyPayloadV1{SchemaVersion: 2, DirtyID: marker.DirtyID, SessionID: marker.SessionID, StorageID: marker.StorageID, BaseRevision: marker.BaseRevision, TurnSequence: marker.TurnSequence, OperationClass: marker.OperationClass, MayHaveSideEffect: true, StartedAt: marker.StartedAt}, File: &fileWriteAheadV2{ToolCallID: legacyArchive.ToolCallID, Operation: legacyArchive.Operation, Path: legacyArchive.Path, ArchivePath: legacyArchive.ArchivePath, Kind: legacyArchive.Kind, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}}
	old, err := encodeStrict(legacy)
	if err != nil {
		t.Fatal(err)
	}
	writeDirtyPayloadForTest(t, store, handle.dataKey, record, old)
	writeRecordPayloadForTest(t, store, handle.dataKey, record, 3, nil)
	before := readSessionArtifactForTest(t, store, dirtyName(record.StorageID))
	loaded, err := handle.Load()
	if err != nil || !recordsEqual(record, loaded.Record) || loaded.Interrupted == nil {
		t.Fatalf("load=%+v err=%v", loaded, err)
	}
	if loaded.Interrupted.File.Effect.Source.Version != "" || loaded.Interrupted.File.Effect.Directories.Count != 0 {
		t.Fatal("fabricated legacy metadata or creations")
	}
	if !bytes.Equal(before, readSessionArtifactForTest(t, store, dirtyName(record.StorageID))) {
		t.Fatal("legacy dirty evidence rewritten by load")
	}
	for _, field := range []string{`"effect":null,`, `"effect":{},`, `"created":0,`} {
		writeRecordPayloadForTest(t, store, handle.dataKey, record, 3, func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"tool_call_id":`), []byte(field+`"tool_call_id":`), 1)
		})
		if _, _, _, _, err := store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("record v3 accepted %s: %v", field, err)
		}
		malformed := bytes.Replace(old, []byte(`"tool_call_id":`), []byte(field+`"tool_call_id":`), 1)
		writeDirtyPayloadForTest(t, store, handle.dataKey, record, malformed)
		if _, _, err := store.readDirty(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("dirty v2 accepted %s: %v", field, err)
		}
	}
}
