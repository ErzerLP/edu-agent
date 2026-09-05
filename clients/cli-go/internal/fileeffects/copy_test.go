package fileeffects

import (
	"strings"
	"testing"
)

func TestCopyFileEffectTargetOnlyAndSourceVersionPlan(t *testing.T) {
	e := New("copy", "input/source", "output/target", "file")
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if e.ReferencePath() != "output/target" || e.ReferenceKind() != "copy" {
		t.Fatal(e)
	}
	for _, kind := range []string{"file", "entry_metadata", "directory_listing", "find_result", "search_result"} {
		if e.Affects("input/source", kind) || e.Affects("input", kind) {
			t.Fatal("source invalidated", kind)
		}
		if !e.Affects("output/target", kind) {
			t.Fatal("target not invalidated", kind)
		}
	}
	for _, kind := range []string{"entry_metadata", "directory_listing", "find_result", "search_result"} {
		if !e.Affects("output", kind) || !e.Affects(".", kind) {
			t.Fatal("parent/scope not invalidated", kind)
		}
	}
	if e.Affects("output/target", "copy") || e.Affects("output/target", "file_effect") {
		t.Fatal("operation fact invalidated")
	}
	completed := e
	completed.Target.Version = "sha256:" + strings.Repeat("b", 64)
	if !e.SamePlan(completed) || completed.Validate() != nil {
		t.Fatal("completion not same plan")
	}
	for _, modify := range []func(*Effect){func(v *Effect) { v.Source.Version = "entry-v1:" + strings.Repeat("c", 64) }, func(v *Effect) { v.Source.Path = "other" }, func(v *Effect) { v.Target.Path = "other" }} {
		bad := e
		modify(&bad)
		if e.SamePlan(bad) {
			t.Fatal("accepted different plan")
		}
	}
	for _, modify := range []func(*Effect){func(v *Effect) { v.Source.Version = "" }, func(v *Effect) { v.Target.Path = ArchiveDirectory + "/copy" }, func(v *Effect) { v.Source.Path = ArchiveDirectory + "/source" }, func(v *Effect) { v.Scope = "subtree" }, func(v *Effect) { v.Directories = DirectoryChain{Anchor: ".", Count: 1} }, func(v *Effect) { v.Target.Kind = "directory" }} {
		bad := e
		modify(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid copy effect accepted", bad)
		}
	}
}
