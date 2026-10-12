package learningcontent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

func TestStudioQuizCandidate(t *testing.T) {
	raw := `{"title":"偶数测验","coverage":"complete","questions":[{"prompt":"说明偶数的定义。","rubric":[{"id":"definition","criterion":"说明可被 2 整除"}],"allowed_help":["hint","scaffold"],"citations":[{"source":1,"fragment":1}]}]}`
	sources := []knowledge.SourceDetail{{Fragments: []knowledge.SourceFragment{{Text: "偶数可被 2 整除。"}}}}
	manifest := &knowledge.SourceManifest{Entries: []knowledge.SourceManifestEntry{{SHA256: "document-hash"}}}
	body, err := DecodeStudioCandidate("quiz", []byte(raw), manifest, sources)
	if err != nil {
		t.Fatalf("valid quiz rejected: %v", err)
	}
	encoded, _ := json.Marshal(body)
	if !strings.Contains(string(encoded), `"allowed_help":["hint","scaffold"]`) || len(body.Citations) != 1 {
		t.Fatal("quiz help/citations not preserved")
	}
	for _, invalid := range []string{
		strings.Replace(raw, `"prompt":"说明偶数的定义。"`, `"prompt":" "`, 1),
		strings.Replace(raw, `"rubric":[{"id":"definition","criterion":"说明可被 2 整除"}]`, `"rubric":[]`, 1),
		strings.Replace(raw, `"criterion":"说明可被 2 整除"`, `"criterion":""`, 1),
		strings.Replace(raw, `["hint","scaffold"]`, `["hint","hint"]`, 1),
		strings.Replace(raw, `["hint","scaffold"]`, `["unknown"]`, 1),
		strings.Replace(raw, `"source":1`, `"source":2`, 1),
		strings.Replace(raw, `"prompt":`, `"extra":true,"prompt":`, 1),
		strings.Replace(raw, `"title":`, `"title":"duplicate","title":`, 1),
	} {
		if _, err := DecodeStudioCandidate("quiz", []byte(invalid), manifest, sources); err == nil {
			t.Fatal("invalid quiz accepted")
		}
	}
	if (StudioTarget{Kind: "quiz", Generation: 1}).Validate() == nil {
		t.Fatal("quiz without explicit goal accepted")
	}
}
