package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMkdirFrozenAuthorizationAndStrictArguments(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, raw := range []string{`{}`, `{"path":"."}`, `{"path":"../x"}`, `{"path":".edu-agent-archive/x","parents":true}`, `{"path":"x","parents":"true"}`, `{"path":"x","parents":null}`, `{"path":"x","Parents":true}`, `{"path":"x","parents":true,"parents":false}`, `{"path":"x","extra":true}`, `{"path":"a/b"}`} {
		p, r := w.PrepareMutation(t.Context(), ToolMkdir, raw)
		if p != nil || resultCode(t, r) == "" {
			t.Fatalf("accepted %s: %+v", raw, r)
		}
	}
	p, r := w.PrepareMutation(t.Context(), ToolMkdir, `{"path":"a/b","parents":true}`)
	if p == nil {
		t.Fatalf("prepare %+v", r)
	}
	if p.Presentation.Truncated || !strings.Contains(p.Presentation.Preview, "a/b") || !strings.Contains(p.Presentation.Preview, "父锚点：.") {
		t.Fatalf("preview=%+v", p.Presentation)
	}
	if _, err = os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatalf("prepare side effect %v", err)
	}
	denied := MutationDenied(p)
	if denied.Publication != PublicationUnchanged {
		t.Fatal(denied)
	}
	r = w.CommitMutation(t.Context(), p)
	if r.Publication != PublicationCompleted || r.Effect == nil || r.Effect.Directories.Created != 2 {
		t.Fatalf("commit=%+v", r)
	}
	if r = w.CommitMutation(t.Context(), p); r.Publication != PublicationUnchanged {
		t.Fatal("reused prepared")
	}
	existing, r := w.PrepareMutation(t.Context(), ToolMkdir, `{"path":"a/b"}`)
	if existing != nil || r.Publication != PublicationUnchanged || r.Effect != nil {
		t.Fatalf("existing=%+v", r)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if p, r = w.PrepareMutation(ctx, ToolMkdir, `{"path":"cancelled"}`); p != nil || r.Publication != PublicationUnchanged {
		t.Fatal("cancel ignored")
	}
}
func TestMkdirRejectsChangedPreviewAndParent(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	p, r := w.PrepareMutation(t.Context(), ToolMkdir, `{"path":"new"}`)
	if p == nil {
		t.Fatal(r)
	}
	p.Presentation.Path = "different"
	if r = w.CommitMutation(t.Context(), p); r.Publication != PublicationUnchanged {
		t.Fatal(r)
	}
	if _, err = os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("tampered approval created")
	}
}
