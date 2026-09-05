//go:build !linux && !darwin && !windows

package securefile

import (
	"context"
	"os"
)

func openCopyState(context.Context, *Root, *CopyPlan) (*copyState, error) {
	return nil, ErrArchiveUnsupported
}
func (*copyState) verify(context.Context, *CopyPlan) error          { return ErrArchiveUnsupported }
func (*copyState) verifyPublished(context.Context, *CopyPlan) error { return ErrArchiveUnsupported }
func (*copyState) createTemp() (*os.File, error)                    { return nil, ErrArchiveUnsupported }
func (*copyState) cleanupTemp(*CopyPlan, *os.File) error            { return ErrArchiveUnsupported }
func (*copyState) verifyTemp(*os.File) error                        { return ErrArchiveUnsupported }
func (*copyState) publishTemp(*os.File, string) (PublishOutcome, error) {
	return PublishUnchanged, ErrArchiveUnsupported
}
func (*copyState) verifyPublication(*os.File, string) error { return ErrArchiveUnsupported }
func (*copyState) syncParent() error                        { return ErrArchiveUnsupported }
func copySetPermission(*os.File, os.FileMode) error         { return ErrArchiveUnsupported }
