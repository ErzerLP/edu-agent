//go:build windows

package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func TestReadDocumentRejectsDeterministicIntermediateDirectorySwap(t *testing.T) {
	rootPath := t.TempDir()
	originalDirectory := filepath.Join(rootPath, "section")
	outsideDirectory := t.TempDir()
	if err := os.Mkdir(originalDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(originalDirectory, "note.md"), []byte("# Safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDirectory, "note.md"), []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := securefile.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	_, err = readDocumentFromRoot("section/note.md", root, "section/note.md", func() {
		if renameErr := os.Rename(originalDirectory, originalDirectory+".original"); renameErr != nil {
			t.Fatal(renameErr)
		}
		if symlinkErr := os.Symlink(outsideDirectory, originalDirectory); symlinkErr != nil {
			t.Fatal(symlinkErr)
		}
	})
	if err == nil {
		t.Fatal("readDocument escaped through a swapped intermediate directory reparse point")
	}
}
