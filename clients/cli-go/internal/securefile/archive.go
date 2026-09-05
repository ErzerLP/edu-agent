package securefile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const ArchiveDirectory = ".edu-agent-archive"

var (
	ErrArchiveProtected   = errors.New("secure archive path is protected")
	ErrCrossDevice        = errors.New("secure archive cannot cross filesystems")
	ErrArchiveUnsupported = errors.New("secure no-replace archive is unsupported")
)

// ArchiveEntry describes only the source entry, not its contents or descendants.
// Version is an opaque metadata version, not a content hash or a subtree snapshot.
type ArchiveEntry struct {
	Kind     EntryType
	Size     int64
	Identity string
	Version  string
}

type ArchiveResult struct {
	Outcome            PublishOutcome
	DirectoriesCreated bool
}

// CheckArchiveWritePath rejects the archive tree, including filesystem aliases.
// Missing ordinary path components are allowed. This check does not create them.
// Callers must check again at their mutation boundary; it is not a lasting lock.
func (r *Root) CheckArchiveWritePath(ctx context.Context, relative string) error {
	components, err := r.archiveSourceComponents(ctx, relative)
	if err != nil {
		return err
	}
	return checkArchiveWritePath(ctx, r, components)
}

// InspectArchiveSource freezes entry identity and metadata without reading any
// file contents, traversing descendants, or creating the archive directory.
func (r *Root) InspectArchiveSource(ctx context.Context, relative string) (ArchiveEntry, error) {
	components, err := r.archiveSourceComponents(ctx, relative)
	if err != nil {
		return ArchiveEntry{}, err
	}
	return inspectArchiveSource(ctx, r, components)
}

// Archive moves an entry using a no-replace rename. The caller supplies a frozen
// destination under ArchiveDirectory, normally <unique-container>/<source>.
// There is no copy, deletion, overwrite, or cleanup fallback. Failed operations
// may leave created containers; Unknown must not be automatically retried.
// External non-cooperating processes can still race the final Unix validation:
// this is not a cross-process compare-and-swap or a directory-content snapshot.
func (r *Root) Archive(ctx context.Context, source, destination string, expected ArchiveEntry) (result ArchiveResult, err error) {
	result.Outcome = PublishUnchanged
	src, err := r.archiveSourceComponents(ctx, source)
	if err != nil {
		return result, err
	}
	dst, err := relativeComponents(destination)
	if err != nil {
		return result, err
	}
	if len(dst) < 3 || dst[0] != ArchiveDirectory {
		return result, ErrArchiveProtected
	}
	if (expected.Kind != EntryFile && expected.Kind != EntryDirectory) || expected.Size < 0 || expected.Identity == "" || !validArchiveVersion(expected.Version) {
		return result, errors.New("secure archive entry precondition is invalid")
	}
	return archiveWithinRoot(ctx, r, src, dst, expected)
}

func (r *Root) archiveSourceComponents(ctx context.Context, relative string) ([]string, error) {
	if r == nil || r.file == nil {
		return nil, errors.New("secure root is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if relative == "." {
		return nil, ErrArchiveProtected
	}
	components, err := relativeComponents(relative)
	if err != nil {
		return nil, err
	}
	// Conservatively reserve this spelling on case-sensitive filesystems too.
	if strings.EqualFold(components[0], ArchiveDirectory) {
		return nil, ErrArchiveProtected
	}
	return components, nil
}

func archiveSourceChangedError(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrLink) || errors.Is(err, ErrNotDirectory) || errors.Is(err, ErrNotRegular) {
		return errors.Join(ErrChanged, err)
	}
	return err
}

func archiveMetadataVersion(metadata string) string {
	digest := sha256.Sum256([]byte(metadata))
	return "entry-v1:" + hex.EncodeToString(digest[:])
}

func validArchiveVersion(version string) bool {
	if !strings.HasPrefix(version, "entry-v1:") || len(version) != len("entry-v1:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(version, "entry-v1:"))
	return err == nil
}

// Keep the original cause discoverable even when container side effects mean
// the operation as a whole can no longer be described as unchanged.
func finishArchive(result *ArchiveResult, err *error) {
	if *err != nil && result.DirectoriesCreated {
		result.Outcome = PublishUnknown
	}
	if result.Outcome == PublishUnknown && !errors.Is(*err, ErrOutcomeUnknown) {
		*err = errors.Join(ErrOutcomeUnknown, *err)
	}
}

func closeArchiveHandles(files []*os.File, result *ArchiveResult, err *error) {
	for i := len(files) - 1; i >= 0; i-- {
		if closeErr := files[i].Close(); closeErr != nil {
			*err = errors.Join(*err, fmt.Errorf("close secure archive handle: %w", closeErr))
			if result.Outcome != PublishUnchanged || result.DirectoriesCreated {
				result.Outcome = PublishUnknown
			}
		}
	}
}
