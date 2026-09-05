package workspace

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyStrictArgumentsAndFrozenPreview(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("\x00\xffsecret-source-not-model-body"), 50000)
	if err := os.WriteFile(filepath.Join(dir, "source"), data, 0640); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	stat := w.Execute(t.Context(), ToolStat, `{"path":"source"}`)
	version := stat.Value.(map[string]any)["entry_version"].(string)
	args := copyArguments{Source: "source", Destination: "target", ExpectedVersion: version}
	raw, _ := json.Marshal(args)
	bad := []string{`null`, `[]`, `{}`, `{"source":null,"destination":"target","expected_version":"` + version + `"}`, `{"Source":"source","destination":"target","expected_version":"` + version + `"}`, `{"source":"source","source":"other","destination":"target","expected_version":"` + version + `"}`, `{"source":"source","destination":"target","expected_version":null}`, `{"source":"source","destination":"target","expected_version":"sha256:` + strings.Repeat("a", 64) + `"}`, string(raw) + ` {}`, strings.TrimSuffix(string(raw), "}") + `,"overwrite":false}`, strings.TrimSuffix(string(raw), "}") + `,"force":true}`}
	for _, raw := range bad {
		p, r := w.PrepareMutation(t.Context(), ToolCopy, raw)
		if p != nil || resultCode(t, r) != CodeInvalidArguments {
			t.Fatalf("accepted %s: %+v", raw, r)
		}
	}
	p, r := w.PrepareMutation(t.Context(), ToolCopy, string(raw))
	if p == nil {
		t.Fatal(r)
	}
	if len(p.candidate) != 0 || p.Presentation.DestinationPath != "target" || p.Presentation.BaseVersion != version || p.FileEffect().Validate() != nil || strings.Contains(p.Presentation.Preview, "secret-source") {
		t.Fatal("bad plan", p.Presentation)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("prepare wrote", entries)
	}
	r = w.CommitMutation(t.Context(), p)
	if r.Publication != PublicationCompleted || r.Reference.Path != "target" || r.Reference.Kind != "copy" || r.Effect.Source.Version != version || r.Effect.Target.Version == "" {
		t.Fatal(r)
	}
	encoded, _ := json.Marshal(r.Value)
	if bytes.Contains(encoded, []byte("secret-source")) {
		t.Fatal("body leaked")
	}
	got, err := os.ReadFile(filepath.Join(dir, "target"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("not actual bytes", err)
	}
	if len(Definitions()) != 11 {
		t.Fatal("tool count", len(Definitions()))
	}
}
func TestCopyRejectsTamperedAuthorizationAndHistoryOverflow(t *testing.T) {
	for _, field := range []string{"path", "destination", "version", "preview", "truncated", "operation", "kind"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "source"), nil, 0600); err != nil {
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
			raw, _ := json.Marshal(copyArguments{Source: "source", Destination: "target", ExpectedVersion: stat.Version})
			p, r := w.PrepareMutation(t.Context(), ToolCopy, string(raw))
			if p == nil {
				t.Fatal(r)
			}
			switch field {
			case "path":
				p.Presentation.Path = "different"
			case "destination":
				p.Presentation.DestinationPath = "different"
			case "version":
				p.Presentation.BaseVersion = "different"
			case "preview":
				p.Presentation.Preview = "different"
			case "truncated":
				p.Presentation.Truncated = true
			case "operation":
				p.Presentation.Operation = "archive"
			case "kind":
				p.Presentation.EntryKind = "directory"
			}
			if r = w.CommitMutation(t.Context(), p); r.Publication != PublicationUnchanged {
				t.Fatal(r)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatal("tampered publication")
			}
		})
	}
	dir := t.TempDir()
	long := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 200)
	if err := os.MkdirAll(filepath.Join(dir, long), 0700); err != nil {
		t.Fatal(err)
	}
	source := long + "/source"
	destination := long + "/target"
	if err := os.WriteFile(filepath.Join(dir, source), nil, 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	stat, err := w.root.Stat(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(copyArguments{Source: source, Destination: destination, ExpectedVersion: stat.Version})
	p, r := w.PrepareMutation(t.Context(), ToolCopy, string(raw))
	if p != nil || resultCode(t, r) != CodeInvalidPath {
		t.Fatal("authorized truncated history", r)
	}
}
