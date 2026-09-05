//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

var moveRenameUnix = renameArchiveNoReplace
var moveSyncUnix = unix.Fsync

func openMoveState(ctx context.Context, r *Root, p *MovePlan, _ bool) (state *moveState, err error) {
	s := &moveState{root: r}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	s.sourceParent, err = openUnixStatParent(ctx, r, p.source[:len(p.source)-1])
	if err != nil {
		return nil, err
	}
	s.handles = append(s.handles, s.sourceParent)
	s.destinationParent, err = openUnixStatParent(ctx, r, p.destination[:len(p.destination)-1])
	if err != nil {
		return nil, err
	}
	s.handles = append(s.handles, s.destinationParent)
	for _, parent := range []*os.File{s.sourceParent, s.destinationParent} {
		if err = checkArchivePublishParent(ctx, r, parent); err != nil {
			return nil, err
		}
	}
	stat, err := unixArchiveStat(s.sourceParent, p.source[len(p.source)-1])
	if err != nil {
		return nil, err
	}
	s.entry, err = unixArchiveEntry(stat)
	if err != nil {
		return nil, err
	}
	if same, e := unixArchiveSame(stat, r.file); e != nil {
		return nil, e
	} else if same {
		return nil, ErrMovePath
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
	var opened unix.Stat_t
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
	var parent unix.Stat_t
	if err = unix.Fstat(int(s.destinationParent.Fd()), &parent); err != nil {
		return nil, err
	}
	if opened.Dev != parent.Dev {
		return nil, ErrCrossDevice
	}
	return s, nil
}
func (s *moveState) verifyParents(ctx context.Context, p *MovePlan) error {
	for i, parent := range []*os.File{s.sourceParent, s.destinationParent} {
		parts, want := p.source, p.sourceParentID
		if i == 1 {
			parts, want = p.destination, p.destinationParentID
		}
		id, err := mkdirHandleIdentity(parent)
		if err != nil {
			return err
		}
		if id != want {
			return ErrChanged
		}
		if err = checkArchivePublishParent(ctx, s.root, parent); err != nil {
			return err
		}
		reopened, err := openUnixStatParent(ctx, s.root, parts[:len(parts)-1])
		if err != nil {
			return errors.Join(ErrChanged, err)
		}
		other, idErr := mkdirHandleIdentity(reopened)
		closeErr := reopened.Close()
		if err = errors.Join(idErr, closeErr); err != nil {
			return err
		}
		if other != id {
			return ErrChanged
		}
	}
	return nil
}
func (s *moveState) verify(ctx context.Context, p *MovePlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	for _, path := range []string{p.Source(), p.Destination()} {
		if err := s.root.CheckArchiveWritePath(ctx, path); err != nil {
			return err
		}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(s.source.Fd()), &stat); err != nil {
		return err
	}
	entry, err := unixArchiveEntry(stat)
	if err != nil {
		return err
	}
	if entry != p.entry {
		return ErrChanged
	}
	stat, err = unixArchiveStat(s.sourceParent, p.source[len(p.source)-1])
	if err != nil {
		return errors.Join(ErrChanged, err)
	}
	entry, err = unixArchiveEntry(stat)
	if err != nil {
		return err
	}
	if entry != p.entry {
		return ErrChanged
	}
	if p.entry.Kind == EntryDirectory {
		// An identity ancestry walk catches aliases, not just string-prefix children.
		if err = verifyUnixArchiveLocation(s.root.file, s.destinationParent, s.source); err != nil {
			if errors.Is(err, ErrArchiveProtected) {
				return ErrMovePath
			}
			return err
		}
	}
	if p.Unchanged() {
		return ctx.Err()
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
func (s *moveState) rename(p *MovePlan) (PublishOutcome, error) {
	err := moveRenameUnix(int(s.sourceParent.Fd()), p.source[len(p.source)-1], int(s.destinationParent.Fd()), p.destination[len(p.destination)-1])
	if err == nil {
		return PublishCompleted, nil
	}
	if unixArchiveRenameUnchanged(err) {
		return PublishUnchanged, archiveUnixError(err)
	}
	return PublishUnknown, archiveUnixError(err)
}
func (s *moveState) verifyPublication(ctx context.Context, p *MovePlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	for _, path := range []string{p.Source(), p.Destination()} {
		if err := s.root.CheckArchiveWritePath(ctx, path); err != nil {
			return err
		}
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
	// Rename changes metadata. Compare identity and kind, never reuse the old
	// entry version as a new target version or synthesize a content hash.
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
func (s *moveState) syncParents() error {
	if err := moveSyncUnix(int(s.sourceParent.Fd())); err != nil {
		return err
	}
	return moveSyncUnix(int(s.destinationParent.Fd()))
}
