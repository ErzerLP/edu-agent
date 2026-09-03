package agentsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/filelock"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	indexName             = "profile.index.enc"
	profileLockName       = "profile.lock"
	maxProfileSecretBytes = 4 << 10
	maxProjectionBytes    = 64 << 10
)

type Store struct {
	root     *securefile.Root
	rootPath string
	profile  [32]byte
	locator  keybackend.Locator
	backend  SecretBackend
	limits   Limits
	now      func() time.Time
	lockWait time.Duration

	stateMu       sync.RWMutex
	generation    uint64
	profileKey    []byte
	closed        bool
	indexDegraded bool
	publish       func(context.Context, string, []byte, securefile.PublishOptions) (securefile.PublishResult, error)
	deleteFile    func(string) error
}

type Handle struct {
	store      *Store
	sessionID  string
	storageID  string
	generation uint64
	dataKey    []byte
	lock       *filelock.Lock
	mu         sync.Mutex
	closed     bool
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rootPath, err := filepath.Abs(strings.TrimSpace(options.Root))
	if err != nil || strings.TrimSpace(options.Root) == "" {
		return nil, fmt.Errorf("%w: root", ErrInvalid)
	}
	profileBytes, err := hex.DecodeString(options.ProfileFingerprint)
	if err != nil || len(profileBytes) != 32 || strings.ToLower(options.ProfileFingerprint) != options.ProfileFingerprint {
		return nil, fmt.Errorf("%w: profile fingerprint", ErrInvalid)
	}
	if err := ensurePrivateRoot(rootPath); err != nil {
		return nil, err
	}
	root, err := securefile.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	backend := options.Secrets
	if backend == nil {
		backend = systemSecretBackend{}
	}
	locator := keybackend.Locator{Service: profileSecretService, Account: "profile-" + options.ProfileFingerprint}
	if err := backend.Available(locator); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
	}
	limits := normalizedLimits(options.Limits)
	now := options.Now
	if now == nil {
		now = time.Now
	}
	lockWait := options.LockTimeout
	if lockWait == 0 {
		lockWait = 2 * time.Second
	}
	store := &Store{
		root: root, rootPath: rootPath, locator: locator, backend: backend,
		limits: limits, now: now, lockWait: lockWait,
	}
	store.publish = root.Publish
	store.deleteFile = root.Delete
	copy(store.profile[:], profileBytes)
	profileLock, err := store.acquireProfileLock(ctx)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	defer profileLock.Close()
	secret, loadErr := backend.Load(locator, maxProfileSecretBytes)
	if errors.Is(loadErr, keybackend.ErrNotFound) {
		hasData, inspectErr := store.hasEncryptedData()
		if inspectErr != nil {
			_ = root.Close()
			return nil, inspectErr
		}
		if hasData {
			_ = root.Close()
			return nil, fmt.Errorf("%w: persisted session data exists without its native profile key", ErrKeyUnavailable)
		}
		secret, err = newEncodedProfileSecret(1)
		if err == nil {
			err = storeSecretVerified(backend, locator, secret)
		}
		if err != nil {
			_ = root.Close()
			if errors.Is(err, ErrOutcomeUnknown) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
		}
	} else if loadErr != nil {
		_ = root.Close()
		return nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, loadErr)
	}
	generation, key, err := decodeProfileSecret(secret)
	zero(secret)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	store.generation = generation
	store.profileKey = key
	return store, nil
}

func (s *Store) Reopen(ctx context.Context) (*Store, error) {
	if s == nil {
		return nil, ErrInvalid
	}
	s.stateMu.RLock()
	if s.closed {
		s.stateMu.RUnlock()
		return nil, ErrKeyUnavailable
	}
	options := Options{
		Root: s.rootPath, ProfileFingerprint: hex.EncodeToString(s.profile[:]), Secrets: s.backend,
		Limits: s.limits, Now: s.now, LockTimeout: s.lockWait,
	}
	s.stateMu.RUnlock()
	return Open(ctx, options)
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil
	}
	s.closed = true
	zero(s.profileKey)
	s.profileKey = nil
	s.stateMu.Unlock()
	if s.root != nil {
		return s.root.Close()
	}
	return nil
}

func (s *Store) Create(ctx context.Context, input CreateInput) (*Handle, SessionRecord, error) {
	if int64(len(input.Checkpoint)) > s.limits.SessionPlaintextBytes {
		return nil, SessionRecord{}, ErrStoreFull
	}
	// Canonicalize before assigning IDs so invalid transcript input publishes nothing.
	transcript, err := canonicalTranscriptBlob(input.Transcript, s.limits)
	if err != nil {
		return nil, SessionRecord{}, err
	}
	transcriptValue, err := DecodeTranscript(transcript, s.limits)
	if err != nil {
		return nil, SessionRecord{}, err
	}
	input.Transcript = transcript
	if err := validateCreateInput(input, s.limits); err != nil {
		return nil, SessionRecord{}, err
	}
	sessionID, sessionBytes, err := randomUUID()
	if err != nil {
		return nil, SessionRecord{}, err
	}
	storageID, storageBytes, err := randomStorageID()
	if err != nil {
		return nil, SessionRecord{}, err
	}
	commitID, _, err := randomUUID()
	if err != nil {
		return nil, SessionRecord{}, err
	}
	dataKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		return nil, SessionRecord{}, err
	}
	retainDataKey := false
	defer func() {
		if !retainDataKey {
			zero(dataKey)
		}
	}()
	lock, err := s.acquireSessionLock(ctx, storageID)
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = lock.Close()
		}
	}()

	profileLock, err := s.acquireProfileLock(ctx)
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	defer profileLock.Close()
	generation, profileKey, err := s.refreshProfileLocked()
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	defer zero(profileKey)
	index, entries, snapshot, indexDegraded, err := s.rebuildIndexLocked(ctx, profileKey, generation)
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	s.persistIndexBestEffort(ctx, profileKey, generation, index, snapshot, indexDegraded)
	if len(entries) >= s.limits.Sessions {
		zero(dataKey)
		return nil, SessionRecord{}, ErrStoreFull
	}
	now := s.now().UTC()
	record := SessionRecord{
		SchemaVersion: recordPayloadSchemaVersion, SessionID: sessionID, StorageID: storageID,
		PrivacyGeneration: generation, RecordRevision: 1,
		CommitID: commitID, CreatedAt: now, UpdatedAt: now, LastOpenedAt: now,
		CheckpointRevision: 1, ServerProfileFingerprint: hex.EncodeToString(s.profile[:]), Lifecycle: "active",
		Title: input.Title, TitleSource: "auto", TitleRevision: 1,
		WorkspaceID: input.WorkspaceID, WorkspaceRoot: input.WorkspaceRoot, WorkspaceLabel: input.WorkspaceLabel,
		WorkspacePathHash: input.WorkspacePathHash, WorkspaceRootIdentityHash: input.WorkspaceRootIdentityHash,
		ProviderName: input.ProviderName, ProviderEndpoint: input.ProviderEndpoint, ProviderModel: input.ProviderModel,
		PrivacyLearnerGeneration: input.PrivacyLearnerGeneration, PrivacyMemoryGeneration: input.PrivacyMemoryGeneration,
		PrivacyVerified: input.PrivacyVerified, TranscriptCount: uint64(len(transcriptValue.Entries)),
		Checkpoint: append(json.RawMessage(nil), input.Checkpoint...), Transcript: append(json.RawMessage(nil), input.Transcript...),
	}
	recordPlain, err := encodeStrict(record)
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	if int64(len(recordPlain)) > s.limits.SessionPlaintextBytes {
		zero(dataKey)
		return nil, SessionRecord{}, ErrStoreFull
	}
	envelope := keyEnvelope{
		SchemaVersion: envelopeSchemaVersion, SessionID: sessionID, StorageID: storageID,
		PrivacyGeneration: generation,
		DataKey:           base64.RawStdEncoding.EncodeToString(dataKey),
	}
	envelopePlain, err := encodeStrict(envelope)
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	recordCipher, err := sealContainer(dataKey, containerHeader{
		SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: s.profile, Generation: generation,
		Session: sessionBytes, Storage: storageBytes, Revision: 1,
	}, recordPlain)
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	envelopeCipher, err := sealContainer(profileKey, containerHeader{
		SchemaVersion: envelopeSchemaVersion, Kind: kindEnvelope, Profile: s.profile, Generation: generation,
		Session: sessionBytes, Storage: storageBytes, Revision: 1,
	}, envelopePlain)
	if err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	if err := s.checkQuotaLocked(int64(len(recordCipher)+len(envelopeCipher)), int64(len(recordCipher)+len(envelopeCipher))); err != nil {
		zero(dataKey)
		return nil, SessionRecord{}, err
	}
	recordPublishErr := s.publishCreate(ctx, recordName(storageID), recordCipher)
	if errors.Is(recordPublishErr, ErrOutcomeUnknown) {
		observed, _, _, _, observeErr := s.readRecord(storageID, sessionID, dataKey, generation)
		switch {
		case observeErr == nil && recordsEqual(observed, record):
			recordPublishErr = nil
		case errors.Is(observeErr, ErrNotFound):
			return nil, SessionRecord{}, ErrCheckpointSaveFailed
		default:
			return nil, SessionRecord{}, ErrOutcomeUnknown
		}
	}
	if recordPublishErr != nil {
		zero(dataKey)
		return nil, SessionRecord{}, recordPublishErr
	}
	keyPublishErr := s.publishCreate(ctx, keyName(storageID), envelopeCipher)
	if errors.Is(keyPublishErr, ErrOutcomeUnknown) {
		observedEnvelope, observedKey, _, _, observeErr := s.readEnvelope(storageID, profileKey, generation)
		confirmed := observeErr == nil && observedEnvelope.SessionID == sessionID && bytes.Equal(observedKey, dataKey)
		zero(observedKey)
		if confirmed {
			keyPublishErr = nil
		} else if errors.Is(observeErr, ErrNotFound) {
			keyPublishErr = ErrCheckpointSaveFailed
		}
	}
	if keyPublishErr != nil {
		cleanupErr := s.deleteFile(recordName(storageID))
		zero(dataKey)
		if cleanupErr != nil {
			return nil, SessionRecord{}, fmt.Errorf("%w: failed create record cleanup", ErrOutcomeUnknown)
		}
		return nil, SessionRecord{}, keyPublishErr
	}
	index, _, snapshot, indexDegraded, rebuildErr := s.rebuildIndexLocked(ctx, profileKey, generation)
	if rebuildErr == nil {
		s.persistIndexBestEffort(ctx, profileKey, generation, index, snapshot, indexDegraded)
	} else {
		s.setIndexDegraded(true)
	}
	handle := &Handle{store: s, sessionID: sessionID, storageID: storageID, generation: generation, dataKey: dataKey, lock: lock}
	retainDataKey = true
	keepLock = true
	return handle, cloneRecord(record), nil
}

