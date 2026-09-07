//go:build !linux && !darwin

package securefile

import "context"

func prepareRestore(context.Context, *Root, *RestorePlan, string) error {
	return ErrArchiveUnsupported
}

func restoreWithinRoot(context.Context, *Root, *RestorePlan) (MoveResult, error) {
	return MoveResult{Outcome: PublishUnchanged}, ErrArchiveUnsupported
}
