package fileeffects

import (
	"strings"
	"testing"
)

func directoryCopyEffectForTest() Effect {
	e := New("copy", "input/tree", "output/tree", "directory")
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	return e
}

func TestDirectoryCopyEffectRootContract(t *testing.T) {
	e := directoryCopyEffectForTest()
	if e.SchemaVersion != 2 || !e.IsDirectoryCopy() || e.Validate() != nil || e.ReferenceKind() != "copy" || e.ReferencePath() != e.Target.Path || len(e.PlannedPaths()) != 0 || len(e.CreatedPaths()) != 0 {
		t.Fatalf("invalid directory-copy root: %+v", e)
	}
	for _, test := range []struct {
		name string
		edit func(*Effect)
	}{
		{"legacy-version", func(e *Effect) { e.SchemaVersion = 1 }},
		{"future-version", func(e *Effect) { e.SchemaVersion = 3 }},
		{"entry-scope", func(e *Effect) { e.Scope = "entry" }},
		{"source-file", func(e *Effect) { e.Source.Kind = "file" }},
		{"target-file", func(e *Effect) { e.Target.Kind = "file" }},
		{"source-empty-version", func(e *Effect) { e.Source.Version = "" }},
		{"source-hash", func(e *Effect) { e.Source.Version = "sha256:" + strings.Repeat("a", 64) }},
		{"source-short-version", func(e *Effect) { e.Source.Version = "entry-v1:a" }},
		{"source-nonhex-version", func(e *Effect) { e.Source.Version = "entry-v1:" + strings.Repeat("z", 64) }},
		{"target-hash", func(e *Effect) { e.Target.Version = "sha256:" + strings.Repeat("b", 64) }},
		{"target-entry-version", func(e *Effect) { e.Target.Version = e.Source.Version }},
		{"source-absolute", func(e *Effect) { e.Source.Path = "/tree" }},
		{"target-traversal", func(e *Effect) { e.Target.Path = "output/../tree" }},
		{"protected-source", func(e *Effect) { e.Source.Path = ".EDU-AGENT-ARCHIVE/tree" }},
		{"protected-target", func(e *Effect) { e.Target.Path = ArchiveDirectory + "/tree" }},
		{"self", func(e *Effect) { e.Target.Path = e.Source.Path }},
		{"alias", func(e *Effect) { e.Target.Path = "INPUT/TREE" }},
		{"descendant", func(e *Effect) { e.Target.Path = "input/tree/child" }},
		{"folded-descendant", func(e *Effect) { e.Target.Path = "INPUT/TREE/child" }},
		{"unicode-folded-descendant", func(e *Effect) { e.Source.Path, e.Target.Path = "input/Σ", "INPUT/ς/child" }},
		{"planned-mkdir", func(e *Effect) { e.Directories = DirectoryChain{Anchor: "output", Count: 1} }},
		{"created-prefix", func(e *Effect) { e.Directories.Created = 1 }},
		{"empty-chain-anchor", func(e *Effect) { e.Directories.Anchor = "." }},
		{"move-v2", func(e *Effect) { e.Operation = "move" }},
		{"mkdir-v2", func(e *Effect) { e.Operation = "mkdir" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := e
			test.edit(&bad)
			if bad.Validate() == nil {
				t.Fatalf("invalid root accepted: %+v", bad)
			}
		})
	}
}

func TestDirectoryCopyEffectScopeAndPlan(t *testing.T) {
	e := directoryCopyEffectForTest()
	for _, kind := range []string{"file", "entry_metadata", "directory_listing", "find_result", "search_result"} {
		if !e.Affects("output/tree/sub/file", kind) || e.Affects("input/tree/sub/file", kind) || e.Affects("output/tree-sibling", kind) {
			t.Fatalf("incorrect subtree invalidation for %s", kind)
		}
	}
	for _, kind := range []string{"entry_metadata", "directory_listing", "find_result", "search_result"} {
		if !e.Affects("output", kind) || !e.Affects(".", kind) {
			t.Fatalf("parent observation not invalidated: %s", kind)
		}
	}
	for _, kind := range []string{"copy", "mkdir", "file_effect"} {
		if e.Affects("output/tree", kind) {
			t.Fatalf("historical receipt invalidated: %s", kind)
		}
	}
	if !e.SamePlan(e) {
		t.Fatal("identical root plan differed")
	}
	for _, edit := range []func(*Effect){
		func(e *Effect) { e.Source.Version = "entry-v1:" + strings.Repeat("b", 64) },
		func(e *Effect) { e.Source.Path = "other/source" },
		func(e *Effect) { e.Target.Path = "other/target" },
		func(e *Effect) { e.SchemaVersion = 1 },
	} {
		other := e
		edit(&other)
		if e.SamePlan(other) {
			t.Fatalf("different root plan accepted: %+v", other)
		}
	}
}

func TestDirectoryCopyPreservesV1Effects(t *testing.T) {
	copy := New("copy", "file", "FILE", "file") // v1 spelling semantics stay frozen.
	copy.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	copy.Target.Version = "sha256:" + strings.Repeat("b", 64)
	mkdir := New("mkdir", "", "parent/child", "directory")
	mkdir.Directories = DirectoryChain{Anchor: ".", Count: 2, Created: 1}
	move := New("move", "tree", "TREE/child", "directory")
	move.Source.Version = copy.Source.Version
	archive := New("archive", "tree", ArchiveDirectory+"/stamp-id/tree", "directory")
	for _, e := range []Effect{copy, mkdir, move, archive, New("write_create", "", "file", "file"), New("write_replace", "", "file", "file"), New("edit", "", "file", "file")} {
		if e.SchemaVersion != 1 || e.IsDirectoryCopy() || e.Validate() != nil {
			t.Fatalf("v1 behavior changed: %+v", e)
		}
		e.SchemaVersion = 2
		if e.IsDirectoryCopy() || e.Validate() == nil {
			t.Fatalf("non-tree-copy effect gained v2: %+v", e)
		}
	}
}