func (s *Store) OpenSession(ctx context.Context, sessionID string) (*Handle, Loaded, error) {
	if _, err := parseUUID(sessionID); err != nil {
		return nil, Loaded{}, ErrInvalid
	}
	profileLock, err := s.acquireProfileLock(ctx)
	if err != nil {
		return nil, Loaded{}, err
	}
	generation, profileKey, err := s.refreshProfileLocked()
	if err != nil {
		_ = profileLock.Close()
		return nil, Loaded{}, err
	}
	index, entries, snapshot, degraded, err := s.rebuildIndexLocked(ctx, profileKey, generation)
	if err == nil {
		s.persistIndexBestEffort(ctx, profileKey, generation, index, snapshot, degraded)
	}
	zero(profileKey)
	_ = profileLock.Close()
	if err != nil {
		return nil, Loaded{}, err
	}
	entry, ok := findIndexEntry(entries, sessionID)
	if !ok {
		return nil, Loaded{}, ErrNotFound
	}
	if entry.VersionUnsupported {
		return nil, Loaded{}, ErrVersionUnsupported
	}
	if !entry.RecordValid {
		return nil, Loaded{}, ErrCorrupt
	}
	lock, err := s.acquireSessionLock(ctx, entry.StorageID)
	if err != nil {
		return nil, Loaded{}, err
	}
	profileLock, err = s.acquireProfileLock(ctx)
	if err != nil {
		_ = lock.Close()
		return nil, Loaded{}, err
	}
	defer profileLock.Close()
	currentGeneration, currentProfileKey, err := s.refreshProfileLocked()
	if err != nil {
		_ = lock.Close()
		return nil, Loaded{}, err
	}
	defer zero(currentProfileKey)
	if currentGeneration != generation {
		_ = lock.Close()
		return nil, Loaded{}, ErrPrivacyInvalidated
	}
	envelope, dataKey, _, _, err := s.readEnvelope(entry.StorageID, currentProfileKey, generation)
	if err != nil {
		_ = lock.Close()
		return nil, Loaded{}, err
	}
	if envelope.SessionID != sessionID {
		zero(dataKey)
		_ = lock.Close()
		return nil, Loaded{}, ErrCorrupt
	}
	handle := &Handle{store: s, sessionID: sessionID, storageID: entry.StorageID, generation: generation, dataKey: dataKey, lock: lock}
	loaded, err := handle.loadLocked(ctx)
	if err != nil {
		_ = handle.Close()
		return nil, Loaded{}, err
	}
	return handle, loaded, nil
}

func (s *Store) List(ctx context.Context) ([]Summary, error) {
	profileLock, err := s.acquireProfileLock(ctx)
	if err != nil {
		return nil, err
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			_ = profileLock.Close()
		}
	}()
	generation, profileKey, err := s.refreshProfileLocked()
	if err != nil {
		return nil, err
	}
	defer zero(profileKey)
	index, entries, snapshot, degraded, err := s.rebuildIndexLocked(ctx, profileKey, generation)
	if err != nil {
		return nil, err
	}
	s.persistIndexBestEffort(ctx, profileKey, generation, index, snapshot, degraded)
	zero(profileKey)
	if err := profileLock.Close(); err != nil {
		return nil, err
	}
	lockHeld = false
	result := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		locked, lockUnavailable := s.probeSessionLock(ctx, entry.StorageID)
		result = append(result, Summary{
			SessionID: entry.SessionID, StorageID: entry.StorageID, RecordRevision: entry.RecordRevision, CheckpointRevision: entry.CheckpointRevision,
			CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt, LastOpenedAt: entry.LastOpenedAt,
			Title: entry.Title, TitleSource: entry.TitleSource, FirstUserSummary: entry.FirstUserSummary, RecentUserSummary: entry.RecentUserSummary, TitleRevision: entry.TitleRevision,
			CommittedUserTurns: entry.CommittedUserTurns, TranscriptCount: entry.TranscriptCount,
			ServerProfileFingerprint: entry.ServerProfileFingerprint, WorkspaceID: entry.WorkspaceID, WorkspaceLabel: entry.WorkspaceLabel,
			ProviderName: entry.ProviderName, ProviderEndpoint: entry.ProviderEndpoint, ProviderModel: entry.ProviderModel,
			Lifecycle: entry.Lifecycle, Corrupt: entry.Corrupt, Locked: locked, Unavailable: lockUnavailable || entry.VersionUnsupported,
			LocatorOnly: entry.LocatorOnly, VersionUnsupported: entry.VersionUnsupported,
		})
	}
	return result, nil
}

