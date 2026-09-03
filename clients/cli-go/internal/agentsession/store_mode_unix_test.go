//go:build !windows

package agentsession

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentSessionUnixStorageModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent-sessions")
	store := openTestStore(t, root, &memorySecretBackend{}, Limits{})
	handle, _, err := store.Create(t.Context(), CreateInput{Title: "mode evidence", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("session root mode=%v err=%v", modeOf(info), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("unprotected session entry %q mode=%v", entry.Name(), info.Mode())
		}
	}
}

func modeOf(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}
