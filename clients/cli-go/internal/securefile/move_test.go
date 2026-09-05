package securefile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func moveFixture(t *testing.T, directory bool) (string, *Root, *MovePlan) {
	t.Helper()
	dir := t.TempDir()
	if directory {
		if err := os.MkdirAll(filepath.Join(dir, "source/child"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "source/child/file"), []byte("nested"), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(filepath.Join(dir, "source"), []byte("original\x00\xff"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	stat, err := root.Stat(t.Context(), "source")
	if err != nil {
		t.Fatal(err)
	}
	p, err := root.PrepareMove(t.Context(), "source", "target", stat.Version)
	if err != nil {
		t.Fatal(err)
	}
	return dir, root, p
}
func TestMoveFileBytesWithoutSizeLimit(t *testing.T) {
	for _, size := range []int{0, 19, (32 << 20) + 7} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir, root, _ := moveFixture(t, false)
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i * 17)
			}
			if err := os.WriteFile(filepath.Join(dir, "source"), data, 0600); err != nil {
				t.Fatal(err)
			}
			stat, err := root.Stat(t.Context(), "source")
			if err != nil {
				t.Fatal(err)
			}
			p, err := root.PrepareMove(t.Context(), "source", "target", stat.Version)
			if err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatal("prepare side effects", entries)
			}
			got, err := root.Move(t.Context(), p)
			if err != nil || got.Outcome != PublishCompleted {
				t.Fatal(got, err)
			}
			actual, err := os.ReadFile(filepath.Join(dir, "target"))
			if err != nil || !bytes.Equal(data, actual) {
				t.Fatal("wrong bytes", err)
			}
			if _, err = os.Lstat(filepath.Join(dir, "source")); !os.IsNotExist(err) {
				t.Fatal("source remains", err)
			}
			entries, _ = os.ReadDir(dir)
			if len(entries) != 1 || entries[0].Name() != "target" {
				t.Fatal("unexpected entries", entries)
			}
			newStat, err := root.Stat(t.Context(), "target")
			if err != nil || newStat.Identity != stat.Identity {
				t.Fatal("not rename", newStat, err)
			}
			if got, err = root.Move(t.Context(), p); !errors.Is(err, ErrChanged) || got.Outcome != PublishUnchanged {
				t.Fatal("reused plan", got, err)
			}
		})
	}
}
func TestMoveWholeDirectoriesAndInternalLinks(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "untouched"), []byte("external"), 0600); err != nil {
				t.Fatal(err)
			}
			if !empty {
				if err := os.Mkdir(filepath.Join(dir, "source/child"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "source/child/file"), []byte("inside"), 0600); err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" {
					if err := os.Symlink(outside, filepath.Join(dir, "source/link")); err != nil {
						t.Fatal(err)
					}
				}
			}
			root, err := OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			stat, err := root.Stat(t.Context(), "source")
			if err != nil {
				t.Fatal(err)
			}
			p, err := root.PrepareMove(t.Context(), "source", "target", stat.Version)
			if err != nil {
				t.Fatal(err)
			}
			// A child body edit is not a directory-entry metadata snapshot violation.
			if !empty {
				if err := os.WriteFile(filepath.Join(dir, "source/child/file"), []byte("latest child"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := root.Move(t.Context(), p)
			if err != nil || result.Outcome != PublishCompleted {
				t.Fatal(result, err)
			}
			if !empty {
				b, e := os.ReadFile(filepath.Join(dir, "target/child/file"))
				if e != nil || string(b) != "latest child" {
					t.Fatal(string(b), e)
				}
				if runtime.GOOS != "windows" {
					link, e := os.Readlink(filepath.Join(dir, "target/link"))
					if e != nil || link != outside {
						t.Fatal(link, e)
					}
				}
			}
			b, e := os.ReadFile(filepath.Join(outside, "untouched"))
			if e != nil || string(b) != "external" {
				t.Fatal("followed link", e)
			}
		})
	}
}
func TestMoveSamePathStillChecksVersionAndType(t *testing.T) {
	for _, directory := range []bool{false, true} {
		_, root, p := moveFixture(t, directory)
		same, err := root.PrepareMove(t.Context(), "source", "source", p.Version())
		if err != nil || !same.Unchanged() {
			t.Fatal(same, err)
		}
		result, err := root.Move(t.Context(), same)
		if err != nil || result.Outcome != PublishUnchanged {
			t.Fatal(result, err)
		}
		if _, err = root.PrepareMove(t.Context(), "source", "source", "entry-v1:"+strings.Repeat("a", 64)); !errors.Is(err, ErrChanged) {
			t.Fatal("samepath bypass", err)
		}
	}
}
func TestMoveRejectsUnsafePathsAndStaleSource(t *testing.T) {
	dir, root, p := moveFixture(t, true)
	if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ArchiveDirectory, "saved"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	archived, err := root.Stat(t.Context(), ArchiveDirectory+"/saved")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][3]string{{".", "target", p.Version()}, {"source", ".", p.Version()}, {"../outside", "target", p.Version()}, {"source", "../outside", p.Version()}, {"source", "SOURCE", p.Version()}, {"source", "missing/target", p.Version()}, {"source", "source/child/target", p.Version()}, {"source", ArchiveDirectory + "/target", p.Version()}, {ArchiveDirectory + "/saved", "target", archived.Version}, {"source", "target", "sha256:" + strings.Repeat("a", 64)}} {
		if _, err = root.PrepareMove(t.Context(), pair[0], pair[1], pair[2]); err == nil {
			t.Fatal("unsafe accepted", pair)
		}
	}
	if err = os.Mkdir(filepath.Join(dir, "source/changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if result, err := root.Move(t.Context(), p); err == nil || result.Outcome != PublishUnchanged {
		t.Fatal(result, err)
	}
}
func TestMoveCancellationAndRootBinding(t *testing.T) {
	_, root, p := moveFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := root.Move(ctx, p); !errors.Is(err, context.Canceled) || result.Outcome != PublishUnchanged {
		t.Fatal(result, err)
	}
	_, other, q := moveFixture(t, false)
	if result, err := root.Move(t.Context(), q); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
		t.Fatal("unbound", result, err)
	}
	if result, err := other.Move(t.Context(), q); err != nil || result.Outcome != PublishCompleted {
		t.Fatal(result, err)
	}
}
