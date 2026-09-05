//go:build !linux && !darwin && !windows

package securefile

import (
	"context"
	"errors"
)

var errStatUnsupported = errors.New("secure entry inspection is unsupported on this platform")

func statWithinRoot(context.Context, *Root, []string) (EntryInfo, error) {
	return EntryInfo{}, errStatUnsupported
}

func hashEntryWithinRoot(context.Context, *Root, []string, EntryInfo, int64) (string, error) {
	return "", errStatUnsupported
}
