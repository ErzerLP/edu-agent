package agentsession

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const archiveTargetForTest = ".edu-agent-archive/20260721T120000.000000000Z-0123456789abcdef/notes/topic.md"

func archiveReceiptForTest(kind, outcome string) fileReceiptV3 {
	code := FilePublicationUnknownCode
	if outcome == NoticeOutcomeCompleted {
		code = FilePublicationCompletedCode
	}
	return fileReceiptV3{
		ToolCallID: "archive-call", Operation: "archive", Path: "notes/topic.md", ArchivePath: archiveTargetForTest,
		Kind: kind, InvalidateObserved: true, StableCode: code, Outcome: outcome,
	}
}

func archiveWriteAheadForTest(value fileReceiptV3) FileWriteAhead {
	return upcastFileWriteAhead(fileWriteAheadV2{
		ToolCallID: value.ToolCallID, Operation: value.Operation, Path: value.Path, ArchivePath: value.ArchivePath,
		Kind: value.Kind, ContentHash: value.ContentHash, InvalidateObserved: value.InvalidateObserved,
		StableCode: value.StableCode, PublicationOutcome: value.Outcome,
	})
}

func TestArchiveFileEffectsRoundTrip(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		for _, outcome := range []string{NoticeOutcomeCompleted, NoticeOutcomeUnknown} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
				defer store.Close()
				handle, record, err := store.Create(t.Context(), CreateInput{Title: "archive", Checkpoint: []byte(`{"v":1}`)})
				if err != nil {
					t.Fatal(err)
				}
				defer handle.Close()
				marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
				if err != nil {
					t.Fatal(err)
				}
				receipt := archiveReceiptForTest(kind, outcome)
				ahead := archiveWriteAheadForTest(receipt)
				marker.MayHaveSideEffect, marker.File = true, &ahead
				updated, err := handle.UpdateDirty(t.Context(), marker)
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := handle.Load()
				if err != nil || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, updated) {
					t.Fatalf("dirty round trip=%+v err=%v", loaded, err)
				}
				plain, header := dirtyPayloadOnDiskForTest(t, store, handle.dataKey, record)
				version, err := probeRecordPayloadSchema(plain, store.limits.DirtyMarkerBytes)
				if err != nil || version != dirtySchemaVersion || header.SchemaVersion != 1 || bytes.Contains(plain, []byte("content_hash")) {
					t.Fatalf("dirty version=%d header=%d payload=%s err=%v", version, header.SchemaVersion, plain, err)
				}
				candidate := loaded.Record
				candidate.FileReceipts = []FileReceipt{upcastFileReceipt(receipt)}
				candidate.LastConsumedDirtyID = updated.DirtyID
				saved, err := handle.Save(t.Context(), record.RecordRevision, candidate)
				if err != nil {
					t.Fatal(err)
				}
				if version, header := recordPayloadVersionOnDiskForTest(t, store, handle.dataKey, saved); version != recordPayloadSchemaVersion || header.SchemaVersion != 1 {
					t.Fatalf("record payload=%d container=%d", version, header.SchemaVersion)
				}
				if err := handle.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, final, err := store.OpenSession(t.Context(), record.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				if final.Interrupted != nil || !reflect.DeepEqual(final.Record.FileReceipts, []FileReceipt{upcastFileReceipt(receipt)}) {
					t.Fatalf("receipt round trip=%+v", final)
				}
			})
		}
	}
}

