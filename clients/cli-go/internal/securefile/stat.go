package securefile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// EntryInfo describes one entry, never its descendants. Version is an opaque
// metadata version compatible with ArchiveEntry, not a content hash or CAS.
// Identity is internal filesystem identity and must not be exposed to models.
type EntryInfo struct {
	Kind     EntryType
	Size     int64
	ModTime  time.Time
	Identity string
	Version  string
}

// Stat observes metadata without opening entry contents or following links.
// Unlike InspectArchiveSource it permits the root and explicit archive paths.
func (r *Root) Stat(ctx context.Context, relative string) (EntryInfo, error) {
	components, err := r.statComponents(ctx, relative)
	if err != nil {
		return EntryInfo{}, err
	}
	return statWithinRoot(ctx, r, components)
}

// HashEntry hashes bounded raw bytes of a regular file matching expected.
// It uses a non-following, non-blocking open and validates metadata before and
// after reading. It is not a cross-process linearizable snapshot or lock.
func (r *Root) HashEntry(ctx context.Context, relative string, expected EntryInfo, limit int64) (string, error) {
	components, err := r.statComponents(ctx, relative)
	if err != nil {
		return "", err
	}
	if len(components) == 0 || expected.Kind != EntryFile {
		return "", ErrNotRegular
	}
	if limit < 1 {
		return "", errors.New("secure hash size limit is invalid")
	}
	if expected.Size > limit {
		return "", ErrTooLarge
	}
	return hashEntryWithinRoot(ctx, r, components, expected, limit)
}

func (r *Root) statComponents(ctx context.Context, relative string) ([]string, error) {
	if r == nil || r.file == nil {
		return nil, errors.New("secure root is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return directoryComponents(relative)
}

type entryHashReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r entryHashReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func readEntryHash(ctx context.Context, reader io.Reader, expected EntryInfo, limit int64) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(entryHashReader{ctx, reader}, limit+1))
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if n > limit {
		return "", ErrTooLarge
	}
	if n != expected.Size {
		return "", ErrChanged
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
