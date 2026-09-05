//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var mkdirAtUnix = unix.Mkdirat
var mkdirSyncUnix = unix.Fsync

func openMkdirDirectory(ctx context.Context, root *Root, components []string) (*os.File, error) {
	parent, _, err := openPublishParentWithinRoot(root, components, false)
	if err != nil {
		return nil, archiveUnixError(err)
	}
	if err = checkArchivePublishParent(ctx, root, parent); err != nil {
		_ = parent.Close()
		return nil, err
	}
	return parent, nil
}
func inspectMkdirDirectory(ctx context.Context, root *Root, components []string) (identity string, err error) {
	file, err := openMkdirDirectory(ctx, root, components)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	return mkdirHandleIdentity(file)
}
func verifyMkdirParent(ctx context.Context, root *Root, parent *os.File, components []string, expected string) (err error) {
	// Verify both the held handle's location and original spelling immediately
	// before mkdirat. This is deliberately not claimed as cross-process CAS.
	reopened, err := openMkdirDirectory(ctx, root, components)
	if err != nil {
		return errors.Join(ErrChanged, err)
	}
	defer func() { err = errors.Join(err, reopened.Close()) }()
	actual, err := mkdirHandleIdentity(reopened)
	if err != nil {
		return err
	}
	held, err := mkdirHandleIdentity(parent)
	if err != nil {
		return err
	}
	if actual != held || held != expected {
		return ErrChanged
	}
	return checkArchivePublishParent(ctx, root, parent)
}
func mkdirWithinRoot(ctx context.Context, root *Root, plan *MkdirPlan) (result MkdirResult, err error) {
	result.Outcome = PublishUnchanged
	var handles []*os.File
	defer func() { closeMkdirHandles(handles, &result, &err) }()
	parent, err := openMkdirDirectory(ctx, root, plan.components[:plan.anchor])
	if err != nil {
		return result, errors.Join(ErrChanged, err)
	}
	handles = append(handles, parent)
	expected := plan.identity
	for depth := plan.anchor; depth < len(plan.components); depth++ {
		if err = verifyMkdirParent(ctx, root, parent, plan.components[:depth], expected); err != nil {
			return result, err
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if createErr := mkdirAtUnix(int(parent.Fd()), plan.components[depth], 0o700); createErr != nil {
			// Ambiguous filesystem errors may have committed even without a known
			// success. EEXIST never authorizes accepting somebody else's directory.
			if !unixArchiveRenameUnchanged(createErr) {
				result.Outcome = PublishUnknown
			}
			return result, archiveUnixError(createErr)
		}
		result.Created++ // Before reopen, fsync, cancellation or any further failure.
		next, openErr := openUnixArchiveDirectory(parent, plan.components[depth])
		if openErr != nil {
			return result, openErr
		}
		handles = append(handles, next)
		if err = mkdirSyncUnix(int(parent.Fd())); err != nil {
			return result, err
		}
		parent = next
		expected, err = mkdirHandleIdentity(parent)
		if err != nil {
			return result, err
		}
	}
	if err = verifyMkdirParent(ctx, root, parent, plan.components, expected); err != nil {
		return result, err
	}
	if err = mkdirSyncUnix(int(parent.Fd())); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Outcome = PublishCompleted
	return result, nil
}
