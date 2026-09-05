package agentsession

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"reflect"
	"strings"
	"testing"
)

func copyEffectForTest() fileeffects.Effect {
	e := fileeffects.New("copy", "input/source.bin", "output/copy.bin", "file")
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	return e
}
func TestCopyCurrentPayloadRoundTrip(t *testing.T) {
	for _, outcome := range []string{NoticeOutcomeCompleted, NoticeOutcomeUnknown} {
		t.Run(outcome, func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer store.Close()
			handle, record, err := store.Create(t.Context(), CreateInput{Title: "copy", Checkpoint: []byte(`{"v":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
			if err != nil {
				t.Fatal(err)
			}
			effect := copyEffectForTest()
			marker.MayHaveSideEffect = true
			marker.File = &FileWriteAhead{ToolCallID: "copy", Effect: effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
			marker, err = handle.UpdateDirty(t.Context(), marker)
			if err != nil {
				t.Fatal(err)
			}
			plain, header := dirtyPayloadOnDiskForTest(t, store, handle.dataKey, record)
			version, err := probeRecordPayloadSchema(plain, store.limits.DirtyMarkerBytes)
			if err != nil || version != dirtySchemaVersion || header.SchemaVersion != 1 {
				t.Fatal(version, header, err)
			}
			if bytes.Contains(plain, []byte("identity")) || bytes.Contains(plain, []byte("content_hash")) {
				t.Fatal("unsafe WAL", string(plain))
			}
			receipt := FileReceipt{ToolCallID: "copy", Effect: effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: outcome}
			if outcome == NoticeOutcomeCompleted {
				receipt.Effect.Target.Version = "sha256:" + strings.Repeat("b", 64)
				receipt.InvalidateObserved = false
				receipt.StableCode = FilePublicationCompletedCode
			}
			record.FileReceipts = []FileReceipt{receipt}
			record.LastConsumedDirtyID = marker.DirtyID
			saved, err := handle.Save(t.Context(), record.RecordRevision, record)
			if err != nil {
				t.Fatal(err)
			}
			if version, header := recordPayloadVersionOnDiskForTest(t, store, handle.dataKey, saved); version != recordPayloadSchemaVersion || header.SchemaVersion != 1 {
				t.Fatal(version, header)
			}
			loaded, err := handle.Load()
			if err != nil || loaded.Interrupted != nil || !reflect.DeepEqual(loaded.Record.FileReceipts, []FileReceipt{receipt}) {
				t.Fatal(loaded, err)
			}
			bad := receipt
			bad.Effect.Source.Version = ""
			if validateFileReceipt(bad) == nil {
				t.Fatal("missing source version accepted")
			}
			bad = receipt
			bad.Outcome = NoticeOutcomeUnknown
			bad.StableCode = FilePublicationUnknownCode
			bad.InvalidateObserved = true
			bad.Effect.Target.Version = "sha256:" + strings.Repeat("c", 64)
			if validateFileReceipt(bad) == nil {
				t.Fatal("unknown hash fabricated")
			}
		})
	}
}
func TestCopyPayloadFrozenV4DirtyV3AndLegacyVersions(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "legacy", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
	if err != nil {
		t.Fatal(err)
	}
	mkdir := fileeffects.New("mkdir", "", "a/b", "directory")
	mkdir.Directories = fileeffects.DirectoryChain{Anchor: ".", Count: 2, Created: 1}
	record.FileReceipts = []FileReceipt{{ToolCallID: "mkdir", Effect: mkdir, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown}}
	writeRecordPayloadForTest(t, store, handle.dataKey, record, 4, nil)
	marker.SchemaVersion = 3
	marker.MayHaveSideEffect = true
	marker.File = &FileWriteAhead{ToolCallID: "mkdir", Effect: mkdir, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
	plain, err := encodeStrict(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeDirtyPayloadForTest(t, store, handle.dataKey, record, plain)
	before := readSessionArtifactForTest(t, store, dirtyName(record.StorageID))
	loaded, err := handle.Load()
	if err != nil || !recordsEqual(record, loaded.Record) || loaded.Interrupted == nil || loaded.Interrupted.SchemaVersion != dirtySchemaVersion {
		t.Fatal(loaded, err)
	}
	if !bytes.Equal(before, readSessionArtifactForTest(t, store, dirtyName(record.StorageID))) {
		t.Fatal("load rewrote legacy dirty")
	}
	for _, field := range []string{`"new_field":null,`, `"overwrite":false,`} {
		bad := bytes.Replace(plain, []byte(`"effect":{`), []byte(`"effect":{`+field), 1)
		if _, err := decodeDirtyPayload(bad, 1<<20); !errors.Is(err, ErrCorrupt) {
			t.Fatal("old nested field accepted", err)
		}
		writeRecordPayloadForTest(t, store, handle.dataKey, record, 4, func(b []byte) []byte { return bytes.Replace(b, []byte(`"effect":{`), []byte(`"effect":{`+field), 1) })
		if _, _, _, _, err := store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
			t.Fatal("old record field accepted", err)
		}
	}
	record.FileReceipts = []FileReceipt{{ToolCallID: "copy", Effect: copyEffectForTest(), InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown}}
	for _, version := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			writeRecordPayloadForTest(t, store, handle.dataKey, record, version, nil)
			if _, _, _, _, err := store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
				t.Fatal("copy smuggled into legacy record", version, err)
			}
		})
	}
	marker.File.Effect = copyEffectForTest()
	plain, err = encodeStrict(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeDirtyPayload(plain, 1<<20); !errors.Is(err, ErrCorrupt) {
		t.Fatal("copy smuggled into dirty3", err)
	}
	// Authenticated future shapes are unsupported, not corruption/cleanup input.
	marker.SchemaVersion = dirtySchemaVersion + 1
	plain, _ = encodeStrict(marker)
	plain = bytes.Replace(plain, []byte(`"file":{`), []byte(`"file":{"future":true,`), 1)
	writeDirtyPayloadForTest(t, store, handle.dataKey, record, plain)
	if _, _, err = store.readDirty(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatal("future dirty classified corrupt", err)
	}
	writeRecordPayloadForTest(t, store, handle.dataKey, record, recordPayloadSchemaVersion+1, func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"effect":{`), []byte(`"effect":{"future":true,`), 1)
	})
	if _, _, _, _, err = store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatal("future record classified corrupt", err)
	}
}
