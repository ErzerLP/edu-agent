package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveStrictArgumentsFrozenPreviewAndSamePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("private-body\x00\xff"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	stat := w.Execute(t.Context(), ToolStat, `{"path":"source"}`)
	version := stat.Value.(map[string]any)["entry_version"].(string)
	raw := `{"source":"source","destination":"target","expected_version":"` + version + `"}`
	for _, bad := range []string{`null`, `[]`, `{}`, strings.Replace(raw, `"source":"source"`, `"Source":"source"`, 1), strings.Replace(raw, `"source":"source"`, `"source":"source","source":"other"`, 1), strings.Replace(raw, `"source":"source"`, `"source":null`, 1), strings.Replace(raw, `"target"`, `true`, 1), strings.Replace(raw, version, "sha256:"+strings.Repeat("a", 64), 1), raw + ` {}`, strings.TrimSuffix(raw, "}") + `,"overwrite":false}`, strings.TrimSuffix(raw, "}") + `,"force":true}`} {
		p, r := w.PrepareMutation(t.Context(), ToolMove, bad)
		if p != nil || resultCode(t, r) != CodeInvalidArguments {
			t.Fatal("invalid accepted", bad, r)
		}
	}
	same := strings.Replace(raw, `"destination":"target"`, `"destination":"source"`, 1)
	if p, r := w.PrepareMutation(t.Context(), ToolMove, same); p != nil || r.Publication != PublicationUnchanged || r.Effect != nil || r.Value.(map[string]any)["complete"] != true {
		t.Fatal("invalid no-op", p, r)
	}
	if p, r := w.PrepareMutation(t.Context(), ToolMove, strings.Replace(same, version, "entry-v1:"+strings.Repeat("b", 64), 1)); p != nil || r.Value.(map[string]any)["complete"] == true {
		t.Fatal("stale no-op", r)
	}
	p, r := w.PrepareMutation(t.Context(), ToolMove, raw)
	if p == nil {
		t.Fatal(r)
	}
	if p.Presentation.DestinationPath != "target" || p.Presentation.BaseVersion != version || !strings.Contains(p.Presentation.Preview, version) || len(p.candidate) != 0 || p.FileEffect().Validate() != nil {
		t.Fatal("bad plan", p.Presentation)
	}
	denied := MutationDenied(p)
	if denied.Effect != nil || denied.Publication != PublicationUnchanged || denied.Value.(map[string]any)["destination"] != "target" {
		t.Fatal(denied)
	}
	r = w.CommitMutation(t.Context(), p)
	if r.Publication != PublicationCompleted || r.Reference.Kind != "move_file" || r.Reference.Path != "source" || r.Reference.ContentHash != "" || r.Effect.Source.Version != version || r.Effect.Target.Version != "" {
		t.Fatal(r)
	}
	encoded, _ := json.Marshal(r.Value)
	if strings.Contains(string(encoded), "private-body") {
		t.Fatal("body leaked")
	}
	if _, err = os.Stat(filepath.Join(dir, "source")); !os.IsNotExist(err) {
		t.Fatal("source remains")
	}
	data, err := os.ReadFile(filepath.Join(dir, "target"))
	if err != nil || string(data) != "private-body\x00\xff" {
		t.Fatal(data, err)
	}
}
func TestMoveRejectsTamperedPlanAndHistoryOverflow(t *testing.T) {
	for _, field := range []string{"source", "destination", "version", "kind", "operation", "preview", "truncated", "tool"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
				t.Fatal(err)
			}
			w, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			stat, err := w.root.Stat(t.Context(), "source")
			if err != nil {
				t.Fatal(err)
			}
			p, r := w.PrepareMutation(t.Context(), ToolMove, `{"source":"source","destination":"target","expected_version":"`+stat.Version+`"}`)
			if p == nil {
				t.Fatal(r)
			}
			switch field {
			case "source":
				p.Presentation.Path = "other"
			case "destination":
				p.Presentation.DestinationPath = "other"
			case "version":
				p.Presentation.BaseVersion = "other"
			case "kind":
				p.Presentation.EntryKind = "file"
			case "operation":
				p.Presentation.Operation = "copy"
			case "preview":
				p.Presentation.Preview = "other"
			case "truncated":
				p.Presentation.Truncated = true
			case "tool":
				p.Presentation.Tool = ToolCopy
			}
			r = w.CommitMutation(t.Context(), p)
			if r.Publication != PublicationUnchanged {
				t.Fatal(r)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 || entries[0].Name() != "source" {
				t.Fatal("tampered commit", entries)
			}
		})
	}
	dir := t.TempDir()
	long := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 200)
	if err := os.MkdirAll(filepath.Join(dir, long, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	stat, err := w.root.Stat(t.Context(), long+"/source")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"source": long + "/source", "destination": long + "/target", "expected_version": stat.Version})
	p, r := w.PrepareMutation(t.Context(), ToolMove, string(raw))
	if p != nil || resultCode(t, r) != CodeInvalidPath {
		t.Fatal("overflow authorized", r)
	}
}
