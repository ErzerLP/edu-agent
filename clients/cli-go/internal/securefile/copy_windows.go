//go:build windows

package securefile

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func openCopyState(ctx context.Context, r *Root, p *CopyPlan) (state *copyState, err error) {
	s := &copyState{root: r}
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
	s.destinationParent, err = openWindowsArchiveParents(ctx, r, p.destination[:len(p.destination)-1], guard, &s.handles)
	if err != nil {
		return nil, err
	}
	file, info, err := openWindowsFileRelative(windows.Handle(s.sourceParent.Fd()), p.source[len(p.source)-1], windowsPinnedReadShare)
	if err != nil {
		return nil, archiveWindowsError(err)
	}
	s.source = file
	s.handles = append(s.handles, file)
	s.permission = info.Mode().Perm()
	if err = rejectWindowsArchiveIdentity(file, guard, r.file); err != nil {
		return nil, err
	}
	s.entry, err = windowsArchiveEntry(file)
	if err != nil {
		return nil, err
	}
	if s.entry.Kind != EntryFile {
		return nil, ErrNotRegular
	}
	if s.entry.Size < 0 || s.entry.Size > CopyMaxBytes {
		return nil, ErrTooLarge
	}
	if err = s.targetAbsent(p); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *copyState) targetAbsent(p *CopyPlan) error {
	file, err := openWindowsArchiveEntry(s.destinationParent, p.destination[len(p.destination)-1], 0, windowsReadShare)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(ErrAlreadyExists, file.Close())
}
func (s *copyState) verifyParents(ctx context.Context, p *CopyPlan) error {
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
	// All original ancestors are pinned without FILE_SHARE_DELETE, including
	// source itself (and the root); neither spelling nor ancestry can migrate.
	return nil
}
func (s *copyState) verifyPublished(ctx context.Context, p *CopyPlan) error {
	if err := s.verifyParents(ctx, p); err != nil {
		return err
	}
	if err := s.root.CheckArchiveWritePath(ctx, p.Source()); err != nil {
		return err
	}
	entry, err := windowsArchiveEntry(s.source)
	if err != nil {
		return err
	}
	if entry != p.entry {
		return ErrChanged
	}
	return nil
}
func (s *copyState) verify(ctx context.Context, p *CopyPlan) error {
	if err := s.verifyPublished(ctx, p); err != nil {
		return err
	}
	return s.targetAbsent(p)
}
func (s *copyState) createTemp() (*os.File, error) {
	name, err := secureWindowsTempName()
	if err != nil {
		return nil, err
	}
	handle, err := ntCreateWindowsRelative(windows.Handle(s.destinationParent.Fd()), name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_WRITE_THROUGH,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_ATTRIBUTE_TEMPORARY, 0)
	if err != nil {
		mapped, unchanged := classifyWindowsRenameError(err, PublishCreate)
		if !unchanged {
			return nil, errors.Join(ErrOutcomeUnknown, mapped)
		}
		return nil, mapped
	}
	// FILE_CREATE returns our exclusive new regular entry. Retain its handle
	// directly so later inspection failure can still delete only this owned item.
	return os.NewFile(uintptr(handle), name), nil
}
func (s *copyState) verifyTemp(temp *os.File) error {
	entry, err := windowsArchiveEntry(temp)
	if err != nil {
		return err
	}
	if entry.Kind != EntryFile {
		return ErrNotRegular
	}
	return nil
}
func (s *copyState) cleanupTemp(p *CopyPlan, temp *os.File) error {
	// The exclusive handle pins the original temp. Cleanup is handle-relative,
	// never a delete by reconstructed path (also safe if a rename failed).
	if err := s.verifyParents(context.Background(), p); err != nil {
		return errors.Join(err, copyClose(temp))
	}
	return deleteWindowsFileHandle(temp)
}
func copySetPermission(f *os.File, mode os.FileMode) error { return f.Chmod(mode.Perm()) }
func (s *copyState) publishTemp(temp *os.File, target string) (PublishOutcome, error) {
	err := renameWindowsFileHandle(windows.Handle(temp.Fd()), windows.Handle(s.destinationParent.Fd()), target, false)
	if err == nil {
		return PublishCompleted, nil
	}
	mapped, unchanged := classifyWindowsRenameError(err, PublishCreate)
	if unchanged {
		return PublishUnchanged, mapped
	}
	return PublishUnknown, mapped
}
func (*copyState) verifyPublication(*os.File, string) error { return nil } // Exclusive handle pins the published entry.
func (s *copyState) syncParent() error                      { return nil } // No supported directory fsync on Windows.