func (s *Store) probeSessionLock(ctx context.Context, storageID string) (locked, unavailable bool) {
	if _, err := parseStorageID(storageID); err != nil {
		return false, true
	}
	lock, err := filelock.Acquire(ctx, filepath.Join(s.rootPath, sessionLockName(storageID)), filelock.Exclusive, 0)
	if errors.Is(err, filelock.ErrBusy) {
		return true, false
	}
	if err != nil {
		return false, true
	}
	if err := lock.Close(); err != nil {
		return false, true
	}
	return false, false
}

func (s *Store) Delete(ctx context.Context, target DeleteTarget) error {
	if target.SessionID == "" && target.StorageID == "" {
		return ErrInvalid
	}
	if target.SessionID != "" {
		if _, err := parseUUID(target.SessionID); err != nil {
			return ErrInvalid
		}
	}
	if target.StorageID != "" {
		if _, err := parseStorageID(target.StorageID); err != nil {
			return ErrInvalid
		}
	}
	if target.ExpectedRecordRevision == 0 && target.StorageID == "" {
		return ErrInvalid
	}
	profileLock, err := s.acquireProfileLock(ctx)
	if err != nil {
		return err
	}
	generation, profileKey, err := s.refreshProfileLocked()
	if err != nil {
		_ = profileLock.Close()
		return err
	}
	index, entries, snapshot, degraded, err := s.rebuildIndexLocked(ctx, profileKey, generation)
	if err == nil {
		s.persistIndexBestEffort(ctx, profileKey, generation, index, snapshot, degraded)
	}
	zero(profileKey)
	_ = profileLock.Close()
	if err != nil {
		return err
	}
	entry, ok := findDeleteTarget(entries, target)
	if !ok {
		return ErrNotFound
	}
	if entry.VersionUnsupported {
		return ErrVersionUnsupported
	}
	if !deleteExpectationMatches(entry, target.ExpectedRecordRevision) {
		return ErrCheckpointConflict
	}
	target.SessionID, target.StorageID = entry.SessionID, entry.StorageID
	lock, err := s.acquireSessionLock(ctx, entry.StorageID)
	if err != nil {
		return err
	}
	defer lock.Close()
	profileLock, err = s.acquireProfileLock(ctx)
	if err != nil {
		return err
	}
	defer profileLock.Close()
	currentGeneration, currentKey, err := s.refreshProfileLocked()
	if err != nil {
		return err
	}
	defer zero(currentKey)
	if currentGeneration != generation {
		return ErrPrivacyInvalidated
	}
	index, entries, snapshot, degraded, err = s.rebuildIndexLocked(ctx, currentKey, generation)
	if err != nil {
		return err
	}
	entry, ok = findDeleteTarget(entries, target)
	if !ok {
		return ErrNotFound
	}
	if entry.VersionUnsupported {
		return ErrVersionUnsupported
	}
	if !deleteExpectationMatches(entry, target.ExpectedRecordRevision) {
		return ErrCheckpointConflict
	}

	envelope, dataKey, _, _, envelopeErr := s.readEnvelope(entry.StorageID, currentKey, generation)
	if errors.Is(envelopeErr, ErrVersionUnsupported) {
		return ErrVersionUnsupported
	}
	if envelopeErr == nil {
		defer zero(dataKey)
		if entry.SessionID != "" && envelope.SessionID != entry.SessionID {
			return ErrCorrupt
		}
		record, _, _, _, recordErr := s.readRecord(entry.StorageID, envelope.SessionID, dataKey, generation)
		if errors.Is(recordErr, ErrVersionUnsupported) {
			return ErrVersionUnsupported
		}
		projection, _, _, projectionErr := s.readProjection(entry.StorageID, envelope.SessionID, dataKey, generation, 0)
		if errors.Is(projectionErr, ErrVersionUnsupported) {
			return ErrVersionUnsupported
		}
		switch {
		case recordErr == nil:
			if target.ExpectedRecordRevision == 0 || record.RecordRevision != target.ExpectedRecordRevision {
				return ErrCheckpointConflict
			}
		case projectionErr == nil:
			if target.ExpectedRecordRevision == 0 || projection.RecordRevision != target.ExpectedRecordRevision {
				return ErrCheckpointConflict
			}
		case target.ExpectedRecordRevision != 0:
			return ErrCheckpointConflict
		}
		if _, _, dirtyErr := s.readDirty(entry.StorageID, envelope.SessionID, dataKey, generation); errors.Is(dirtyErr, ErrVersionUnsupported) {
			return ErrVersionUnsupported
		}
	} else if target.ExpectedRecordRevision != 0 {
		return ErrCheckpointConflict
	}

	keyCleanupFailed, keyErr := s.deleteWrappedKeyFirst(entry.StorageID)
	if keyErr != nil {
		return keyErr
	}
	cleanupFailed := keyCleanupFailed
	for _, name := range []string{recordName(entry.StorageID), indexProjectionName(entry.StorageID), dirtyName(entry.StorageID)} {
		if deleteErr := s.deleteFile(name); deleteErr != nil {
			cleanupFailed = true
		}
	}
	index.Entries = removeIndexLocator(index.Entries, entry.StorageID)
	if indexErr := s.persistIndexLocked(ctx, currentKey, generation, index, snapshot); indexErr != nil {
		degraded = true
		cleanupFailed = true
	}
	s.setIndexDegraded(degraded || cleanupFailed)
	if cleanupFailed {
		return ErrDeleteFailed
	}
	return nil
}

func (s *Store) deleteWrappedKeyFirst(storageID string) (cleanupFailed bool, err error) {
	deleteErr := s.deleteFile(keyName(storageID))
	_, readErr := s.root.ReadSnapshot(keyName(storageID), 1<<20, true)
	if errors.Is(readErr, securefile.ErrNotFound) {
		return deleteErr != nil, nil
	}
	// A readable file is unchanged; any other read result leaves the outcome
	// unknown. In both cases the wrapped DEK may still be reachable, so no
	// record, projection, dirty marker, or catalog cleanup is allowed.
	return false, ErrDeleteFailed
}

func (s *Store) Clear(ctx context.Context) error {
	profileLock, err := s.acquireProfileLock(ctx)
	if err != nil {
		return err
	}
	defer profileLock.Close()
	previousEncoded, err := s.backend.Load(s.locator, maxProfileSecretBytes)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
	}
	defer zero(previousEncoded)
	generation, currentKey, err := decodeProfileSecret(previousEncoded)
	if err != nil {
		return err
	}
	s.setProfileState(generation, currentKey)
	defer zero(currentKey)
	if generation == ^uint64(0) {
		return ErrInvalid
	}
	encoded, err := newEncodedProfileSecret(generation + 1)
	if err != nil {
		return err
	}
	newGeneration, newKey, err := decodeProfileSecret(encoded)
	if err != nil {
		zero(encoded)
		return err
	}
	replaceErr := replaceSecretVerified(s.backend, s.locator, previousEncoded, encoded)
	zero(encoded)
	if replaceErr != nil {
		zero(newKey)
		if errors.Is(replaceErr, ErrOutcomeUnknown) {
			return replaceErr
		}
		return fmt.Errorf("%w: %v", ErrKeyUnavailable, replaceErr)
	}
	// Native-secret replacement is the privacy-clear linearization point.
	// Cleanup failures remain explicit, but no old envelope can decrypt under
	// the new wrapping key/generation.
	s.setProfileState(newGeneration, newKey)
	defer zero(newKey)
	entries, _, complete, readErr := s.root.ReadDir(".", s.limits.DirectoryEntries)
	if readErr != nil || !complete {
		s.setIndexDegraded(true)
		return ErrDeleteFailed
	}
	cleanupFailed := false
	for _, entry := range entries {
		if entry.Type != securefile.EntryFile || !isSessionCleanupName(entry.Name) {
			continue
		}
		if deleteErr := s.deleteFile(entry.Name); deleteErr != nil {
			cleanupFailed = true
		}
	}
	var snapshot *securefile.Snapshot
	if current, snapshotErr := s.root.ReadSnapshot(indexName, s.limits.SessionCiphertextBytes, true); snapshotErr == nil {
		snapshot = &current
	} else if !errors.Is(snapshotErr, securefile.ErrNotFound) {
		cleanupFailed = true
	}
	empty := indexFile{SchemaVersion: indexSchemaVersion, PrivacyGeneration: newGeneration, Entries: []indexLocator{}}
	if indexErr := s.persistIndexLocked(ctx, newKey, newGeneration, empty, snapshot); indexErr != nil {
		s.setIndexDegraded(true)
		cleanupFailed = true
	} else {
		s.setIndexDegraded(false)
	}
	if cleanupFailed {
		return ErrDeleteFailed
	}
	return nil
}

