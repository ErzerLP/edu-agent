package agentloop

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestFileArtifactStrictArgumentsAndProjection(t *testing.T) {
	for _, raw := range []string{`{"action":"list","id":"r_x"}`, `{"action":"read","id":"r_x","needle":"x"}`, `{"action":"search","id":"r_x","needle":""}`, `{"action":"search","id":"r_x","needle":"x","limit":101}`, `{"action":"read","id":null}`, `{"action":"list","action":"read"}`} {
		if _, err := decodeArtifactArgs(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	data := []byte("text\x1b[31m\xfftail")
	result := artifactToolResult{Page: &localartifact.Page{Info: localartifact.Info{ID: "r_page", Kind: "diff", Bytes: int64(len(data)), Saved: true}, Data: data, NextOffset: int64(len(data))}}
	for n := 0; n <= len(data); n++ {
		v := result.value(n, 100, false, false)
		if v["next_offset"] != int64(n) || v["more"] != (n < len(data)) {
			t.Fatalf("cursor: %+v", v)
		}
		if n > 0 {
			body := []byte(v["data"].(string))
			if v["encoding"] == "base64" {
				var err error
				body, err = base64.StdEncoding.DecodeString(string(body))
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(body, data[:n]) {
				t.Fatal("projection changed acknowledged bytes")
			}
		}
	}
	history := result.project(2048, nil, true)
	if bytes.Contains([]byte(history), []byte("text")) || bytes.Contains([]byte(history), []byte(`"data"`)) {
		t.Fatal("body entered history")
	}
	search := artifactToolResult{Search: &localartifact.SearchPage{Info: result.Page.Info, Offsets: []int64{2, 5, 8}, Offset: 1, NextOffset: 20}}
	v := search.value(0, 1, false, false)
	if v["next_offset"] != int64(3) || v["more"] != true {
		t.Fatalf("search skipped omitted matches: %+v", v)
	}
	var projected map[string]any
	if err := json.Unmarshal([]byte(result.project(200, nil, false)), &projected); err != nil {
		t.Fatal(err)
	}
	if projected["id"] != "r_page" || projected["saved"] != true {
		t.Fatalf("locator lost: %+v", projected)
	}
	result.Page.Info.Hash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := json.Unmarshal([]byte(result.project(180, nil, false)), &projected); err != nil {
		t.Fatal(err)
	}
	if next, _ := projected["next_offset"].(float64); next == 0 {
		t.Fatalf("minimal metadata needlessly prevented forward progress: %+v", projected)
	}
}

func TestFileArtifactCallIdentityCannotReuseAnotherDiff(t *testing.T) {
	w, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newDurableTestSession(t, &fakeModel{}, &fakeServer{}, w, &durabilitySink{})
	defer s.Close()
	first, failure := w.PrepareMutation(t.Context(), workspace.ToolWrite, `{"path":"first","mode":"create","content":"one"}`)
	if first == nil {
		t.Fatalf("prepare first: %+v", failure)
	}
	info, err := s.retainMutationArtifact(t.Context(), "same-call", first)
	if err != nil {
		t.Fatal(err)
	}
	second, failure := w.PrepareMutation(t.Context(), workspace.ToolWrite, `{"path":"second","mode":"create","content":"two"}`)
	if second == nil {
		t.Fatalf("prepare second: %+v", failure)
	}
	if _, err := s.retainMutationArtifact(t.Context(), "same-call", second); err == nil {
		t.Fatal("new candidate reused an older diff reference")
	}
	page, err := s.options.Artifacts.Read(t.Context(), s.options.ArtifactOwner, info.ID, 0, 4096)
	if err != nil || string(page.Data) != first.FullDiff() {
		t.Fatal("original artifact identity changed")
	}
}
