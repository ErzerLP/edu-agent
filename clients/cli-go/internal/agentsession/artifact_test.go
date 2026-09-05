package agentsession

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func artifactTestSession(t *testing.T, store *Store) (*Handle, SessionRecord) {
	t.Helper()
	h, record, err := store.Create(t.Context(), CreateInput{Title: "artifact test", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, record
}

func artifactTestRead(t *testing.T, h *Handle, name string, want []byte) {
	t.Helper()
	got, err := h.ReadArtifact(t.Context(), name)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("artifact mismatch: bytes=%d want=%d error=%v", len(got), len(want), err)
	}
}

// All fixture ciphertext publication uses the same confined, private API.
func artifactTestPublish(t *testing.T, store *Store, name string, data []byte) {
	t.Helper()
	old, err := store.root.ReadSnapshot(name, 1<<20, true)
	if errors.Is(err, securefile.ErrNotFound) {
		err = store.publishCreate(t.Context(), name, data)
	} else if err == nil {
		err = store.publishReplace(t.Context(), name, data, old.Data)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestArtifactRoundTripNamesAndIndependentLifetime(t *testing.T) {
	root, backend := t.TempDir(), &memorySecretBackend{}
	store := openTestStore(t, root, backend, Limits{})
	defer store.Close()
	h, record := artifactTestSession(t, store)
	other, _ := artifactTestSession(t, store)
	marker := []byte("sensitive-output-marker\x00\xff\x1b[31m\r\n")
	for _, name := range []string{"Task_Z-0002", "Task_Z-0001", "metadata", strings.Repeat("A", 160)} {
		if err := h.WriteArtifact(t.Context(), name, marker); err != nil {
			t.Fatal(err)
		}
		artifactTestRead(t, h, name, marker)
		snapshot, err := store.root.ReadSnapshot(artifactName(h.storageID, name), maxArtifactCiphertextBytes, true)
		if err != nil || bytes.Contains(snapshot.Data, marker) || snapshot.Mode.Perm() != 0o600 {
			t.Fatalf("ciphertext privacy: mode=%v error=%v", snapshot.Mode, err)
		}
	}
	if err := other.WriteArtifact(t.Context(), "Task_Z-0003", marker); err != nil {
		t.Fatal(err)
	}
	if got, err := h.ListArtifacts(t.Context(), "Task_Z-"); err != nil || !reflect.DeepEqual(got, []string{"Task_Z-0001", "Task_Z-0002"}) {
		t.Fatalf("prefix list=%v error=%v", got, err)
	}
	if got, err := h.ListArtifacts(t.Context(), ""); err != nil || len(got) != 4 {
		t.Fatalf("all list=%v error=%v", got, err)
	}
	if _, err := h.ReadArtifact(t.Context(), "Task_Z-0003"); err != ErrNotFound {
		t.Fatalf("cross-session missing error=%v", err)
	}
	if err := h.WriteArtifact(nil, "empty", nil); err != nil {
		t.Fatal(err)
	}
	artifactTestRead(t, h, "empty", nil)
	maximum := bytes.Repeat([]byte{0x93}, MaxArtifactBytes)
	if err := h.WriteArtifact(t.Context(), "maximum", maximum); err != nil {
		t.Fatal(err)
	}
	artifactTestRead(t, h, "maximum", maximum)
	if err := h.WriteArtifact(t.Context(), "maximum", make([]byte, MaxArtifactBytes+1)); err != ErrStoreFull {
		t.Fatalf("oversized error=%v", err)
	}
	artifactTestRead(t, h, "maximum", maximum)

	for _, name := range []string{"", ".", "..", "a.b", "a/b", `a\b`, " a", "a\n", "中文", strings.Repeat("a", 161)} {
		if err := h.WriteArtifact(t.Context(), name, marker); err != ErrInvalid {
			t.Fatalf("invalid name write=%v", err)
		}
		if _, err := h.ReadArtifact(t.Context(), name); err != ErrInvalid {
			t.Fatalf("invalid name read=%v", err)
		}
		if name != "" {
			if _, err := h.ListArtifacts(t.Context(), name); err != ErrInvalid {
				t.Fatalf("invalid prefix=%v", err)
			}
		}
	}
	artifactTestPublish(t, store, artifactName(h.storageID, "invalid.dot"), []byte("malformed fixture"))
	if got, err := h.ListArtifacts(t.Context(), "invalid"); err != nil || len(got) != 0 {
		t.Fatalf("invalid filename exposed: %v error=%v", got, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadArtifact(t.Context(), "metadata"); err != ErrNotFound {
		t.Fatalf("closed read=%v", err)
	}
	if err := h.WriteArtifact(t.Context(), "metadata", marker); err != ErrNotFound {
		t.Fatalf("closed write=%v", err)
	}
	if _, err := h.ListArtifacts(t.Context(), ""); err != ErrNotFound {
		t.Fatalf("closed list=%v", err)
	}
	_ = other.Close()
	_ = store.Close()
	store = openTestStore(t, root, backend, Limits{})
	defer store.Close()
	reopened, loaded, err := store.OpenSession(t.Context(), record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	artifactTestRead(t, reopened, "metadata", marker)
	if bytes.Contains(loaded.Record.Checkpoint, marker) || bytes.Contains(loaded.Record.Transcript, marker) {
		t.Fatal("artifact entered core record")
	}
}

func TestArtifactSurvivesWriterProcessExit(t *testing.T) {
	const env = "EDU_AGENT_ARTIFACT_PROCESS_ROOT"
	marker := []byte("process-private-output\x00\xff")
	if root := os.Getenv(env); root != "" {
		store := openTestStore(t, filepath.Join(root, "sessions"), fileSecretBackend{path: filepath.Join(root, "native-key")}, Limits{})
		h, _ := artifactTestSession(t, store)
		if err := h.WriteArtifact(t.Context(), "chunk-0001", marker); err != nil {
			t.Fatal(err)
		}
		_ = h.Close()
		_ = store.Close()
		return
	}
	root := t.TempDir()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestArtifactSurvivesWriterProcessExit$", "-test.count=1")
	command.Env = append(os.Environ(), env+"="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("writer process: %v %s", err, output)
	}
	store := openTestStore(t, filepath.Join(root, "sessions"), fileSecretBackend{path: filepath.Join(root, "native-key")}, Limits{})
	defer store.Close()
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 1 {
		t.Fatalf("reopened sessions=%d error=%v", len(listed), err)
	}
	h, _, err := store.OpenSession(t.Context(), listed[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	artifactTestRead(t, h, "chunk-0001", marker)
}

func TestArtifactAuthenticatedIdentityVersionsAndTamper(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	other, _ := artifactTestSession(t, store)
	name := artifactName(h.storageID, "source")
	if err := h.WriteArtifact(t.Context(), "source", []byte("original")); err != nil {
		t.Fatal(err)
	}
	original, err := store.root.ReadSnapshot(name, maxArtifactCiphertextBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	artifactTestPublish(t, store, artifactName(h.storageID, "renamed"), original.Data)
	artifactTestPublish(t, store, artifactName(other.storageID, "source"), original.Data)
	core, err := store.root.ReadSnapshot(recordName(h.storageID), 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	artifactTestPublish(t, store, artifactName(h.storageID, "record-swap"), core.Data)
	for _, target := range []struct {
		h *Handle
		n string
	}{{h, "renamed"}, {other, "source"}, {h, "record-swap"}} {
		if _, err := target.h.ReadArtifact(t.Context(), target.n); err != ErrCorrupt {
			t.Fatalf("swap read=%v", err)
		}
		if err := target.h.WriteArtifact(t.Context(), target.n, []byte("replacement")); err != ErrCorrupt {
			t.Fatalf("swap overwrite=%v", err)
		}
	}
	header, err := unmarshalHeader(original.Data[:containerHeaderSize])
	if err != nil {
		t.Fatal(err)
	}
	key := deriveArtifactKey(h.dataKey, name)
	defer zero(key)
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"future-schema", sealContainerVersionForTest(t, key, header, containerVersion, artifactContainerSchemaVersion+1, []byte("future")), ErrVersionUnsupported},
		{"future-container", sealContainerVersionForTest(t, key, header, containerVersion+1, artifactContainerSchemaVersion, []byte("future")), ErrVersionUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifactTestPublish(t, store, name, tc.data)
			if data, err := h.ReadArtifact(t.Context(), "source"); err != tc.want || data != nil {
				t.Fatalf("future read returned data=%d error=%v", len(data), err)
			}
			if err := h.WriteArtifact(t.Context(), "source", []byte("overwrite")); err != tc.want {
				t.Fatalf("future overwrite=%v", err)
			}
			current, err := store.root.ReadSnapshot(name, maxArtifactCiphertextBytes, true)
			if err != nil || !bytes.Equal(current.Data, tc.data) {
				t.Fatal("future evidence overwritten")
			}
		})
	}
	for _, offset := range []int{8, 10, 12, 16, 48, 56, 72, 88, 100, len(original.Data) - 1} {
		t.Run(fmt.Sprint("tamper-", offset), func(t *testing.T) {
			tampered := bytes.Clone(original.Data)
			tampered[offset] ^= 1
			artifactTestPublish(t, store, name, tampered)
			if data, err := h.ReadArtifact(t.Context(), "source"); err != ErrCorrupt || data != nil {
				t.Fatalf("tamper exposed data=%d error=%v", len(data), err)
			}
		})
	}
	for _, mutate := range []func(*containerHeader){
		func(v *containerHeader) { v.Kind = kindRecord },
		func(v *containerHeader) { v.Profile[0] ^= 1 },
		func(v *containerHeader) { v.Session[0] ^= 1 },
		func(v *containerHeader) { v.Storage[0] ^= 1 },
	} {
		changed := header
		mutate(&changed)
		encoded := sealContainerVersionForTest(t, key, changed, containerVersion, artifactContainerSchemaVersion, []byte("identity"))
		artifactTestPublish(t, store, name, encoded)
		if _, err := h.ReadArtifact(t.Context(), "source"); err != ErrCorrupt {
			t.Fatalf("authenticated identity mismatch=%v", err)
		}
	}
	// Artifact ciphertext is not a valid core record, even with the same DEK.
	artifactTestPublish(t, store, recordName(h.storageID), original.Data)
	if _, err := h.Load(); err != ErrCorrupt {
		t.Fatalf("artifact substituted for core record=%v", err)
	}
}

func TestArtifactQuotasAreIndependentAndNonEvicting(t *testing.T) {
	const overhead = containerHeaderSize + 16
	for _, tc := range []struct {
		name   string
		limits Limits
		other  bool
	}{
		{"session", Limits{ArtifactSessionCiphertextBytes: overhead + 4}, false},
		{"profile", Limits{ArtifactProfileCiphertextBytes: overhead + 4}, true},
		{"files", Limits{ArtifactFiles: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, tc.limits)
			defer store.Close()
			h, record := artifactTestSession(t, store)
			other, _ := artifactTestSession(t, store)
			if err := h.WriteArtifact(t.Context(), "one", []byte("1234")); err != nil {
				t.Fatal(err)
			}
			target := h
			if tc.other {
				target = other
			}
			if err := target.WriteArtifact(t.Context(), "two", nil); err != ErrStoreFull {
				t.Fatalf("quota create=%v", err)
			}
			if _, err := target.ReadArtifact(t.Context(), "two"); err != ErrNotFound {
				t.Fatalf("failed write published: %v", err)
			}
			if tc.name != "files" {
				if err := h.WriteArtifact(t.Context(), "one", []byte("12345")); err != ErrStoreFull {
					t.Fatalf("quota replace=%v", err)
				}
			}
			artifactTestRead(t, h, "one", []byte("1234"))
			if err := h.WriteArtifact(t.Context(), "one", []byte("12")); err != nil {
				t.Fatalf("replacement charged twice: %v", err)
			}
			artifactTestRead(t, h, "one", []byte("12"))
			if _, err := h.Save(t.Context(), record.RecordRevision, record); err != nil {
				t.Fatalf("artifact quota blocked core save: %v", err)
			}
			if _, err := h.Load(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.List(t.Context()); err != nil {
				t.Fatalf("artifact quota blocked core list: %v", err)
			}
		})
	}
	t.Run("core budget does not count artifacts", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{SessionCiphertextBytes: 16 << 10, ProfileCiphertextBytes: 32 << 10})
		defer store.Close()
		h, record := artifactTestSession(t, store)
		if err := h.WriteArtifact(t.Context(), "large", bytes.Repeat([]byte("s"), MaxArtifactBytes)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Save(t.Context(), record.RecordRevision, record); err != nil {
			t.Fatalf("output consumed core quota: %v", err)
		}
		artifactTestSession(t, store)
	})
}

func TestArtifactDirectoryBudgetsAndRecovery(t *testing.T) {
	limits := Limits{DirectoryEntries: 16, ArtifactFiles: 24, Sessions: 2}
	root, backend := t.TempDir(), &memorySecretBackend{}
	store := openTestStore(t, root, backend, limits)
	defer store.Close()
	h, record := artifactTestSession(t, store)
	other, _ := artifactTestSession(t, store)
	for i := range limits.ArtifactFiles {
		if err := h.WriteArtifact(t.Context(), fmt.Sprintf("part-%02d", i), []byte("chunk")); err != nil {
			t.Fatalf("write part %d: %v", i, err)
		}
	}
	if got, err := h.ListArtifacts(t.Context(), "part-"); err != nil || len(got) != limits.ArtifactFiles {
		t.Fatalf("full list=%d error=%v", len(got), err)
	}
	if err := store.root.Delete(indexName); err != nil {
		t.Fatal(err)
	}
	if listed, err := store.List(t.Context()); err != nil || len(listed) != 2 {
		t.Fatalf("index recovery with output=%d error=%v", len(listed), err)
	}
	if _, _, err := store.Create(t.Context(), CreateInput{Title: "over core session count", Checkpoint: []byte(`{"v":1}`)}); err != ErrStoreFull {
		t.Fatalf("core count loosened=%v", err)
	}
	_ = h.Close()
	_ = other.Close()
	_ = store.Close()
	store = openTestStore(t, root, backend, limits)
	defer store.Close()
	h, _, err := store.OpenSession(t.Context(), record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	artifactTestRead(t, h, "part-23", []byte("chunk"))
	if err := store.Clear(t.Context()); err != nil {
		t.Fatalf("clear with expanded artifact entries=%v", err)
	}
	entries, err := store.readRootEntries()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isArtifactDataName(entry.Name) {
			t.Fatal("clear left artifact")
		}
	}
}

func TestArtifactDirectoryOverflowNeverReturnsPartialList(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	for i := range 10 {
		if err := h.WriteArtifact(t.Context(), fmt.Sprintf("part-%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	store.limits.DirectoryEntries, store.limits.ArtifactFiles = 8, 1
	if got, err := h.ListArtifacts(t.Context(), ""); got != nil || err != ErrStoreFull {
		t.Fatalf("partial list=%v error=%v", got, err)
	}
	if err := store.Clear(t.Context()); err != ErrDeleteFailed {
		t.Fatalf("incomplete clear=%v", err)
	}
	if _, err := h.ReadArtifact(t.Context(), "part-0"); err != ErrPrivacyInvalidated {
		t.Fatalf("failed cleanup did not fence old handle=%v", err)
	}
	// The old core-only directory budget remains effective independently.
	store.limits.DirectoryEntries, store.limits.ArtifactFiles = 1, 100
	if _, err := store.readRootEntries(); err != ErrStoreFull {
		t.Fatalf("artifact allowance enlarged core budget=%v", err)
	}
	store.limits.DirectoryEntries, store.limits.ArtifactFiles = int(^uint(0)>>1), 1
	if got := store.rootEntryLimit(); got != int(^uint(0)>>1)-1 {
		t.Fatalf("scan limit overflow=%d", got)
	}
}

func TestArtifactDefaultsAndNormalizedLimits(t *testing.T) {
	defaults := DefaultLimits()
	if defaults.ArtifactSessionCiphertextBytes != 256<<20 || defaults.ArtifactProfileCiphertextBytes != 1<<30 || defaults.ArtifactFiles != 8192 || MaxArtifactBytes != 512<<10 {
		t.Fatal("artifact contract defaults changed")
	}
	got := normalizedLimits(Limits{ArtifactSessionCiphertextBytes: -1, ArtifactProfileCiphertextBytes: -1, ArtifactFiles: -1})
	if got != defaults {
		t.Fatal("artifact defaults not normalized")
	}
	got = normalizedLimits(Limits{ArtifactSessionCiphertextBytes: 1, ArtifactProfileCiphertextBytes: 2, ArtifactFiles: 3})
	if got.ArtifactSessionCiphertextBytes != 1 || got.ArtifactProfileCiphertextBytes != 2 || got.ArtifactFiles != 3 {
		t.Fatal("injected artifact limits ignored")
	}
}

func TestArtifactPublicationUnknownIsVerifiedWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name           string
		commit, tamper bool
		want           error
	}{
		{"committed", true, false, nil},
		{"unchanged", false, false, ErrOutcomeUnknown},
		{"corrupt-observation", true, true, ErrOutcomeUnknown},
	} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replace=%t", tc.name, replace), func(t *testing.T) {
				store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
				defer store.Close()
				h, _ := artifactTestSession(t, store)
				if replace {
					if err := h.WriteArtifact(t.Context(), "chunk", []byte("old")); err != nil {
						t.Fatal(err)
					}
				}
				publish, calls := store.publish, 0
				var attempted []byte
				store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
					calls++
					if options.Private != true || options.Permission != 0o600 || replace && options.ExpectedHash == "" {
						t.Fatal("publication dropped private/CAS preconditions")
					}
					attempted = bytes.Clone(data)
					if tc.tamper {
						attempted[len(attempted)-1] ^= 1
					}
					if tc.commit {
						if _, err := publish(ctx, name, attempted, options); err != nil {
							return securefile.PublishResult{}, err
						}
					}
					return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
				}
				if err := h.WriteArtifact(t.Context(), "chunk", []byte("new")); err != tc.want || calls != 1 {
					t.Fatalf("publication error=%v calls=%d", err, calls)
				}
				if tc.commit {
					current, err := store.root.ReadSnapshot(artifactName(h.storageID, "chunk"), maxArtifactCiphertextBytes, true)
					if err != nil || !bytes.Equal(current.Data, attempted) {
						t.Fatal("publication evidence removed")
					}
					if !tc.tamper {
						artifactTestRead(t, h, "chunk", []byte("new"))
					}
				} else if replace {
					artifactTestRead(t, h, "chunk", []byte("old"))
				} else if _, err := h.ReadArtifact(t.Context(), "chunk"); err != ErrNotFound {
					t.Fatalf("unpublished artifact=%v", err)
				}
			})
		}
	}
}

func TestArtifactDeleteKeyFirstIsolationAndCleanupFailures(t *testing.T) {
	for _, failure := range []string{"none", "key", "artifact"} {
		t.Run(failure, func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer store.Close()
			h, record := artifactTestSession(t, store)
			other, _ := artifactTestSession(t, store)
			for _, name := range []string{"one", "two"} {
				if err := h.WriteArtifact(t.Context(), name, []byte("target")); err != nil {
					t.Fatal(err)
				}
			}
			if err := other.WriteArtifact(t.Context(), "one", []byte("other")); err != nil {
				t.Fatal(err)
			}
			_ = h.Close()
			original, calls := store.deleteFile, []string{}
			store.deleteFile = func(name string) error {
				calls = append(calls, name)
				if failure == "key" && name == keyName(record.StorageID) || failure == "artifact" && name == artifactName(record.StorageID, "one") {
					return errors.New("private injected cleanup detail")
				}
				return original(name)
			}
			want := error(nil)
			if failure != "none" {
				want = ErrDeleteFailed
			}
			if err := store.Delete(t.Context(), DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision}); err != want {
				t.Fatalf("delete=%v", err)
			}
			if len(calls) == 0 || calls[0] != keyName(record.StorageID) || failure == "key" && len(calls) != 1 {
				t.Fatalf("delete order=%v", calls)
			}
			for _, name := range []string{"one", "two"} {
				_, err := store.root.Stat(t.Context(), artifactName(record.StorageID, name))
				preserved := failure == "key" || failure == "artifact" && name == "one"
				if preserved && err != nil || !preserved && !errors.Is(err, securefile.ErrNotFound) {
					t.Fatalf("artifact cleanup preservation=%t error=%v", preserved, err)
				}
			}
			artifactTestRead(t, other, "one", []byte("other"))
			listed, err := store.List(t.Context())
			wantCount := 1
			if failure == "key" {
				wantCount = 2
			}
			if err != nil || len(listed) != wantCount {
				t.Fatalf("deleted output session re-registered: %d error=%v", len(listed), err)
			}
		})
	}
}