func (h *Handle) Load() (Loaded, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return Loaded{}, ErrNotFound
	}
	profileLock, err := h.store.acquireProfileLock(context.Background())
	if err != nil {
		return Loaded{}, err
	}
	defer profileLock.Close()
	generation, key, err := h.store.refreshProfileLocked()
	zero(key)
	if err != nil {
		return Loaded{}, err
	}
	if generation != h.generation {
		return Loaded{}, ErrPrivacyInvalidated
	}
	return h.loadLocked(context.Background())
}

func (h *Handle) Save(ctx context.Context, expectedRevision uint64, candidate SessionRecord) (SessionRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return SessionRecord{}, ErrNotFound
	}
	if expectedRevision == 0 {
		return SessionRecord{}, ErrInvalid
	}
	profileLock, err := h.store.acquireProfileLock(ctx)
	if err != nil {
		return SessionRecord{}, err
	}
	defer profileLock.Close()
	generation, profileKey, err := h.store.refreshProfileLocked()
	if err != nil {
		return SessionRecord{}, err
	}
	defer zero(profileKey)
	if generation != h.generation {
		return SessionRecord{}, ErrPrivacyInvalidated
	}
	current, raw, _, _, err := h.store.readRecord(h.storageID, h.sessionID, h.dataKey, h.generation)
	if err != nil {
		return SessionRecord{}, err
	}
	if current.RecordRevision != expectedRevision {
		return SessionRecord{}, ErrCheckpointConflict
	}
	commitID, _, err := randomUUID()
	if err != nil {
		return SessionRecord{}, err
	}
	candidate.CommitID = commitID
	candidate.SchemaVersion = recordPayloadSchemaVersion
	candidate.SessionID, candidate.StorageID = h.sessionID, h.storageID
	candidate.PrivacyGeneration = h.generation
	candidate.RecordRevision = expectedRevision + 1
	candidate.CreatedAt = current.CreatedAt
	candidate.ServerProfileFingerprint = current.ServerProfileFingerprint
	candidate.UpdatedAt = h.store.now().UTC()
	if candidate.LastOpenedAt.IsZero() {
		candidate.LastOpenedAt = current.LastOpenedAt
	}
	candidate.CheckpointRevision = current.CheckpointRevision
	if !bytes.Equal(candidate.Checkpoint, current.Checkpoint) {
		candidate.CheckpointRevision++
	}
	candidate.Checkpoint = append(json.RawMessage(nil), candidate.Checkpoint...)
	candidate.QuarantinedCheckpoint = append(json.RawMessage(nil), candidate.QuarantinedCheckpoint...)
	candidate.Transcript, err = canonicalTranscriptBlob(candidate.Transcript, h.store.limits)
	if err != nil {
		return SessionRecord{}, err
	}
	transcriptValue, err := DecodeTranscript(candidate.Transcript, h.store.limits)
	if err != nil {
		return SessionRecord{}, err
	}
	candidate.TranscriptCount = uint64(len(transcriptValue.Entries))
	if int64(len(candidate.Checkpoint)) > h.store.limits.SessionPlaintextBytes || int64(len(candidate.QuarantinedCheckpoint)) > h.store.limits.SessionPlaintextBytes {
		return SessionRecord{}, ErrStoreFull
	}
	if err := validateRecord(candidate, h.store.limits); err != nil {
		return SessionRecord{}, err
	}
	if candidate.LastConsumedDirtyID != "" {
		marker, _, markerErr := h.store.readDirty(h.storageID, h.sessionID, h.dataKey, h.generation)
		if markerErr != nil || marker.DirtyID != candidate.LastConsumedDirtyID || marker.BaseRevision != expectedRevision {
			return SessionRecord{}, ErrCorrupt
		}
	}
	plain, err := encodeStrict(candidate)
	if err != nil {
		return SessionRecord{}, err
	}
	if int64(len(plain)) > h.store.limits.SessionPlaintextBytes {
		return SessionRecord{}, ErrStoreFull
	}
	sessionBytes, _ := parseUUID(h.sessionID)
	storageBytes, _ := parseStorageID(h.storageID)
	ciphertext, err := sealContainer(h.dataKey, containerHeader{
		SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: h.store.profile,
		Generation: h.generation, Session: sessionBytes, Storage: storageBytes, Revision: candidate.RecordRevision,
	}, plain)
	if err != nil {
		return SessionRecord{}, err
	}
	if int64(len(ciphertext)) > h.store.limits.SessionCiphertextBytes {
		return SessionRecord{}, ErrStoreFull
	}
	currentSessionBytes, err := h.store.sessionCiphertextBytesLocked(h.storageID)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := h.store.checkQuotaLocked(int64(len(ciphertext)-len(raw)), currentSessionBytes-int64(len(raw))+int64(len(ciphertext))); err != nil {
		return SessionRecord{}, err
	}
	publishErr := h.store.publishReplace(ctx, recordName(h.storageID), ciphertext, raw)
	if errors.Is(publishErr, ErrOutcomeUnknown) {
		observed, _, _, _, observeErr := h.store.readRecord(h.storageID, h.sessionID, h.dataKey, h.generation)
		switch {
		case observeErr == nil && observed.RecordRevision == candidate.RecordRevision && recordsEqual(observed, candidate):
			publishErr = nil
		case observeErr == nil && observed.RecordRevision == expectedRevision:
			return SessionRecord{}, ErrCheckpointSaveFailed
		default:
			return SessionRecord{}, ErrOutcomeUnknown
		}
	}
	if publishErr != nil {
		return SessionRecord{}, publishErr
	}
	if candidate.LastConsumedDirtyID != "" {
		_ = h.store.deleteFile(dirtyName(h.storageID))
	}
	index, _, snapshot, degraded, rebuildErr := h.store.rebuildIndexLocked(ctx, profileKey, h.generation)
	if rebuildErr == nil {
		h.store.persistIndexBestEffort(ctx, profileKey, h.generation, index, snapshot, degraded)
	} else {
		h.store.setIndexDegraded(true)
	}
	return cloneRecord(candidate), nil
}

