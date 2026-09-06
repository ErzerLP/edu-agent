package workspace

import (
	"fmt"
	"strings"
	"testing"
)

func TestPatchPlanBudgets(t *testing.T) {
	for _, tt := range []struct {
		name, old, next string
		patch, edit     int64
		code            string
	}{
		{"original sum", "xx\n", "y", 5, 64, CodePatchTooLarge},
		{"candidate sum", "x\n", "yy", 5, 64, CodePatchTooLarge},
		{"independent totals", "x\n", "y", 4, 64, ""},
		{"edit original", "xxx\n", "y", 64, 3, CodeFileTooLarge},
		{"edit candidate", "x\n", "yyy", 64, 3, CodeFileTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.PatchBytes = tt.patch
			limits.EditFileBytes = tt.edit
			w, root := patchPlanWorkspace(t, limits, map[string]string{"a": tt.old, "b": tt.old})
			body := ""
			hashes := map[string]string{}
			for _, p := range []string{"a", "b"} {
				body += "*** Update File: " + p + "\n@@\n-" + strings.TrimSuffix(tt.old, "\n") + "\n+" + tt.next + "\n"
				hashes[p] = contentHash([]byte(tt.old))
			}
			raw := patchPlanArgs(t, body, hashes)
			if tt.code != "" {
				patchPlanReject(t, w, raw, tt.code)
			} else {
				patchPlanPrepare(t, w, raw)
			}
			patchPlanDisk(t, root, "a", tt.old)
			patchPlanDisk(t, root, "b", tt.old)
		})
	}
	limits := DefaultLimits()
	limits.FileBytes = 1
	w, root := patchPlanWorkspace(t, limits, nil)
	patchPlanReject(t, w, patchPlanArgs(t, "*** Add File: a\n+x\n", nil), CodeFileTooLarge)
	patchPlanAbsent(t, root, "a")
	limits = DefaultLimits()
	limits.PatchBytes = 3
	w, root = patchPlanWorkspace(t, limits, nil)
	patchPlanReject(t, w, patchPlanArgs(t, "*** Add File: a\n+x\n*** Add File: b\n+y\n", nil), CodePatchTooLarge)
	patchPlanAbsent(t, root, "a")
	patchPlanAbsent(t, root, "b")
	limits = DefaultLimits()
	limits.PatchBytes = 1
	w, _ = patchPlanWorkspace(t, limits, map[string]string{"a": "", "b": ""})
	// Zero remaining original/candidate bytes still allow empty text deletes.
	patchPlanPrepare(t, w, patchPlanArgs(t, "*** Add File: c\n*** Delete File: a\n*** Delete File: b\n", map[string]string{"a": contentHash(nil), "b": contentHash(nil)}))
	limits = DefaultLimits()
	limits.EditFileBytes = 1
	w, root = patchPlanWorkspace(t, limits, map[string]string{"a": "old\n"})
	patchPlanReject(t, w, patchPlanArgs(t, "*** Delete File: a\n", map[string]string{"a": contentHash([]byte("old\n"))}), CodeFileTooLarge)
	ordinary, r := w.PrepareMutation(t.Context(), ToolArchive, `{"path":"a"}`)
	if ordinary == nil || r.Value != nil || ordinary.archiveContentHash != "" || ordinary.FullDiff() != "" {
		t.Fatal("patch text budget changed ordinary archive")
	}
	patchPlanDisk(t, root, "a", "old\n")
	patchPlanAbsent(t, root, ArchiveDirectory)
}

func TestPatchPlanDiffBudgetAndSummary(t *testing.T) {
	w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"a": "old\n"})
	raw := patchPlanArgs(t, "*** Add File: new\n+new\n*** Delete File: a\n", map[string]string{"a": contentHash([]byte("old\n"))})
	p := patchPlanPrepare(t, w, raw)
	size := int64(len(p.FullDiff()))
	w.limits.DiffBytes = size
	if exact := patchPlanPrepare(t, w, raw); int64(len(exact.FullDiff())) != size {
		t.Fatal("delete archive annotation has unstable byte length")
	}
	for _, limit := range []int64{size - 1, 1} {
		w.limits.DiffBytes = limit
		patchPlanReject(t, w, raw, CodeDiffTooLarge)
	}
	patchPlanDisk(t, root, "a", "old\n")
	patchPlanAbsent(t, root, "new")
	patchPlanAbsent(t, root, ArchiveDirectory)
	limits := DefaultLimits()
	limits.MutationPreviewBytes = 1024
	w, root = patchPlanWorkspace(t, limits, nil)
	var body strings.Builder
	for i := 0; i < MaxPatchFiles; i++ {
		fmt.Fprintf(&body, "*** Add File: %02d-%s\n", i, strings.Repeat("x", 100))
	}
	p = patchPlanPrepare(t, w, patchPlanArgs(t, body.String(), nil))
	if len(p.patchItems) != 16 || !p.Presentation.Truncated || len(p.Presentation.Preview) > 1024 || !strings.Contains(p.Presentation.Preview, "摘要已截断") || !strings.Contains(p.FullDiff(), "15-"+strings.Repeat("x", 100)) {
		t.Fatal("summary/full diff budget contract")
	}
	body.WriteString("*** Add File: seventeenth\n")
	patchPlanReject(t, w, patchPlanArgs(t, body.String(), nil), CodeInvalidPatch)
	patchPlanAbsent(t, root, "seventeenth")
}

func TestPatchPlanLimitsAndDefinition(t *testing.T) {
	if DefaultPatchBytes != 64<<20 || DefaultLimits().PatchBytes != DefaultPatchBytes || !IsMutationTool(ToolPatch) || IsReadTool(ToolPatch) {
		t.Fatal("patch default/dispatch contract")
	}
	maxLimit := int64(^uint(0)>>1) - 1
	for _, limit := range []int64{0, 1, 4096, maxLimit} {
		limits := DefaultLimits()
		limits.PatchBytes = limit
		w, _ := patchPlanWorkspace(t, limits, nil)
		want := limit
		if limit == 0 {
			want = DefaultPatchBytes
		}
		if w.limits.PatchBytes != want {
			t.Fatal("patch budget fallback")
		}
		found := false
		for _, definition := range w.Definitions() {
			if definition.Function.Name != ToolPatch {
				continue
			}
			found = true
			if !strings.Contains(definition.Function.Description, fmt.Sprint(want)) || !strings.Contains(string(definition.Function.Parameters), `"expected_hashes"`) {
				t.Fatal("missing patch runtime budget/schema")
			}
		}
		if !found {
			t.Fatal("patch definition missing")
		}
	}
	for _, limit := range []int64{-1, maxLimit + 1} {
		limits := DefaultLimits()
		limits.PatchBytes = limit
		if w, err := OpenWithLimits(t.TempDir(), limits); err == nil {
			w.Close()
			t.Fatalf("invalid patch limit accepted: %d", limit)
		}
	}
}
