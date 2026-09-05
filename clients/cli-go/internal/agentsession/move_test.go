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

func moveEffectForTest(kind string) fileeffects.Effect {
	e := fileeffects.New("move", "input/source", "output/target", kind)
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	return e
}
func TestMovePayloadV6DirtyCurrentRoundTrip(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		for _, outcome := range []string{NoticeOutcomeCompleted, NoticeOutcomeUnknown} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
				defer store.Close()
				handle, record, err := store.Create(t.Context(), CreateInput{Title: "move", Checkpoint: []byte(`{"v":1}`)})
				if err != nil {
					t.Fatal(err)
				}
				defer handle.Close()
				marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
				if err != nil {
					t.Fatal(err)
				}
				effect := moveEffectForTest(kind)
				marker.MayHaveSideEffect = true
				marker.File = &FileWriteAhead{ToolCallID: "move", Effect: effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
				marker, err = handle.UpdateDirty(t.Context(), marker)
				if err != nil {
					t.Fatal(err)
				}
				plain, header := dirtyPayloadOnDiskForTest(t, store, handle.dataKey, record)
				version, err := probeRecordPayloadSchema(plain, store.limits.DirtyMarkerBytes)
				if err != nil || version != dirtySchemaVersion || header.SchemaVersion != 1 {
					t.Fatal(version, header, err)
				}
				if bytes.Contains(plain, []byte("content_hash")) || bytes.Contains(plain, []byte("identity")) {
					t.Fatal("unsafe WAL", string(plain))
				}
				receipt := FileReceipt{ToolCallID: "move", Effect: effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: outcome}
				if outcome == NoticeOutcomeCompleted {
					receipt.StableCode = FilePublicationCompletedCode
				}
				record.FileReceipts = []FileReceipt{receipt}
				record.LastConsumedDirtyID = marker.DirtyID
				saved, err := handle.Save(t.Context(), record.RecordRevision, record)
				if err != nil {
					t.Fatal(err)
				}
				if v, h := recordPayloadVersionOnDiskForTest(t, store, handle.dataKey, saved); v != 6 || h.SchemaVersion != 1 {
					t.Fatal(v, h)
				}
				loaded, err := handle.Load()
				if err != nil || loaded.Interrupted != nil || !reflect.DeepEqual(loaded.Record.FileReceipts, []FileReceipt{receipt}) {
					t.Fatal(loaded, err)
				}
				for _, mutate := range []func(*FileReceipt){func(r *FileReceipt) { r.Effect.Source.Version = "" }, func(r *FileReceipt) { r.Effect.Source.Version = "sha256:" + strings.Repeat("b", 64) }, func(r *FileReceipt) { r.Effect.Target.Version = effect.Source.Version }, func(r *FileReceipt) { r.Effect.Target.Version = "sha256:" + strings.Repeat("b", 64) }, func(r *FileReceipt) { r.InvalidateObserved = false }, func(r *FileReceipt) { r.Effect.Target.Kind = "other" }} {
					bad := receipt
					mutate(&bad)
					if validateFileReceipt(bad) == nil {
						t.Fatal("invalid move receipt accepted", bad)
					}
				}
			})
		}
	}
}
func TestMoveFrozenRecordV5DirtyV4MigrationAndStrictOldVersions(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "legacy-copy", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
	if err != nil {
		t.Fatal(err)
	}
	effect := copyEffectForTest()
	record.FileReceipts = []FileReceipt{{ToolCallID: "copy", Effect: effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown}}
	writeRecordPayloadForTest(t, store, handle.dataKey, record, 5, nil)
	marker.SchemaVersion = 4
	marker.MayHaveSideEffect = true
	marker.File = &FileWriteAhead{ToolCallID: "copy", Effect: effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
	plain, err := encodeStrict(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeDirtyPayloadForTest(t, store, handle.dataKey, record, plain)
	before := readSessionArtifactForTest(t, store, dirtyName(record.StorageID))
	loaded, err := handle.Load()
	if err != nil || !recordsEqual(record, loaded.Record) || loaded.Interrupted == nil || loaded.Interrupted.SchemaVersion != dirtySchemaVersion || loaded.Interrupted.File.Effect != effect {
		t.Fatal(loaded, err)
	}
	if !bytes.Equal(before, readSessionArtifactForTest(t, store, dirtyName(record.StorageID))) {
		t.Fatal("legacy dirty rewritten")
	}
	for _, point := range []string{`"effect":{`, `"source":{`, `"target":{`, `"directories":{`} {
		for _, value := range []string{"null", "{}", "\"\""} {
			inserted := point + `"new_field":` + value + `,`
			bad := bytes.Replace(plain, []byte(point), []byte(inserted), 1)
			if _, err := decodeDirtyPayload(bad, 1<<20); !errors.Is(err, ErrCorrupt) {
				t.Fatal("legacy dirty accepted new field", point, value, err)
			}
			writeRecordPayloadForTest(t, store, handle.dataKey, record, 5, func(b []byte) []byte { return bytes.Replace(b, []byte(point), []byte(inserted), 1) })
			if _, _, _, _, err := store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
				t.Fatal("legacy record accepted new field", point, value, err)
			}
		}
	}
	record.FileReceipts[0].Effect = moveEffectForTest("file")
	for version := 1; version <= 5; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			writeRecordPayloadForTest(t, store, handle.dataKey, record, version, nil)
			if _, _, _, _, err := store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
				t.Fatal("move smuggled into legacy record", version, err)
			}
		})
	}
	for version := 1; version <= 4; version++ {
		marker.SchemaVersion = version
		marker.File.Effect = moveEffectForTest("file")
		var old any = marker
		if version <= 2 {
			old = dirtyPayloadV2{dirtyPayloadV1: dirtyPayloadV1{SchemaVersion: version, DirtyID: marker.DirtyID, SessionID: marker.SessionID, StorageID: marker.StorageID, BaseRevision: marker.BaseRevision, TurnSequence: marker.TurnSequence, OperationClass: marker.OperationClass, MayHaveSideEffect: true, StartedAt: marker.StartedAt}, File: &fileWriteAheadV2{ToolCallID: "move", Operation: "move", Path: "source", Kind: "file", InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}}
		}
		b, err := encodeStrict(old)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeDirtyPayload(b, 1<<20); !errors.Is(err, ErrCorrupt) {
			t.Fatal("move smuggled into legacy dirty", version, err)
		}
	}
	marker.SchemaVersion = dirtySchemaVersion + 1
	plain, _ = encodeStrict(marker)
	plain = bytes.Replace(plain, []byte(`"file":{`), []byte(`"file":{"future":null,`), 1)
	writeDirtyPayloadForTest(t, store, handle.dataKey, record, plain)
	futureBefore := readSessionArtifactForTest(t, store, dirtyName(record.StorageID))
	if _, _, err = store.readDirty(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatal("future dirty classified corrupt", err)
	}
	if !bytes.Equal(futureBefore, readSessionArtifactForTest(t, store, dirtyName(record.StorageID))) {
		t.Fatal("future dirty cleaned")
	}
	writeRecordPayloadForTest(t, store, handle.dataKey, record, 7, func(b []byte) []byte {
		return bytes.Replace(b, []byte(`"effect":{`), []byte(`"effect":{"future":null,`), 1)
	})
	if _, _, _, _, err = store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatal("future record classified corrupt", err)
	}
}