func (h *Handle) MarkDirty(ctx context.Context, expectedRevision, turnSequence uint64, operationClass string, mayHaveSideEffect bool) (DirtyMarker, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	operationClass = strings.TrimSpace(operationClass)
	if h.closed || expectedRevision == 0 || turnSequence == 0 || !validStableToken(operationClass, 128) || mayHaveSideEffect {
		return DirtyMarker{}, ErrInvalid
	}
	profileLock, err := h.store.acquireProfileLock(ctx)
	if err != nil {
		return DirtyMarker{}, err
	}
	defer profileLock.Close()
	generation, profileKey, err := h.store.refreshProfileLocked()
	zero(profileKey)
	if err != nil {
		return DirtyMarker{}, err
	}
	if generation != h.generation {
		return DirtyMarker{}, ErrPrivacyInvalidated
	}
	record, _, _, _, err := h.store.readRecord(h.storageID, h.sessionID, h.dataKey, h.generation)
	if err != nil {
		return DirtyMarker{}, err
	}
	if record.RecordRevision != expectedRevision {
		return DirtyMarker{}, ErrCheckpointConflict
	}
	if existing, _, existingErr := h.store.readDirty(h.storageID, h.sessionID, h.dataKey, h.generation); existingErr == nil {
		return existing, ErrCheckpointConflict
	} else if !errors.Is(existingErr, ErrNotFound) {
		return DirtyMarker{}, existingErr
	}
	dirtyID, _, err := randomUUID()
	if err != nil {
		return DirtyMarker{}, err
	}
	marker := DirtyMarker{
		SchemaVersion: dirtySchemaVersion, DirtyID: dirtyID, SessionID: h.sessionID, StorageID: h.storageID,
		BaseRevision: expectedRevision, TurnSequence: turnSequence, OperationClass: operationClass,
		MayHaveSideEffect: false, StartedAt: h.store.now().UTC(),
	}
	if err := validateDirtyMarker(marker); err != nil {
		return DirtyMarker{}, err
	}
	plain, err := encodeStrict(marker)
	if err != nil {
		return DirtyMarker{}, err
	}
	if int64(len(plain)) > h.store.limits.DirtyMarkerBytes {
		return DirtyMarker{}, ErrStoreFull
	}
	sessionBytes, _ := parseUUID(h.sessionID)
	storageBytes, _ := parseStorageID(h.storageID)
	dirtyRevision, err := randomRevision()
	if err != nil {
		return DirtyMarker{}, err
	}
	ciphertext, err := sealContainer(h.dataKey, containerHeader{
		SchemaVersion: dirtySchemaVersion, Kind: kindDirty, Profile: h.store.profile,
		Generation: h.generation, Session: sessionBytes, Storage: storageBytes, Revision: dirtyRevision,
	}, plain)
	if err != nil {
		return DirtyMarker{}, err
	}
	currentSessionBytes, err := h.store.sessionCiphertextBytesLocked(h.storageID)
	if err != nil {
		return DirtyMarker{}, err
	}
	if err := h.store.checkQuotaLocked(int64(len(ciphertext)), currentSessionBytes+int64(len(ciphertext))); err != nil {
		return DirtyMarker{}, err
	}
	publishErr := h.store.publishCreate(ctx, dirtyName(h.storageID), ciphertext)
	if errors.Is(publishErr, ErrOutcomeUnknown) {
		observed, _, observeErr := h.store.readDirty(h.storageID, h.sessionID, h.dataKey, h.generation)
		switch {
		case observeErr == nil && reflect.DeepEqual(observed, marker):
			publishErr = nil
		case errors.Is(observeErr, ErrNotFound):
			return DirtyMarker{}, ErrCheckpointSaveFailed
		default:
			return DirtyMarker{}, ErrOutcomeUnknown
		}
	}
	if publishErr != nil {
		return DirtyMarker{}, publishErr
	}
	return marker, nil
}

func (h *Handle) UpdateDirty(ctx context.Context, candidate DirtyMarker) (DirtyMarker, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return DirtyMarker{}, ErrNotFound
	}
	profileLock, err := h.store.acquireProfileLock(ctx)
	if err != nil {
		return DirtyMarker{}, err
	}
	defer profileLock.Close()
	generation, profileKey, err := h.store.refreshProfileLocked()
	zero(profileKey)
	if err != nil {
		return DirtyMarker{}, err
	}
	if generation != h.generation {
		return DirtyMarker{}, ErrPrivacyInvalidated
	}
	current, raw, err := h.store.readDirty(h.storageID, h.sessionID, h.dataKey, h.generation)
	if err != nil {
		return DirtyMarker{}, err
	}
	if candidate.SchemaVersion != current.SchemaVersion || candidate.DirtyID != current.DirtyID || candidate.SessionID != current.SessionID ||
		candidate.StorageID != current.StorageID || candidate.BaseRevision != current.BaseRevision || candidate.TurnSequence != current.TurnSequence ||
		candidate.OperationClass != current.OperationClass || !candidate.StartedAt.Equal(current.StartedAt) ||
		current.MayHaveSideEffect && !candidate.MayHaveSideEffect || validateDirtyMarker(candidate) != nil {
		return DirtyMarker{}, ErrInvalid
	}
	plain, err := encodeStrict(candidate)
	if err != nil || int64(len(plain)) > h.store.limits.DirtyMarkerBytes {
		return DirtyMarker{}, ErrStoreFull
	}
	sessionBytes, _ := parseUUID(h.sessionID)
	storageBytes, _ := parseStorageID(h.storageID)
	dirtyRevision, err := randomRevision()
	if err != nil {
		return DirtyMarker{}, err
	}
	ciphertext, err := sealContainer(h.dataKey, containerHeader{
		SchemaVersion: dirtySchemaVersion, Kind: kindDirty, Profile: h.store.profile,
		Generation: h.generation, Session: sessionBytes, Storage: storageBytes, Revision: dirtyRevision,
	}, plain)
	if err != nil {
		return DirtyMarker{}, err
	}
	currentSessionBytes, err := h.store.sessionCiphertextBytesLocked(h.storageID)
	if err != nil {
		return DirtyMarker{}, err
	}
	if err := h.store.checkQuotaLocked(int64(len(ciphertext)-len(raw)), currentSessionBytes-int64(len(raw))+int64(len(ciphertext))); err != nil {
		return DirtyMarker{}, err
	}
	publishErr := h.store.publishReplace(ctx, dirtyName(h.storageID), ciphertext, raw)
	if errors.Is(publishErr, ErrOutcomeUnknown) {
		observed, _, observeErr := h.store.readDirty(h.storageID, h.sessionID, h.dataKey, h.generation)
		switch {
		case observeErr == nil && reflect.DeepEqual(observed, candidate):
			publishErr = nil
		case observeErr == nil && reflect.DeepEqual(observed, current):
			return DirtyMarker{}, ErrCheckpointSaveFailed
		default:
			return DirtyMarker{}, ErrOutcomeUnknown
		}
	}
	if publishErr != nil {
		return DirtyMarker{}, publishErr
	}
	return candidate, nil
}

func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	zero(h.dataKey)
	h.dataKey = nil
	if h.lock != nil {
		err := h.lock.Close()
		h.lock = nil
		return err
	}
	return nil
}

func (h *Handle) publishRecordMigration(ctx context.Context, record SessionRecord, originalRaw []byte) error {
	plain, err := encodeStrict(record)
	if err != nil {
		return err
	}
	if int64(len(plain)) > h.store.limits.SessionPlaintextBytes {
		return ErrStoreFull
	}
	sessionBytes, _ := parseUUID(h.sessionID)
	storageBytes, _ := parseStorageID(h.storageID)
	ciphertext, err := sealContainer(h.dataKey, containerHeader{
		SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: h.store.profile,
		Generation: h.generation, Session: sessionBytes, Storage: storageBytes, Revision: record.RecordRevision,
	}, plain)
	if err != nil {
		return err
	}
	if int64(len(ciphertext)) > h.store.limits.SessionCiphertextBytes {
		return ErrStoreFull
	}
	currentSessionBytes, err := h.store.sessionCiphertextBytesLocked(h.storageID)
	if err != nil {
		return err
	}
	if err := h.store.checkQuotaLocked(int64(len(ciphertext)-len(originalRaw)), currentSessionBytes-int64(len(originalRaw))+int64(len(ciphertext))); err != nil {
		return err
	}
	publishErr := h.store.publishReplace(ctx, recordName(h.storageID), ciphertext, originalRaw)
	if errors.Is(publishErr, ErrOutcomeUnknown) {
		observed, observedRaw, _, observedPending, observeErr := h.store.readRecord(h.storageID, h.sessionID, h.dataKey, h.generation)
		switch {
		case observeErr == nil && !observedPending && recordsEqual(observed, record):
			publishErr = nil
		case observeErr == nil && observedPending && bytes.Equal(observedRaw, originalRaw) && recordsEqual(observed, record):
			return ErrCheckpointSaveFailed
		default:
			return ErrOutcomeUnknown
		}
	}
	if publishErr != nil {
		return fmt.Errorf("%w: record migration publication: %v", ErrCheckpointSaveFailed, publishErr)
	}
	h.refreshProjectionAfterMigrationBestEffort(ctx, record)
	return nil
}

