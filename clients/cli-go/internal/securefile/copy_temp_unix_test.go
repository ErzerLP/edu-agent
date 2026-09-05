//go:build linux || darwin

package securefile

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"testing"
)

func TestCopyUncertainTempCreateRetainsUnknownWithoutUnsafeCleanup(t *testing.T) {
	dir, root, p := copyFixture(t, []byte("source"))
	open := copyTempOpenUnix
	defer func() { copyTempOpenUnix = open }()
	copyTempOpenUnix = func(parent int, name string, flags int, mode uint32) (int, error) {
		fd, err := open(parent, name, flags, mode)
		if err != nil {
			return fd, err
		}
		if err = unix.Close(fd); err != nil {
			t.Fatal(err)
		}
		return -1, unix.EIO // Models an ambiguous filesystem create response.
	}
	result, err := root.Copy(t.Context(), p)
	if result.Outcome != PublishUnknown || !errors.Is(err, ErrOutcomeUnknown) || result.ContentHash != "" {
		t.Fatal(result, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("unknown temp removed", entries)
	}
	for _, entry := range entries {
		if entry.Name() != "source" && !strings.HasPrefix(entry.Name(), ".edu-agent-") {
			t.Fatal("unexpected publication", entry.Name())
		}
	}
}
