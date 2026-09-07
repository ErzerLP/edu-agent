//go:build !linux && !darwin

package securefile

import "context"

func prepareCopyTreeWithinRoot(context.Context, *Root, string, string, string, CopyTreeLimits) (*CopyTreePlan, error) {
	return nil, ErrArchiveUnsupported
}

func copyTreeWithinRoot(_ context.Context, _ *Root, _ *CopyTreePlan, _ CopyTreeObserver, result CopyTreeResult) (CopyTreeResult, error) {
	return result, ErrArchiveUnsupported
}
