package agentsession

import (
	"context"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

// rootEntryLimit leaves room for ReadDir's limit+1 probe and saturates on
// injected integer overflow. Artifacts do not enlarge the core entry budget.
func (s *Store) rootEntryLimit() int {
	maxLimit := int(^uint(0)>>1) - 1
	if s.limits.DirectoryEntries > maxLimit-s.limits.ArtifactFiles {
		return maxLimit
	}
	return s.limits.DirectoryEntries + s.limits.ArtifactFiles
}

// Every store-wide scan uses the same complete bounded snapshot. Missing entries
// during enumeration are an error, not an apparently complete empty/partial list.
func (s *Store) readRootEntries() ([]securefile.DirEntry, error) {
	entries, skipped, complete, err := s.root.ReadDir(".", s.rootEntryLimit())
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, ErrStoreFull
	}
	if skipped != 0 {
		return nil, ErrCorrupt
	}
	coreEntries := 0
	for _, entry := range entries {
		if !isArtifactDataName(entry.Name) {
			coreEntries++
			if coreEntries > s.limits.DirectoryEntries {
				return nil, ErrStoreFull
			}
		}
	}
	return entries, nil
}

// The caller holds both session and profile locks. Charge the replacement once
// and stat (never decrypt/read) other ciphertext files. Arithmetic is checked by
// subtraction before addition, including injected small/large quota values.
func (s *Store) checkArtifactQuotaLocked(ctx context.Context, fileName string, old *securefile.Snapshot, nextBytes int64) error {
	entries, err := s.readRootEntries()
	if err != nil {
		return err
	}
	if old == nil && len(entries) >= s.rootEntryLimit() {
		return ErrStoreFull
	}
	limits := s.limits
	sessionBytes, profileBytes, files := nextBytes, nextBytes, 1
	if sessionBytes > limits.ArtifactSessionCiphertextBytes || profileBytes > limits.ArtifactProfileCiphertextBytes {
		return ErrStoreFull
	}
	oldSeen := false
	for _, entry := range entries {
		if !isArtifactDataName(entry.Name) {
			continue
		}
		info, err := s.root.Stat(ctx, entry.Name)
		if err != nil {
			return err
		}
		if entry.Type != securefile.EntryFile || info.Kind != securefile.EntryFile || info.Size < 0 {
			return ErrCorrupt
		}
		if entry.Name == fileName {
			if old == nil || old.Size != info.Size || old.Identity != info.Identity || !old.ModTime.Equal(info.ModTime) {
				return ErrCorrupt
			}
			oldSeen = true
			continue
		}
		if files >= limits.ArtifactFiles || info.Size > limits.ArtifactProfileCiphertextBytes-profileBytes {
			return ErrStoreFull
		}
		files++
		profileBytes += info.Size
		if _, belongs := sessionArtifactName(entry.Name, artifactStorageID(fileName)); belongs {
			if info.Size > limits.ArtifactSessionCiphertextBytes-sessionBytes {
				return ErrStoreFull
			}
			sessionBytes += info.Size
		}
	}
	if old != nil && !oldSeen {
		return ErrCorrupt
	}
	return nil
}

// fileName is constructed only by artifactName from a live Handle's storage ID.
func artifactStorageID(fileName string) string {
	return fileName[len("artifact-") : len("artifact-")+32]
}

// Called only after wrapped-key deletion has been confirmed. Keep going after
// an individual failure so all reachable cleanup is attempted, but report it.
func (s *Store) deleteArtifactsLocked(storageID string) error {
	entries, err := s.readRootEntries()
	if err != nil {
		return ErrDeleteFailed
	}
	failed := false
	for _, entry := range entries {
		if _, belongs := sessionArtifactName(entry.Name, storageID); !belongs {
			continue
		}
		if err := s.deleteFile(entry.Name); err != nil {
			failed = true
		}
	}
	if failed {
		return ErrDeleteFailed
	}
	return nil
}
