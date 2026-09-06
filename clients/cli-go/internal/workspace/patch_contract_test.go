package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchPlanByteConventions(t *testing.T) {
	for _, tt := range []struct{ name, before, hunk, want string }{
		{"BOM CRLF and missing LF", "\ufeffa\r\nb\nc\r\nlast", "@@\n a\n-b\n+B\n c\n@@\n-last\n+LAST\n", "\ufeffa\r\nB\r\nc\r\nLAST"},
		{"mixed untouched", "a\r\nb\nc\r\nlast\n", "@@\n-b\n+B\n", "a\r\nB\nc\r\nlast\n"},
		{"replace unterminated", "old", "@@\n-old\n+new\n", "new"},
		{"replace with two lines", "old", "@@\n-old\n+first\n+last\n", "first\nlast"},
		{"EOF insert", "a", "@@\n+b\n*** End of File\n", "a\nb"},
		{"unterminated context insert", "a", "@@\n a\n+b\n*** End of File\n", "a\nb"},
		{"insert empty", "", "@@\n+b\n*** End of File\n", "b\n"},
		{"insert BOM only", "\ufeff", "@@\n+b\n*** End of File\n", "\ufeffb\n"},
		{"delete all leaving BOM", "\ufeffold", "@@\n-old\n", "\ufeff"},
		{"delete unterminated tail", "a\nb", "@@\n-b\n*** End of File\n", "a\n"},
		{"preserve final context", "a\nb", "@@\n-a\n+A\n b\n", "A\nb"},
		{"context insertion", "a\nb", "@@\n a\n+x\n b\n", "a\nx\nb"},
		{"EOF disambiguates", "a\na\n", "@@\n-a\n+b\n*** End of File\n", "a\nb\n"},
		{"literal whitespace", " a \n", "@@\n- a \n+ b \n", " b \n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"a": tt.before})
			p := patchPlanPrepare(t, w, patchPlanArgs(t, "*** Update File: a\n"+tt.hunk, map[string]string{"a": contentHash([]byte(tt.before))}))
			items, err := p.ClaimPatchItems()
			if err != nil {
				t.Fatal(err)
			}
			if got := applyCompleteDiffForTest(t, tt.before, p.FullDiff()); got != tt.want || string(items[0].candidate) != tt.want {
				t.Fatalf("got=%q candidate=%q want=%q", got, items[0].candidate, tt.want)
			}
			if r := w.CommitMutation(t.Context(), items[0]); r.Publication != PublicationCompleted {
				t.Fatalf("commit: %+v", r)
			}
			patchPlanDisk(t, root, "a", tt.want)
		})
	}
	w, root := patchPlanWorkspace(t, DefaultLimits(), nil)
	text := "*** Begin Patch\r\n*** Add File: a\r\n+x\r\n+\r\n*** End Patch\r\n"
	p := patchPlanPrepare(t, w, completeDiffJSON(t, patchArguments{Patch: text, ExpectedHashes: map[string]string{}}))
	items, _ := p.ClaimPatchItems()
	if string(items[0].candidate) != "x\n\n" {
		t.Fatal("CRLF transport changed add default LF")
	}
	patchPlanAbsent(t, root, "a")
}

func TestPatchPlanClaimIntegrity(t *testing.T) {
	w, root := patchPlanWorkspace(t, DefaultLimits(), nil)
	raw := patchPlanArgs(t, "*** Add File: a\n+x\n", nil)
	for _, mutate := range []func(*MutationPresentation){
		func(p *MutationPresentation) { p.Tool = ToolEdit }, func(p *MutationPresentation) { p.Path = "elsewhere" },
		func(p *MutationPresentation) { p.Preview += "changed" }, func(p *MutationPresentation) { p.Operation = "edit" },
		func(p *MutationPresentation) { p.Truncated = !p.Truncated }, func(p *MutationPresentation) { p.ArchivePath = "fake" },
	} {
		p := patchPlanPrepare(t, w, raw)
		original := p.Presentation
		mutate(&p.Presentation)
		if items, err := p.ClaimPatchItems(); err == nil || items != nil {
			t.Fatal("tampered plan claimed")
		}
		p.Presentation = original
		if _, err := p.ClaimPatchItems(); err == nil {
			t.Fatal("failed claim did not consume token")
		}
	}
	if (*PreparedMutation)(nil).IsPatch() {
		t.Fatal("nil is patch")
	}
	if _, err := (*PreparedMutation)(nil).ClaimPatchItems(); err == nil {
		t.Fatal("nil claim")
	}
	if _, err := (&PreparedMutation{}).ClaimPatchItems(); err == nil {
		t.Fatal("ordinary claim")
	}
	p := patchPlanPrepare(t, w, raw)
	items, err := p.ClaimPatchItems()
	if err != nil {
		t.Fatal(err)
	}
	child := items[0]
	items[0] = nil
	if p.patchItems[0] != child {
		t.Fatal("claim exposed owned slice")
	}
	patchPlanAbsent(t, root, "a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r := w.CommitMutation(ctx, child); resultCode(t, r) != CodeCancelled || r.Publication != PublicationUnchanged {
		t.Fatal("cancelled child published")
	}
	patchPlanAbsent(t, root, "a")
}

func TestPatchPlanRevalidateAfterAuthorization(t *testing.T) {
	for _, action := range []string{"Update", "Delete", "Add"} {
		t.Run(action, func(t *testing.T) {
			files := map[string]string{"a": "old\n"}
			body := "*** " + action + " File: a\n"
			hashes := map[string]string{"a": contentHash([]byte("old\n"))}
			if action == "Update" {
				body += "@@\n-old\n+new\n"
			}
			if action == "Add" {
				files = nil
				hashes = nil
				body += "+new\n"
			}
			w, root := patchPlanWorkspace(t, DefaultLimits(), files)
			p := patchPlanPrepare(t, w, patchPlanArgs(t, body, hashes))
			items, _ := p.ClaimPatchItems()
			if err := os.WriteFile(filepath.Join(root, "a"), []byte("changed\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if action == "Delete" {
				// Keep the metadata guard current to independently falsify a
				// missing patch-only raw-hash check (no metadata/hash conflation).
				entry, err := w.root.InspectArchiveSource(t.Context(), "a")
				if err != nil {
					t.Fatal(err)
				}
				items[0].archiveEntry = &entry
				items[0].baseVersion = entry.Version
				items[0].Presentation.BaseVersion = entry.Version
			}
			r := w.CommitMutation(t.Context(), items[0])
			want := CodeContentChanged
			if action == "Add" {
				want = CodeAlreadyExists
			}
			if resultCode(t, r) != want || r.Publication != PublicationUnchanged || r.Effect != nil {
				t.Fatalf("revalidation: %+v", r)
			}
			patchPlanDisk(t, root, "a", "changed\n")
			patchPlanAbsent(t, root, ArchiveDirectory)
			if action == "Delete" && !strings.HasPrefix(items[0].baseVersion, "entry-v1:") {
				t.Fatal("delete version changed format")
			}
		})
	}
}
