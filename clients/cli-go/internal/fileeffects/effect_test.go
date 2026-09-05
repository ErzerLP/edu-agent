package fileeffects

import (
	"reflect"
	"strings"
	"testing"
)

func TestFileEffectBoundedPlanAndScope(t *testing.T) {
	e := New("mkdir", "", "existing/a/b", "directory")
	e.Directories = DirectoryChain{Anchor: "existing", Count: 2, Created: 1}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.PlannedPaths(), []string{"existing/a", "existing/a/b"}) || !reflect.DeepEqual(e.CreatedPaths(), []string{"existing/a"}) {
		t.Fatalf("plan=%+v", e)
	}
	for _, kind := range []string{"file", "entry_metadata", "directory_listing", "find_result", "search_result"} {
		if !e.Affects("existing/a/b", kind) {
			t.Fatalf("missing %s", kind)
		}
	}
	for _, kind := range []string{"entry_metadata", "directory_listing", "find_result", "search_result"} {
		if !e.Affects("existing", kind) {
			t.Fatalf("missing parent %s", kind)
		}
	}
	for _, kind := range []string{"archive_directory", "mkdir", "file_effect"} {
		if e.Affects("existing/a", kind) {
			t.Fatalf("invalidated fact %s", kind)
		}
	}
	if e.Affects("sibling", "file") {
		t.Fatal("invalidated unrelated file")
	}
	for _, mutate := range []func(*Effect){
		func(e *Effect) { e.Directories.Created = 3 }, func(e *Effect) { e.Directories.Count = 65 }, func(e *Effect) { e.Directories.Anchor = "unrelated" },
		func(e *Effect) { e.Target.Path = "/outside" }, func(e *Effect) { e.Target.Path = ".edu-agent-archive/a" }, func(e *Effect) { e.Target.Kind = "file" },
		func(e *Effect) { e.Target.Version = "sha256:" + strings.Repeat("a", 64) }, func(e *Effect) { e.Operation = "copy" }, func(e *Effect) { e.Source = Endpoint{Path: "secret", Kind: "file"} },
	} {
		bad := e
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatalf("invalid=%+v", bad)
		}
	}
}
func TestFileEffectVersionKinds(t *testing.T) {
	write := New("write_create", "", " leading/a/b", "file")
	if err := write.Validate(); err != nil {
		t.Fatal("safe workspace spelling rejected after publication", err)
	}
	if !write.Affects(" leading", "directory_listing") {
		t.Fatal("possibly created intermediate parent listing stayed current")
	}
	e := New("archive", "notes", ".edu-agent-archive/stamp-id/notes", "file")
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Source.Version = "sha256:" + strings.Repeat("b", 64)
	if e.Validate() == nil {
		t.Fatal("raw hash accepted as archive metadata")
	}
	e = New("edit", "", "notes", "file")
	e.Target.Version = "sha256:" + strings.Repeat("a", 64)
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Target.Version = "entry-v1:" + strings.Repeat("a", 64)
	if e.Validate() == nil {
		t.Fatal("metadata accepted as raw hash")
	}
}
