//go:build !linux && !darwin

package securefile

import "context"

func preparePurgeWithinRoot(context.Context, *Root, string, string, PurgeLimits) (*PurgePlan, error) {
	return nil, ErrArchiveUnsupported
}

func purgeWithinRoot(_ context.Context, _ *Root, _ *PurgePlan, _ PurgeObserver, result PurgeResult) (PurgeResult, error) {
	return result, ErrArchiveUnsupported
}
