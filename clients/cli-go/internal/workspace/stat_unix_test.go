//go:build linux || darwin

package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStatWorkspaceLinksFIFOsAndPermission(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "denied"), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for path, kind := range map[string]string{"link": "link", "fifo": "other"} {
		raw, _ := json.Marshal(map[string]any{"path": path})
		value := resultObject(t, w.Execute(t.Context(), ToolStat, string(raw)))
		if value["entry_type"] != kind || value["exists"] != true || value["size"] != nil {
			t.Fatalf("type metadata: %+v", value)
		}
		raw, _ = json.Marshal(map[string]any{"path": path, "hash": true})
		if code := resultCode(t, w.Execute(t.Context(), ToolStat, string(raw))); code != CodeUnsupportedType {
			t.Fatal(code)
		}
	}
	for _, path := range []string{"link/secret", "link/missing"} {
		raw, _ := json.Marshal(map[string]any{"path": path})
		value := resultObject(t, w.Execute(t.Context(), ToolStat, string(raw)))
		if value["code"] != CodeLinkNotAllowed || value["exists"] != nil {
			t.Fatalf("unsafe absence: %+v", value)
		}
	}
	if os.Geteuid() == 0 {
		t.Log("permission case not exercised as root; link/FIFO checks passed")
		return
	}
	if err := os.Chmod(filepath.Join(dir, "denied"), 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "denied"), 0700)
	value := resultObject(t, w.Execute(t.Context(), ToolStat, `{"path":"denied/missing"}`))
	if value["code"] != CodePermissionDenied || value["exists"] != nil {
		t.Fatalf("permission disguised as absence: %+v", value)
	}
}
