package fileeffects

import (
	"strings"
	"testing"
)

func TestArchivePurgeEffectContract(t *testing.T) {
	for _, kind := range []string{"file", "directory", "link"} {
		e := New("purge_archive", ArchiveDirectory+"/chosen", ArchiveDirectory+"/chosen", kind)
		e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
		if e.Validate() != nil || !e.IsArchivePurge() || e.SchemaVersion != 4 || e.ReferencePath() != e.Source.Path || e.ReferenceKind() != "purge_"+kind || e.Target.Kind != "absent" {
			t.Fatal(e)
		}
		if !e.Affects(e.Source.Path, "entry_metadata") || !e.Affects(".", "directory_listing") || e.Affects("unselected", "file") || e.Affects(e.Source.Path, e.ReferenceKind()) {
			t.Fatal("invalidated wrong evidence", e)
		}
		for _, mutate := range []func(*Effect){func(x *Effect) { x.SchemaVersion = 1 }, func(x *Effect) { x.SchemaVersion = 2 }, func(x *Effect) { x.SchemaVersion = 3 }, func(x *Effect) { x.SchemaVersion = 5 }, func(x *Effect) { x.Source.Path = ArchiveDirectory }, func(x *Effect) { x.Source.Path = ".EDU-AGENT-ARCHIVE/chosen" }, func(x *Effect) { x.Source.Path = "ordinary" }, func(x *Effect) { x.Source.Path = ArchiveDirectory + "/../ordinary" }, func(x *Effect) { x.Target.Path += "/other" }, func(x *Effect) { x.Target.Kind = kind }, func(x *Effect) { x.Target.Version = e.Source.Version }, func(x *Effect) { x.Source.Version = "" }, func(x *Effect) { x.Source.Version = "sha256:" + strings.Repeat("a", 64) }, func(x *Effect) { x.Source.Kind = "other" }, func(x *Effect) { x.Directories.Count = 1 }} {
			bad := e
			mutate(&bad)
			if bad.Validate() == nil {
				t.Fatal("accepted invalid purge", bad)
			}
		}
	}
	if New("copy", "a", "b", "file").SchemaVersion != 1 || New("copy", "a", "b", "directory").SchemaVersion != 2 || New("restore_archive", ArchiveDirectory+"/c/a", "b", "file").SchemaVersion != 3 {
		t.Fatal("changed older constructor versions")
	}
}
