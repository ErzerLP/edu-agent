package agentsession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/filelock"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"golang.org/x/crypto/hkdf"
)

// MaxArtifactBytes bounds one plaintext blob. Callers own chunking and metadata;
// artifacts never enter the conversation record, transcript, or dirty marker.
const MaxArtifactBytes = 512 << 10

const (
	artifactContainerSchemaVersion = 1
	maxArtifactNameBytes           = 160
	maxArtifactCiphertextBytes     = MaxArtifactBytes + containerHeaderSize + 16
)

// ReadArtifact returns an authenticated, session-owned blob. The caller owns
// the returned plaintext and its lifetime. Missing blobs return ErrNotFound.
func (h *Handle) ReadArtifact(ctx context.Context, name string) ([]byte, error) {
	if h == nil {
		return nil, ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrNotFound
	}
	if !validArtifactName(name, false) {
		return nil, ErrInvalid
	}
	ctx = artifactContext(ctx)
	lock, err := h.lockArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	plain, _, _, err := h.readArtifactLocked(name)
	return plain, artifactError(err)
}

// CheckArtifactAccess checks the handle lifetime, private root and profile
// generation fence without requiring a saved blob or scanning the directory.
// Consumers retaining plaintext must perform this check before returning it,
// including empty/EOF results. It does not authenticate any cached blob body.
func (h *Handle) CheckArtifactAccess(ctx context.Context) error {
	if h == nil {
		return ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrNotFound
	}
	lock, err := h.lockArtifacts(artifactContext(ctx))
	if err != nil {
		return err
	}
	return artifactError(lock.Close())
}

