//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Restore owns its fault boundaries; ordinary Move never acquires archive
// privileges and cannot accidentally use a restore-specific test hook.
var (
	restoreRenameUnix = renameArchiveNoReplace
	restoreSyncUnix   = unix.Fsync
	restoreClose      = func(f *os.File) error { return f.Close() }
)

type restoreState struct {
	root                                             *Root
	archive, sourceParent, destinationParent, source *os.File
	handles                                          []*os.File
	entry                                            ArchiveEntry
}

func (s *restoreState) close() error {
	var err error
	for i := len(s.handles) - 1; i >= 0; i-- {
		err = errors.Join(err, restoreClose(s.handles[i]))
	}
	return err
}

func openRestoreState(ctx context.Context, r *Root, p *RestorePlan) (_ *restoreState, err error) {
	s := &restoreState{root: r}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	s.archive, err = openUnixArchiveGuard(r)
	if err != nil {
		return nil, err
	}
	if s.archive == nil {
		return nil, ErrNotFound
	}
	s.handles = append(s.handles, s.archive)
	if err = verifyUnixArchiveGuard(r, s.archive); err != nil {
		return nil, err
	}
	// Anchor archive traversal to the held canonical archive, not a second
	// lookup that could silently switch to a replacement archive root.
	s.sourceParent, err = openUnixStatParent(ctx, &Root{file: s.archive}, p.source[1:len(p.source)-1])
	if err != nil {
		return nil, err
	}
	s.handles = append(s.handles, s.sourceParent)
	s.destinationParent, err = openUnixStatParent(ctx, r, p.destination[:len(p.destination)-1])
	if err != nil {
		return nil, err
	}
	s.handles = append(s.handles, s.destinationParent)
	if err = verifyUnixArchiveLocation(r.file, s.destinationParent, s.archive); err != nil {
		return nil, err
	}
	stat, err := unixArchiveStat(s.sourceParent, p.source[len(p.source)-1])
	if err != nil {
		return nil, err
	}
	s.entry, err = unixArchiveEntry(stat)
	if err != nil {
		return nil, err
	}
	for _, protected := range []*os.File{r.file, s.archive} {
		if same, e := unixArchiveSame(stat, protected); e != nil {
			return nil, e
		} else if same {
			return nil, ErrArchiveProtected
		}
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if s.entry.Kind == EntryDirectory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(s.sourceParent.Fd()), p.source[len(p.source)-1], flags, 0)
	if err != nil {
		return nil, archiveUnixError(err)
	}
	s.source = os.NewFile(uintptr(fd), p.Source())
	s.handles = append(s.handles, s.source)
	var opened, parent unix.Stat_t
	if err = unix.Fstat(fd, &opened); err != nil {
		return nil, err
	}
	entry, err := unixArchiveEntry(opened)
	if err != nil {
		return nil, err
	}
	if entry != s.entry {
		return nil, ErrChanged
	}
	if err = unix.Fstat(int(s.destinationParent.Fd()), &parent); err != nil {
		return nil, err
	}
	if opened.Dev != parent.Dev {
		return nil, ErrCrossDevice
	}
	return s, nil
}

func prepareRestore(ctx context.Context, r *Root, p *RestorePlan, expectedVersion string) (err error) {
	s, err := openRestoreState(ctx, r, p)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	if s.entry.Version != expectedVersion {
		return ErrChanged
	}
	p.entry = s.entry
	p.archiveID, err = mkdirHandleIdentity(s.archive)
	if err != nil {
		return err
	}
	p.sourceParentID, err = mkdirHandleIdentity(s.sourceParent)
	if err != nil {
		return err
	}
	p.destinationParentID, err = mkdirHandleIdentity(s.destinationParent)
	if err != nil {
		return err
	}
	return s.verify(ctx, p)
}

