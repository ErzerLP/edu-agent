//go:build windows

package securefile

import (
	"errors"
	"testing"
)

func TestArchiveRestoreUnsupportedPlatform(t *testing.T) {
	root, _ := archiveTestRoot(t)
	version := archiveMetadataVersion("metadata")
	plan, err := root.PrepareRestore(t.Context(), ArchiveDirectory+"/container/file", "target", version)
	if plan != nil || !errors.Is(err, ErrArchiveUnsupported) {
		t.Fatal(plan, err)
	}
	// A valid plan cannot be obtained on this platform; exercise the platform
	// boundary directly to ensure it cannot fall back to the Windows Move API.
	result, err := restoreWithinRoot(t.Context(), root, nil)
	if result.Outcome != PublishUnchanged || !errors.Is(err, ErrArchiveUnsupported) {
		t.Fatal(result, err)
	}
}
