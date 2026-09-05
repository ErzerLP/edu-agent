package agentsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type memorySecretBackend struct {
	mu    sync.Mutex
	value []byte
}

func (*memorySecretBackend) Available(keybackend.Locator) error { return nil }
func (b *memorySecretBackend) Load(keybackend.Locator, int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.value) == 0 {
		return nil, keybackend.ErrNotFound
	}
	return append([]byte(nil), b.value...), nil
}
func (b *memorySecretBackend) Store(_ keybackend.Locator, value []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value = append([]byte(nil), value...)
	return nil
}
func (b *memorySecretBackend) Delete(keybackend.Locator) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value = nil
	return nil
}

type fileSecretBackend struct{ path string }

func (fileSecretBackend) Available(keybackend.Locator) error { return nil }
func (b fileSecretBackend) Load(keybackend.Locator, int) ([]byte, error) {
	value, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, keybackend.ErrNotFound
	}
	return value, err
}
func (b fileSecretBackend) Store(_ keybackend.Locator, value []byte) error {
	return os.WriteFile(b.path, value, 0o600)
}
func (b fileSecretBackend) Delete(keybackend.Locator) error {
	err := os.Remove(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type faultSecretBackend struct {
	base               *memorySecretBackend
	storeCalls         int
	failStoreCall      int
	commitBeforeError  bool
	failLoadAfterStore bool
	failNextLoad       bool
}

func (b *faultSecretBackend) Available(locator keybackend.Locator) error {
	return b.base.Available(locator)
}
func (b *faultSecretBackend) Load(locator keybackend.Locator, limit int) ([]byte, error) {
	if b.failNextLoad {
		b.failNextLoad = false
		return nil, keybackend.ErrUnavailable
	}
	return b.base.Load(locator, limit)
}
func (b *faultSecretBackend) Store(locator keybackend.Locator, value []byte) error {
	b.storeCalls++
	if b.storeCalls != b.failStoreCall {
		return b.base.Store(locator, value)
	}
	if b.commitBeforeError {
		if err := b.base.Store(locator, value); err != nil {
			return err
		}
	}
	if b.failLoadAfterStore {
		b.failNextLoad = true
	}
	return errors.New("injected native-secret store failure")
}
func (b *faultSecretBackend) Delete(locator keybackend.Locator) error {
	return b.base.Delete(locator)
}

func TestCreateRejectsUnsafeMetadataText(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	for _, title := range []string{"line one\nline two", "terminal\x1b[31m", "bidi\u202eevil"} {
		if _, _, err := store.Create(t.Context(), CreateInput{Title: title, Checkpoint: []byte(`{"v":1}`)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("title=%q error=%v", title, err)
		}
	}
}

func TestDefaultLimitsMatchSessionDesign(t *testing.T) {
	limits := DefaultLimits()
	if limits.Sessions != 256 || limits.ProfileCiphertextBytes != 1<<30 || limits.SessionPlaintextBytes != 48<<20 || limits.SessionCiphertextBytes != 64<<20 || limits.DirtyMarkerBytes != 16<<10 || limits.TranscriptEntries != 2048 || limits.TranscriptBytes != 4<<20 || limits.TranscriptEntryBytes != 64<<10 || limits.TranscriptEventBytes != 16<<10 ||
		limits.PickerQueryRunes != 128 || limits.PickerResults != 256 || limits.SearchSummaryRunes != 160 || limits.SearchSummaryBytes != 512 ||
		limits.ManualTitleBytes != 256 || limits.ManualTitleRunes != 80 || limits.ManualTitleColumns != 60 || limits.AutoTitleInputBytes != 6000 || limits.AutoTitlePartBytes != 1600 || limits.AutoTitleResponseBytes != 256 || limits.AutoTitleMaxTokens != 96 || limits.AutoTitleTurnInterval != 3 || limits.AutoTitleMinInterval != 10*time.Minute || limits.AutoTitleRequestTimeout != 15*time.Second || limits.AutoTitleSaveTimeout != 5*time.Second || limits.NoticeCount != 32 || limits.ReceiptCount != 32 {
		t.Fatalf("limits=%+v", limits)
	}
}

func TestNormalizedLimitsDefaultNewProductionBoundaries(t *testing.T) {
	defaults := DefaultLimits()
	got := normalizedLimits(Limits{
		PickerQueryRunes: -1, PickerResults: -1, SearchSummaryRunes: -1, SearchSummaryBytes: -1,
		ManualTitleBytes: -1, ManualTitleRunes: -1, ManualTitleColumns: -1,
		AutoTitleInputBytes: -1, AutoTitlePartBytes: -1, AutoTitleResponseBytes: -1, AutoTitleMaxTokens: -1,
		AutoTitleTurnInterval: 0, AutoTitleMinInterval: -time.Second, AutoTitleRequestTimeout: -time.Second, AutoTitleSaveTimeout: -time.Second, NoticeCount: -1, ReceiptCount: -1,
	})
	if got.PickerQueryRunes != defaults.PickerQueryRunes || got.PickerResults != defaults.PickerResults ||
		got.SearchSummaryRunes != defaults.SearchSummaryRunes || got.SearchSummaryBytes != defaults.SearchSummaryBytes ||
		got.ManualTitleBytes != defaults.ManualTitleBytes || got.ManualTitleRunes != defaults.ManualTitleRunes || got.ManualTitleColumns != defaults.ManualTitleColumns ||
		got.AutoTitleInputBytes != defaults.AutoTitleInputBytes || got.AutoTitlePartBytes != defaults.AutoTitlePartBytes || got.AutoTitleResponseBytes != defaults.AutoTitleResponseBytes ||
		got.AutoTitleMaxTokens != defaults.AutoTitleMaxTokens || got.AutoTitleTurnInterval != defaults.AutoTitleTurnInterval || got.AutoTitleMinInterval != defaults.AutoTitleMinInterval || got.AutoTitleRequestTimeout != defaults.AutoTitleRequestTimeout || got.AutoTitleSaveTimeout != defaults.AutoTitleSaveTimeout || got.NoticeCount != defaults.NoticeCount || got.ReceiptCount != defaults.ReceiptCount {
		t.Fatalf("normalized=%+v defaults=%+v", got, defaults)
	}
}

func TestStoreLimitsReturnsConfiguredValueCopy(t *testing.T) {
	configured := Limits{PickerQueryRunes: 3, PickerResults: 2, ReceiptCount: 1}
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, configured)
	defer store.Close()
	got := store.Limits()
	if got.PickerQueryRunes != 3 || got.PickerResults != 2 || got.ReceiptCount != 1 {
		t.Fatalf("limits=%+v", got)
	}
	got.PickerResults = 99
	if store.Limits().PickerResults != 2 {
		t.Fatal("Limits accessor exposed mutable store state")
	}
	if (*Store)(nil).Limits() != DefaultLimits() {
		t.Fatal("nil store did not return default limits")
	}
}

func TestCreateReconcilesPublicationAndNeverDeletesUnconfirmedEnvelope(t *testing.T) {
	t.Run("record committed before unknown", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
		defer store.Close()
		original := store.publish
		store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
			result, err := original(ctx, name, data, options)
			if err == nil && strings.HasPrefix(name, "record-") {
				return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
			}
			return result, err
		}
		handle, _, err := store.Create(t.Context(), CreateInput{Title: "record-unknown", Checkpoint: []byte(`{"v":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		_ = handle.Close()
	})

	t.Run("envelope committed before unknown", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
		defer store.Close()
		original := store.publish
		store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
			result, err := original(ctx, name, data, options)
			if err == nil && strings.HasPrefix(name, "key-") {
				return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
			}
			return result, err
		}
		handle, _, err := store.Create(t.Context(), CreateInput{Title: "key-unknown", Checkpoint: []byte(`{"v":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		_ = handle.Close()
	})

	t.Run("record definitely absent", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
		defer store.Close()
		store.publish = func(context.Context, string, []byte, securefile.PublishOptions) (securefile.PublishResult, error) {
			return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
		}
		if _, _, err := store.Create(t.Context(), CreateInput{Title: "absent", Checkpoint: []byte(`{"v":1}`)}); !errors.Is(err, ErrCheckpointSaveFailed) {
			t.Fatalf("create error=%v", err)
		}
	})

	t.Run("unconfirmed envelope is preserved", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
		defer store.Close()
		original := store.publish
		var envelopeName, recordNameValue string
		store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
			if strings.HasPrefix(name, "record-") {
				recordNameValue = name
			}
			if strings.HasPrefix(name, "key-") {
				envelopeName = name
				if _, err := original(ctx, name, []byte("preexisting-envelope"), options); err != nil {
					return securefile.PublishResult{}, err
				}
				return securefile.PublishResult{Outcome: securefile.PublishUnchanged}, securefile.ErrAlreadyExists
			}
			return original(ctx, name, data, options)
		}
		if _, _, err := store.Create(t.Context(), CreateInput{Title: "collision", Checkpoint: []byte(`{"v":1}`)}); !errors.Is(err, ErrCheckpointConflict) {
			t.Fatalf("create error=%v", err)
		}
		if raw, err := os.ReadFile(filepath.Join(store.rootPath, envelopeName)); err != nil || string(raw) != "preexisting-envelope" {
			t.Fatalf("unconfirmed envelope=%q err=%v", raw, err)
		}
		if _, err := os.Stat(filepath.Join(store.rootPath, recordNameValue)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed create record remained: %v", err)
		}
	})
}

func TestIndexFailureIsDegradedCacheNotCommittedRecordFailure(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	original := store.publish
	store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
		if name == indexName {
			return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
		}
		return original(ctx, name, data, options)
	}
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "degraded", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !store.IndexDegraded() {
		t.Fatal("index failure was not marked degraded")
	}
	candidate := record
	candidate.Checkpoint = []byte(`{"v":2}`)
	if saved, err := handle.Save(t.Context(), 1, candidate); err != nil || saved.RecordRevision != 2 {
		t.Fatalf("save=%+v err=%v", saved, err)
	}
	_ = handle.Close()
	store.publish = original
	summaries, err := store.List(t.Context())
	if err != nil || len(summaries) != 1 || summaries[0].SessionID != record.SessionID {
		t.Fatalf("summaries=%+v err=%v", summaries, err)
	}
	if store.IndexDegraded() {
		t.Fatal("successful index rebuild did not clear degraded state")
	}
}