// WriteArtifact atomically creates or replaces a blob without evicting any
// existing artifact. Unknown publication is resolved only by authenticating the
// exact attempted ciphertext/revision and comparing its plaintext; no retries.
func (h *Handle) WriteArtifact(ctx context.Context, name string, data []byte) error {
	if h == nil {
		return ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrNotFound
	}
	if !validArtifactName(name, false) {
		return ErrInvalid
	}
	ctx = artifactContext(ctx)
	lock, err := h.lockArtifacts(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	if len(data) > MaxArtifactBytes {
		return ErrStoreFull
	}

	oldPlain, old, _, readErr := h.readArtifactLocked(name)
	zero(oldPlain)
	if readErr != nil && !errors.Is(readErr, ErrNotFound) {
		return artifactError(readErr)
	}
	fileName := artifactName(h.storageID, name)
	if err := h.store.checkArtifactQuotaLocked(ctx, fileName, old, int64(len(data)+containerHeaderSize+16)); err != nil {
		return artifactError(err)
	}
	key := deriveArtifactKey(h.dataKey, fileName)
	defer zero(key)
	revision, err := randomRevision()
	if err != nil {
		return artifactError(err)
	}
	session, _ := parseUUID(h.sessionID)
	storage, _ := parseStorageID(h.storageID)
	ciphertext, err := sealContainer(key, containerHeader{
		SchemaVersion: artifactContainerSchemaVersion, Kind: kindArtifact, Profile: h.store.profile,
		Generation: h.generation, Session: session, Storage: storage, Revision: revision,
	}, data)
	if err != nil {
		return artifactError(err)
	}
	var publishErr error
	if old == nil {
		publishErr = h.store.publishCreate(ctx, fileName, ciphertext)
	} else {
		publishErr = h.store.publishReplace(ctx, fileName, ciphertext, old.Data)
	}
	if errors.Is(publishErr, ErrOutcomeUnknown) {
		observed, snapshot, header, observeErr := h.readArtifactLocked(name)
		defer zero(observed)
		if observeErr == nil && header.Revision == revision && bytes.Equal(snapshot.Data, ciphertext) && bytes.Equal(observed, data) {
			return nil
		}
		return ErrOutcomeUnknown
	}
	return artifactError(publishErr)
}

// ListArtifacts returns the complete, lexically sorted set of valid blob names
// in this session matching prefix. Empty prefix selects all names. This is a
// namespace listing, not authentication of each listed blob's contents.
func (h *Handle) ListArtifacts(ctx context.Context, prefix string) ([]string, error) {
	if h == nil {
		return nil, ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrNotFound
	}
	if !validArtifactName(prefix, true) {
		return nil, ErrInvalid
	}
	ctx = artifactContext(ctx)
	lock, err := h.lockArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	entries, err := h.store.readRootEntries()
	if err != nil {
		return nil, artifactError(err)
	}
	names := make([]string, 0)
	for _, entry := range entries {
		name, ok := sessionArtifactName(entry.Name, h.storageID)
		if !ok || !validArtifactName(name, false) || !strings.HasPrefix(name, prefix) {
			continue
		}
		if entry.Type != securefile.EntryFile {
			return nil, ErrCorrupt
		}
		names = append(names, name)
	}
	if err := ctx.Err(); err != nil {
		return nil, artifactError(err)
	}
	sort.Strings(names)
	return names, nil
}

// h.mu and the Handle's lifetime session lock precede the profile generation
// fence. Only a short-lived copy of the refreshed wrapping key is obtained.
func (h *Handle) lockArtifacts(ctx context.Context) (*filelock.Lock, error) {
	lock, err := h.store.acquireProfileLock(ctx)
	if err != nil {
		return nil, artifactError(err)
	}
	generation, key, err := h.store.refreshProfileLocked()
	zero(key)
	if err == nil && generation != h.generation {
		err = ErrPrivacyInvalidated
	}
	if err == nil {
		err = securefile.CheckPrivateDirectory(h.store.rootPath)
	}
	if err != nil {
		_ = lock.Close()
		return nil, artifactError(err)
	}
	return lock, nil
}

func (h *Handle) readArtifactLocked(name string) ([]byte, *securefile.Snapshot, containerHeader, error) {
	fileName := artifactName(h.storageID, name)
	snapshot, err := h.store.root.ReadSnapshot(fileName, maxArtifactCiphertextBytes, true)
	if err != nil {
		return nil, nil, containerHeader{}, artifactError(err)
	}
	key := deriveArtifactKey(h.dataKey, fileName)
	defer zero(key)
	session, _ := parseUUID(h.sessionID)
	storage, _ := parseStorageID(h.storageID)
	plain, header, err := openContainer(key, snapshot.Data, containerExpectation{
		SchemaVersion: artifactContainerSchemaVersion, Kind: kindArtifact, Profile: h.store.profile,
		Generation: h.generation, Session: session, Storage: storage, MaxPayload: MaxArtifactBytes,
	})
	return plain, &snapshot, header, artifactError(err)
}

func deriveArtifactKey(dataKey []byte, fileName string) []byte {
	key := make([]byte, 32)
	// The complete, canonical filename is part of the HKDF domain, so even a
	// same-session rename cannot authenticate under the destination's key.
	info := []byte("edu-agent/session/artifact-key/v1/" + fileName)
	if _, err := io.ReadFull(hkdf.New(sha256.New, dataKey, nil, info), key); err != nil {
		zero(key)
		return nil
	}
	return key
}

func artifactName(storageID, name string) string {
	return "artifact-" + storageID + "-" + name + ".enc"
}

func validArtifactName(name string, prefix bool) bool {
	if len(name) > maxArtifactNameBytes || !prefix && len(name) == 0 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c != '_' && c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// Recognize the whole reserved artifact namespace for quotas, key-loss guards,
// and clear, even if a filename is malformed. Lists expose only valid names.
func isArtifactDataName(name string) bool {
	return strings.HasPrefix(name, "artifact-") && strings.HasSuffix(name, ".enc")
}

func sessionArtifactName(fileName, storageID string) (string, bool) {
	prefix := "artifact-" + storageID + "-"
	if !strings.HasPrefix(fileName, prefix) || !strings.HasSuffix(fileName, ".enc") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(fileName, prefix), ".enc"), true
}

func artifactContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Never expose OS paths, backend error details, or decrypted payloads through
// this boundary. Existing public sentinel identities remain usable by callers.
func artifactError(err error) error {
	if err == nil {
		return nil
	}
	for _, stable := range []error{ErrInvalid, ErrStoreFull, ErrCorrupt, ErrVersionUnsupported, ErrPrivacyInvalidated, ErrOutcomeUnknown, ErrNotFound, ErrInUse, ErrKeyUnavailable, ErrCheckpointConflict} {
		if errors.Is(err, stable) {
			return stable
		}
	}
	switch {
	case errors.Is(err, securefile.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, securefile.ErrTooLarge):
		return ErrStoreFull
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrInvalid
	default:
		return ErrCorrupt
	}
}
