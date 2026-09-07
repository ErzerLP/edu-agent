//go:build windows

package securefile

import (
	"errors"
	"testing"
)

func TestArchivePurgeUnsupportedPlatform(t *testing.T) {
	root, _ := archiveTestRoot(t)
	plan, err := root.PreparePurge(t.Context(), ArchiveDirectory+"/container", archiveMetadataVersion("metadata"), PurgeLimits{Entries: 10, PlanBytes: 1 << 20})
	if plan != nil || !errors.Is(err, ErrArchiveUnsupported) {
		t.Fatal(plan, err)
	}
	result, err := purgeWithinRoot(t.Context(), root, nil, PurgeObserver{}, PurgeResult{Outcome: PublishUnchanged})
	if result.Outcome != PublishUnchanged || !errors.Is(err, ErrArchiveUnsupported) {
		t.Fatal(result, err)
	}
}