func TestDeleteUsesEnvelopeFirstAndClassifiesCleanupFailure(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "delete", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	originalDelete := store.deleteFile
	calls := []string{}
	store.deleteFile = func(name string) error {
		calls = append(calls, name)
		if strings.HasPrefix(name, "key-") {
			return errors.New("injected key delete failure")
		}
		return originalDelete(name)
	}
	if err := store.Delete(t.Context(), DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: 1}); !errors.Is(err, ErrDeleteFailed) {
		t.Fatalf("key delete error=%v", err)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "key-") {
		t.Fatalf("delete order=%v", calls)
	}
	if summaries, err := store.List(t.Context()); err != nil || len(summaries) != 1 {
		t.Fatalf("failed key delete removed session: %+v err=%v", summaries, err)
	}

	calls = nil
	store.deleteFile = func(name string) error {
		calls = append(calls, name)
		if strings.HasPrefix(name, "record-") {
			return errors.New("injected record cleanup failure")
		}
		return originalDelete(name)
	}
	if err := store.Delete(t.Context(), DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: 1}); !errors.Is(err, ErrDeleteFailed) {
		t.Fatalf("record cleanup error=%v", err)
	}
	if len(calls) < 2 || !strings.HasPrefix(calls[0], "key-") {
		t.Fatalf("delete order=%v", calls)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, keyName(record.StorageID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("envelope remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, recordName(record.StorageID))); err != nil {
		t.Fatalf("fault-injected residual record missing: %v", err)
	}
	if summaries, err := store.List(t.Context()); err != nil || len(summaries) != 0 {
		t.Fatalf("residual ciphertext re-registered: %+v err=%v", summaries, err)
	}
}

func TestClearReconcilesNativeSecretAndRejectsOldGenerationRegistration(t *testing.T) {
	t.Run("native replacement committed before error", func(t *testing.T) {
		backend := &faultSecretBackend{base: &memorySecretBackend{}, failStoreCall: 2, commitBeforeError: true}
		store := openTestStore(t, t.TempDir(), backend, Limits{})
		defer store.Close()
		handle, _, err := store.Create(t.Context(), CreateInput{Title: "clear", Checkpoint: []byte(`{"v":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		tempName := ".edu-agent-" + strings.Repeat("a", 32)
		if err := os.WriteFile(filepath.Join(store.rootPath, tempName), []byte("temporary ciphertext"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.Clear(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(store.rootPath, tempName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary residue remains: %v", err)
		}
		if _, err := handle.Load(); !errors.Is(err, ErrPrivacyInvalidated) {
			t.Fatalf("old handle load=%v", err)
		}
	})

	t.Run("native replacement definitely failed", func(t *testing.T) {
		backend := &faultSecretBackend{base: &memorySecretBackend{}, failStoreCall: 2}
		store := openTestStore(t, t.TempDir(), backend, Limits{})
		defer store.Close()
		handle, record, err := store.Create(t.Context(), CreateInput{Title: "preserved", Checkpoint: []byte(`{"v":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		if err := store.Clear(t.Context()); !errors.Is(err, ErrKeyUnavailable) {
			t.Fatalf("clear error=%v", err)
		}
		loaded, err := handle.Load()
		if err != nil || loaded.Record.SessionID != record.SessionID {
			t.Fatalf("old generation was not preserved: %+v err=%v", loaded, err)
		}
	})

	t.Run("native replacement outcome unknown", func(t *testing.T) {
		backend := &faultSecretBackend{base: &memorySecretBackend{}, failStoreCall: 2, commitBeforeError: true, failLoadAfterStore: true}
		store := openTestStore(t, t.TempDir(), backend, Limits{})
		defer store.Close()
		if _, _, err := store.Create(t.Context(), CreateInput{Title: "clear", Checkpoint: []byte(`{"v":1}`)}); err != nil {
			t.Fatal(err)
		}
		if err := store.Clear(t.Context()); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("clear error=%v", err)
		}
	})

	t.Run("stale envelope cannot register", func(t *testing.T) {
		store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
		defer store.Close()
		handle, record, err := store.Create(t.Context(), CreateInput{Title: "stale", Checkpoint: []byte(`{"v":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		originalDelete := store.deleteFile
		store.deleteFile = func(name string) error {
			if name == keyName(record.StorageID) {
				return errors.New("injected stale envelope residue")
			}
			return originalDelete(name)
		}
		if err := store.Clear(t.Context()); !errors.Is(err, ErrDeleteFailed) {
			t.Fatalf("clear cleanup error=%v", err)
		}
		store.deleteFile = originalDelete
		summaries, err := store.List(t.Context())
		if err != nil || len(summaries) != 1 || !summaries[0].Corrupt || !summaries[0].LocatorOnly || summaries[0].SessionID != "" || summaries[0].StorageID != record.StorageID {
			t.Fatalf("old-generation residue was not safely isolated: %+v err=%v", summaries, err)
		}
	})
}

func TestProjectionValidationRejectsInvalidStableSummaryMetadata(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "index", Checkpoint: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	base := projectionFromRecord(record)
	cases := []struct {
		name   string
		mutate func(*indexProjection)
	}{
		{name: "workspace", mutate: func(value *indexProjection) { value.WorkspaceID = "workspace"; value.WorkspaceLabel = "root" }},
		{name: "provider", mutate: func(value *indexProjection) { value.ProviderName = "custom" }},
		{name: "lifecycle", mutate: func(value *indexProjection) { value.Lifecycle = "archived" }},
		{name: "profile", mutate: func(value *indexProjection) { value.ServerProfileFingerprint = strings.Repeat("A", 64) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			if err := validateProjection(candidate, record.PrivacyGeneration); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("validation error=%v", err)
			}
		})
	}
}

func TestSaveRejectsInvalidStableSessionMetadataAndTypedReceipts(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "strict", Checkpoint: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	validPayload := PreferencePayload{
		Content: "回答先给结论", Reason: "用户明确要求", Category: "interaction_preference",
		Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	cases := []struct {
		name   string
		mutate func(*SessionRecord)
	}{
		{name: "last-opened", mutate: func(value *SessionRecord) { value.LastOpenedAt = value.CreatedAt.Add(-time.Second) }},
		{name: "lifecycle", mutate: func(value *SessionRecord) { value.Lifecycle = "archived" }},
		{name: "title-source", mutate: func(value *SessionRecord) { value.TitleSource = "generated" }},
		{name: "workspace-binding", mutate: func(value *SessionRecord) { value.WorkspaceID = "workspace" }},
		{name: "provider-binding", mutate: func(value *SessionRecord) { value.ProviderName = "custom" }},
		{name: "typed-receipt", mutate: func(value *SessionRecord) {
			value.PreferenceReceipts = []PreferenceReceipt{{
				ToolCallID: "call", CreateOperationID: "10000000-0000-4000-8000-000000000001",
				AdmitOperationID: "10000000-0000-4000-8000-000000000002", RejectOperationID: "10000000-0000-4000-8000-000000000003",
				Payload: validPayload, Stage: PreferenceStageCreate, StableCode: "preference_saved", Outcome: NoticeOutcomeCompleted,
			}}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneRecord(record)
			test.mutate(&candidate)
			if _, err := handle.Save(t.Context(), record.RecordRevision, candidate); !errors.Is(err, ErrInvalid) {
				t.Fatalf("save error=%v", err)
			}
		})
	}
}

func TestStoreRoundTripIndexRebuildDirtyRecoveryAndDeletion(t *testing.T) {
	backend := &memorySecretBackend{}
	store := openTestStore(t, t.TempDir(), backend, Limits{})
	defer store.Close()

	workspaceRoot := filepath.Clean(t.TempDir())
	pathHash := "sha256:" + strings.Repeat("b", 64)
	identityHash := "sha256:" + strings.Repeat("c", 64)
	handle, created, err := store.Create(t.Context(), CreateInput{
		Title: "Graph lesson", WorkspaceID: identityHash, WorkspaceRoot: workspaceRoot, WorkspaceLabel: filepath.Base(workspaceRoot),
		WorkspacePathHash: pathHash, WorkspaceRootIdentityHash: identityHash,
		ProviderName: "custom", ProviderEndpoint: "https://model.example/v1", ProviderModel: "test-model",
		Checkpoint: []byte(`{"schema_version":1,"messages":["hello"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.RecordRevision != 1 || created.CheckpointRevision != 1 || created.CommitID == "" ||
		created.LastOpenedAt.IsZero() || created.ServerProfileFingerprint != strings.Repeat("a", 64) ||
		created.TitleSource != "auto" || created.TitleRevision != 1 || created.Lifecycle != "active" || created.TranscriptCount != 0 {
		t.Fatalf("created stable metadata=%+v", created)
	}
	persistedEntries, err := os.ReadDir(store.rootPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range persistedEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".enc") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(store.rootPath, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("Graph lesson")) || bytes.Contains(raw, []byte("hello")) {
			t.Fatalf("plaintext leaked into %s", entry.Name())
		}
	}

	if _, _, err := store.OpenSession(t.Context(), created.SessionID); !errors.Is(err, ErrInUse) {
		t.Fatalf("second writer error=%v", err)
	}
	marker, err := handle.MarkDirty(t.Context(), 1, 1, "tool-write", false)
	if err != nil {
		t.Fatal(err)
	}
	if marker.BaseRevision != 1 {
		t.Fatalf("dirty=%+v", marker)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, loaded, err := store.OpenSession(t.Context(), created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Interrupted == nil || loaded.Interrupted.DirtyID != marker.DirtyID {
		t.Fatalf("interrupted=%+v", loaded.Interrupted)
	}
	candidate := loaded.Record
	candidate.Title = "Graph lesson continued"
	candidate.Checkpoint = []byte(`{"schema_version":1,"messages":["hello","continued"]}`)
	candidate.CommittedUserTurns = 2
	candidate.Lifecycle = "closed"
	candidate.LastConsumedDirtyID = marker.DirtyID
	saved, err := reopened.Save(t.Context(), 1, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RecordRevision != 2 || saved.CheckpointRevision != 2 || saved.CommitID == created.CommitID || saved.CommittedUserTurns != 2 || saved.Lifecycle != "closed" {
		t.Fatalf("saved stable metadata=%+v previous_commit=%q", saved, created.CommitID)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(store.rootPath, indexName)); err != nil {
		t.Fatal(err)
	}
	summaries, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].SessionID != created.SessionID || summaries[0].RecordRevision != 2 ||
		summaries[0].CheckpointRevision != 2 || summaries[0].CommittedUserTurns != 2 || summaries[0].TranscriptCount != 0 ||
		summaries[0].ServerProfileFingerprint != strings.Repeat("a", 64) || summaries[0].TitleSource != "auto" ||
		summaries[0].TitleRevision != 1 || summaries[0].Lifecycle != "closed" || summaries[0].LastOpenedAt.IsZero() {
		t.Fatalf("rebuilt summaries=%+v", summaries)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, indexName)); err != nil {
		t.Fatalf("index was not rebuilt: %v", err)
	}

	finalHandle, final, err := store.OpenSession(t.Context(), created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Interrupted != nil || final.Record.LastConsumedDirtyID != marker.DirtyID {
		t.Fatalf("final=%+v", final)
	}
	if err := finalHandle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), DeleteTarget{SessionID: created.SessionID, StorageID: created.StorageID, ExpectedRecordRevision: 2}); err != nil {
		t.Fatal(err)
	}
	if summaries, err := store.List(t.Context()); err != nil || len(summaries) != 0 {
		t.Fatalf("after delete=%+v err=%v", summaries, err)
	}
}

func TestDirtyMarkerAndFileReceiptsUseStrictBoundedRecoverySchema(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root, &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "recovery", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 7, "agent-turn", false)
	if err != nil {
		t.Fatal(err)
	}
	if marker.TurnSequence != 7 || marker.OperationClass != "agent-turn" || marker.MayHaveSideEffect || marker.Preference != nil || marker.File != nil {
		t.Fatalf("ordinary marker=%+v", marker)
	}
	if _, err := handle.MarkDirty(t.Context(), record.RecordRevision, 8, "agent-turn", false); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("second marker error=%v", err)
	}
	invalid := marker
	invalid.File = &FileWriteAhead{
		ToolCallID: "write-call", Operation: "write_replace", Path: "notes.md", Kind: "file",
		InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown,
	}
	if _, err := handle.UpdateDirty(t.Context(), invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("file marker without side-effect bit error=%v", err)
	}
	invalid.MayHaveSideEffect = true
	invalid.File.Path = filepath.Join(root, "notes.md")
	if _, err := handle.UpdateDirty(t.Context(), invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("absolute path accepted: %v", err)
	}

	candidate := marker
	candidate.MayHaveSideEffect = true
	candidate.File = &FileWriteAhead{
		ToolCallID: "write-call", Operation: "write_replace", Path: "notes.md", Kind: "file",
		InvalidateObserved: true, StableCode: FilePublicationUnknownCode, PublicationOutcome: NoticeOutcomeUnknown,
	}
	updated, err := handle.UpdateDirty(t.Context(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.MayHaveSideEffect || updated.File == nil || updated.File.PublicationOutcome != NoticeOutcomeUnknown {
		t.Fatalf("file marker=%+v", updated)
	}
	plain, err := encodeStrict(updated)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"candidate", "expected_hash", "preview", "authorization", "workspace_root", "raw_args", "secret-body"} {
		if bytes.Contains(plain, []byte(forbidden)) {
			t.Fatalf("dirty marker persisted forbidden field %q: %s", forbidden, plain)
		}
	}
	if !bytes.Contains(plain, []byte(`"turn_sequence":7`)) || !bytes.Contains(plain, []byte(`"may_have_side_effect":true`)) ||
		!bytes.Contains(plain, []byte(FilePublicationUnknownCode)) || !bytes.Contains(plain, []byte(`"publication_outcome":"unknown"`)) {
		t.Fatalf("dirty marker missing recovery contract: %s", plain)
	}
	withUnknown := bytes.Replace(plain, []byte(`"started_at"`), []byte(`"raw_args":{"secret":"secret-body"},"started_at"`), 1)
	var decoded DirtyMarker
	if err := decodeStrict(withUnknown, &decoded, DefaultLimits().DirtyMarkerBytes); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unknown dirty field error=%v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, loaded, err := store.OpenSession(t.Context(), record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Interrupted == nil || !reflect.DeepEqual(*loaded.Interrupted, updated) {
		t.Fatalf("dirty round trip=%+v want=%+v", loaded.Interrupted, updated)
	}
	recordCandidate := loaded.Record
	recordCandidate.FileReceipts = []FileReceipt{
		{ToolCallID: "write-call", Operation: "write_replace", Path: "notes.md", Kind: "file", InvalidateObserved: true, StableCode: FilePublicationUnknownCode, Outcome: NoticeOutcomeUnknown},
		{ToolCallID: "write-completed", Operation: "write_replace", Path: "done.md", Kind: "file", ContentHash: "sha256:" + strings.Repeat("d", 64), StableCode: FilePublicationCompletedCode, Outcome: NoticeOutcomeCompleted},
	}
	recordCandidate.LastConsumedDirtyID = updated.DirtyID
	saved, err := reopened.Save(t.Context(), record.RecordRevision, recordCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	finalHandle, final, err := store.OpenSession(t.Context(), record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer finalHandle.Close()
	if final.Interrupted != nil || !reflect.DeepEqual(final.Record.FileReceipts, saved.FileReceipts) || len(final.Record.FileReceipts) != 2 {
		t.Fatalf("file receipt round trip=%+v", final)
	}
	encodedReceipts, err := encodeStrict(final.Record.FileReceipts)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedReceipts, []byte(root)) || bytes.Contains(encodedReceipts, []byte("secret-body")) ||
		!bytes.Contains(encodedReceipts, []byte(FilePublicationCompletedCode)) || !bytes.Contains(encodedReceipts, []byte(FilePublicationUnknownCode)) {
		t.Fatalf("unsafe or incomplete receipts: %s", encodedReceipts)
	}
}

func TestStoreQuotaDoesNotEvictAndPrivacyClearInvalidatesOpenWriter(t *testing.T) {
	backend := &memorySecretBackend{}
	limits := DefaultLimits()
	limits.Sessions = 1
	store := openTestStore(t, t.TempDir(), backend, limits)
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "one", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(t.Context(), CreateInput{Title: "two", Checkpoint: []byte(`{"v":2}`)}); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("second create error=%v", err)
	}
	if summaries, err := store.List(t.Context()); err != nil || len(summaries) != 1 || summaries[0].SessionID != record.SessionID {
		t.Fatalf("quota evicted data: %+v err=%v", summaries, err)
	}
	candidate := record
	candidate.Checkpoint = []byte(`{"v":3}`)
	if err := store.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Save(t.Context(), 1, candidate); !errors.Is(err, ErrPrivacyInvalidated) {
		t.Fatalf("save after clear error=%v", err)
	}
	if summaries, err := store.List(t.Context()); err != nil || len(summaries) != 0 {
		t.Fatalf("after clear=%+v err=%v", summaries, err)
	}
	_ = handle.Close()
}

func TestSaveAndDirtyMarkerReconcilePublicationFaults(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "faults", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	originalPublish := store.publish
	store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
		result, err := originalPublish(ctx, name, data, options)
		if err == nil && name == recordName(record.StorageID) {
			return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
		}
		return result, err
	}
	candidate := record
	candidate.Checkpoint = []byte(`{"v":2}`)
	saved, err := handle.Save(t.Context(), 1, candidate)
	if err != nil || saved.RecordRevision != 2 {
		t.Fatalf("reconciled save=%+v err=%v", saved, err)
	}

	store.publish = func(context.Context, string, []byte, securefile.PublishOptions) (securefile.PublishResult, error) {
		return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
	}
	candidate = saved
	candidate.Checkpoint = []byte(`{"v":3}`)
	if _, err := handle.Save(t.Context(), 2, candidate); !errors.Is(err, ErrCheckpointSaveFailed) {
		t.Fatalf("pre-publication failure=%v", err)
	}
	store.publish = originalPublish
	loaded, err := handle.Load()
	if err != nil || loaded.Record.RecordRevision != 2 {
		t.Fatalf("after failed save=%+v err=%v", loaded, err)
	}
	if _, err := handle.Save(t.Context(), 1, candidate); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("stale save error=%v", err)
	}

	store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
		result, err := originalPublish(ctx, name, data, options)
		if err == nil && name == dirtyName(record.StorageID) {
			return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
		}
		return result, err
	}
	marker, err := handle.MarkDirty(t.Context(), 2, 2, "fault-injected-write", false)
	if err != nil || marker.DirtyID == "" {
		t.Fatalf("reconciled dirty=%+v err=%v", marker, err)
	}
}

func TestContainerRejectsWrongKeyTamperTruncationAndRecordSwap(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	wrong := []byte(strings.Repeat("w", 32))
	profile := [32]byte{1, 2, 3}
	_, session, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	_, storage, err := randomStorageID()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sealContainer(key, containerHeader{SchemaVersion: 1, Kind: kindRecord, Profile: profile, Generation: 7, Session: session, Storage: storage, Revision: 9}, []byte(`{"value":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	expected := containerExpectation{SchemaVersion: 1, Kind: kindRecord, Profile: profile, Generation: 7, Session: session, Storage: storage, Revision: 9, MaxPayload: 1024}
	if _, _, err := openContainer(wrong, encoded, expected); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key error=%v", err)
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(tampered)-1] ^= 0x40
	if _, _, err := openContainer(key, tampered, expected); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tamper error=%v", err)
	}
	if _, _, err := openContainer(key, encoded[:len(encoded)-1], expected); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncate error=%v", err)
	}
	_, otherStorage, err := randomStorageID()
	if err != nil {
		t.Fatal(err)
	}
	expected.Storage = otherStorage
	if _, _, err := openContainer(key, encoded, expected); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("swap error=%v", err)
	}
	profileSecretValue, err := newEncodedProfileSecret(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(profileSecretValue) != profileSecretSize {
		t.Fatalf("profile secret size=%d", len(profileSecretValue))
	}
	if generation, decodedKey, err := decodeProfileSecret(profileSecretValue); err != nil || generation != 1 || len(decodedKey) != 32 {
		t.Fatalf("profile secret generation=%d key=%d err=%v", generation, len(decodedKey), err)
	} else {
		zero(decodedKey)
	}
	trailingProfileSecret := append(append([]byte(nil), profileSecretValue...), 0)
	if _, _, err := decodeProfileSecret(trailingProfileSecret); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("trailing profile secret error=%v", err)
	}
}

func TestRecordSwapIsIsolatedAsCorrupt(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	firstHandle, first, err := store.Create(t.Context(), CreateInput{Title: "first", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	secondHandle, second, err := store.Create(t.Context(), CreateInput{Title: "second", Checkpoint: []byte(`{"v":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	_ = firstHandle.Close()
	_ = secondHandle.Close()
	firstPath := filepath.Join(store.rootPath, recordName(first.StorageID))
	secondPath := filepath.Join(store.rootPath, recordName(second.StorageID))
	firstRaw, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstPath, secondRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, firstRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.OpenSession(t.Context(), first.SessionID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("first swap error=%v", err)
	}
	summaries, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	corrupt := 0
	for _, summary := range summaries {
		if summary.Corrupt {
			corrupt++
		}
	}
	if corrupt != 2 {
		t.Fatalf("summaries=%+v", summaries)
	}
}

func TestCorruptEnvelopeWithoutCatalogDoesNotTrustRawSessionIdentity(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	firstHandle, first, err := store.Create(t.Context(), CreateInput{Title: "first", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	secondHandle, second, err := store.Create(t.Context(), CreateInput{Title: "second", Checkpoint: []byte(`{"v":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	_ = firstHandle.Close()
	_ = secondHandle.Close()
	if err := store.deleteFile(indexName); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(store.rootPath, keyName(first.StorageID))
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := parseUUID("ffffffff-ffff-4fff-bfff-ffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	copy(raw[56:72], forged[:])
	raw[len(raw)-1] ^= 0x40
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	summaries, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries=%+v", summaries)
	}
	var locator, healthy *Summary
	for index := range summaries {
		current := &summaries[index]
		if current.StorageID == first.StorageID {
			locator = current
		}
		if current.StorageID == second.StorageID {
			healthy = current
		}
	}
	if locator == nil || !locator.Corrupt || !locator.LocatorOnly || locator.SessionID != "" || healthy == nil || healthy.Corrupt || healthy.SessionID != second.SessionID {
		t.Fatalf("corrupt isolation locator=%+v healthy=%+v", locator, healthy)
	}
}

func TestMissingOrReplacedNativeProfileKeyFailsClosed(t *testing.T) {
	backend := &memorySecretBackend{}
	root := t.TempDir()
	store := openTestStore(t, root, backend, Limits{})
	handle, _, err := store.Create(t.Context(), CreateInput{Title: "protected", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	_ = store.Close()
	backend.mu.Lock()
	originalSecret := append([]byte(nil), backend.value...)
	backend.value = nil
	backend.mu.Unlock()
	if reopened, err := Open(t.Context(), Options{Root: root, ProfileFingerprint: strings.Repeat("a", 64), Secrets: backend}); err == nil {
		_ = reopened.Close()
		t.Fatal("missing native key silently replaced persisted profile")
	} else if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("missing key error=%v", err)
	}
	backend.mu.Lock()
	backend.value = originalSecret
	backend.mu.Unlock()
	replacement, err := newEncodedProfileSecret(1)
	if err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.value = replacement
	backend.mu.Unlock()
	reopened, err := Open(t.Context(), Options{Root: root, ProfileFingerprint: strings.Repeat("a", 64), Secrets: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	listed, err := reopened.List(t.Context())
	if err != nil || len(listed) != 1 || !listed[0].Corrupt || !listed[0].LocatorOnly || listed[0].SessionID != "" {
		t.Fatalf("replaced key list=%+v error=%v", listed, err)
	}
}

func TestStoreRejectsSymlinkedRootAndProfileQuotaDoesNotPublish(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatalf("create required symlink fixture: %v", err)
	}
	if _, err := Open(t.Context(), Options{
		Root: filepath.Join(linkParent, "sessions"), ProfileFingerprint: strings.Repeat("a", 64), Secrets: &memorySecretBackend{},
	}); err == nil {
		t.Fatal("symlinked session root was accepted")
	}

	limits := DefaultLimits()
	limits.ProfileCiphertextBytes = 256
	store := openTestStore(t, filepath.Join(base, "quota"), &memorySecretBackend{}, limits)
	defer store.Close()
	if _, _, err := store.Create(t.Context(), CreateInput{Title: "too-large", Checkpoint: []byte(`{"payload":"abcdefghijklmnopqrstuvwxyz"}`)}); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("profile quota error=%v", err)
	}
	if summaries, err := store.List(t.Context()); err != nil || len(summaries) != 0 {
		t.Fatalf("quota left published session: %+v err=%v", summaries, err)
	}
}

func TestCatalogContainsOnlyOpaqueLocatorsAndProjectionUsesSessionDEK(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	workspaceRoot := filepath.Clean(t.TempDir())
	workspaceID := "sha256:" + strings.Repeat("b", 64)
	handle, record, err := store.Create(t.Context(), CreateInput{
		Title: "sensitive title", WorkspaceID: workspaceID, WorkspaceRoot: workspaceRoot, WorkspaceLabel: "private workspace",
		WorkspacePathHash: workspaceID, ProviderName: "custom", ProviderEndpoint: "https://provider.example/v1", ProviderModel: "secret-model",
		Checkpoint: []byte(`{"secret":"body"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	store.stateMu.RLock()
	profileKey := append([]byte(nil), store.profileKey...)
	generation := store.generation
	store.stateMu.RUnlock()
	defer zero(profileKey)
	catalogSnapshot, err := store.root.ReadSnapshot(indexName, store.limits.SessionCiphertextBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	catalogPlain, _, err := openContainer(profileKey, catalogSnapshot.Data, containerExpectation{
		SchemaVersion: indexSchemaVersion, Kind: kindIndex, Profile: store.profile, Generation: generation, MaxPayload: store.limits.SessionPlaintextBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	var catalog indexFile
	if err := decodeStrict(catalogPlain, &catalog, store.limits.SessionPlaintextBytes); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Entries) != 1 || catalog.Entries[0] != (indexLocator{SessionID: record.SessionID, StorageID: record.StorageID}) {
		t.Fatalf("catalog=%+v", catalog)
	}
	for _, forbidden := range []string{"sensitive title", "private workspace", "provider.example", "secret-model", "record_revision", "updated_at", "lifecycle", "search"} {
		if bytes.Contains(catalogPlain, []byte(forbidden)) {
			t.Fatalf("catalog leaked %q: %s", forbidden, catalogPlain)
		}
	}

	projection, _, header, err := store.readProjection(record.StorageID, record.SessionID, handle.dataKey, generation, record.RecordRevision)
	if err != nil {
		t.Fatal(err)
	}
	if header.Kind != kindProjection || projection.RecordCommitID != record.CommitID || projection.Title != record.Title || projection.WorkspaceLabel != "private workspace" {
		t.Fatalf("projection=%+v header=%+v", projection, header)
	}
	if _, _, _, _, err := store.readEnvelope(record.StorageID, profileKey, generation); err != nil {
		t.Fatal(err)
	}
	envelopeRaw, err := store.root.ReadLimit(keyName(record.StorageID), 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	envelopePlain, _, err := openContainer(profileKey, envelopeRaw, containerExpectation{
		SchemaVersion: envelopeSchemaVersion, Kind: kindEnvelope, Profile: store.profile, Generation: generation,
		Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), Revision: 1, MaxPayload: 64 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelopePlain, []byte("created_at")) {
		t.Fatalf("key envelope retained sensitive created_at metadata: %s", envelopePlain)
	}
}

func TestProjectionRejectsWrongKeySwapTamperAndRevisionMismatch(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	firstHandle, first, err := store.Create(t.Context(), CreateInput{Title: "first", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer firstHandle.Close()
	secondHandle, second, err := store.Create(t.Context(), CreateInput{Title: "second", Checkpoint: []byte(`{"v":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer secondHandle.Close()
	firstRaw, err := store.root.ReadLimit(indexProjectionName(first.StorageID), store.limits.SessionCiphertextBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	expected := containerExpectation{
		SchemaVersion: projectionSchemaVersion, Kind: kindProjection, Profile: store.profile, Generation: first.PrivacyGeneration,
		Session: mustParseUUIDForTest(t, first.SessionID), Storage: mustParseStorageIDForTest(t, first.StorageID), Revision: first.RecordRevision, MaxPayload: maxProjectionBytes,
	}
	if _, _, err := openContainer(secondHandle.dataKey, firstRaw, expected); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key error=%v", err)
	}
	swapped := expected
	swapped.Session = mustParseUUIDForTest(t, second.SessionID)
	swapped.Storage = mustParseStorageIDForTest(t, second.StorageID)
	if _, _, err := openContainer(firstHandle.dataKey, firstRaw, swapped); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("swap error=%v", err)
	}
	tampered := append([]byte(nil), firstRaw...)
	tampered[len(tampered)-1] ^= 0x80
	if _, _, err := openContainer(firstHandle.dataKey, tampered, expected); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tamper error=%v", err)
	}

	mismatched := projectionFromRecord(first)
	mismatched.RecordRevision++
	plain, err := encodeStrict(mismatched)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sealContainer(firstHandle.dataKey, containerHeader{
		SchemaVersion: projectionSchemaVersion, Kind: kindProjection, Profile: store.profile, Generation: first.PrivacyGeneration,
		Session: mustParseUUIDForTest(t, first.SessionID), Storage: mustParseStorageIDForTest(t, first.StorageID), Revision: first.RecordRevision,
	}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath, indexProjectionName(first.StorageID)), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.readProjection(first.StorageID, first.SessionID, firstHandle.dataKey, first.PrivacyGeneration, 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("revision mismatch error=%v", err)
	}

	wrongCommit := projectionFromRecord(first)
	wrongCommit.RecordCommitID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
	plain, err = encodeStrict(wrongCommit)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = sealContainer(firstHandle.dataKey, containerHeader{
		SchemaVersion: projectionSchemaVersion, Kind: kindProjection, Profile: store.profile, Generation: first.PrivacyGeneration,
		Session: mustParseUUIDForTest(t, first.SessionID), Storage: mustParseStorageIDForTest(t, first.StorageID), Revision: first.RecordRevision,
	}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath, indexProjectionName(first.StorageID)), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	repaired, _, _, err := store.readProjection(first.StorageID, first.SessionID, firstHandle.dataKey, first.PrivacyGeneration, first.RecordRevision)
	if err != nil || repaired.RecordCommitID != first.CommitID {
		t.Fatalf("repaired projection=%+v err=%v", repaired, err)
	}
}

func TestCatalogAndProjectionMissingAreRebuiltFromAuthoritativeRecord(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "rebuild me", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.deleteFile(indexName); err != nil {
		t.Fatal(err)
	}
	if err := store.deleteFile(indexProjectionName(record.StorageID)); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 1 || listed[0].Corrupt || listed[0].Title != record.Title || listed[0].RecordRevision != record.RecordRevision {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	for _, name := range []string{indexName, indexProjectionName(record.StorageID)} {
		if _, err := os.Stat(filepath.Join(store.rootPath, name)); err != nil {
			t.Fatalf("%s was not rebuilt: %v", name, err)
		}
	}
}

func TestCorruptRecordUsesValidProjectionAndCanBeDeletedByProjectionRevision(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "recoverable corrupt", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(store.rootPath, recordName(record.StorageID))
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x40
	if err := os.WriteFile(recordPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 1 || !listed[0].Corrupt || listed[0].LocatorOnly || listed[0].Title != record.Title || listed[0].RecordRevision != record.RecordRevision {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if err := store.Delete(t.Context(), DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision + 1}); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("stale projection delete error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, keyName(record.StorageID))); err != nil {
		t.Fatalf("stale delete removed wrapped key: %v", err)
	}
	if err := store.Delete(t.Context(), DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{keyName(record.StorageID), recordName(record.StorageID), indexProjectionName(record.StorageID), dirtyName(record.StorageID)} {
		if _, err := os.Stat(filepath.Join(store.rootPath, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains: %v", name, err)
		}
	}
}

func TestLocatorOnlyCorruptEntryRequiresExplicitZeroRevisionDelete(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "locator", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.deleteFile(indexName); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(store.rootPath, keyName(record.StorageID))
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x20
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 1 || !listed[0].LocatorOnly || listed[0].SessionID != "" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if err := store.Delete(t.Context(), DeleteTarget{StorageID: record.StorageID, ExpectedRecordRevision: 1}); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("nonzero locator delete error=%v", err)
	}
	if err := store.Delete(t.Context(), DeleteTarget{StorageID: record.StorageID}); err != nil {
		t.Fatal(err)
	}
	if listed, err := store.List(t.Context()); err != nil || len(listed) != 0 {
		t.Fatalf("after locator delete=%+v err=%v", listed, err)
	}
}

func TestDeleteAfterKeyRemovalFaultCannotReregisterResidualCiphertext(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "key first", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	originalDelete := store.deleteFile
	store.deleteFile = func(name string) error {
		err := originalDelete(name)
		if name == keyName(record.StorageID) && err == nil {
			return errors.New("injected sync uncertainty after key removal")
		}
		return err
	}
	if err := store.Delete(t.Context(), DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision}); !errors.Is(err, ErrDeleteFailed) {
		t.Fatalf("delete error=%v", err)
	}
	store.deleteFile = originalDelete
	if _, err := os.Stat(filepath.Join(store.rootPath, keyName(record.StorageID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrapped key remains: %v", err)
	}
	if listed, err := store.List(t.Context()); err != nil || len(listed) != 0 {
		t.Fatalf("residual ciphertext re-registered: %+v err=%v", listed, err)
	}
}

func TestDeleteRejectsLockedSessionAndAuthenticatedFutureProjection(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "locked", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	target := DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision}
	if err := store.Delete(t.Context(), target); !errors.Is(err, ErrInUse) {
		t.Fatalf("locked delete error=%v", err)
	}
	projection, _, header, err := store.readProjection(record.StorageID, record.SessionID, handle.dataKey, record.PrivacyGeneration, record.RecordRevision)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := encodeStrict(projection)
	if err != nil {
		t.Fatal(err)
	}
	future := sealContainerVersionForTest(t, handle.dataKey, header, containerVersion+1, projectionSchemaVersion, plain)
	if err := os.WriteFile(filepath.Join(store.rootPath, indexProjectionName(record.StorageID)), future, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 1 || !listed[0].VersionUnsupported || !listed[0].Unavailable {
		t.Fatalf("future projection listed=%+v err=%v", listed, err)
	}
	if err := store.Delete(t.Context(), target); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("future delete error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, keyName(record.StorageID))); err != nil {
		t.Fatalf("future delete removed key: %v", err)
	}
}

func TestDeleteRejectsAuthenticatedFutureEnvelopeAndRecord(t *testing.T) {
	for _, test := range []struct {
		name string
		kind payloadKind
	}{
		{name: "envelope", kind: kindEnvelope},
		{name: "record", kind: kindRecord},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer store.Close()
			handle, record, err := store.Create(t.Context(), CreateInput{Title: "future", Checkpoint: []byte(`{"v":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			var key, plain []byte
			var header containerHeader
			var path string
			switch test.kind {
			case kindEnvelope:
				store.stateMu.RLock()
				key = append([]byte(nil), store.profileKey...)
				store.stateMu.RUnlock()
				path = filepath.Join(store.rootPath, keyName(record.StorageID))
				raw, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				plain, header, err = openContainer(key, raw, containerExpectation{
					SchemaVersion: envelopeSchemaVersion, Kind: kindEnvelope, Profile: store.profile, Generation: record.PrivacyGeneration,
					Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), MaxPayload: 64 << 10,
				})
				header.Revision = 2
			case kindRecord:
				key = append([]byte(nil), handle.dataKey...)
				path = filepath.Join(store.rootPath, recordName(record.StorageID))
				raw, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				plain, header, err = openContainer(key, raw, containerExpectation{
					SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: store.profile, Generation: record.PrivacyGeneration,
					Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), Revision: record.RecordRevision, MaxPayload: store.limits.SessionPlaintextBytes,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			future := sealContainerVersionForTest(t, key, header, containerVersion+1, header.SchemaVersion, plain)
			zero(key)
			if err := os.WriteFile(path, future, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			listed, err := store.List(t.Context())
			if err != nil || len(listed) != 1 || !listed[0].VersionUnsupported {
				t.Fatalf("listed=%+v err=%v", listed, err)
			}
			target := DeleteTarget{SessionID: record.SessionID, StorageID: record.StorageID, ExpectedRecordRevision: record.RecordRevision}
			if err := store.Delete(t.Context(), target); !errors.Is(err, ErrVersionUnsupported) {
				t.Fatalf("delete error=%v", err)
			}
			if _, err := os.Stat(filepath.Join(store.rootPath, keyName(record.StorageID))); err != nil {
				t.Fatalf("future item key removed: %v", err)
			}
		})
	}
}

func TestProjectionPublicationFailureOnlyDegradesCacheAndClearRemovesProjection(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	originalPublish := store.publish
	store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
		if strings.HasPrefix(name, "index-") {
			return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
		}
		return originalPublish(ctx, name, data, options)
	}
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "degraded projection", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !store.IndexDegraded() {
		t.Fatal("projection failure did not mark the index cache degraded")
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	store.publish = originalPublish
	if listed, err := store.List(t.Context()); err != nil || len(listed) != 1 || listed[0].Corrupt {
		t.Fatalf("rebuilt list=%+v err=%v", listed, err)
	}
	if store.IndexDegraded() {
		t.Fatal("successful projection rebuild did not clear degraded state")
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, indexProjectionName(record.StorageID))); err != nil {
		t.Fatalf("projection missing after rebuild: %v", err)
	}
	if err := store.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{keyName(record.StorageID), recordName(record.StorageID), indexProjectionName(record.StorageID), dirtyName(record.StorageID)} {
		if _, err := os.Stat(filepath.Join(store.rootPath, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("clear left %s: %v", name, err)
		}
	}
}

func TestRecordPayloadV1MigrationIsBoundedAndAtomicallyPublished(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "migrate", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	dataKey := append([]byte(nil), handle.dataKey...)
	defer zero(dataKey)
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	writeRecordPayloadForTest(t, store, dataKey, record, 1, nil)

	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 1 || listed[0].SessionID != record.SessionID || listed[0].Corrupt {
		t.Fatalf("v1 list=%+v err=%v", listed, err)
	}
	if version, header := recordPayloadVersionOnDiskForTest(t, store, dataKey, record); version != 1 || header.SchemaVersion != recordContainerSchemaVersion {
		t.Fatalf("after list payload=%d container=%d", version, header.SchemaVersion)
	}

	migratedHandle, loaded, err := store.OpenSession(t.Context(), record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer migratedHandle.Close()
	if loaded.Record.SchemaVersion != recordPayloadSchemaVersion || !recordsEqual(loaded.Record, withRecordSchema(record, recordPayloadSchemaVersion)) {
		t.Fatalf("migrated record=%+v", loaded.Record)
	}
	if version, header := recordPayloadVersionOnDiskForTest(t, store, dataKey, record); version != recordPayloadSchemaVersion || header.SchemaVersion != recordContainerSchemaVersion {
		t.Fatalf("published payload=%d container=%d", version, header.SchemaVersion)
	}
}

func TestRecordPayloadMigrationPublicationReconciliation(t *testing.T) {
	for _, test := range []struct {
		name        string
		publish     func(securefile.PublishResult, error) (securefile.PublishResult, error)
		callPublish bool
		wantErr     error
		wantVersion int
	}{
		{name: "unknown completed", callPublish: true, wantVersion: recordPayloadSchemaVersion, publish: func(securefile.PublishResult, error) (securefile.PublishResult, error) {
			return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
		}},
		{name: "unknown unchanged", wantErr: ErrCheckpointSaveFailed, wantVersion: 1, publish: func(securefile.PublishResult, error) (securefile.PublishResult, error) {
			return securefile.PublishResult{Outcome: securefile.PublishUnknown}, securefile.ErrOutcomeUnknown
		}},
		{name: "definite unchanged", wantErr: ErrCheckpointSaveFailed, wantVersion: 1, publish: func(securefile.PublishResult, error) (securefile.PublishResult, error) {
			return securefile.PublishResult{Outcome: securefile.PublishUnchanged}, errors.New("injected migration publication failure")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
			defer store.Close()
			handle, record, err := store.Create(t.Context(), CreateInput{Title: "reconcile", Checkpoint: []byte(`{"v":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			dataKey := append([]byte(nil), handle.dataKey...)
			defer zero(dataKey)
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			writeRecordPayloadForTest(t, store, dataKey, record, 1, nil)
			originalPublish := store.publish
			store.publish = func(ctx context.Context, name string, data []byte, options securefile.PublishOptions) (securefile.PublishResult, error) {
				if name != recordName(record.StorageID) || options.Mode != securefile.PublishReplace {
					return originalPublish(ctx, name, data, options)
				}
				result, publishErr := securefile.PublishResult{Outcome: securefile.PublishUnchanged}, error(nil)
				if test.callPublish {
					result, publishErr = originalPublish(ctx, name, data, options)
				}
				return test.publish(result, publishErr)
			}
			opened, _, openErr := store.OpenSession(t.Context(), record.SessionID)
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(openErr, test.wantErr) {
				t.Fatalf("open error=%v want=%v", openErr, test.wantErr)
			}
			if version, _ := recordPayloadVersionOnDiskForTest(t, store, dataKey, record); version != test.wantVersion {
				t.Fatalf("payload version=%d want=%d", version, test.wantVersion)
			}
			if test.wantVersion == 1 {
				store.publish = originalPublish
				recovered, loaded, err := store.OpenSession(t.Context(), record.SessionID)
				if err != nil {
					t.Fatalf("retry migration: %v", err)
				}
				if loaded.Record.SchemaVersion != recordPayloadSchemaVersion {
					t.Fatalf("retry record schema=%d", loaded.Record.SchemaVersion)
				}
				_ = recovered.Close()
			}
		})
	}
}

func TestRecordPayloadMigrationRejectsMalformedAndBoundsVersions(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "strict", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	dataKey := append([]byte(nil), handle.dataKey...)
	defer zero(dataKey)
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	writeRecordPayloadForTest(t, store, dataKey, record, 1, func(plain []byte) []byte {
		return append(append([]byte(nil), plain[:len(plain)-1]...), []byte(`,"unexpected":true}`)...)
	})
	if _, _, err := store.OpenSession(t.Context(), record.SessionID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("malformed v1 error=%v", err)
	}
	writeRecordPayloadForTest(t, store, dataKey, record, 1, func(plain []byte) []byte {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(plain, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "privacy_verified")
		plain, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	})
	if _, _, err := store.OpenSession(t.Context(), record.SessionID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing required v1 field error=%v", err)
	}

	if recordMigrationMaxSteps != 2 {
		t.Fatalf("migration bound=%d", recordMigrationMaxSteps)
	}
	if recordPayloadSchemaVersion-recordMigrationMaxSteps != 1 {
		t.Fatalf("current=%d bound=%d", recordPayloadSchemaVersion, recordMigrationMaxSteps)
	}
	v1Payload := legacyRecordPayloadV1ForTest(t, record)
	v1Payload.SchemaVersion = 1
	v1, err := encodeStrict(v1Payload)
	if err != nil {
		t.Fatal(err)
	}
	migrated, source, err := decodeRecordPayload(v1, int64(len(v1)))
	if err != nil || source != 1 || migrated.SchemaVersion != recordPayloadSchemaVersion {
		t.Fatalf("v1 decode source=%d schema=%d err=%v", source, migrated.SchemaVersion, err)
	}
	for _, malformed := range [][]byte{
		[]byte(`{"schema_version":0}`),
		[]byte(`{"schema_version":-1}`),
		[]byte(`{"schema_version":"1"}`),
		[]byte(`{"schema_version":1,"schema_version":1}`),
	} {
		if _, _, err := decodeRecordPayload(malformed, int64(len(malformed))); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("malformed probe %s error=%v", malformed, err)
		}
	}
	future := cloneRecord(record)
	future.SchemaVersion = recordPayloadSchemaVersion + 1
	futurePlain, err := encodeStrict(future)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeRecordPayload(futurePlain, int64(len(futurePlain))); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("future payload error=%v", err)
	}
}

func withRecordSchema(record SessionRecord, schema int) SessionRecord {
	record.SchemaVersion = schema
	return record
}

// Round-trip only legacy-compatible fixtures into the frozen DTO, rather than
// relying on a struct conversion that would couple it to new receipt fields.
func legacyRecordPayloadV1ForTest(t *testing.T, record SessionRecord) recordPayloadV1 {
	t.Helper()
	plain, err := encodeStrict(record)
	if err != nil {
		t.Fatal(err)
	}
	var payload recordPayloadV1
	if err := decodeStrict(plain, &payload, int64(len(plain))); err != nil {
		t.Fatal(err)
	}
	return payload
}

func writeRecordPayloadForTest(t *testing.T, store *Store, dataKey []byte, record SessionRecord, version int, mutate func([]byte) []byte) {
	t.Helper()
	var payload any
	switch version {
	case 1, 2:
		legacy := legacyRecordPayloadV1ForTest(t, record)
		legacy.SchemaVersion = version
		if version == 1 {
			payload = legacy
		} else {
			payload = recordPayloadV2(legacy)
		}
	default:
		current := cloneRecord(record)
		current.SchemaVersion = version
		payload = current
	}
	plain, err := encodeStrict(payload)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		plain = mutate(plain)
	}
	ciphertext, err := sealContainer(dataKey, containerHeader{
		SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: store.profile,
		Generation: record.PrivacyGeneration, Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), Revision: record.RecordRevision,
	}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath, recordName(record.StorageID)), ciphertext, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordPayloadVersionOnDiskForTest(t *testing.T, store *Store, dataKey []byte, record SessionRecord) (int, containerHeader) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store.rootPath, recordName(record.StorageID)))
	if err != nil {
		t.Fatal(err)
	}
	plain, header, err := openContainer(dataKey, raw, containerExpectation{
		SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: store.profile,
		Generation: record.PrivacyGeneration, Session: mustParseUUIDForTest(t, record.SessionID), Storage: mustParseStorageIDForTest(t, record.StorageID), Revision: record.RecordRevision,
		MaxPayload: store.limits.SessionPlaintextBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := probeRecordPayloadSchema(plain, store.limits.SessionPlaintextBytes)
	if err != nil {
		t.Fatal(err)
	}
	return version, header
}

func mustParseUUIDForTest(t *testing.T, value string) [16]byte {
	t.Helper()
	result, err := parseUUID(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustParseStorageIDForTest(t *testing.T, value string) [16]byte {
	t.Helper()
	result, err := parseStorageID(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProjectionCountsTowardPerSessionCiphertextQuota(t *testing.T) {
	calibration := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	handle, record, err := calibration.Create(t.Context(), CreateInput{Title: "quota calibration", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	sizes := make(map[string]int64)
	for _, name := range []string{keyName(record.StorageID), recordName(record.StorageID), indexProjectionName(record.StorageID)} {
		info, statErr := os.Stat(filepath.Join(calibration.rootPath, name))
		if statErr != nil {
			t.Fatal(statErr)
		}
		sizes[name] = info.Size()
	}
	if err := calibration.Close(); err != nil {
		t.Fatal(err)
	}

	limits := DefaultLimits()
	limits.SessionCiphertextBytes = sizes[keyName(record.StorageID)] + sizes[recordName(record.StorageID)] + sizes[indexProjectionName(record.StorageID)] - 1
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, limits)
	defer store.Close()
	handle, created, err := store.Create(t.Context(), CreateInput{Title: "quota constrained", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if !store.IndexDegraded() {
		t.Fatal("projection quota failure did not degrade only the cache")
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, indexProjectionName(created.StorageID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("over-quota projection was published: %v", err)
	}
	loaded, err := handle.Load()
	if err != nil || loaded.Record.SessionID != created.SessionID {
		t.Fatalf("authoritative record/key were not retained: %+v err=%v", loaded, err)
	}
}

func TestAgentSessionProcessSingleWriter(t *testing.T) {
	if os.Getenv("EDU_AGENT_SESSION_HELPER") == "1" {
		store, err := Open(context.Background(), Options{
			Root: os.Getenv("EDU_AGENT_SESSION_ROOT"), ProfileFingerprint: strings.Repeat("a", 64),
			Secrets: fileSecretBackend{path: os.Getenv("EDU_AGENT_SESSION_SECRET")}, LockTimeout: 100 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if _, _, err := store.OpenSession(context.Background(), os.Getenv("EDU_AGENT_SESSION_ID")); !errors.Is(err, ErrInUse) {
			t.Fatalf("expected in-use, got %v", err)
		}
		t.Log("AGENT_SESSION_LOCK_HELPER_SUCCESS")
		return
	}
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	secretPath := filepath.Join(base, "native-secret")
	backend := fileSecretBackend{path: secretPath}
	store := openTestStore(t, root, backend, Limits{})
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "locked", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAgentSessionProcessSingleWriter$", "-test.v")
	command.Env = append(os.Environ(),
		"EDU_AGENT_SESSION_HELPER=1", "EDU_AGENT_SESSION_ROOT="+root,
		"EDU_AGENT_SESSION_SECRET="+secretPath, "EDU_AGENT_SESSION_ID="+record.SessionID,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("AGENT_SESSION_LOCK_HELPER_SUCCESS")) {
		t.Fatalf("helper success marker missing:\n%s", output)
	}
	t.Log(string(output))
}

func openTestStore(t *testing.T, root string, backend SecretBackend, limits Limits) *Store {
	t.Helper()
	store, err := Open(t.Context(), Options{
		Root: root, ProfileFingerprint: strings.Repeat("a", 64), Secrets: backend,
		Limits: limits, LockTimeout: 30 * time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 9, 2, 6, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
