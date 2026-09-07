//go:build linux || darwin

package localexec

import (
	"context"
	"testing"
)

func TestPersistentOutputAccessFenceIncludesUnsavedCacheAndEOF(t *testing.T) {
	for _, saveFailure := range []bool{false, true} {
		name := "saved"
		if saveFailure {
			name = "initial_metadata_unsaved"
		}
		t.Run(name, func(t *testing.T) {
			store := newByteArtifactStore()
			if saveFailure {
				store.beforeWrite = func(context.Context, string, []byte) error { return failure("output_store_full") }
			}
			m := testManager(t, Options{OutputBytesPerTask: 64, OutputBytesTotal: 256})
			bindOutput(t, m, "owner", store)
			s := fileOutput(t, m, "owner", "guarded", []byte("private-prefix"), nil)
			page, err := m.Read("owner", s.TaskID, "stdout", 0, 64)
			if err != nil || string(page.Data) != "private-prefix" {
				t.Fatalf("valid authority lost memory output: %+v %v", page, err)
			}
			if saveFailure && s.StdoutSaved != 0 {
				t.Fatalf("failed initial metadata claimed saved bytes: %+v", s)
			}
			store.mu.Lock()
			lists := store.lists
			store.accessError = failure("output_unavailable")
			store.mu.Unlock()
			for _, offset := range []int64{0, s.StdoutBytes} {
				page, err = m.Read("owner", s.TaskID, "stdout", offset, 32)
				requireCode(t, err, "output_unavailable")
				if len(page.Data) != 0 || page.NextOffset != 0 {
					t.Fatalf("revoked read returned bytes/cursor: %+v", page)
				}
				found, searchErr := m.Search(t.Context(), "owner", s.TaskID, "stdout", "private", offset, 1)
				requireCode(t, searchErr, "output_unavailable")
				if len(found.Offsets) != 0 || found.NextOffset != 0 {
					t.Fatalf("revoked search returned matches/cursor: %+v", found)
				}
			}
			if err = m.BindArchive("owner", nil); err != nil {
				t.Fatal(err)
			}
			_, err = m.Read("owner", s.TaskID, "stdout", 0, 32)
			requireCode(t, err, "output_unavailable")
			fresh := fileOutput(t, m, "owner", "memory-only", []byte("new-unsaved"), nil)
			page, err = m.Read("owner", fresh.TaskID, "stdout", 0, 32)
			if err != nil || string(page.Data) != "new-unsaved" || fresh.StdoutSaved != 0 {
				t.Fatalf("old authority blocked new memory-only task: %+v %v", page, err)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.lists != lists {
				t.Fatal("output access fence scanned the artifact directory")
			}
		})
	}
}

func TestPersistentOutputRevocationDoesNotBlockSafeStop(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{})
	bindOutput(t, m, "owner", store)
	s, err := m.Start(t.Context(), "owner", "live", StartArgs{CWD: t.TempDir(), Shell: "/bin/sh", Command: "cat", Stdin: true})
	if err != nil || !s.Controllable {
		t.Fatalf("start: %+v %v", s, err)
	}
	store.mu.Lock()
	store.accessError = failure("output_unavailable")
	store.mu.Unlock()
	_, err = m.Read("owner", s.TaskID, "stdout", 0, 32)
	requireCode(t, err, "output_unavailable")
	stopped, err := m.Stop(t.Context(), "owner", s.TaskID)
	if err != nil || stopped.Controllable || stopped.State != StateCanceled {
		t.Fatalf("privacy revocation prevented safe stop: %+v %v", stopped, err)
	}
}