func (h *Handle) refreshProjectionAfterMigrationBestEffort(ctx context.Context, record SessionRecord) {
	desired := projectionFromRecord(record)
	observed, snapshot, _, err := h.store.readProjection(h.storageID, h.sessionID, h.dataKey, h.generation, record.RecordRevision)
	if err == nil && projectionPayloadEqual(observed, desired) {
		return
	}
	if errors.Is(err, ErrVersionUnsupported) {
		return
	}
	if persistErr := h.store.persistProjectionLocked(ctx, h.dataKey, h.generation, desired, snapshot); persistErr != nil {
		h.store.setIndexDegraded(true)
	}
}

func (h *Handle) loadLocked(ctx context.Context) (Loaded, error) {
	record, raw, _, migrationPending, err := h.store.readRecord(h.storageID, h.sessionID, h.dataKey, h.generation)
	if err != nil {
		return Loaded{}, err
	}
	if migrationPending {
		if err := h.publishRecordMigration(ctx, record, raw); err != nil {
			return Loaded{}, err
		}
	}
	marker, _, err := h.store.readDirty(h.storageID, h.sessionID, h.dataKey, h.generation)
	if errors.Is(err, ErrNotFound) {
		return Loaded{Record: cloneRecord(record)}, nil
	}
	if err != nil {
		return Loaded{}, err
	}
	if marker.DirtyID == record.LastConsumedDirtyID {
		_ = h.store.deleteFile(dirtyName(h.storageID))
		return Loaded{Record: cloneRecord(record)}, nil
	}
	if marker.BaseRevision != record.RecordRevision {
		return Loaded{}, ErrCorrupt
	}
	copyMarker := marker
	return Loaded{Record: cloneRecord(record), Interrupted: &copyMarker}, nil
}

func (s *Store) rebuildIndexLocked(ctx context.Context, profileKey []byte, generation uint64) (indexFile, []indexProjection, *securefile.Snapshot, bool, error) {
	index := indexFile{SchemaVersion: indexSchemaVersion, PrivacyGeneration: generation, Entries: []indexLocator{}}
	var snapshot *securefile.Snapshot
	priorByStorage := make(map[string]indexLocator)
	if current, err := s.root.ReadSnapshot(indexName, s.limits.SessionCiphertextBytes, true); err == nil {
		snapshot = &current
		plain, header, openErr := openContainer(profileKey, current.Data, containerExpectation{
			SchemaVersion: indexSchemaVersion, Kind: kindIndex, Profile: s.profile, Generation: generation, MaxPayload: s.limits.SessionPlaintextBytes,
		})
		if errors.Is(openErr, ErrVersionUnsupported) {
			return index, nil, snapshot, false, ErrVersionUnsupported
		}
		if openErr == nil {
			var existing indexFile
			decodeErr := decodeStrict(plain, &existing, s.limits.SessionPlaintextBytes)
			validationErr := error(nil)
			if decodeErr == nil {
				validationErr = validateIndex(existing, generation)
			}
			if errors.Is(validationErr, ErrVersionUnsupported) {
				return index, nil, snapshot, false, ErrVersionUnsupported
			}
			if decodeErr == nil && validationErr == nil && existing.IndexRevision == header.Revision {
				index.IndexRevision = existing.IndexRevision
				index.Entries = append([]indexLocator(nil), existing.Entries...)
				for _, entry := range existing.Entries {
					priorByStorage[entry.StorageID] = entry
				}
			}
		}
	} else if !errors.Is(err, securefile.ErrNotFound) {
		return index, nil, nil, false, err
	}

	directoryEntries, _, complete, err := s.root.ReadDir(".", s.limits.DirectoryEntries)
	if err != nil {
		return index, nil, snapshot, false, err
	}
	if !complete {
		return index, nil, snapshot, false, ErrStoreFull
	}
	storageIDs := make([]string, 0)
	for _, entry := range directoryEntries {
		if storageID, ok := storageFromKeyName(entry.Name); entry.Type == securefile.EntryFile && ok {
			storageIDs = append(storageIDs, storageID)
		}
	}
	sort.Strings(storageIDs)

	rebuilt := make([]indexProjection, 0, len(storageIDs))
	locators := make([]indexLocator, 0, len(storageIDs))
	seenSession := make(map[string]struct{})
	degraded := false
	appendEntry := func(entry indexProjection, trustedSession bool) {
		if entry.SessionID != "" {
			if _, duplicate := seenSession[entry.SessionID]; duplicate {
				entry.SessionID = ""
				entry.Corrupt = true
				entry.LocatorOnly = true
				trustedSession = false
			} else {
				seenSession[entry.SessionID] = struct{}{}
			}
		}
		if trustedSession && entry.SessionID != "" {
			locators = append(locators, indexLocator{SessionID: entry.SessionID, StorageID: entry.StorageID})
		}
		rebuilt = append(rebuilt, entry)
	}

	for _, storageID := range storageIDs {
		prior, hasPrior := priorByStorage[storageID]
		envelope, dataKey, _, envelopeHeader, envelopeErr := s.readEnvelope(storageID, profileKey, generation)
		if errors.Is(envelopeErr, ErrPrivacyInvalidated) || errors.Is(envelopeErr, ErrNotFound) {
			continue
		}
		if errors.Is(envelopeErr, ErrVersionUnsupported) {
			sessionID := ""
			if hasPrior {
				sessionID = prior.SessionID
			} else if authenticatedSessionID, ok := authenticatedHeaderSessionID(envelopeHeader); ok {
				sessionID = authenticatedSessionID
			}
			appendEntry(indexProjection{SessionID: sessionID, StorageID: storageID, VersionUnsupported: true, LocatorOnly: sessionID == ""}, sessionID != "")
			continue
		}
		if envelopeErr != nil {
			sessionID := ""
			if hasPrior {
				sessionID = prior.SessionID
			}
			appendEntry(indexProjection{SessionID: sessionID, StorageID: storageID, Corrupt: true, LocatorOnly: sessionID == ""}, sessionID != "")
			continue
		}

		record, _, _, _, recordErr := s.readRecord(storageID, envelope.SessionID, dataKey, generation)
		if errors.Is(recordErr, ErrVersionUnsupported) {
			zero(dataKey)
			appendEntry(indexProjection{SessionID: envelope.SessionID, StorageID: storageID, EnvelopeValid: true, VersionUnsupported: true}, true)
			continue
		}
		if recordErr == nil {
			desired := projectionFromRecord(record)
			desired.EnvelopeValid = true
			desired.RecordValid = true
			observed, projectionSnapshot, _, projectionErr := s.readProjection(storageID, envelope.SessionID, dataKey, generation, 0)
			switch {
			case errors.Is(projectionErr, ErrVersionUnsupported):
				desired.VersionUnsupported = true
			case projectionErr == nil && projectionPayloadEqual(observed, desired):
				desired.ProjectionValid = true
			default:
				if persistErr := s.persistProjectionLocked(ctx, dataKey, generation, desired, projectionSnapshot); persistErr != nil {
					degraded = true
				} else {
					desired.ProjectionValid = true
				}
			}
			zero(dataKey)
			appendEntry(desired, true)
			continue
		}

		projection, _, _, projectionErr := s.readProjection(storageID, envelope.SessionID, dataKey, generation, 0)
		zero(dataKey)
		switch {
		case errors.Is(projectionErr, ErrVersionUnsupported):
			appendEntry(indexProjection{SessionID: envelope.SessionID, StorageID: storageID, EnvelopeValid: true, VersionUnsupported: true}, true)
		case projectionErr == nil:
			projection.Corrupt = true
			projection.EnvelopeValid = true
			projection.ProjectionValid = true
			appendEntry(projection, true)
		default:
			appendEntry(indexProjection{SessionID: envelope.SessionID, StorageID: storageID, EnvelopeValid: true, Corrupt: true}, true)
		}
	}

	sortIndexEntries(rebuilt)
	sortIndexLocators(locators)
	index.Entries = locators
	return index, rebuilt, snapshot, degraded, nil
}

