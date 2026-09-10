package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

func directoryCopyRootForTest() fileeffects.Effect {
	e := fileeffects.New("copy", "input/tree", "output/tree", "directory")
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	return e
}

func directoryCopyReceiptForTest(outcome string) FileReceipt {
	r := FileReceipt{ToolCallID: "tree-copy", Effect: directoryCopyRootForTest(), InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: outcome}
	if outcome == NoticeOutcomeCompleted {
		r.StableCode = FilePublicationCompletedCode
	}
	return r
}

func TestDirectoryCopyCurrentPayloadRoundTrip(t *testing.T) {
	for _, outcome := range []string{NoticeOutcomeCompleted, NoticeOutcomeUnknown} {
		t.Run(outcome, func(t *testing.T) {
			s := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer s.Close()
			h, record, err := s.Create(t.Context(), CreateInput{Title: "tree copy", Checkpoint: []byte(`{"v":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			marker, err := h.MarkDirty(t.Context(), record.RecordRevision, 1, "agent-turn", false)
			if err != nil {
				t.Fatal(err)
			}
			ahead := FileWriteAhead{ToolCallID: "tree-copy", Effect: directoryCopyRootForTest(), InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
			marker.MayHaveSideEffect, marker.File = true, &ahead
			marker, err = h.UpdateDirty(t.Context(), marker)
			if err != nil {
				t.Fatal(err)
			}
			plain, header := dirtyPayloadOnDiskForTest(t, s, h.dataKey, record)
			version, err := probeRecordPayloadSchema(plain, s.limits.DirtyMarkerBytes)
			if err != nil || version != dirtySchemaVersion || header.SchemaVersion != 1 || bytes.Contains(plain, []byte("sha256:")) {
				t.Fatalf("dirty version=%d header=%+v err=%v payload=%s", version, header, err, plain)
			}
			loaded, err := h.Load()
			if err != nil || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, marker) {
				t.Fatalf("root WAL round trip: %+v %v", loaded, err)
			}
			receipt := directoryCopyReceiptForTest(outcome)
			marker.File = nil
			marker.FileJournal = []FileJournalEntry{{WriteAhead: ahead, Result: &receipt}}
			marker, err = h.UpdateDirty(t.Context(), marker)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err = h.Load()
			if err != nil || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, marker) {
				t.Fatalf("root settlement round trip: %+v %v", loaded, err)
			}
			record.FileReceipts = []FileReceipt{receipt}
			record.LastConsumedDirtyID = marker.DirtyID
			saved, err := h.Save(t.Context(), record.RecordRevision, record)
			if err != nil {
				t.Fatal(err)
			}
			if version, header := recordPayloadVersionOnDiskForTest(t, s, h.dataKey, saved); version != recordPayloadSchemaVersion || header.SchemaVersion != 1 {
				t.Fatalf("record version=%d header=%+v", version, header)
			}
			loaded, err = h.Load()
			if err != nil || loaded.Interrupted != nil || !reflect.DeepEqual(loaded.Record.FileReceipts, []FileReceipt{receipt}) {
				t.Fatalf("root receipt round trip: %+v %v", loaded, err)
			}
		})
	}
}

func TestDirectoryCopyReceiptValidationAndV1Regression(t *testing.T) {
	for _, outcome := range []string{NoticeOutcomeCompleted, NoticeOutcomeUnknown} {
		r := directoryCopyReceiptForTest(outcome)
		if validateFileReceipt(r) != nil {
			t.Fatalf("valid root receipt rejected: %+v", r)
		}
		for _, test := range []struct {
			name string
			edit func(*FileReceipt)
		}{
			{"no-invalidation", func(r *FileReceipt) { r.InvalidateObserved = false }},
			{"wrong-code", func(r *FileReceipt) { r.StableCode = "tree_copy_completed" }},
			{"target-hash", func(r *FileReceipt) { r.Effect.Target.Version = "sha256:" + strings.Repeat("b", 64) }},
			{"v1-directory", func(r *FileReceipt) { r.Effect.SchemaVersion = 1 }},
			{"entry-scope", func(r *FileReceipt) { r.Effect.Scope = "entry" }},
			{"created-chain", func(r *FileReceipt) {
				r.Effect.Directories = fileeffects.DirectoryChain{Anchor: "output", Count: 1, Created: 1}
			}},
			{"unchanged-receipt", func(r *FileReceipt) { r.Outcome = "unchanged" }},
		} {
			t.Run(outcome+"/"+test.name, func(t *testing.T) {
				bad := r
				test.edit(&bad)
				if validateFileReceipt(bad) == nil {
					t.Fatalf("invalid root receipt accepted: %+v", bad)
				}
			})
		}
		ahead := FileWriteAhead{ToolCallID: r.ToolCallID, Effect: r.Effect, InvalidateObserved: r.InvalidateObserved, StableCode: r.StableCode, PublicationOutcome: outcome}
		if err := validateFileWriteAhead(ahead); (err == nil) != (outcome == NoticeOutcomeUnknown) {
			t.Fatalf("WAL must remain unknown: %+v err=%v", ahead, err)
		}
	}
	copy := FileReceipt{ToolCallID: "file-copy", Effect: copyEffectForTest(), StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted}
	if validateFileReceipt(copy) == nil {
		t.Fatal("ordinary completed file copy accepted without SHA256")
	}
	copy.Effect.Target.Version = "sha256:" + strings.Repeat("b", 64)
	if validateFileReceipt(copy) != nil {
		t.Fatal("ordinary completed file copy rejected")
	}
	copy.InvalidateObserved = true
	if validateFileReceipt(copy) == nil {
		t.Fatal("ordinary completed file copy gained subtree invalidation")
	}
	mkdirAhead := journalAhead("parent/child")
	mkdirAhead.Effect.Directories = fileeffects.DirectoryChain{Anchor: ".", Count: 2, Created: 1}
	mkdir := FileReceipt{ToolCallID: mkdirAhead.ToolCallID, Effect: mkdirAhead.Effect, InvalidateObserved: true, StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted}
	if validateFileReceipt(mkdir) == nil {
		t.Fatal("partial mkdir was completed")
	}
	mkdir.Effect.Directories.Created = 2
	if validateFileReceipt(mkdir) != nil {
		t.Fatal("completed mkdir rejected")
	}
}

func TestDirectoryCopyLegacyMigrationPreservesV1Fields(t *testing.T) {
	s, h, record, marker := journalStore(t)
	marker = journalSettled(marker)
	copyAhead := FileWriteAhead{ToolCallID: "copy-file", Effect: copyEffectForTest(), InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
	marker.File = &copyAhead
	marker.LocalEffects = []LocalEffectIntent{
		{ToolCallID: "shell", Operation: "shell"},
		{ToolCallID: "input", Operation: "task_input", TaskID: "task_1"},
		{ToolCallID: "close", Operation: "task_close_input", TaskID: "task_1"},
		{ToolCallID: "stop", Operation: "task_stop", TaskID: "task_1"},
	}
	marker.SchemaVersion = 7
	plain, err := encodeStrict(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeDirtyPayloadForTest(t, s, h.dataKey, record, plain)
	before := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
	got, err := decodeDirtyPayload(plain, s.limits.DirtyMarkerBytes)
	marker.SchemaVersion = dirtySchemaVersion
	if err != nil || !reflect.DeepEqual(got, marker) {
		t.Fatalf("v7 migration lost facts: got=%+v want=%+v err=%v", got, marker, err)
	}
	// Local-effects-only markers must not be rejected by a partial v6 validator.
	localOnly := marker
	localOnly.SchemaVersion, localOnly.File, localOnly.FileJournal = 7, nil, nil
	localPlain, _ := encodeStrict(localOnly)
	localGot, err := decodeDirtyPayload(localPlain, s.limits.DirtyMarkerBytes)
	localOnly.SchemaVersion = dirtySchemaVersion
	if err != nil || !reflect.DeepEqual(localGot, localOnly) {
		t.Fatalf("local-only migration: got=%+v want=%+v err=%v", localGot, localOnly, err)
	}

	record.TitleSource, record.FirstUserSummary, record.RecentUserSummary = "manual", "first", "recent"
	record.AutoTitleTurns, record.TitleRevision, record.LastTitleAt = 3, 4, record.CreatedAt
	record.WorkspaceRoot, record.WorkspaceLabel = t.TempDir(), "workspace"
	record.WorkspacePathHash = "sha256:" + strings.Repeat("c", 64)
	record.WorkspaceRootIdentityHash = "sha256:" + strings.Repeat("d", 64)
	record.WorkspaceID = record.WorkspaceRootIdentityHash
	record.ProviderName, record.ProviderEndpoint, record.ProviderModel = "provider", "https://example.test/api", "model"
	record.PrivacyLearnerGeneration, record.PrivacyMemoryGeneration, record.PrivacyVerified = 2, 3, true
	record.CommittedUserTurns = 2
	record.QuarantinedCheckpoint = []byte(`{"legacy":true}`)
	record.LastConsumedDirtyID = "00000000-0000-4000-8000-000000000001"
	completedCopy := FileReceipt{ToolCallID: copyAhead.ToolCallID, Effect: copyAhead.Effect, StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted}
	completedCopy.Effect.Target.Version = "sha256:" + strings.Repeat("e", 64)
	record.FileReceipts = []FileReceipt{*marker.FileJournal[0].Result, completedCopy}
	writeRecordPayloadForTest(t, s, h.dataKey, record, 6, nil)
	loaded, err := h.Load()
	if err != nil || !recordsEqual(loaded.Record, record) || loaded.Record.SchemaVersion != recordPayloadSchemaVersion || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, marker) {
		t.Fatalf("legacy migration lost data: %+v err=%v", loaded, err)
	}
	if !bytes.Equal(before, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
		t.Fatal("read-only load rewrote old dirty evidence")
	}
}

func TestDirectoryCopyLegacyPayloadRejectsNewEffects(t *testing.T) {
	_, _, record, marker := journalStore(t)
	for version := 1; version <= 6; version++ {
		for _, effectVersion := range []int{1, 2} {
			r := directoryCopyReceiptForTest(NoticeOutcomeUnknown)
			r.Effect.SchemaVersion = effectVersion
			record.SchemaVersion, record.FileReceipts = version, []FileReceipt{r}
			plain, _ := encodeStrict(record)
			if _, _, err := decodeRecordPayload(plain, 1<<20); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("record v%d accepted directory effect v%d: %v", version, effectVersion, err)
			}
		}
	}
	for version := 1; version <= 7; version++ {
		for _, effectVersion := range []int{1, 2} {
			for _, slot := range []string{"file", "journal-wal", "journal-result"} {
				r := directoryCopyReceiptForTest(NoticeOutcomeUnknown)
				r.Effect.SchemaVersion = effectVersion
				candidate := marker
				candidate.SchemaVersion, candidate.File, candidate.FileJournal = version, nil, nil
				ahead := FileWriteAhead{ToolCallID: r.ToolCallID, Effect: r.Effect, InvalidateObserved: true, StableCode: r.StableCode, PublicationOutcome: r.Outcome}
				switch slot {
				case "file":
					candidate.File = &ahead
				case "journal-wal":
					candidate.FileJournal = []FileJournalEntry{{WriteAhead: ahead}}
				case "journal-result":
					legacy := journalAhead(r.ToolCallID)
					candidate.FileJournal = []FileJournalEntry{{WriteAhead: legacy, Result: &r}}
				}
				plain, _ := encodeStrict(candidate)
				if _, err := decodeDirtyPayload(plain, 1<<20); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("dirty v%d/%s accepted directory effect v%d: %v", version, slot, effectVersion, err)
				}
			}
		}
	}
}

func TestDirectoryCopyFrozenDTORejectsFutureFieldsAndEnums(t *testing.T) {
	_, _, record, marker := journalStore(t)
	record.FileReceipts = []FileReceipt{{ToolCallID: marker.File.ToolCallID, Effect: marker.File.Effect, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown}}
	for version := 4; version <= 6; version++ {
		record.SchemaVersion = version
		plain, _ := encodeStrict(record)
		for _, point := range []string{`"file_receipts":[{`, `"effect":{`, `"source":{`, `"target":{`, `"directories":{`} {
			for _, value := range []string{"null", "{}", "[]", `""`} {
				bad := bytes.Replace(plain, []byte(point), []byte(point+`"future":`+value+`,`), 1)
				if _, _, err := decodeRecordPayload(bad, 1<<20); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("record v%d accepted %s future=%s: %v", version, point, value, err)
				}
			}
		}
	}
	for version := 3; version <= 7; version++ {
		candidate := marker
		candidate.SchemaVersion = version
		if version >= 6 {
			candidate = journalSettled(candidate)
		}
		if version == 7 {
			candidate.LocalEffects = []LocalEffectIntent{{ToolCallID: "shell", Operation: "shell"}}
		}
		plain, _ := encodeStrict(candidate)
		points := []string{`"effect":{`, `"source":{`, `"target":{`, `"directories":{`}
		if version >= 6 {
			points = append(points, `"file_journal":[{`, `"write_ahead":{`, `"result":{`)
		} else {
			points = append(points, `"file":{`)
		}
		if version == 7 {
			points = append(points, `"local_effects":[{`)
		}
		for _, point := range points {
			for _, value := range []string{"null", "{}", "[]", `""`} {
				bad := bytes.ReplaceAll(plain, []byte(point), []byte(point+`"future":`+value+`,`))
				if _, err := decodeDirtyPayload(bad, 1<<20); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("dirty v%d accepted %s future=%s: %v", version, point, value, err)
				}
			}
		}
	}
	// A schema-1 copy stays file-only even if the live validator later grows.
	for name, upcast := range map[string]func(fileEffectV4) (fileeffects.Effect, error){"v4": upcastEffectV4, "v5": upcastEffectV5, "v6": upcastEffectV6} {
		for _, operation := range []string{"restore", "tree_copy", "COPY"} {
			e := fileEffectV4{SchemaVersion: 1, Operation: operation, Target: fileEndpointV4{Path: "file", Kind: "file"}, Scope: "entry"}
			if _, err := upcast(e); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s accepted unknown operation %s: %v", name, operation, err)
			}
		}
		for _, version := range []int{0, 2, 3} {
			plain, _ := encodeStrict(copyEffectForTest())
			var e fileEffectV4
			if err := json.Unmarshal(plain, &e); err != nil {
				t.Fatal(err)
			}
			e.SchemaVersion = version
			if _, err := upcast(e); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s accepted file effect version %d: %v", name, version, err)
			}
		}
	}
	marker.SchemaVersion, marker.File, marker.FileJournal = 7, nil, nil
	for _, intent := range []LocalEffectIntent{{ToolCallID: "shell", Operation: "future"}, {ToolCallID: "shell", Operation: "shell", TaskID: "task_1"}, {ToolCallID: "input", Operation: "task_input"}} {
		marker.LocalEffects = []LocalEffectIntent{intent}
		plain, _ := encodeStrict(marker)
		if _, err := decodeDirtyPayload(plain, 1<<20); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("v7 local contract expanded: %+v err=%v", intent, err)
		}
	}
}

func TestDirectoryCopyAuthenticatedFutureVersionsPreserveEvidence(t *testing.T) {
	for _, kind := range []string{"record", "dirty"} {
		t.Run(kind, func(t *testing.T) {
			s, h, record, marker := journalStore(t)
			name := recordName(record.StorageID)
			if kind == "record" {
				writeRecordPayloadForTest(t, s, h.dataKey, record, recordPayloadSchemaVersion+1, func(b []byte) []byte {
					return append(b[:len(b)-1], []byte(`,"future":null}`)...)
				})
			} else {
				marker.SchemaVersion = dirtySchemaVersion + 1
				plain, _ := encodeStrict(marker)
				plain = append(plain[:len(plain)-1], []byte(`,"future":null}`)...)
				writeDirtyPayloadForTest(t, s, h.dataKey, record, plain)
				name = dirtyName(record.StorageID)
			}
			before := readSessionArtifactForTest(t, s, name)
			if _, err := h.Load(); !errors.Is(err, ErrVersionUnsupported) {
				t.Fatalf("authenticated future %s was not unsupported: %v", kind, err)
			}
			if kind == "dirty" {
				marker.SchemaVersion = dirtySchemaVersion
				if _, err := h.UpdateDirty(t.Context(), marker); !errors.Is(err, ErrVersionUnsupported) {
					t.Fatalf("future dirty update: %v", err)
				}
			}
			record.LastConsumedDirtyID = marker.DirtyID
			if _, err := h.Save(t.Context(), record.RecordRevision, record); !errors.Is(err, ErrVersionUnsupported) {
				t.Fatalf("future %s save/consume: %v", kind, err)
			}
			if !bytes.Equal(before, readSessionArtifactForTest(t, s, name)) {
				t.Fatalf("future %s evidence changed", kind)
			}
		})
	}
	if recordPayloadSchemaVersion-recordMigrationMaxSteps != 1 || recordContainerSchemaVersion != 1 || dirtyContainerSchemaVersion != 1 {
		t.Fatal("bounded migration/container contract changed")
	}
	// Every older record version still has a complete bounded migration path.
	_, _, record, _ := journalStore(t)
	for version := 1; version <= recordPayloadSchemaVersion; version++ {
		t.Run(fmt.Sprintf("record-chain-v%d", version), func(t *testing.T) {
			record.SchemaVersion = version
			plain, _ := encodeStrict(record)
			if version < 10 {
				plain = withoutLearningBindingForTest(t, plain)
			}
			got, source, err := decodeRecordPayload(plain, 1<<20)
			if err != nil || source != version || got.SchemaVersion != recordPayloadSchemaVersion {
				t.Fatalf("migration source=%d schema=%d err=%v", source, got.SchemaVersion, err)
			}
		})
	}
}
