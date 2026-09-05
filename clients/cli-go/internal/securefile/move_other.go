//go:build !linux && !darwin && !windows

package securefile

import "context"

func openMoveState(context.Context, *Root, *MovePlan, bool) (*moveState, error) {
	return nil, ErrArchiveUnsupported
}
func (*moveState) verify(context.Context, *MovePlan) error { return ErrArchiveUnsupported }
func (*moveState) rename(*MovePlan) (PublishOutcome, error) {
	return PublishUnchanged, ErrArchiveUnsupported
}
func (*moveState) verifyPublication(context.Context, *MovePlan) error { return ErrArchiveUnsupported }
func (*moveState) syncParents() error                                 { return ErrArchiveUnsupported }
