//go:build !linux && !darwin && !windows

package securefile

import "context"

func inspectMkdirDirectory(context.Context, *Root, []string) (string, error) {
	return "", ErrArchiveUnsupported
}
func mkdirWithinRoot(context.Context, *Root, *MkdirPlan) (MkdirResult, error) {
	return MkdirResult{Outcome: PublishUnchanged}, ErrArchiveUnsupported
}
