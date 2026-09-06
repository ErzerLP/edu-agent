//go:build !linux && !darwin

package securefile

import (
	"context"
	"errors"
)

var errDirectoryScanUnsupported = errors.New("secure directory scanning is unsupported on this platform")

func openDirectoryScanWithinRoot(context.Context, *Root, []string) (*DirectoryScan, error) {
	return nil, errDirectoryScanUnsupported
}

func (*DirectoryScan) verify(context.Context) error {
	return errDirectoryScanUnsupported
}

func (*DirectoryScan) next(context.Context, int) ([]DirEntry, int, bool, error) {
	return nil, 0, false, errDirectoryScanUnsupported
}
