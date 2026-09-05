package agentsession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func journalAhead(id string) FileWriteAhead {
	e := fileeffects.New("mkdir", "", id, "directory")
	e.Directories = fileeffects.DirectoryChain{Anchor: ".", Count: 1}
	return FileWriteAhead{ToolCallID: id, Effect: e, InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown}
}

func journalStore(t *testing.T) (*Store, *Handle, SessionRecord, DirtyMarker) {
	t.Helper()
	s := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	t.Cleanup(func() { _ = s.Close() })
	h, r, err := s.Create(t.Context(), CreateInput{Title: "journal", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	m, err := h.MarkDirty(t.Context(), r.RecordRevision, 1, "agent-turn", false)
	if err != nil {
		t.Fatal(err)
	}
	ahead := journalAhead("alpha")
	m.MayHaveSideEffect, m.File = true, &ahead
	m, err = h.UpdateDirty(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	return s, h, r, m
}

func journalSettled(m DirtyMarker) DirtyMarker {
	ahead := *m.File
	e := ahead.Effect
	e.Directories.Created = e.Directories.Count
	r := FileReceipt{ToolCallID: ahead.ToolCallID, Effect: e, InvalidateObserved: true, StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted}
	m.FileJournal = append(append([]FileJournalEntry(nil), m.FileJournal...), FileJournalEntry{WriteAhead: ahead, Result: &r})
	m.File = nil
	return m
}

func TestJournalStrictTransitionsKeepEveryPriorWALAndSettlement(t *testing.T) {
	s, h, record, m := journalStore(t)
	settled, err := h.UpdateDirty(t.Context(), journalSettled(m))
	if err != nil {
		t.Fatal(err)
	}
	before := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
	for _, test := range []struct {
		name      string
		candidate DirtyMarker
	}{
		{"stale-wal", m},
		{"drop-all", func() DirtyMarker {
			v := settled
			v.FileJournal = nil
			v.File = new(FileWriteAhead)
			*v.File = journalAhead("beta")
			return v
		}()},
		{"change-plan", func() DirtyMarker {
			v := journalSettled(m)
			v.FileJournal[0].WriteAhead.Effect.Target.Path = "other"
			return v
		}()},
		{"change-result", func() DirtyMarker {
			v := journalSettled(m)
			v.FileJournal[0].Result.Outcome = NoticeOutcomeUnknown
			v.FileJournal[0].Result.StableCode = FilePublicationUnknownCode
			return v
		}()},
		{"duplicate-id", func() DirtyMarker { v := journalSettled(m); v.File = m.File; return v }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := h.UpdateDirty(t.Context(), test.candidate); err == nil {
				t.Fatal("conflicting update accepted")
			}
			if !bytes.Equal(before, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
				t.Fatal("failure changed evidence bytes")
			}
		})
	}
	beta := journalAhead("beta")
	candidate := settled
	candidate.File = &beta
	updated, err := h.UpdateDirty(t.Context(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := h.Load()
	if err != nil || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, updated) {
		t.Fatal(loaded, err)
	}
	if len(updated.FileJournal) != 1 || updated.FileJournal[0].Result.Outcome != "completed" || updated.File.ToolCallID != "beta" {
		t.Fatal(updated)
	}
}

func TestJournalQuotaAndPublicationFailuresPreserveCiphertext(t *testing.T) {
	for _, mode := range []string{"bytes", "count", "io-unchanged", "io-committed"} {
		t.Run(mode, func(t *testing.T) {
			s, h, record, marker := journalStore(t)
			before := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
			candidate := journalSettled(marker)
			want := ErrCheckpointSaveFailed
			switch mode {
			case "bytes":
				plain, _ := encodeStrict(marker)
				s.limits.DirtyMarkerBytes = int64(len(plain))
				want = ErrStoreFull
			case "count":
				s.limits.ReceiptCount = 1
				beta := journalAhead("beta")
				candidate.File = &beta
				want = ErrStoreFull
			default:
				original := s.publish
				s.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
					if mode == "io-committed" {
						if _, err := original(ctx, name, data, options); err != nil {
							return securefile.PublishResult{}, err
						}
					}
					return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
				}
			}
			got, err := h.UpdateDirty(t.Context(), candidate)
			if mode == "io-committed" {
				if err != nil || !reflect.DeepEqual(got, candidate) {
					t.Fatal("publication not reconciled", got, err)
				}
			} else {
				if !errors.Is(err, want) {
					t.Fatal("unexpected rejection", err, want)
				}
				if !bytes.Equal(before, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
					t.Fatal("failure changed ciphertext")
				}
				loaded, err := h.Load()
				if err != nil || loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, marker) {
					t.Fatal("old evidence invalid", loaded, err)
				}
			}
		})
	}
}

func TestJournalDirtyV5MigrationFreezesOldShapeAndPreservesBytes(t *testing.T) {
	s, h, record, marker := journalStore(t)
	marker.SchemaVersion = 5
	plain, err := encodeStrict(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeDirtyPayloadForTest(t, s, h.dataKey, record, plain)
	before := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
	loaded, err := h.Load()
	if err != nil || loaded.Interrupted == nil || loaded.Interrupted.SchemaVersion != 6 || loaded.Interrupted.File == nil || loaded.Interrupted.File.ToolCallID != "alpha" || len(loaded.Interrupted.FileJournal) != 0 {
		t.Fatal(loaded, err)
	}
	if !bytes.Equal(before, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
		t.Fatal("load rewrote legacy evidence")
	}
	for version := 1; version <= 5; version++ {
		for _, value := range []string{"null", "[]", "{}", `""`} {
			t.Run(fmt.Sprintf("v%d/%s", version, value), func(t *testing.T) {
				legacy := marker
				legacy.SchemaVersion, legacy.File, legacy.MayHaveSideEffect = version, nil, false
				b, _ := encodeStrict(legacy)
				b = append(b[:len(b)-1], []byte(`,"file_journal":`+value+`}`)...)
				if _, err := decodeDirtyPayload(b, 1<<20); !errors.Is(err, ErrCorrupt) {
					t.Fatal("journal field accepted in frozen payload", string(b), err)
				}
			})
		}
	}
	badVersionAlias := append(append([]byte(nil), plain[:len(plain)-1]...), []byte(`,"SCHEMA_VERSION":6}`)...)
	if _, err := decodeDirtyPayload(badVersionAlias, 1<<20); !errors.Is(err, ErrCorrupt) {
		t.Fatal("dirty v5 schema alias bypassed frozen decoder", err)
	}
	for _, point := range []string{`"file":{`, `"effect":{`, `"source":{`, `"target":{`, `"directories":{`} {
		for _, value := range []string{"null", "[]", "{}", `""`} {
			bad := bytes.Replace(plain, []byte(point), []byte(point+`"result":`+value+`,`), 1)
			if _, err := decodeDirtyPayload(bad, 1<<20); !errors.Is(err, ErrCorrupt) {
				t.Fatal("new nested field accepted in dirty v5", point, value, err)
			}
		}
	}
	// Updating a migrated marker explicitly writes v6, preserving the real
	// legacy singleton rather than inventing any already-lost earlier fact.
	migrated := *loaded.Interrupted
	beta := journalAhead("beta")
	migrated.FileJournal = []FileJournalEntry{{WriteAhead: *migrated.File}}
	migrated.File = &beta
	updated, err := h.UpdateDirty(t.Context(), migrated)
	if err != nil || len(updated.FileJournal) != 1 {
		t.Fatal(updated, err)
	}
	b, header := dirtyPayloadOnDiskForTest(t, s, h.dataKey, record)
	if !bytes.Contains(b, []byte(`"schema_version":6`)) || header.SchemaVersion != 1 {
		t.Fatal(string(b), header)
	}
}

func TestJournalAuthenticatedFutureCannotBeSavedOrConsumed(t *testing.T) {
	s, h, record, marker := journalStore(t)
	marker.SchemaVersion = dirtySchemaVersion + 1
	plain, _ := encodeStrict(marker)
	plain = bytes.Replace(plain, []byte(`"file":{`), []byte(`"file":{"future":null,`), 1)
	writeDirtyPayloadForTest(t, s, h.dataKey, record, plain)
	before := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
	if _, err := h.Load(); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatal(err)
	}
	marker.SchemaVersion = dirtySchemaVersion
	if _, err := h.UpdateDirty(t.Context(), marker); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatal(err)
	}
	record.LastConsumedDirtyID = marker.DirtyID
	if _, err := h.Save(t.Context(), record.RecordRevision, record); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
		t.Fatal("future dirty evidence changed")
	}
	if DefaultLimits().DirtyMarkerBytes != 16<<10 || DefaultLimits().ReceiptCount != 32 || recordPayloadSchemaVersion != 6 || recordContainerSchemaVersion != 1 || strings.Contains(string(plain), "authorization") {
		t.Fatal("unrelated contract changed")
	}
}