func authenticatedHeaderSessionID(header containerHeader) (string, bool) {
	if header.Session[6]>>4 != 4 || header.Session[8]&0xc0 != 0x80 {
		return "", false
	}
	return formatUUID(header.Session), true
}

func (s *Store) persistIndexLocked(ctx context.Context, profileKey []byte, generation uint64, index indexFile, snapshot *securefile.Snapshot) error {
	var existing indexFile
	validExisting := false
	if snapshot != nil {
		plain, header, openErr := openContainer(profileKey, snapshot.Data, containerExpectation{SchemaVersion: indexSchemaVersion, Kind: kindIndex, Profile: s.profile, Generation: generation, MaxPayload: s.limits.SessionPlaintextBytes})
		if errors.Is(openErr, ErrVersionUnsupported) {
			return openErr
		}
		if openErr == nil {
			decodeErr := decodeStrict(plain, &existing, s.limits.SessionPlaintextBytes)
			validationErr := error(nil)
			if decodeErr == nil {
				validationErr = validateIndex(existing, generation)
			}
			if errors.Is(validationErr, ErrVersionUnsupported) {
				return validationErr
			}
			if decodeErr == nil && validationErr == nil && existing.IndexRevision == header.Revision {
				validExisting = true
			}
		}
	}
	if validExisting && reflect.DeepEqual(existing.Entries, index.Entries) {
		return nil
	}
	revision := index.IndexRevision + 1
	if index.IndexRevision == 0 || revision == 0 {
		var revisionErr error
		revision, revisionErr = randomRevision()
		if revisionErr != nil {
			return revisionErr
		}
	}
	index.SchemaVersion = indexSchemaVersion
	index.PrivacyGeneration = generation
	index.IndexRevision = revision
	plain, err := encodeStrict(index)
	if err != nil {
		return err
	}
	ciphertext, err := sealContainer(profileKey, containerHeader{
		SchemaVersion: indexSchemaVersion, Kind: kindIndex, Profile: s.profile, Generation: generation, Revision: revision,
	}, plain)
	if err != nil {
		return err
	}
	if snapshot == nil {
		if err := s.checkQuotaLocked(int64(len(ciphertext)), int64(len(ciphertext))); err != nil {
			return err
		}
		return s.publishCreate(ctx, indexName, ciphertext)
	}
	if err := s.checkQuotaLocked(int64(len(ciphertext)-len(snapshot.Data)), int64(len(ciphertext))); err != nil {
		return err
	}
	return s.publishReplace(ctx, indexName, ciphertext, snapshot.Data)
}

func (s *Store) persistProjectionLocked(ctx context.Context, dataKey []byte, generation uint64, value indexProjection, snapshot *securefile.Snapshot) error {
	if err := validateProjection(value, generation); err != nil {
		return err
	}
	sessionBytes, err := parseUUID(value.SessionID)
	if err != nil {
		return ErrInvalid
	}
	storageBytes, err := parseStorageID(value.StorageID)
	if err != nil {
		return ErrInvalid
	}
	plain, err := encodeStrict(value)
	if err != nil || int64(len(plain)) > maxProjectionBytes {
		return ErrStoreFull
	}
	ciphertext, err := sealContainer(dataKey, containerHeader{
		SchemaVersion: projectionSchemaVersion, Kind: kindProjection, Profile: s.profile, Generation: generation,
		Session: sessionBytes, Storage: storageBytes, Revision: value.RecordRevision,
	}, plain)
	if err != nil {
		return err
	}
	currentSessionBytes, err := s.sessionCiphertextBytesLocked(value.StorageID)
	if err != nil {
		return err
	}
	profileDelta := int64(len(ciphertext))
	nextSessionBytes := currentSessionBytes + int64(len(ciphertext))
	if snapshot != nil {
		profileDelta -= int64(len(snapshot.Data))
		nextSessionBytes -= int64(len(snapshot.Data))
	}
	if err := s.checkQuotaLocked(profileDelta, nextSessionBytes); err != nil {
		return err
	}
	var publishErr error
	if snapshot == nil {
		publishErr = s.publishCreate(ctx, indexProjectionName(value.StorageID), ciphertext)
	} else {
		publishErr = s.publishReplace(ctx, indexProjectionName(value.StorageID), ciphertext, snapshot.Data)
	}
	if errors.Is(publishErr, ErrOutcomeUnknown) {
		observed, _, _, observeErr := s.readProjection(value.StorageID, value.SessionID, dataKey, generation, value.RecordRevision)
		if observeErr == nil && projectionPayloadEqual(observed, value) {
			return nil
		}
	}
	return publishErr
}

func (s *Store) readEnvelope(storageID string, profileKey []byte, generation uint64) (keyEnvelope, []byte, []byte, containerHeader, error) {
	var envelope keyEnvelope
	storageBytes, err := parseStorageID(storageID)
	if err != nil {
		return envelope, nil, nil, containerHeader{}, ErrInvalid
	}
	raw, err := s.root.ReadLimit(keyName(storageID), 1<<20, true)
	if errors.Is(err, securefile.ErrNotFound) {
		return envelope, nil, nil, containerHeader{}, ErrNotFound
	}
	if err != nil {
		return envelope, nil, nil, containerHeader{}, err
	}
	plain, header, err := openContainer(profileKey, raw, containerExpectation{
		SchemaVersion: envelopeSchemaVersion, Kind: kindEnvelope, Profile: s.profile, Generation: generation, Storage: storageBytes, MaxPayload: 64 << 10,
	})
	if err != nil {
		return envelope, nil, raw, header, err
	}
	if err := decodeStrict(plain, &envelope, 64<<10); err != nil {
		return envelope, nil, raw, header, err
	}
	if err := persistedSchemaError(envelope.SchemaVersion, envelopeSchemaVersion); err != nil {
		return envelope, nil, raw, header, err
	}
	if envelope.StorageID != storageID || envelope.PrivacyGeneration != generation || header.Revision != 1 {
		return envelope, nil, raw, header, ErrCorrupt
	}
	sessionBytes, err := parseUUID(envelope.SessionID)
	if err != nil || sessionBytes != header.Session {
		return envelope, nil, raw, header, ErrCorrupt
	}
	dataKey, err := base64.RawStdEncoding.Strict().DecodeString(envelope.DataKey)
	if err != nil || len(dataKey) != 32 || base64.RawStdEncoding.EncodeToString(dataKey) != envelope.DataKey {
		zero(dataKey)
		return envelope, nil, raw, header, ErrCorrupt
	}
	return envelope, dataKey, raw, header, nil
}

