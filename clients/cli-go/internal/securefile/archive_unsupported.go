//go:build !linux && !darwin && !windows

package securefile

import (
	"context"
	"os"
)

// Fail closed on platforms without an implemented root-bound archive backend.
func checkArchivePublishParent(context.Context, *Root, *os.File) error {
	return ErrArchiveUnsupported
}

func checkArchiveWritePath(context.Context, *Root, []string) error {
	return ErrArchiveUnsupported
}

func inspectArchiveSource(context.Context, *Root, []string) (ArchiveEntry, error) {
	return ArchiveEntry{}, ErrArchiveUnsupported
}

func archiveWithinRoot(context.Context, *Root, []string, []string, ArchiveEntry) (ArchiveResult, error) {
	return ArchiveResult{Outcome: PublishUnchanged}, ErrArchiveUnsupported
}
