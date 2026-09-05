package securefile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func copyFixture(t *testing.T, data []byte) (string, *Root, *CopyPlan) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "source"), data, 0640); err != nil {
		t.Fatal(err)
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
	plan, err := root.PrepareCopy(t.Context(), "source", "target", stat.Version)
	if err != nil {
		t.Fatal(err)
	}
	return dir, root, plan
}
func TestCopyStreamingBytesBoundariesAndPermission(t *testing.T) {
	for _, size := range []int{0, 19, (1 << 20) + 7, 32 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i * 17)
			}
			dir, root, p := copyFixture(t, data)
			before, err := os.ReadDir(dir)
			if err != nil || len(before) != 1 {
				t.Fatal("prepare created entries", before, err)
			}
			read := copyRead
			maxRead := 0
			copyRead = func(f *os.File, b []byte) (int, error) { maxRead = max(maxRead, len(b)); return read(f, b) }
			defer func() { copyRead = read }()
			result, err := root.Copy(t.Context(), p)
			if err != nil || result.Outcome != PublishCompleted {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if maxRead > 32<<10 {
				t.Fatal("unbounded buffer", maxRead)
			}
			want := sha256.Sum256(data)
			if result.ContentHash != "sha256:"+hex.EncodeToString(want[:]) {
				t.Fatal("wrong streamed hash")
			}
			for _, name := range []string{"source", "target"} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("%s bytes changed: %v", name, err)
				}
			}
			info, err := os.Stat(filepath.Join(dir, "target"))
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0640 {
				t.Fatal("permission", info.Mode())
			}
			if result, err = root.Copy(t.Context(), p); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
				t.Fatal("plan reused", result, err)
			}
		})
	}
	dir, root, _ := copyFixture(t, nil)
	file, err := os.OpenFile(filepath.Join(dir, "large"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(CopyMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := root.Stat(t.Context(), "large")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = root.PrepareCopy(t.Context(), "large", "never", stat.Version); !errors.Is(err, ErrTooLarge) {
		t.Fatal("32MiB+1 accepted", err)
	}
}
func TestCopyRejectPathsVersionAndPrecommitConflicts(t *testing.T) {
	dir, root, p := copyFixture(t, []byte("original"))
	if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ArchiveDirectory, "saved"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	archived, err := root.Stat(t.Context(), ArchiveDirectory+"/saved")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][3]string{{".", "target", p.Version()}, {"source", ".", p.Version()}, {"source", "missing/child", p.Version()}, {"source", "source", p.Version()}, {"source", ArchiveDirectory + "/x", p.Version()}, {ArchiveDirectory + "/saved", "target", archived.Version}, {"../outside", "target", p.Version()}, {"source", "../outside", p.Version()}, {"source", "target", "sha256:" + strings.Repeat("a", 64)}, {"source", "target", "entry-v1:" + strings.Repeat("a", 64)}} {
		if _, err = root.PrepareCopy(t.Context(), pair[0], pair[1], pair[2]); err == nil {
			t.Fatal("accepted", pair)
		}
	}
	if err = os.WriteFile(filepath.Join(dir, "target"), []byte("competitor"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := root.Copy(t.Context(), p)
	if !errors.Is(err, ErrAlreadyExists) || result.Outcome != PublishUnchanged {
		t.Fatal(result, err)
	}
	target, _ := os.ReadFile(filepath.Join(dir, "target"))
	source, _ := os.ReadFile(filepath.Join(dir, "source"))
	if string(target) != "competitor" || string(source) != "original" {
		t.Fatal("overwrote input")
	}
}
func TestCopyCancelledAndSourceChangeBeforeCommit(t *testing.T) {
	for _, mode := range []string{"cancel", "source"} {
		t.Run(mode, func(t *testing.T) {
			dir, root, p := copyFixture(t, []byte("original"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel" {
				cancel()
			} else if err := os.WriteFile(filepath.Join(dir, "source"), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := root.Copy(ctx, p)
			if result.Outcome != PublishUnchanged || err == nil {
				t.Fatal(result, err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatal("precommit side effect", entries)
			}
		})
	}
}
