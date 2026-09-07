package fileeffects

import (
	"strings"
	"testing"
)

func archiveRestoreEffectForTest(kind string) Effect {
	e := New("restore_archive", ArchiveDirectory+"/legacy-container/old/item", "recovered/item", kind)
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	return e
}

func TestArchiveRestoreEffectContract(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		e := archiveRestoreEffectForTest(kind)
		if e.Validate() != nil || !e.IsArchiveRestore() || e.SchemaVersion != 3 || e.ReferencePath() != e.Source.Path || e.ReferenceKind() != "restore_"+kind {
			t.Fatal(e)
		}
		for _, path := range []string{e.Source.Path, e.Target.Path, ".", "recovered", ArchiveDirectory + "/legacy-container"} {
			if !e.Affects(path, "entry_metadata") {
				t.Fatalf("missing invalidation %s", path)
			}
		}
		if e.Affects("unrelated", "file") || e.Affects(e.Source.Path, "restore_"+kind) || e.Affects(e.Target.Path, "move_"+kind) {
			t.Fatal("unrelated observation or historical fact invalidated")
		}
		if e.Affects(e.Target.Path+"/child", "file") != (kind == "directory") {
			t.Fatal("subtree scope")
		}
		for name, edit := range map[string]func(*Effect){
			"v1":               func(e *Effect) { e.SchemaVersion = 1 },
			"v2":               func(e *Effect) { e.SchemaVersion = 2 },
			"future":           func(e *Effect) { e.SchemaVersion = 4 },
			"move":             func(e *Effect) { e.Operation = "move" },
			"outside":          func(e *Effect) { e.Source.Path = "ordinary/item" },
			"archive-root":     func(e *Effect) { e.Source.Path = ArchiveDirectory },
			"container":        func(e *Effect) { e.Source.Path = ArchiveDirectory + "/legacy-container" },
			"case-alias":       func(e *Effect) { e.Source.Path = ".EDU-AGENT-ARCHIVE/container/item" },
			"protected-target": func(e *Effect) { e.Target.Path = ArchiveDirectory + "/new/item" },
			"case-target":      func(e *Effect) { e.Target.Path = ".EdU-Agent-Archive/new/item" },
			"missing-version":  func(e *Effect) { e.Source.Version = "" },
			"source-hash":      func(e *Effect) { e.Source.Version = "sha256:" + strings.Repeat("b", 64) },
			"target-version":   func(e *Effect) { e.Target.Version = e.Source.Version },
			"target-hash":      func(e *Effect) { e.Target.Version = "sha256:" + strings.Repeat("b", 64) },
			"different-kind":   func(e *Effect) { e.Target.Kind = "link" },
			"scope":            func(e *Effect) { e.Scope = "none" },
			"directory-chain":  func(e *Effect) { e.Directories.Count = 1 },
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				bad := e
				edit(&bad)
				if bad.Validate() == nil {
					t.Fatal("accepted", bad)
				}
			})
		}
	}
	for _, op := range []string{"copy", "move", "archive", "edit"} {
		e := New(op, "source", "target", "file")
		if e.SchemaVersion != 1 {
			t.Fatal("ordinary effect changed", e)
		}
	}
	if New("copy", "source", "target", "directory").SchemaVersion != 2 {
		t.Fatal("directory copy version changed")
	}
}