func TestArtifactClearFencesAllHandlesAndKeepsHonestFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("cleanup-failure=", fail), func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer store.Close()
			h, _ := artifactTestSession(t, store)
			if err := h.WriteArtifact(t.Context(), "one", []byte("old")); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Reopen(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if fail {
				original := reopened.deleteFile
				reopened.deleteFile = func(name string) error {
					if isArtifactDataName(name) {
						return errors.New("cleanup failed")
					}
					return original(name)
				}
			}
			want := error(nil)
			if fail {
				want = ErrDeleteFailed
			}
			if err := reopened.Clear(t.Context()); err != want {
				t.Fatalf("clear=%v", err)
			}
			if _, err := h.ReadArtifact(t.Context(), "one"); err != ErrPrivacyInvalidated {
				t.Fatalf("stale handle read=%v", err)
			}
			if err := h.WriteArtifact(t.Context(), "two", nil); err != ErrPrivacyInvalidated {
				t.Fatalf("stale handle write=%v", err)
			}
			if _, err := h.ListArtifacts(t.Context(), ""); err != ErrPrivacyInvalidated {
				t.Fatalf("stale handle list=%v", err)
			}
			_, err = store.root.Stat(t.Context(), artifactName(h.storageID, "one"))
			if fail && err != nil || !fail && !errors.Is(err, securefile.ErrNotFound) {
				t.Fatalf("cleanup result mismatched reported outcome: %v", err)
			}
		})
	}
}

