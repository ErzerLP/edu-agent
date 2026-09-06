package workspace

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPatchPlanStrictSyntax(t *testing.T) {
	w, root := patchPlanWorkspace(t, DefaultLimits(), map[string]string{"a": "old\n"})
	for _, body := range []string{
		"*** Update File: a\n", "*** Update File: a\n@@ -1 +1 @@\n-old\n+new\n",
		"*** Update File: a\n@@ location\n-old\n+new\n", "*** Update File: a\n*** Move to: b\n",
		"*** Update File: a\n@@\n old\n", "*** Update File: a\n@@\n-old\n+new\n\\ No newline at end of file\n",
		"*** Update File: a\n@@\n-old\n+new\n*** End of File\n@@\n+x\n",
		"*** Delete File: a\n-old\n", "*** Add File: new\nnot-prefixed\n",
		"*** Add File: new\n+x\n\\ No newline at end of file\n", "*** Move to: a\n",
		"*** Add File: new\n+x\rwrong\n", "*** Add File: new\n+x\n\n",
		"diff --git a/a b/a\n", "GIT binary patch\n", "",
	} {
		patchPlanReject(t, w, patchPlanArgs(t, body, map[string]string{"a": contentHash([]byte("old\n"))}), CodeInvalidPatch)
	}
	for _, text := range []string{
		"*** Begin Patch\n*** Add File: new\n+x", "*** Add File: new\n+x\n*** End Patch",
		"*** Begin Patch\n*** Add File: new\n+x\n*** End Patch\n\n",
		"*** Begin Patch\n*** Add File: new\n+x\n*** End Patch\n*** Delete File: a",
	} {
		patchPlanReject(t, w, completeDiffJSON(t, patchArguments{Patch: text, ExpectedHashes: map[string]string{}}), CodeInvalidPatch)
	}
	patchPlanDisk(t, root, "a", "old\n")
	patchPlanAbsent(t, root, "new")
}

func TestPatchPlanStrictJSON(t *testing.T) {
	w, root := patchPlanWorkspace(t, DefaultLimits(), nil)
	valid := patchPlanArgs(t, "*** Add File: a\n+x\n", nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(valid), &fields); err != nil {
		t.Fatal(err)
	}
	patch := string(fields["patch"])
	for _, raw := range []string{
		"null", "[]", valid + " {}", strings.TrimSuffix(valid, "}") + `,"unknown":true}`,
		`{"patch":` + patch + `,"patch":` + patch + `,"expected_hashes":{}}`,
		`{"patch":` + patch + `,"\u0070atch":` + patch + `,"expected_hashes":{}}`,
		`{"patch":null,"expected_hashes":{}}`, `{"patch":` + patch + `,"expected_hashes":null}`,
		`{"patch":` + patch + `}`, `{"expected_hashes":{}}`,
		`{"patch":` + patch + `,"expected_hashes":{"a":null}}`,
		`{"patch":` + patch + `,"expected_hashes":{"a":"invalid"}}`,
		`{"patch":` + patch + `,"expected_hashes":{"a":"` + contentHash(nil) + `","\u0061":"` + contentHash(nil) + `"}}`,
		`{"patch":` + patch + `,"expected_hashes":{},"expected_hashes":{}}`,
		strings.Replace(valid, "+x", "+\xff", 1), strings.Replace(valid, "+x", `+\ud800`, 1),
		strings.Replace(valid, "+x", `+\udfff`, 1), strings.Replace(valid, "+x", `+\ud800\u0061`, 1),
		strings.Replace(valid, "+x", `+\u0000`, 1), strings.Replace(valid, "+x", `+\u001b`, 1),
		strings.Replace(valid, "+x", `+\u000c`, 1),
	} {
		patchPlanReject(t, w, raw, CodeInvalidArguments)
	}
	// A real paired surrogate must decode without silent replacement.
	paired := strings.Replace(valid, "+x", `+\ud83d\ude00`, 1)
	p := patchPlanPrepare(t, w, paired)
	if string(p.patchItems[0].candidate) != "\U0001f600\n" {
		t.Fatal("valid surrogate pair altered")
	}
	patchPlanAbsent(t, root, "a")
}