func (s *restoreState) verifyParents(ctx context.Context, p *RestorePlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := mkdirHandleIdentity(s.archive)
	if err != nil {
		return err
	}
	if id != p.archiveID {
		return ErrChanged
	}
	if err := verifyUnixArchiveGuard(s.root, s.archive); err != nil {
		return errors.Join(ErrChanged, err)
	}
	for i, parent := range []*os.File{s.sourceParent, s.destinationParent} {
		anchor, forbidden := s.archive, (*os.File)(nil)
		parts, want := p.source[1:len(p.source)-1], p.sourceParentID
		if i == 1 {
			anchor, forbidden = s.root.file, s.archive
			parts, want = p.destination[:len(p.destination)-1], p.destinationParentID
		}
		id, err := mkdirHandleIdentity(parent)
		if err != nil {
			return err
		}
		if id != want {
			return ErrChanged
		}
		if err := verifyUnixArchiveLocation(anchor, parent, forbidden); err != nil {
			return errors.Join(ErrChanged, err)
		}
		// Identity ancestry alone would permit a renamed parent at a different
		// spelling. Require both the frozen name and real ancestry to agree.
		reopened, err := openUnixStatParent(ctx, &Root{file: anchor}, parts)
		if err != nil {
			return errors.Join(ErrChanged, err)
		}
		other, idErr := mkdirHandleIdentity(reopened)
		if err := errors.Join(idErr, restoreClose(reopened)); err != nil {
			return err
		}
		if other != id {
			return ErrChanged
		}
	}
	return verifyUnixArchiveGuard(s.root, s.archive)
}

func (s *restoreState) verify(ctx context.Context, p *RestorePlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(s.source.Fd()), &opened); err != nil {
		return err
	}
	entry, err := unixArchiveEntry(opened)
	if err != nil {
		return err
	}
	if entry != p.entry {
		return ErrChanged
	}
	stat, err := unixArchiveStat(s.sourceParent, p.source[len(p.source)-1])
	if err != nil {
		return archiveSourceChangedError(err)
	}
	entry, err = unixArchiveEntry(stat)
	if err != nil {
		return archiveSourceChangedError(err)
	}
	if entry != p.entry {
		return ErrChanged
	}
	if p.entry.Kind == EntryDirectory {
		if err := verifyUnixArchiveLocation(s.archive, s.sourceParent, s.source); err != nil {
			return err
		}
		// Reject true descendants and aliases, not just textual prefixes.
		if err := verifyUnixArchiveLocation(s.root.file, s.destinationParent, s.source); err != nil {
			if errors.Is(err, ErrArchiveProtected) {
				return ErrMovePath
			}
			return err
		}
	}
	_, err = unixArchiveStat(s.destinationParent, p.destination[len(p.destination)-1])
	if errors.Is(err, ErrNotFound) {
		return ctx.Err()
	}
	if err == nil {
		return ErrAlreadyExists
	}
	return err
}

func (s *restoreState) verifyPublication(ctx context.Context, p *RestorePlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	_, err := unixArchiveStat(s.sourceParent, p.source[len(p.source)-1])
	if !errors.Is(err, ErrNotFound) {
		return errors.Join(ErrChanged, err)
	}
	stat, err := unixArchiveStat(s.destinationParent, p.destination[len(p.destination)-1])
	if err != nil {
		return err
	}
	entry, err := unixArchiveEntry(stat)
	if err != nil {
		return err
	}
	// Rename changes metadata. Prove only entry identity/kind at the target,
	// never report the old version as a target version or a content hash.
	if entry.Identity != p.entry.Identity || entry.Kind != p.entry.Kind {
		return ErrChanged
	}
	same, err := unixArchiveSame(stat, s.source)
	if err != nil {
		return err
	}
	if !same {
		return ErrChanged
	}
	return nil
}

func restoreWithinRoot(ctx context.Context, r *Root, p *RestorePlan) (result MoveResult, err error) {
	result.Outcome = PublishUnchanged
	s, err := openRestoreState(ctx, r, p)
	if err != nil {
		return result, archiveSourceChangedError(err)
	}
	defer func() {
		if closeErr := s.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
			if result.Outcome != PublishUnchanged {
				result.Outcome = PublishUnknown
			}
		}
		if result.Outcome == PublishUnknown {
			err = errors.Join(ErrOutcomeUnknown, err)
		}
	}()
	if err = s.verify(ctx, p); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	err = restoreRenameUnix(int(s.sourceParent.Fd()), p.source[len(p.source)-1], int(s.destinationParent.Fd()), p.destination[len(p.destination)-1])
	if err != nil {
		if !unixArchiveRenameUnchanged(err) {
			result.Outcome = PublishUnknown
		}
		return result, archiveUnixError(err)
	}
	result.Outcome = PublishCompleted
	if err = s.verifyPublication(context.WithoutCancel(ctx), p); err != nil {
		result.Outcome = PublishUnknown
		return result, err
	}
	for _, parent := range []*os.File{s.sourceParent, s.destinationParent} {
		if err = restoreSyncUnix(int(parent.Fd())); err != nil {
			result.Outcome = PublishUnknown
			return result, err
		}
	}
	return result, nil
}