func TestArtifactOnlyResiduePreventsSilentKeyReplacement(t *testing.T) {
	root, backend := t.TempDir(), &memorySecretBackend{}
	store := openTestStore(t, root, backend, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	if err := h.WriteArtifact(t.Context(), "residue", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	entries, err := store.readRootEntries()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isEncryptedDataName(entry.Name) && !isArtifactDataName(entry.Name) {
			if err := store.root.Delete(entry.Name); err != nil {
				t.Fatal(err)
			}
		}
	}
	if present, err := store.hasEncryptedData(); !present || err != nil {
		t.Fatalf("artifact not recognized as encrypted data: %t %v", present, err)
	}
	_ = backend.Delete(store.locator)
	_ = store.Close()
	if opened, err := Open(t.Context(), Options{Root: root, ProfileFingerprint: strings.Repeat("a", 64), Secrets: backend}); !errors.Is(err, ErrKeyUnavailable) || opened != nil {
		t.Fatalf("missing key silently replaced: %v", err)
	}
}

func TestArtifactRejectsLinksPermissionsAndSanitizesErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions/link fixture; no Windows expansion in C2")
	}
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	if err := h.WriteArtifact(t.Context(), "private", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(store.rootPath, artifactName(h.storageID, "private"))
	if err := os.Chmod(privatePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadArtifact(t.Context(), "private"); err != ErrCorrupt {
		t.Fatalf("broad permissions read=%v", err)
	}
	if err := h.WriteArtifact(t.Context(), "private", []byte("overwrite")); err != ErrCorrupt {
		t.Fatalf("broad permissions write=%v", err)
	}
	if err := os.Chmod(privatePath, 0o600); err != nil {
		t.Fatal(err)
	}
	link := artifactName(h.storageID, "linked")
	if err := os.Symlink(privatePath, filepath.Join(store.rootPath, link)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadArtifact(t.Context(), "linked"); err != ErrCorrupt {
		t.Fatalf("symlink read=%v", err)
	}
	if err := h.WriteArtifact(t.Context(), "linked", []byte("overwrite")); err != ErrCorrupt {
		t.Fatalf("symlink write=%v", err)
	}
	if got, err := h.ListArtifacts(t.Context(), ""); got != nil || err != ErrCorrupt {
		t.Fatalf("link silently listed/skipped: %v %v", got, err)
	}
	artifactTestRead(t, h, "private", []byte("secret"))
	if err := os.Remove(filepath.Join(store.rootPath, link)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadArtifact(t.Context(), "private"); err != ErrCorrupt {
		t.Fatalf("broad root permissions read=%v", err)
	}
	if err := h.WriteArtifact(t.Context(), "new", nil); err != ErrCorrupt {
		t.Fatalf("broad root permissions write=%v", err)
	}
	if err := os.Chmod(store.rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	store.publish = func(context.Context, string, []byte, securefile.PublishOptions) (securefile.PublishResult, error) {
		return securefile.PublishResult{Outcome: securefile.PublishUnchanged}, fmt.Errorf("%s secret-key/raw-output", store.rootPath)
	}
	if err := h.WriteArtifact(t.Context(), "private", []byte("attempt")); err != ErrCorrupt {
		t.Fatalf("raw error exposed=%v", err)
	}
	artifactTestRead(t, h, "private", []byte("secret"))
}

func TestArtifactFutureCoreSchemaStillPreventsSessionDeletion(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	h, record := artifactTestSession(t, store)
	if err := h.WriteArtifact(t.Context(), "retained", []byte("private-output")); err != nil {
		t.Fatal(err)
	}
	future := cloneRecord(record)
	future.SchemaVersion = recordPayloadSchemaVersion + 1
	plain, err := encodeStrict(future)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := parseUUID(h.sessionID)
	storage, _ := parseStorageID(h.storageID)
	encoded, err := sealContainer(h.dataKey, containerHeader{
		SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: store.profile,
		Generation: h.generation, Session: session, Storage: storage, Revision: record.RecordRevision,
	}, plain)
	if err != nil {
		t.Fatal(err)
	}
	artifactTestPublish(t, store, recordName(h.storageID), encoded)
	_ = h.Close()
	if err := store.Delete(t.Context(), DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision}); err != ErrVersionUnsupported {
		t.Fatalf("future core delete=%v", err)
	}
	for _, name := range []string{keyName(record.StorageID), artifactName(record.StorageID, "retained")} {
		if _, err := store.root.Stat(t.Context(), name); err != nil {
			t.Fatal("future core deletion removed envelope/output")
		}
	}
}

func TestArtifactUnknownRewriteOfEqualBodyNeedsExactPublication(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	if err := h.WriteArtifact(t.Context(), "same", []byte("body")); err != nil {
		t.Fatal(err)
	}
	store.publish = func(context.Context, string, []byte, securefile.PublishOptions) (securefile.PublishResult, error) {
		return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
	}
	if err := h.WriteArtifact(t.Context(), "same", []byte("body")); err != ErrOutcomeUnknown {
		t.Fatalf("stale identical body confirmed new publication: %v", err)
	}
	artifactTestRead(t, h, "same", []byte("body"))
}

func TestArtifactClearLinkResidueIsNotSilent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink fixture")
	}
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	link := filepath.Join(store.rootPath, artifactName(h.storageID, "linked"))
	if err := os.Symlink("missing", link); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(t.Context()); err != ErrDeleteFailed {
		t.Fatalf("clear ignored non-regular artifact residue=%v", err)
	}
	if _, err := h.ListArtifacts(t.Context(), ""); err != ErrPrivacyInvalidated {
		t.Fatalf("clear failure did not advance generation=%v", err)
	}
}

func TestArtifactNonceRevisionIsRandomPerWrite(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	seen := make(map[[12]byte]bool)
	for i := range 16 {
		name := fmt.Sprintf("blob-%d", i%2)
		if err := h.WriteArtifact(t.Context(), name, []byte("same")); err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.root.ReadSnapshot(artifactName(h.storageID, name), maxArtifactCiphertextBytes, true)
		if err != nil {
			t.Fatal(err)
		}
		header, err := unmarshalHeader(snapshot.Data[:containerHeaderSize])
		if err != nil || header.Revision == 0 || binary.BigEndian.Uint64(header.Nonce[4:]) != header.Revision || seen[header.Nonce] {
			t.Fatalf("nonce/revision reuse error=%v", err)
		}
		seen[header.Nonce] = true
	}
}
