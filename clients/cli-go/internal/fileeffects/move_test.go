package fileeffects

import (
	"strings"
	"testing"
)

func TestMoveEffectsBothEndpointsAndHistoricalFacts(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		e := New("move", "old/source", "new/target", kind)
		e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
		if e.Validate() != nil || e.ReferencePath() != "old/source" || e.ReferenceKind() != "move_"+kind {
			t.Fatal(e)
		}
		for _, path := range []string{"old/source", "new/target"} {
			for _, observation := range []string{"file", "entry_metadata", "directory_listing", "find_result", "search_result"} {
				if !e.Affects(path, observation) {
					t.Fatal("not affected", path, observation)
				}
			}
		}
		for _, path := range []string{".", "old", "new"} {
			for _, observation := range []string{"entry_metadata", "directory_listing", "find_result", "search_result"} {
				if !e.Affects(path, observation) {
					t.Fatal("parent not affected", path, observation)
				}
			}
		}
		for _, path := range []string{"old/source/child", "new/target/child"} {
			if e.Affects(path, "file") != (kind == "directory") {
				t.Fatal("wrong subtree", path, kind)
			}
		}
		for _, fact := range []string{"archive_file", "archive_directory", "copy", "mkdir", "move_file", "move_directory", "file_effect"} {
			if e.Affects("old/source", fact) {
				t.Fatal("fact invalidated", fact)
			}
		}
		if e.Affects("unrelated", "file") {
			t.Fatal("unrelated invalidated")
		}
		bad := e
		bad.Source.Version = ""
		if bad.Validate() == nil || bad.SamePlan(e) {
			t.Fatal("missing frozen version")
		}
		bad = e
		bad.Target.Version = e.Source.Version
		if bad.Validate() == nil {
			t.Fatal("metadata fabricated at destination")
		}
		bad.Target.Version = "sha256:" + strings.Repeat("b", 64)
		if bad.Validate() == nil {
			t.Fatal("body hash fabricated")
		}
		bad = e
		bad.Source.Path = ArchiveDirectory + "/x"
		if bad.Validate() == nil {
			t.Fatal("archive accepted")
		}
		bad = e
		bad.Target.Path = e.Source.Path
		if bad.Validate() == nil {
			t.Fatal("no-op became effect")
		}
	}
}
