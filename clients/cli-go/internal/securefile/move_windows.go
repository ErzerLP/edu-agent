//go:build windows

package securefile

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

var moveRenameWindows = renameWindowsFileHandle

func openMoveState(ctx context.Context, r *Root, p *MovePlan, commit bool) (state *moveState, err error) {
	s := &moveState{root: r}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	if err = validateWindowsRelativeComponents(p.source); err != nil {
		return nil, err
	}
	if err = validateWindowsRelativeComponents(p.destination); err != nil {
		return nil, err
	}
	guard, err := openWindowsArchiveGuard(r)
	if err != nil {
		return nil, err
	}
	if guard != nil {
		s.handles = append(s.handles, guard)
	}
	s.sourceParent, err = openWindowsArchiveParents(ctx, r, p.source[:len(p.source)-1], guard, &s.handles)
	if err != nil {
		return nil, err
	}
	// Save the destination ancestor range for the identity self-descendant check.
	first := len(s.handles)
	s.destinationParent, err = openWindowsArchiveParents(ctx, r, p.destination[:len(p.destination)-1], guard, &s.handles)
	if err != nil {
		return nil, err
	}
	access := uint32(0)
	if commit && !p.Unchanged() {
		access = windows.DELETE
	}
	s.source, err = openWindowsArchiveEntry(s.sourceParent, p.source[len(p.source)-1], access, windowsReadShare)
	if err != nil {
		return nil, err
	}
	// Append only after checking the ancestor slice, not the source against itself.
	ancestors := append([]*os.File(nil), s.handles[first:]...)
	s.handles = append(s.handles, s.source)
	if err = rejectWindowsArchiveIdentity(s.source, guard, r.file); err != nil {
		return nil, err
	}
	s.entry, err = windowsArchiveEntry(s.source)
	if err != nil {
		return nil, err
	}
	if s.entry.Kind == EntryDirectory {
		for _, ancestor := range ancestors {
			same, e := windowsArchiveSame(s.source, ancestor)
			if e != nil {
				return nil, e
			}
			if same {
				return nil, ErrMovePath
			}
		}
	}
	var from, to windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(windows.Handle(s.source.Fd()), &from); err != nil {
		return nil, archiveWindowsError(err)
	}
	if err = windows.GetFileInformationByHandle(windows.Handle(s.destinationParent.Fd()), &to); err != nil {
		return nil, archiveWindowsError(err)
	}
	if from.VolumeSerialNumber != to.VolumeSerialNumber {
		return nil, ErrCrossDevice
	}
	return s, nil
}
func (s *moveState) verifyParents(ctx context.Context, p *MovePlan) error {
	for i, parent := range []*os.File{s.sourceParent, s.destinationParent} {
		want := p.sourceParentID
		if i == 1 {
			want = p.destinationParentID
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
	}
	// Every ancestor, including root, is held without FILE_SHARE_DELETE.
	return nil
}
func (s *moveState) verify(ctx context.Context, p *MovePlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	entry, err := windowsArchiveEntry(s.source)
	if err != nil {
		return err
	}
	if entry != p.entry {
		return ErrChanged
	}
	// Reopening must share DELETE owned by our source handle. This does not
	// weaken the no-delete sharing on the original source or ancestor handles.
	reopened, err := openWindowsArchiveEntry(s.sourceParent, p.source[len(p.source)-1], 0, windowsAllShare)
	if err != nil {
		return errors.Join(ErrChanged, err)
	}
	entry, statErr := windowsArchiveEntry(reopened)
	closeErr := reopened.Close()
	if err = errors.Join(statErr, closeErr); err != nil {
		return err
	}
	if entry != p.entry {
		return ErrChanged
	}
	if p.Unchanged() {
		return ctx.Err()
	}
	target, err := openWindowsArchiveEntry(s.destinationParent, p.destination[len(p.destination)-1], 0, windowsAllShare)
	if errors.Is(err, ErrNotFound) {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return errors.Join(ErrAlreadyExists, target.Close())
}
func (s *moveState) rename(p *MovePlan) (PublishOutcome, error) {
	err := moveRenameWindows(windows.Handle(s.source.Fd()), windows.Handle(s.destinationParent.Fd()), p.destination[len(p.destination)-1], false)
	if err == nil {
		return PublishCompleted, nil
	}
	mapped := archiveWindowsError(err)
	if errors.Is(mapped, ErrCrossDevice) || errors.Is(mapped, ErrArchiveUnsupported) {
		return PublishUnchanged, mapped
	}
	mapped, unchanged := classifyWindowsRenameError(err, PublishCreate)
	if unchanged {
		return PublishUnchanged, mapped
	}
	return PublishUnknown, mapped
}
func (s *moveState) verifyPublication(ctx context.Context, p *MovePlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	original, err := openWindowsArchiveEntry(s.sourceParent, p.source[len(p.source)-1], 0, windowsAllShare)
	if err == nil {
		return errors.Join(ErrChanged, original.Close())
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	target, err := openWindowsArchiveEntry(s.destinationParent, p.destination[len(p.destination)-1], 0, windowsAllShare)
	if err != nil {
		return err
	}
	entry, statErr := windowsArchiveEntry(target)
	closeErr := target.Close()
	if err = errors.Join(statErr, closeErr); err != nil {
		return err
	}
	if entry.Identity != p.entry.Identity || entry.Kind != p.entry.Kind {
		return ErrChanged
	}
	return nil
}
func (*moveState) syncParents() error { return nil } // No supported Windows directory fsync.