func TestArchiveFileEffectsRejectInvalidReferences(t *testing.T) {
	for _, outcome := range []string{NoticeOutcomeCompleted, NoticeOutcomeUnknown} {
		for name, mutate := range map[string]func(*fileReceiptV3){
			"missing destination":   func(v *fileReceiptV3) { v.ArchivePath = "" },
			"absolute destination":  func(v *fileReceiptV3) { v.ArchivePath = "/private/notes/topic.md" },
			"drive destination":     func(v *fileReceiptV3) { v.ArchivePath = "C:/notes/topic.md" },
			"backslash destination": func(v *fileReceiptV3) { v.ArchivePath = `.edu-agent-archive\stamp-id\notes\topic.md` },
			"escaping destination":  func(v *fileReceiptV3) { v.ArchivePath = ".edu-agent-archive/stamp-id/../../notes/topic.md" },
			"wrong root":            func(v *fileReceiptV3) { v.ArchivePath = "archive/stamp-id/notes/topic.md" },
			"root only":             func(v *fileReceiptV3) { v.ArchivePath = ".edu-agent-archive" },
			"missing container":     func(v *fileReceiptV3) { v.ArchivePath = ".edu-agent-archive/notes/topic.md" },
			"missing random ID":     func(v *fileReceiptV3) { v.ArchivePath = ".edu-agent-archive/20260721T120000Z-/notes/topic.md" },
			"extra prefix":          func(v *fileReceiptV3) { v.ArchivePath = ".edu-agent-archive/stamp-id/extra/notes/topic.md" },
			"forged source suffix":  func(v *fileReceiptV3) { v.ArchivePath = ".edu-agent-archive/stamp-id/other.md" },
			"destination control":   func(v *fileReceiptV3) { v.ArchivePath = ".edu-agent-archive/stamp\t-id/notes/topic.md" },
			"source control": func(v *fileReceiptV3) {
				v.Path = "notes/to\npic.md"
				v.ArchivePath = ".edu-agent-archive/stamp-id/" + v.Path
			},
			"source root": func(v *fileReceiptV3) { v.Path = "."; v.ArchivePath = ".edu-agent-archive/stamp-id/." },
			"source escape": func(v *fileReceiptV3) {
				v.Path = "../topic.md"
				v.ArchivePath = ".edu-agent-archive/stamp-id/../topic.md"
			},
			"source already archived": func(v *fileReceiptV3) {
				v.Path = ".edu-agent-archive/old-id/topic.md"
				v.ArchivePath = ".edu-agent-archive/stamp-id/" + v.Path
			},
			"source archive alias": func(v *fileReceiptV3) {
				v.Path = ".EDU-AGENT-ARCHIVE"
				v.ArchivePath = ".edu-agent-archive/stamp-id/" + v.Path
			},
			"source trailing dot alias": func(v *fileReceiptV3) {
				v.Path = ".edu-agent-archive./topic.md"
				v.ArchivePath = ".edu-agent-archive/stamp-id/" + v.Path
			},
			"source alternate stream": func(v *fileReceiptV3) {
				v.Path = "notes/topic.md:stream"
				v.ArchivePath = ".edu-agent-archive/stamp-id/" + v.Path
			},
			"destination depth": func(v *fileReceiptV3) {
				v.Path = strings.Repeat("d/", 62) + "topic.md"
				v.ArchivePath = ".edu-agent-archive/stamp-id/" + v.Path
			},
			"destination bytes": func(v *fileReceiptV3) {
				v.Path = strings.Repeat("x", 4090)
				v.ArchivePath = ".edu-agent-archive/stamp-id/" + v.Path
			},
			"fake source hash":         func(v *fileReceiptV3) { v.ContentHash = "sha256:" + strings.Repeat("a", 64) },
			"special entry":            func(v *fileReceiptV3) { v.Kind = "symlink" },
			"missing invalidation":     func(v *fileReceiptV3) { v.InvalidateObserved = false },
			"wrong stable code":        func(v *fileReceiptV3) { v.StableCode = "archive_succeeded" },
			"invalid outcome":          func(v *fileReceiptV3) { v.Outcome = "rejected" },
			"write with archive field": func(v *fileReceiptV3) { v.Operation = "write_replace" },
			"ordinary directory write": func(v *fileReceiptV3) { v.Operation = "write_replace"; v.Kind = "directory"; v.ArchivePath = "" },
		} {
			t.Run(outcome+"/"+name, func(t *testing.T) {
				value := archiveReceiptForTest("file", outcome)
				mutate(&value)
				if err := validateLegacyFileReceipt(value); !errors.Is(err, ErrInvalid) {
					t.Fatalf("receipt error=%v", err)
				}
				if err := validateLegacyFileWriteAhead(fileWriteAheadV2{ToolCallID: value.ToolCallID, Operation: value.Operation, Path: value.Path, ArchivePath: value.ArchivePath, Kind: value.Kind, ContentHash: value.ContentHash, InvalidateObserved: value.InvalidateObserved, StableCode: value.StableCode, PublicationOutcome: value.Outcome}); !errors.Is(err, ErrInvalid) {
					t.Fatalf("write-ahead error=%v", err)
				}
			})
		}
	}
	// Archive's completed invalidation must not weaken write/edit's contract.
	legacy := fileReceiptV3{ToolCallID: "write", Operation: "edit", Path: "topic.md", Kind: "file", ContentHash: "sha256:" + strings.Repeat("b", 64), StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted}
	if err := validateFileReceipt(upcastFileReceipt(legacy)); err != nil {
		t.Fatalf("legacy completed receipt: %v", err)
	}
	if err := validateFileWriteAhead(archiveWriteAheadForTest(legacy)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy completed write-ahead error=%v", err)
	}
	legacy.InvalidateObserved = true
	if err := validateFileReceipt(upcastFileReceipt(legacy)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy completed invalidation error=%v", err)
	}
	legacy.ContentHash, legacy.StableCode, legacy.Outcome = "", FilePublicationUnknownCode, NoticeOutcomeUnknown
	if err := validateFileReceipt(upcastFileReceipt(legacy)); err != nil {
		t.Fatalf("legacy unknown receipt: %v", err)
	}
	if err := validateFileWriteAhead(archiveWriteAheadForTest(legacy)); err != nil {
		t.Fatalf("legacy unknown write-ahead: %v", err)
	}
}

