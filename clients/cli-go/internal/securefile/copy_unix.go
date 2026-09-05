//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

var copyRenameUnix = renameArchiveNoReplace
var copyParentSyncUnix = unix.Fsync
var copyTempOpenUnix = unix.Openat

func openCopyState(ctx context.Context, r *Root, p *CopyPlan) (state *copyState, err error) {
	s := &copyState{root: r}
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
	if s.entry.Kind != EntryFile {
		return nil, ErrNotRegular
	}
	if s.entry.Size < 0 || s.entry.Size > CopyMaxBytes {
		return nil, ErrTooLarge
	}
	fd, err := unix.Openat(int(s.sourceParent.Fd()), p.source[len(p.source)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
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
	s.permission = os.FileMode(uint32(opened.Mode) & 0777)
	if err = s.targetAbsent(p); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *copyState) targetAbsent(p *CopyPlan) error {
	_, err := unixArchiveStat(s.destinationParent, p.destination[len(p.destination)-1])
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err == nil {
		return ErrAlreadyExists
	}
	return err
}
func (s *copyState) verifyParents(ctx context.Context, p *CopyPlan) error {
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
func (s *copyState) verifySource(ctx context.Context, p *CopyPlan) error {
	if err := ctx.Err(); err != nil {
		return err
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
	return nil
}
func (s *copyState) verifyPublished(ctx context.Context, p *CopyPlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	for _, path := range []string{p.Source(), p.Destination()} {
		if err := s.root.CheckArchiveWritePath(ctx, path); err != nil {
			return err
		}
	}
	return s.verifySource(ctx, p)
}
func (s *copyState) verify(ctx context.Context, p *CopyPlan) error {
	if err := s.verifyPublished(ctx, p); err != nil {
		return err
	}
	return s.targetAbsent(p)
}
func (s *copyState) createTemp() (*os.File, error) {
	// Exclusive random name, private while streaming; no user-specified temp path.
	name, err := secureTempName()
	if err != nil {
		return nil, err
	}
	fd, err := copyTempOpenUnix(int(s.destinationParent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		if !unixArchiveRenameUnchanged(err) {
			return nil, errors.Join(ErrOutcomeUnknown, archiveUnixError(err))
		}
		return nil, archiveUnixError(err)
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (s *copyState) verifyTemp(temp *os.File) error {
	stat, err := unixArchiveStat(s.destinationParent, temp.Name())
	if err != nil {
		return err
	}
	if unixEntryType(uint32(stat.Mode)) != EntryFile || stat.Nlink != 1 {
		return ErrChanged
	}
	same, err := unixArchiveSame(stat, temp)
	if err != nil {
		return err
	}
	if !same {
		return ErrChanged
	}
	return nil
}
func (s *copyState) cleanupTemp(p *CopyPlan, temp *os.File) (err error) {
	defer func() { err = errors.Join(err, copyClose(temp)) }()
	// Retain the fd until the ownership check and unlink; dev/inode cannot be
	// recycled while it is open. Never remove a replaced user entry or follow it.
	if err = s.verifyDestinationParent(context.Background(), p); err != nil {
		return err
	}
	if err = s.verifyTemp(temp); err != nil {
		return err
	}
	return unix.Unlinkat(int(s.destinationParent.Fd()), temp.Name(), 0)
}
func copySetPermission(f *os.File, mode os.FileMode) error {
	return unix.Fchmod(int(f.Fd()), uint32(mode.Perm()))
}
func (s *copyState) publishTemp(temp *os.File, target string) (PublishOutcome, error) {
	err := copyRenameUnix(int(s.destinationParent.Fd()), temp.Name(), int(s.destinationParent.Fd()), target)
	if err == nil {
		return PublishCompleted, nil
	}
	if unixArchiveRenameUnchanged(err) {
		return PublishUnchanged, archiveUnixError(err)
	}
	return PublishUnknown, archiveUnixError(err)
}
func (s *copyState) verifyDestinationParent(ctx context.Context, p *CopyPlan) error {
	id, err := mkdirHandleIdentity(s.destinationParent)
	if err != nil {
		return err
	}
	if id != p.destinationParentID {
		return ErrChanged
	}
	if err = checkArchivePublishParent(ctx, s.root, s.destinationParent); err != nil {
		return err
	}
	reopened, err := openUnixStatParent(ctx, s.root, p.destination[:len(p.destination)-1])
	if err != nil {
		return err
	}
	other, idErr := mkdirHandleIdentity(reopened)
	closeErr := reopened.Close()
	if err = errors.Join(idErr, closeErr); err != nil {
		return err
	}
	if other != id {
		return ErrChanged
	}
	return nil
}
func (s *copyState) verifyPublication(temp *os.File, target string) error {
	stat, err := unixArchiveStat(s.destinationParent, target)
	if err != nil {
		return err
	}
	same, err := unixArchiveSame(stat, temp)
	if err != nil {
		return err
	}
	if !same {
		return ErrChanged
	}
	return nil
}
func (s *copyState) syncParent() error { return copyParentSyncUnix(int(s.destinationParent.Fd())) }
