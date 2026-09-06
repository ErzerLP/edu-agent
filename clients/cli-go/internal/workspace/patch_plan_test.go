package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func patchPlanArgs(t *testing.T, body string, hashes map[string]string) string {
	t.Helper()
	if hashes == nil {
		hashes = map[string]string{}
	}
	return completeDiffJSON(t, patchArguments{Patch: "*** Begin Patch\n" + body + "*** End Patch", ExpectedHashes: hashes})
}

func patchPlanWorkspace(t *testing.T, limits Limits, files map[string]string) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	for path, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(text), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	w, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, root
}

func patchPlanPrepare(t *testing.T, w *Workspace, raw string) *PreparedMutation {
	t.Helper()
	p, result := w.PrepareMutation(t.Context(), ToolPatch, raw)
	if p == nil || result.Value != nil || !p.IsPatch() {
		t.Fatalf("prepare: %+v", result)
	}
	return p
}

func patchPlanReject(t *testing.T, w *Workspace, raw string, code string) {
	t.Helper()
	p, result := w.PrepareMutation(t.Context(), ToolPatch, raw)
	if p != nil || resultCode(t, result) != code || result.Publication != PublicationUnchanged || result.Effect != nil {
		t.Fatalf("wanted %s rejection, got prepared=%v result=%+v", code, p != nil, result)
	}
}

func patchPlanDisk(t *testing.T, root, path, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil || string(data) != want {
		t.Fatalf("%s bytes=%q want=%q err=%v", path, data, want, err)
	}
}

func patchPlanAbsent(t *testing.T, root, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
		t.Fatalf("%s unexpectedly exists: %v", path, err)
	}
}

func TestPatchPlanMultiFile(t *testing.T) {
	var source strings.Builder
	for i := 0; i < 45; i++ {
		fmt.Fprintf(&source, "line%02d\n", i)
	}
	before := source.String()
	want := strings.NewReplacer("line02\n", "TWO\nextra\n", "line20\n", "", "line42\n", "FORTY-TWO\n").Replace(before)
	deleted := "\ufeffremove\r\nlast"
	w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"old.txt": before, "gone.txt": deleted})
	body := "*** Add File: new/child.txt\n+new\n+tail\n*** Update File: old.txt\n@@\n-line02\n+TWO\n+extra\n@@\n-line20\n@@\n-line42\n+FORTY-TWO\n*** Delete File: gone.txt\n*** Add File: empty.txt\n"
	p := patchPlanPrepare(t, w, patchPlanArgs(t, body, map[string]string{"old.txt": contentHash([]byte(before)), "gone.txt": contentHash([]byte(deleted))}))
	if p.Presentation.Tool != ToolPatch || p.Presentation.Operation != ToolPatch || p.Presentation.Path != "." || p.Presentation.PreviewKind != "diff" {
		t.Fatal("outer presentation contract")
	}
	patchPlanDisk(t, root, "old.txt", before)
	patchPlanDisk(t, root, "gone.txt", deleted)
	for _, path := range []string{"new", "empty.txt", ArchiveDirectory} {
		patchPlanAbsent(t, root, path)
	}
	if result := w.CommitMutation(t.Context(), p); resultCode(t, result) != CodeInvalidArguments || result.Publication != PublicationUnchanged || result.Effect != nil {
		t.Fatalf("outer commit published: %+v", result)
	}
	items, err := p.ClaimPatchItems()
	if err != nil || len(items) != 4 {
		t.Fatalf("claim: items=%d err=%v", len(items), err)
	}
	if again, err := p.ClaimPatchItems(); again != nil || err == nil {
		t.Fatal("second claim accepted")
	}
	var aggregate strings.Builder
	for i, item := range items {
		aggregate.WriteString(item.FullDiff())
		original, candidate := "", string(item.candidate)
		if i == 1 {
			original = before
			if candidate != want || strings.Count(item.FullDiff(), "\n@@ ") != 3 {
				t.Fatal("distant hunk candidate/diff")
			}
		}
		if i == 2 {
			original = deleted
		}
		diff, _, _ := strings.Cut(item.FullDiff(), "# Archived to: ")
		if applied := applyCompleteDiffForTest(t, original, diff); applied != candidate {
			t.Fatalf("item %d diff does not reconstruct candidate", i)
		}
	}
	if aggregate.String() != p.FullDiff() || !strings.Contains(p.Presentation.Preview, items[2].archivePath) || !strings.Contains(items[2].FullDiff(), "+++ /dev/null\n") {
		t.Fatal("aggregate omitted complete data/actual archive destination")
	}
	if items[2].archiveContentHash != contentHash([]byte(deleted)) || !strings.HasPrefix(items[2].Presentation.BaseVersion, "entry-v1:") {
		t.Fatal("delete content hash/entry version confused")
	}
	if effect := items[2].FileEffect(); !strings.HasPrefix(effect.Source.Version, "entry-v1:") {
		t.Fatalf("archive effect version: %+v", effect)
	}
	w.limits.FileBytes, w.limits.EditFileBytes, w.limits.PatchBytes, w.limits.DiffBytes = 1, 1, 1, 1
	for _, item := range items {
		if result := w.CommitMutation(t.Context(), item); result.Publication != PublicationCompleted || result.Effect == nil {
			t.Fatalf("child commit: %+v", result)
		}
		if result := w.CommitMutation(t.Context(), item); result.Publication != PublicationUnchanged {
			t.Fatal("child committed twice")
		}
	}
	patchPlanDisk(t, root, "new/child.txt", "new\ntail\n")
	patchPlanDisk(t, root, "old.txt", want)
	patchPlanDisk(t, root, "empty.txt", "")
	patchPlanAbsent(t, root, "gone.txt")
	patchPlanDisk(t, root, items[2].archivePath, deleted)
	if info, err := os.Stat(filepath.Join(root, "old.txt")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatal("update changed permissions")
	}
}