func (s *Store) readProjection(storageID, sessionID string, dataKey []byte, generation, expectedRevision uint64) (indexProjection, *securefile.Snapshot, containerHeader, error) {
	var value indexProjection
	sessionBytes, err := parseUUID(sessionID)
	if err != nil {
		return value, nil, containerHeader{}, ErrInvalid
	}
	storageBytes, err := parseStorageID(storageID)
	if err != nil {
		return value, nil, containerHeader{}, ErrInvalid
	}
	snapshot, err := s.root.ReadSnapshot(indexProjectionName(storageID), maxProjectionBytes+containerHeaderSize+32, true)
	if errors.Is(err, securefile.ErrNotFound) {
		return value, nil, containerHeader{}, ErrNotFound
	}
	if err != nil {
		return value, nil, containerHeader{}, err
	}
	plain, header, err := openContainer(dataKey, snapshot.Data, containerExpectation{
		SchemaVersion: projectionSchemaVersion, Kind: kindProjection, Profile: s.profile, Generation: generation,
		Session: sessionBytes, Storage: storageBytes, Revision: expectedRevision, MaxPayload: maxProjectionBytes,
	})
	if err != nil {
		return value, &snapshot, header, err
	}
	if err := decodeStrict(plain, &value, maxProjectionBytes); err != nil {
		return value, &snapshot, header, err
	}
	if err := validateProjection(value, generation); err != nil {
		return value, &snapshot, header, err
	}
	if value.SessionID != sessionID || value.StorageID != storageID || value.RecordRevision != header.Revision ||
		value.ServerProfileFingerprint != hex.EncodeToString(s.profile[:]) {
		return value, &snapshot, header, ErrCorrupt
	}
	return value, &snapshot, header, nil
}

func (s *Store) readRecord(storageID, sessionID string, dataKey []byte, generation uint64) (SessionRecord, []byte, containerHeader, bool, error) {
	var record SessionRecord
	sessionBytes, err := parseUUID(sessionID)
	if err != nil {
		return record, nil, containerHeader{}, false, ErrInvalid
	}
	storageBytes, err := parseStorageID(storageID)
	if err != nil {
		return record, nil, containerHeader{}, false, ErrInvalid
	}
	raw, err := s.root.ReadLimit(recordName(storageID), s.limits.SessionCiphertextBytes, true)
	if errors.Is(err, securefile.ErrNotFound) {
		return record, nil, containerHeader{}, false, ErrNotFound
	}
	if err != nil {
		return record, nil, containerHeader{}, false, err
	}
	plain, header, err := openContainer(dataKey, raw, containerExpectation{
		SchemaVersion: recordContainerSchemaVersion, Kind: kindRecord, Profile: s.profile, Generation: generation, Session: sessionBytes, Storage: storageBytes,
		MaxPayload: s.limits.SessionPlaintextBytes,
	})
	if err != nil {
		return record, raw, header, false, err
	}
	decoded, sourceVersion, err := decodeRecordPayload(plain, s.limits.SessionPlaintextBytes)
	if err != nil {
		return record, raw, header, false, err
	}
	record = decoded
	if err := validateRecord(record, s.limits); err != nil {
		if errors.Is(err, ErrInvalid) {
			err = ErrCorrupt
		}
		return record, raw, header, false, err
	}
	if record.SessionID != sessionID || record.StorageID != storageID || record.PrivacyGeneration != generation || record.RecordRevision != header.Revision ||
		record.ServerProfileFingerprint != hex.EncodeToString(s.profile[:]) {
		return record, raw, header, false, ErrCorrupt
	}
	return record, raw, header, sourceVersion != recordPayloadSchemaVersion, nil
}

func (s *Store) readDirty(storageID, sessionID string, dataKey []byte, generation uint64) (DirtyMarker, []byte, error) {
	var marker DirtyMarker
	sessionBytes, err := parseUUID(sessionID)
	if err != nil {
		return marker, nil, ErrInvalid
	}
	storageBytes, err := parseStorageID(storageID)
	if err != nil {
		return marker, nil, ErrInvalid
	}
	raw, err := s.root.ReadLimit(dirtyName(storageID), s.limits.DirtyMarkerBytes+containerHeaderSize+32, true)
	if errors.Is(err, securefile.ErrNotFound) {
		return marker, nil, ErrNotFound
	}
	if err != nil {
		return marker, nil, err
	}
	plain, header, err := openContainer(dataKey, raw, containerExpectation{
		SchemaVersion: dirtySchemaVersion, Kind: kindDirty, Profile: s.profile, Generation: generation, Session: sessionBytes, Storage: storageBytes,
		MaxPayload: s.limits.DirtyMarkerBytes,
	})
	if err != nil {
		return marker, raw, err
	}
	if err := decodeStrict(plain, &marker, s.limits.DirtyMarkerBytes); err != nil {
		return marker, raw, err
	}
	if err := persistedSchemaError(marker.SchemaVersion, dirtySchemaVersion); err != nil {
		return marker, raw, err
	}
	if marker.SessionID != sessionID || marker.StorageID != storageID || header.Revision == 0 || validateDirtyMarker(marker) != nil {
		return marker, raw, ErrCorrupt
	}
	return marker, raw, nil
}

func (s *Store) refreshProfileLocked() (uint64, []byte, error) {
	encoded, err := s.backend.Load(s.locator, maxProfileSecretBytes)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
	}
	generation, key, err := decodeProfileSecret(encoded)
	zero(encoded)
	if err != nil {
		return 0, nil, err
	}
	s.setProfileState(generation, key)
	result := append([]byte(nil), key...)
	zero(key)
	return generation, result, nil
}

func (s *Store) Limits() Limits {
	if s == nil {
		return DefaultLimits()
	}
	return s.limits
}

func (s *Store) IndexDegraded() bool {
	if s == nil {
		return false
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.indexDegraded
}

func (s *Store) setIndexDegraded(degraded bool) {
	s.stateMu.Lock()
	s.indexDegraded = degraded
	s.stateMu.Unlock()
}

func (s *Store) persistIndexBestEffort(ctx context.Context, profileKey []byte, generation uint64, index indexFile, snapshot *securefile.Snapshot, degraded bool) {
	if s.persistIndexLocked(ctx, profileKey, generation, index, snapshot) != nil {
		degraded = true
	}
	s.setIndexDegraded(degraded)
}

func (s *Store) setProfileState(generation uint64, key []byte) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	zero(s.profileKey)
	s.profileKey = append([]byte(nil), key...)
	s.generation = generation
}

func (s *Store) acquireProfileLock(ctx context.Context) (*filelock.Lock, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.rootPath, profileLockName)
	lock, err := filelock.Acquire(ctx, path, filelock.Exclusive, s.lockWait)
	if errors.Is(err, filelock.ErrBusy) {
		return nil, ErrInUse
	}
	if err != nil {
		return nil, err
	}
	if err := enforcePrivateSessionFile(path); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func (s *Store) acquireSessionLock(ctx context.Context, storageID string) (*filelock.Lock, error) {
	if _, err := parseStorageID(storageID); err != nil {
		return nil, ErrInvalid
	}
	path := filepath.Join(s.rootPath, sessionLockName(storageID))
	lock, err := filelock.Acquire(ctx, path, filelock.Exclusive, s.lockWait)
	if errors.Is(err, filelock.ErrBusy) {
		return nil, ErrInUse
	}
	if err != nil {
		return nil, err
	}
	if err := enforcePrivateSessionFile(path); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func (s *Store) ensureOpen() error {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.closed || s.root == nil {
		return ErrNotFound
	}
	return nil
}

func (s *Store) checkQuotaLocked(profileDelta, sessionBytes int64) error {
	if sessionBytes > s.limits.SessionCiphertextBytes {
		return ErrStoreFull
	}
	total, err := s.profileCiphertextBytesLocked()
	if err != nil {
		return err
	}
	if profileDelta > 0 && total > s.limits.ProfileCiphertextBytes-profileDelta {
		return ErrStoreFull
	}
	return nil
}
