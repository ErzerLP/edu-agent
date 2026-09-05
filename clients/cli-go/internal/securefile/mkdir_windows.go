//go:build windows

package securefile

import (
	"context"
	"errors"
	"os"
)

func openMkdirDirectories(ctx context.Context, root *Root, components []string, handles *[]*os.File) (*os.File, error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return nil, err
	}
	guard, err := openWindowsArchiveGuard(root)
	if err != nil {
		return nil, err
	}
	if guard != nil {
		*handles = append(*handles, guard)
	}
	parent, err := openWindowsArchiveParents(ctx, root, components, guard, handles)
	if err != nil {
		return nil, err
	}
	if err = checkArchivePublishParent(ctx, root, parent); err != nil {
		return nil, err
	}
	return parent, nil
}
func inspectMkdirDirectory(ctx context.Context, root *Root, components []string) (identity string, err error) {
	var handles []*os.File
	result := MkdirResult{Outcome: PublishUnchanged}
	defer func() { closeMkdirHandles(handles, &result, &err) }()
	file, err := openMkdirDirectories(ctx, root, components, &handles)
	if err != nil {
		return "", err
	}
	return mkdirHandleIdentity(file)
}
func mkdirWithinRoot(ctx context.Context, root *Root, plan *MkdirPlan) (result MkdirResult, err error) {
	result.Outcome = PublishUnchanged
	var handles []*os.File
	defer func() { closeMkdirHandles(handles, &result, &err) }()
	parent, err := openMkdirDirectories(ctx, root, plan.components[:plan.anchor], &handles)
	if err != nil {
		return result, errors.Join(ErrChanged, err)
	}
	identity, err := mkdirHandleIdentity(parent)
	if err != nil {
		return result, err
	}
	if identity != plan.identity {
		return result, ErrChanged
	}
	for depth := plan.anchor; depth < len(plan.components); depth++ {
		// Every original and created ancestor is held without delete sharing.
		reopened, openErr := openMkdirDirectories(ctx, root, plan.components[:depth], &handles)
		if openErr != nil {
			return result, errors.Join(ErrChanged, openErr)
		}
		same, sameErr := windowsArchiveSame(parent, reopened)
		if sameErr != nil {
			return result, sameErr
		}
		if !same {
			return result, ErrChanged
		}
		if err = checkArchivePublishParent(ctx, root, parent); err != nil {
			return result, err
		}
		created := ArchiveResult{Outcome: PublishUnchanged}
		next, createErr := createWindowsArchiveDirectory(parent, plan.components[depth], &created)
		if created.DirectoriesCreated {
			result.Created++
		}
		if createErr != nil {
			_, unchanged := classifyWindowsRenameError(createErr, PublishCreate)
			if !created.DirectoriesCreated && !unchanged && !errors.Is(createErr, ErrAlreadyExists) {
				result.Outcome = PublishUnknown
			}
			return result, createErr
		}
		handles = append(handles, next)
		parent = next
	}
	if err = checkArchivePublishParent(ctx, root, parent); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	// No supported Windows directory-fsync equivalent; handle create + close.
	result.Outcome = PublishCompleted
	return result, nil
}
