//go:build linux

package securefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArchivePurgeLinuxDescriptorsAndNativeGuardsReleased(t *testing.T) {
	root, dir := archiveTestRoot(t)
	count := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	for round := 0; round < 4; round++ {
		archiveTestWrite(t, filepath.Join(dir, purgeTestPath, "nested/file"), []byte("data"))
		plan := purgeTestPlan(t, root, purgeTestPath)
		if got := count(); got != before {
			t.Fatal("prepare retained descriptors", before, got)
		}
		result, err := root.Purge(t.Context(), plan, purgeTestObserver())
		if err != nil || result.Outcome != PublishCompleted {
			t.Fatal(result, err)
		}
		if got := count(); got != before {
			t.Fatal("execution retained descriptors", before, got)
		}
		directoryGuards.mu.Lock()
		idle := directoryGuards.fd == -1 && len(directoryGuards.guards) == 0 && len(directoryGuards.watches) == 0
		directoryGuards.mu.Unlock()
		if !idle {
			t.Fatal("purge retained native guards")
		}
	}
}