func TestArchivePayloadLegacyMigration(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("record-v%d-dirty-v1", version), func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer store.Close()
			handle, record, err := store.Create(t.Context(), CreateInput{Title: "legacy", Checkpoint: []byte(`{"v":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			record.FileReceipts = []FileReceipt{
				upcastFileReceipt(fileReceiptV3{ToolCallID: "write-completed", Operation: "write_replace", Path: "done.md", Kind: "file", ContentHash: "sha256:" + strings.Repeat("c", 64), StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted}),
				upcastFileReceipt(fileReceiptV3{ToolCallID: "edit-unknown", Operation: "edit", Path: "notes.md", Kind: "file", InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown}),
			}
			record.FirstUserSummary, record.RecentUserSummary = "first", "recent"
			record.AutoTitleTurns, record.CommittedUserTurns = 3, 5
			record.LastTitleAt = record.CreatedAt
			record.QuarantinedCheckpoint = json.RawMessage(`{"old":true}`)
			marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 9, "agent-turn", false)
			if err != nil {
				t.Fatal(err)
			}
			legacyFile := fileWriteAheadV1{ToolCallID: "interrupted-write", Operation: "write_replace", Path: "pending.md", Kind: "file", InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
			legacy := dirtyPayloadV1{SchemaVersion: 1, DirtyID: marker.DirtyID, SessionID: marker.SessionID, StorageID: marker.StorageID, BaseRevision: marker.BaseRevision, TurnSequence: marker.TurnSequence, OperationClass: marker.OperationClass, MayHaveSideEffect: true, StartedAt: marker.StartedAt, File: &legacyFile}
			oldDirty, err := encodeStrict(legacy)
			if err != nil {
				t.Fatal(err)
			}
			writeDirtyPayloadForTest(t, store, handle.dataKey, record, oldDirty)
			writeRecordPayloadForTest(t, store, handle.dataKey, record, version, nil)
			before := readSessionArtifactForTest(t, store, dirtyName(record.StorageID))
			loaded, err := handle.Load()
			if err != nil || !recordsEqual(loaded.Record, record) || loaded.Interrupted == nil {
				t.Fatalf("legacy load=%+v err=%v", loaded, err)
			}
			if loaded.Interrupted.SchemaVersion != dirtySchemaVersion || loaded.Interrupted.DirtyID != legacy.DirtyID || loaded.Interrupted.File == nil || loaded.Interrupted.File.Effect.Target.Path != legacyFile.Path || loaded.Interrupted.File.Effect.Source.Path != "" || !loaded.Interrupted.MayHaveSideEffect {
				t.Fatalf("migrated dirty=%+v", loaded.Interrupted)
			}
			if !bytes.Equal(before, readSessionArtifactForTest(t, store, dirtyName(record.StorageID))) {
				t.Fatal("loading a legacy dirty marker rewrote its side-effect evidence")
			}
			if diskVersion, _ := recordPayloadVersionOnDiskForTest(t, store, handle.dataKey, record); diskVersion != recordPayloadSchemaVersion {
				t.Fatalf("record migration not published: %d", diskVersion)
			}
			receipt := archiveReceiptForTest("directory", NoticeOutcomeUnknown)
			ahead := archiveWriteAheadForTest(receipt)
			candidateDirty := *loaded.Interrupted
			candidateDirty.File = &ahead
			if _, err := handle.UpdateDirty(t.Context(), candidateDirty); !errors.Is(err, ErrCheckpointConflict) {
				t.Fatalf("migration allowed an earlier WAL to disappear: %v", err)
			}
			candidateDirty.FileJournal = []FileJournalEntry{{WriteAhead: *loaded.Interrupted.File}}
			if _, err := handle.UpdateDirty(t.Context(), candidateDirty); err != nil {
				t.Fatalf("update migrated dirty: %v", err)
			}
			candidate := loaded.Record
			old := loaded.Interrupted.File
			candidate.FileReceipts = append(candidate.FileReceipts, FileReceipt{ToolCallID: old.ToolCallID, Effect: old.Effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown}, upcastFileReceipt(receipt))
			candidate.LastConsumedDirtyID = candidateDirty.DirtyID
			if _, err := handle.Save(t.Context(), record.RecordRevision, candidate); err != nil {
				t.Fatalf("save migrated session with archive: %v", err)
			}
			final, err := handle.Load()
			if err != nil || final.Interrupted != nil || !reflect.DeepEqual(final.Record.FileReceipts, candidate.FileReceipts) {
				t.Fatalf("saved migrated session=%+v err=%v", final, err)
			}
		})
	}
}

func TestArchivePayloadLegacyRejectsNewFields(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "frozen", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	record.FileReceipts = []FileReceipt{upcastFileReceipt(fileReceiptV3{ToolCallID: "old-write", Operation: "write_replace", Path: "notes/topic.md", Kind: "file", InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown})}
	for _, version := range []int{1, 2} {
		writeRecordPayloadForTest(t, store, handle.dataKey, record, version, func(plain []byte) []byte {
			return append(plain[:len(plain)-1], []byte(`,"SCHEMA_VERSION":3}`)...)
		})
		if _, _, _, _, err := store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("record v%d allowed schema alias to bypass migration: %v", version, err)
		}
		for _, target := range []string{"", archiveTargetForTest} {
			writeRecordPayloadForTest(t, store, handle.dataKey, record, version, func(plain []byte) []byte {
				return bytes.Replace(plain, []byte(`"kind":"file"`), []byte(fmt.Sprintf(`"kind":"file","archive_path":%q`, target)), 1)
			})
			if _, _, _, _, err := store.readRecord(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("record v%d accepted new nested field %q: %v", version, target, err)
			}
		}
	}
	legacy := dirtyPayloadV1{
		SchemaVersion: 1, DirtyID: "00000000-0000-4000-8000-000000000001", SessionID: record.SessionID, StorageID: record.StorageID,
		BaseRevision: record.RecordRevision, TurnSequence: 1, OperationClass: "agent-turn", MayHaveSideEffect: true, StartedAt: record.CreatedAt,
		File: &fileWriteAheadV1{ToolCallID: "old-write", Operation: "write_replace", Path: "notes/topic.md", Kind: "file", InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown},
	}
	legacyPlain, err := encodeStrict(legacy)
	if err != nil {
		t.Fatal(err)
	}
	writeDirtyPayloadForTest(t, store, handle.dataKey, record, legacyPlain)
	if _, _, err := store.readDirty(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); err != nil {
		t.Fatalf("unmodified legacy dirty fixture: %v", err)
	}
	for _, malformed := range [][]byte{
		append(append([]byte(nil), legacyPlain[:len(legacyPlain)-1]...), []byte(`,"SCHEMA_VERSION":2}`)...),
		append(append([]byte(nil), legacyPlain[:len(legacyPlain)-1]...), []byte(`,"schema_version":2}`)...),
	} {
		writeDirtyPayloadForTest(t, store, handle.dataKey, record, malformed)
		if _, _, err := store.readDirty(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("dirty v1 allowed ambiguous schema to bypass migration: %v", err)
		}
	}
	for _, target := range []string{"", archiveTargetForTest} {
		plain := bytes.Replace(legacyPlain, []byte(`"kind":"file"`), []byte(fmt.Sprintf(`"kind":"file","archive_path":%q`, target)), 1)
		writeDirtyPayloadForTest(t, store, handle.dataKey, record, plain)
		if _, _, err := store.readDirty(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("dirty v1 accepted new nested field %q: %v", target, err)
		}
	}
}

func TestArchivePayloadLegacyDirtyPreservesNonFileMarkers(t *testing.T) {
	base := dirtyPayloadV1{
		SchemaVersion: 1, DirtyID: "00000000-0000-4000-8000-000000000001", SessionID: "00000000-0000-4000-8000-000000000002",
		StorageID: strings.Repeat("a", 32), BaseRevision: 1, TurnSequence: 2, OperationClass: "agent-turn", StartedAt: time.Unix(1, 0).UTC(),
	}
	preference := PreferenceWriteAhead{
		ToolCallID: "preference", CreateOperationID: "00000000-0000-4000-8000-000000000003", AdmitOperationID: "00000000-0000-4000-8000-000000000004", RejectOperationID: "00000000-0000-4000-8000-000000000005",
		Payload: PreferencePayload{Content: "short answers", Reason: "requested", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: time.Unix(100, 0).UTC()}, Stage: PreferenceStageCreate,
	}
	for _, withPreference := range []bool{false, true} {
		value := base
		if withPreference {
			value.Preference, value.MayHaveSideEffect = &preference, true
		}
		plain, err := encodeStrict(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeDirtyPayload(plain, DefaultLimits().DirtyMarkerBytes)
		if err != nil || validateDirtyMarker(got) != nil || got.SchemaVersion != dirtySchemaVersion || got.MayHaveSideEffect != withPreference || !reflect.DeepEqual(got.Preference, value.Preference) || got.File != nil {
			t.Fatalf("preference=%t migrated=%+v err=%v", withPreference, got, err)
		}
	}
}

func TestArchivePayloadFutureVersionsFailClosed(t *testing.T) {
	for _, kind := range []payloadKind{kindRecord, kindDirty} {
		for _, scenario := range []string{"payload with new fields", "container schema", "container version", "tampered header"} {
			t.Run(fmt.Sprintf("kind-%d/%s", kind, scenario), func(t *testing.T) {
				store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
				defer store.Close()
				handle, record, err := store.Create(t.Context(), CreateInput{Title: "future", Checkpoint: []byte(`{"v":1}`)})
				if err != nil {
					t.Fatal(err)
				}
				defer handle.Close()
				marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
				if err != nil {
					t.Fatal(err)
				}
				name, schema, revision := recordName(record.StorageID), uint16(recordContainerSchemaVersion), record.RecordRevision
				var payload any = record
				if kind == kindDirty {
					name, schema, revision = dirtyName(record.StorageID), dirtyContainerSchemaVersion, 42
					payload = marker
				}
				if scenario == "payload with new fields" {
					if kind == kindRecord {
						record.SchemaVersion = recordPayloadSchemaVersion + 1
						payload = record
					} else {
						future := marker
						future.SchemaVersion = dirtySchemaVersion + 1
						payload = future
					}
				}
				plain, err := encodeStrict(payload)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "payload with new fields" {
					plain = append(plain[:len(plain)-1], []byte(`,"future_recovery_contract":{"unknown":true}}`)...)
				}
				header := containerHeader{SchemaVersion: schema, Kind: kind, Profile: store.profile, Generation: record.PrivacyGeneration, Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), Revision: revision}
				container := uint16(containerVersion)
				if scenario == "container schema" {
					schema++
				}
				if scenario == "container version" {
					container++
				}
				raw := sealContainerVersionForTest(t, handle.dataKey, header, container, schema, plain)
				wantErr := ErrVersionUnsupported
				if scenario == "tampered header" {
					binary.BigEndian.PutUint16(raw[10:12], schema+1)
					wantErr = ErrCorrupt
				}
				if err := os.WriteFile(filepath.Join(store.rootPath, name), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := handle.Load(); !errors.Is(err, wantErr) {
					t.Fatalf("load error=%v want=%v", err, wantErr)
				}
				candidate := record
				candidate.LastConsumedDirtyID = marker.DirtyID
				if _, err := handle.Save(t.Context(), record.RecordRevision, candidate); !errors.Is(err, wantErr) {
					t.Fatalf("save error=%v want=%v", err, wantErr)
				}
				if kind == kindDirty {
					if _, err := handle.UpdateDirty(t.Context(), marker); !errors.Is(err, wantErr) {
						t.Fatalf("update dirty error=%v want=%v", err, wantErr)
					}
				}
				if err := handle.Close(); err != nil {
					t.Fatal(err)
				}
				if wantErr == ErrVersionUnsupported {
					target := DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision}
					if err := store.Delete(t.Context(), target); !errors.Is(err, ErrVersionUnsupported) {
						t.Fatalf("delete classified future evidence as deletable: %v", err)
					}
					if _, err := os.Stat(filepath.Join(store.rootPath, keyName(record.StorageID))); err != nil {
						t.Fatalf("future session key was removed: %v", err)
					}
				}
				if !bytes.Equal(raw, readSessionArtifactForTest(t, store, name)) {
					t.Fatal("unsupported or unauthenticated artifact was overwritten")
				}
			})
		}
	}
}

func writeDirtyPayloadForTest(t *testing.T, store *Store, dataKey []byte, record SessionRecord, plain []byte) {
	t.Helper()
	raw, err := sealContainer(dataKey, containerHeader{SchemaVersion: dirtyContainerSchemaVersion, Kind: kindDirty, Profile: store.profile, Generation: record.PrivacyGeneration, Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), Revision: 42}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath, dirtyName(record.StorageID)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirtyPayloadOnDiskForTest(t *testing.T, store *Store, dataKey []byte, record SessionRecord) ([]byte, containerHeader) {
	t.Helper()
	raw := readSessionArtifactForTest(t, store, dirtyName(record.StorageID))
	plain, header, err := openContainer(dataKey, raw, containerExpectation{SchemaVersion: dirtyContainerSchemaVersion, Kind: kindDirty, Profile: store.profile, Generation: record.PrivacyGeneration, Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), MaxPayload: store.limits.DirtyMarkerBytes})
	if err != nil {
		t.Fatal(err)
	}
	return plain, header
}

func readSessionArtifactForTest(t *testing.T, store *Store, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store.rootPath, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
