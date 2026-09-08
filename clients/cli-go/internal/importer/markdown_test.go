package importer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDirectoryIsDeterministicAndDoesNotExposeRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "topic"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"z.md": []byte("# Z\n"), "a.md": []byte("# A\n"), "topic/b.md": []byte("# B\n"), "ignored.txt": []byte("ignore"),
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{batch.Documents[0].Path, batch.Documents[1].Path, batch.Documents[2].Path}
	want := []string{"a.md", "topic/b.md", "z.md"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
		if filepath.IsAbs(got[index]) {
			t.Fatalf("absolute path leaked: %s", got[index])
		}
	}
}

func TestLoadRejectsInvalidUTF8SymlinkAndUnicodePathConflict(t *testing.T) {
	t.Parallel()
	t.Run("invalid UTF-8", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.md")
		if err := os.WriteFile(path, []byte{0xff}, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("Load accepted invalid UTF-8")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.md")
		if err := os.WriteFile(target, []byte("# Target"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.md")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(link); err == nil {
			t.Fatal("Load accepted symlink input")
		}
	})
	t.Run("case fold conflict", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"A.md", "a.md"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("# X"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Load(dir); err == nil {
			t.Fatal("Load accepted case-fold conflicting paths")
		}
	})
	t.Run("non NFC path", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "e\u0301.md"), []byte("# X"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil {
			t.Fatal("Load accepted a non-NFC path")
		}
	})
}
