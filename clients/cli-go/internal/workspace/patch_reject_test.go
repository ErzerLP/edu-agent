package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchPlanPreflightRejects(t *testing.T) {
	before := "alpha\nbeta\ngamma\nrepeat\nrepeat\nlast"
	for _, tt := range []struct{ name, body, code string }{
		{"missing", "@@\n-absent\n+new\n", CodeReplacementMissing},
		{"not unique", "@@\n-repeat\n+new\n", CodeReplacementNotUnique},
		{"no whitespace trim", "@@\n-alpha \n+new\n", CodeReplacementMissing},
		{"no fuzzy", "@@\n-ALPHA\n+new\n", CodeReplacementMissing},
		{"overlap", "@@\n alpha\n-beta\n+B\n@@\n-beta\n+C\n", CodeReplacementOverlap},
		{"duplicate hunk", "@@\n-alpha\n+A\n@@\n-alpha\n+B\n", CodeReplacementOverlap},
		{"out of order", "@@\n-gamma\n+G\n@@\n-alpha\n+A\n", CodeReplacementOverlap},
		{"same original", "@@\n-alpha\n+A\n@@\n-A\n+again\n", CodeReplacementMissing},
		{"wrong EOF", "@@\n-alpha\n+A\n*** End of File\n", CodeReplacementMissing},
		{"no changes", "@@\n-alpha\n+alpha\n", CodeNoChanges},
		{"unanchored insertion", "@@\n+extra\n", CodeInvalidPatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"a": before})
			raw := patchPlanArgs(t, "*** Add File: new/first\n+new\n*** Update File: a\n"+tt.body, map[string]string{"a": contentHash([]byte(before))})
			patchPlanReject(t, w, raw, tt.code)
			patchPlanDisk(t, root, "a", before)
			patchPlanAbsent(t, root, "new")
			patchPlanAbsent(t, root, ArchiveDirectory)
		})
	}
	w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"a": before})
	body := "*** Add File: new\n+x\n*** Update File: a\n@@\n-alpha\n+A\n"
	patchPlanReject(t, w, patchPlanArgs(t, body, map[string]string{"a": contentHash([]byte("stale"))}), CodeContentChanged)
	for _, hashes := range []map[string]string{nil, {"./a": contentHash([]byte(before))}, {"a": contentHash([]byte(before)), "unused": contentHash(nil)}, {"a": contentHash([]byte(before)), "new": contentHash(nil)}} {
		patchPlanReject(t, w, patchPlanArgs(t, body, hashes), CodeInvalidArguments)
	}
	patchPlanDisk(t, root, "a", before)
	patchPlanAbsent(t, root, "new")
}

func TestPatchPlanPathConflicts(t *testing.T) {
	for _, paths := range [][2]string{{"a", "a"}, {"a", "A"}, {"a", "a/b"}, {"a/b", "a"}, {"Dir/a", "dir/b"}, {"K/a", "K/b"}} {
		w, root := patchPlanWorkspace(t, DefaultLimits(), nil)
		body := "*** Add File: " + paths[0] + "\n+x\n*** Add File: " + paths[1] + "\n+y\n"
		patchPlanReject(t, w, patchPlanArgs(t, body, nil), CodePatchPathConflict)
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("path conflict modified disk")
		}
	}
	for _, path := range []string{"../outside", "/absolute", "./a", "a//b", "a/../b", "a\\b"} {
		w, root := patchPlanWorkspace(t, DefaultLimits(), nil)
		p, r := w.PrepareMutation(t.Context(), ToolPatch, patchPlanArgs(t, "*** Add File: "+path+"\n+x\n", nil))
		if p != nil || (resultCode(t, r) != CodeInvalidPath && resultCode(t, r) != CodePathOutsideWorkspace) {
			t.Fatalf("invalid path accepted: %q %+v", path, r)
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Fatal("invalid path modified disk")
		}
	}
}

func TestPatchPlanUnsafeSources(t *testing.T) {
	for _, tt := range []struct{ name, text, code string }{
		{"binary", "a\x00b", CodeBinaryFile}, {"UTF8", "a\xffb", CodeInvalidUTF8}, {"controls", "a\x01b", CodeBinaryFile},
	} {
		for _, action := range []string{"Update", "Delete"} {
			t.Run(tt.name+action, func(t *testing.T) {
				w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"a": tt.text})
				body := "*** Add File: new\n+x\n*** " + action + " File: a\n"
				if action == "Update" {
					body += "@@\n-a\n+b\n"
				}
				patchPlanReject(t, w, patchPlanArgs(t, body, map[string]string{"a": contentHash([]byte(tt.text))}), tt.code)
				patchPlanDisk(t, root, "a", tt.text)
				patchPlanAbsent(t, root, "new")
				patchPlanAbsent(t, root, ArchiveDirectory)
			})
		}
	}
	w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"target": "old\n", ArchiveDirectory + "/saved": "old\n"})
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ArchiveDirectory, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"Add", "Update", "Delete"} {
		for _, path := range []string{"link", ArchiveDirectory + "/saved", "alias/saved"} {
			body := "*** Add File: new\n+x\n*** " + action + " File: " + path + "\n"
			hashes := map[string]string{}
			if action == "Add" {
				body += "+new\n"
			} else {
				hashes[path] = contentHash([]byte("old\n"))
				if action == "Update" {
					body += "@@\n-old\n+new\n"
				}
			}
			p, r := w.PrepareMutation(t.Context(), ToolPatch, patchPlanArgs(t, body, hashes))
			code := resultCode(t, r)
			if p != nil || (code != CodeLinkNotAllowed && code != CodeNotDirectory && code != CodeArchiveProtected) {
				t.Fatalf("unsafe %s %s: %+v", action, path, r)
			}
		}
	}
	patchPlanReject(t, w, patchPlanArgs(t, "*** Delete File: "+ArchiveDirectory+"\n", map[string]string{ArchiveDirectory: contentHash(nil)}), CodeArchiveProtected)
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	patchPlanReject(t, w, patchPlanArgs(t, "*** Delete File: directory\n", map[string]string{"directory": contentHash(nil)}), CodeUnsupportedType)
	patchPlanAbsent(t, root, "new")
	patchPlanDisk(t, root, "target", "old\n")
	patchPlanDisk(t, root, ArchiveDirectory+"/saved", "old\n")
	entries, _ := os.ReadDir(filepath.Join(root, ArchiveDirectory))
	if len(entries) != 1 || !strings.EqualFold(entries[0].Name(), "saved") {
		t.Fatal("unsafe plan created archive entry")
	}
}
