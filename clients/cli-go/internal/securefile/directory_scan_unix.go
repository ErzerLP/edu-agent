//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

func openDirectoryScanWithinRoot(ctx context.Context, root *Root, components []string) (_ *DirectoryScan, err error) {
	anchor, err := openDirectoryWithinRoot(root, nil)
	if err != nil {
		return nil, archiveUnixError(err)
	}
	scan := &DirectoryScan{root: &Root{file: anchor}, components: components}
	defer func() {
		if err != nil {
			err = errors.Join(err, scan.closeLocked())
		}
	}()
	// Freeze the original spelling before opening the enumeration descriptor;
	// the verification below rejects a different object opened in between.
	scan.entry, err = statWithinRoot(ctx, scan.root, components)
	if err != nil {
		return nil, err
	}
	if scan.entry.Kind == EntryLink {
		return nil, ErrLink
	}
	if scan.entry.Kind != EntryDirectory {
		return nil, ErrNotDirectory
	}
	scan.directory, err = openUnixStatParent(ctx, scan.root, components)
	if err != nil {
		return nil, directoryScanValidationError(err)
	}
	scan.guard, err = newDirectoryChangeGuard(scan.directory)
	if err != nil {
		return nil, err
	}
	if err := scan.verify(ctx); err != nil {
		return nil, err
	}
	return scan, nil
}

func (s *DirectoryScan) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Check the held handle first, so a relocated ancestor cannot masquerade as
	// a missing original path or as a fresh replacement inside the root.
	if err := verifyUnixArchiveLocation(s.root.file, s.directory, nil); err != nil {
		return directoryScanValidationError(archiveUnixError(err))
	}
	current, err := statWithinRoot(ctx, s.root, s.components)
	if err != nil {
		return directoryScanValidationError(err)
	}
	if current.Kind == EntryLink {
		return ErrLink
	}
	if current != s.entry {
		return ErrChanged
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(s.directory.Fd()), &stat); err != nil {
		return archiveUnixError(err)
	}
	if unixEntryInfo(stat) != s.entry {
		return ErrChanged
	}
	// Traversing the original spelling itself takes time; recheck the held
	// descriptor's ancestry afterwards. No reconstructed absolute paths or
	// archive guard are involved in a read-only scan.
	if err := verifyUnixArchiveLocation(s.root.file, s.directory, nil); err != nil {
		return directoryScanValidationError(archiveUnixError(err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Preserve confinement/link errors above rather than hiding them behind a
	// generic notification failure. Native observation supplements metadata.
	return s.guard.Check()
}

func directoryScanValidationError(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDirectory) {
		return errors.Join(ErrChanged, err)
	}
	return err
}

func (s *DirectoryScan) next(ctx context.Context, limit int) ([]DirEntry, int, bool, error) {
	if err := s.verify(ctx); err != nil {
		return nil, 0, false, err
	}
	// Readdirnames retains both its OS position and Go's buffered names in the
	// same *os.File across calls. In particular, do not reopen, seek or consume
	// limit+1: a full page is not evidence that EOF has been reached.
	names, readErr := s.directory.Readdirnames(limit)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, 0, false, fmt.Errorf("read secure directory scan: %w", archiveUnixError(readErr))
	}
	entries := make([]DirEntry, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		stat, err := unixArchiveStat(s.directory, name)
		if err != nil {
			// A missing enumerated name proves a change even on filesystems
			// whose timestamp granularity could hide it from the final stat.
			return nil, 0, false, directoryScanValidationError(err)
		}
		entries = append(entries, DirEntry{Name: name, Type: unixEntryType(uint32(stat.Mode))})
	}
	if err := s.verify(ctx); err != nil {
		return nil, 0, false, err
	}
	return entries, 0, errors.Is(readErr, io.EOF), nil
}
