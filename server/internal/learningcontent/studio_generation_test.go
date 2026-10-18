package learningcontent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

func TestStudioTargetRejectsNonCanonicalEncryptedIdentity(t *testing.T) {
	id := "abcdef00-0000-4000-8000-000000000001"
	upper := strings.ToUpper(id)
	for _, target := range []StudioTarget{{Kind: "guide", Generation: 1, GoalID: &upper}, {Kind: "brief", Generation: 1, ArtifactID: &upper, ExpectedVersion: 1}} {
		if target.Validate() == nil {
			t.Fatal("noncanonical UUID would change the revision encryption AAD on read")
		}
	}
}

func TestStudioSliceProofCannotBeChanged(t *testing.T) {
	fragment := knowledge.SourceFragment{NodeID: "node", NodeRevisionID: "revision", Text: "原文", Range: knowledge.SourceRange{Start: 0, End: 6, StartLine: 1, EndLine: 1}}
	fragment.Proof = &knowledge.SourceSliceProof{Schema: 2, OriginalRange: knowledge.SourceRange{Start: 0, End: 60, StartLine: 1, EndLine: 10}, Range: fragment.Range, SHA256: knowledge.SourceTextHash(fragment.Text)}
	source := knowledge.SourceDetail{Fragments: []knowledge.SourceFragment{fragment}}
	body := StudioBody{SourceManifest: &knowledge.SourceManifest{Entries: []knowledge.SourceManifestEntry{{SHA256: "document"}}}, Citations: []StudioCitation{{ID: "1", Fragment: fragment, DocumentSHA256: "document", SliceSHA256: knowledge.SourceTextHash(fragment.Text)}}}
	if err := VerifyStudioSources(body, []knowledge.SourceDetail{source}); err != nil {
		t.Fatal(err)
	}
	changed := *fragment.Proof
	changed.OriginalRange.End = 600
	body.Citations[0].Fragment.Proof = &changed
	if err := VerifyStudioSources(body, []knowledge.SourceDetail{source}); err == nil {
		t.Fatal("发布接受了篡改的子范围证明")
	}
	// Adding the subrange schema must not relax the old complete-source contract.
	body.Citations[0].Fragment.Proof = nil
	source.Fragments[0].Proof = nil
	body.SourceManifest.Entries = append(body.SourceManifest.Entries, knowledge.SourceManifestEntry{SHA256: "another-document"})
	if err := VerifyStudioSources(body, []knowledge.SourceDetail{source}); err == nil {
		t.Fatal("旧整片引用接受了不完整来源集合")
	}
}

func TestStudioStructuredClaims(t *testing.T) {
	sources := []knowledge.SourceDetail{{Fragments: []knowledge.SourceFragment{{Text: "偶数可被 2 整除。"}}}}
	manifest := &knowledge.SourceManifest{Entries: []knowledge.SourceManifestEntry{{SHA256: "document-hash"}}}
	claim := `{"text":"偶数可被 2 整除。","citations":[{"source":1,"fragment":1}]}`
	guide := `{"title":"偶数指南","coverage":"complete","key_points":[` + claim + `],"terms":[` + claim + `],"learning_order":[` + claim + `],"misconceptions":[` + claim + `]}`
	brief := `{"title":"偶数简报","coverage":"partial","overview":[` + claim + `],"differences":[` + claim + `],"gaps":[` + claim + `]}`
	for _, tc := range []struct{ kind, raw string }{{"guide", guide}, {"brief", brief}} {
		body, err := DecodeStudioCandidate(tc.kind, []byte(tc.raw), manifest, sources)
		if err != nil || len(body.Citations) != 1 || (body.Guide != nil) != (tc.kind == "guide") || (body.Brief != nil) != (tc.kind == "brief") {
			t.Fatalf("%s: body=%+v err=%v", tc.kind, body, err)
		}
		for _, mutate := range []func(map[string]any){
			func(v map[string]any) { v["unknown"] = true },
			func(v map[string]any) { delete(v, "coverage") },
			func(v map[string]any) { v["title"] = "" },
			func(v map[string]any) {
				for _, field := range []string{"key_points", "overview"} {
					if arr, ok := v[field].([]any); ok {
						arr[0].(map[string]any)["citations"] = []any{map[string]any{"source": 2, "fragment": 1}}
					}
				}
			},
		} {
			var value map[string]any
			_ = json.Unmarshal([]byte(tc.raw), &value)
			mutate(value)
			raw, _ := json.Marshal(value)
			if _, err := DecodeStudioCandidate(tc.kind, raw, manifest, sources); err == nil {
				t.Fatalf("accepted invalid %s", raw)
			}
		}
	}
	if _, err := DecodeStudioCandidate("brief", []byte(guide), manifest, sources); err == nil {
		t.Fatal("guide accepted as brief")
	}
	if _, err := DecodeStudioCandidate("guide", []byte(guide), nil, nil); err == nil {
		t.Fatal("no sources accepted")
	}
	sources[0].Fragments[0].Truncated = true
	if body, err := DecodeStudioCandidate("guide", []byte(guide), manifest, sources); err != nil || body.Coverage != "partial" {
		t.Fatal("truncated source was presented as complete", err)
	}
}
