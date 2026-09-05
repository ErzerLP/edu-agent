//go:build linux || darwin

package workspace

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGitignoreFIFOClosesSubtreeWithoutBlocking(t *testing.T) {
	w, root := openSearchOutputFixture(t, map[string]string{"safe.txt": "needle", "bad/must-not-scan.txt": "needle"}, DefaultLimits())
	path := filepath.Join(root, "bad", ".gitignore")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{ToolFind, ToolSearch} {
		args := `{"pattern":"*.txt","type":"file","respect_gitignore":true}`
		if tool == ToolSearch {
			args = `{"query":"needle","output":"files","respect_gitignore":true}`
		}
		done := make(chan Result, 1)
		go func() { done <- w.Execute(t.Context(), tool, args) }()
		select {
		case result := <-done:
			value := resultObject(t, result)
			var paths []string
			if tool == ToolFind {
				paths = findPaths(t, result)
			} else {
				paths, _ = value["files"].([]string)
			}
			if !reflect.DeepEqual(paths, []string{"safe.txt"}) || value["complete"] != false || value["truncation_reason"] != "gitignore_unsafe_type" {
				t.Fatalf("%s %+v", tool, value)
			}
		case <-time.After(time.Second):
			fd, _ := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
			if fd >= 0 {
				defer unix.Close(fd)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			t.Fatalf("%s blocked on gitignore FIFO", tool)
		}
	}
}
