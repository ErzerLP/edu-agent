//go:build linux || darwin

package securefile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func assertLargeCopyFile(t *testing.T, path string, size int64, wantHash string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, file, make([]byte, 32<<10))
	if err != nil || n != size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != wantHash {
		t.Fatalf("%s: bytes=%d want=%d hash=%x want=%s err=%v", path, n, size, hash.Sum(nil), wantHash, err)
	}
}

func TestLargeCopyStreamsFrozenLimitAndHash(t *testing.T) {
	const size = CopyMaxBytes + (64 << 10) + 17
	dir := t.TempDir()
	file, err := os.OpenFile(filepath.Join(dir, "source"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// Write real binary bytes using a bounded buffer, not a sparse truncate.
	buffer := make([]byte, 32<<10)
	for i := range buffer {
		buffer[i] = byte(i*17 + i/251)
	}
	hash := sha256.New()
	for total := int64(0); total < size; {
		chunk := buffer[:int(min(int64(len(buffer)), size-total))]
		n, err := file.Write(chunk)
		if err != nil || n != len(chunk) {
			t.Fatalf("fixture write: n=%d err=%v", n, err)
		}
		_, _ = hash.Write(chunk)
		total += int64(n)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	wantHash := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before, err := root.Stat(t.Context(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := root.PrepareCopy(t.Context(), "source", "legacy", before.Version); p != nil || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("legacy limit bypassed: plan=%v err=%v", p, err)
	}
	limit := int64(40 << 20)
	p, err := root.PrepareCopyWithLimit(t.Context(), "source", "target", before.Version, limit)
	if err != nil {
		t.Fatal(err)
	}
	limit = 1 // Changing the caller's budget cannot change an already prepared plan.
	if p.Limit() != 40<<20 || p.Limit() == limit || p.Size() != size || p.Version() != before.Version || p.Permission() != 0640 {
		t.Fatalf("incorrect frozen plan: limit=%d size=%d permission=%v", p.Limit(), p.Size(), p.Permission())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("prepare side effects: entries=%v err=%v", entries, err)
	}
	read, write := copyRead, copyWrite
	defer func() { copyRead, copyWrite = read, write }()
	maxRead, maxWrite, maxCapacity := 0, 0, 0
	var readBytes int64
	copyRead = func(f *os.File, b []byte) (int, error) {
		maxRead, maxCapacity = max(maxRead, len(b)), max(maxCapacity, cap(b))
		n, err := read(f, b)
		readBytes += int64(n)
		return n, err
	}
	copyWrite = func(f *os.File, b []byte) (int, error) {
		maxWrite = max(maxWrite, len(b))
		return write(f, b)
	}
	result, err := root.Copy(t.Context(), p)
	if err != nil || result.Outcome != PublishCompleted || result.ContentHash != wantHash {
		t.Fatalf("result=%+v err=%v want hash=%s", result, err, wantHash)
	}
	if maxRead != 32<<10 || maxWrite > 32<<10 || maxCapacity != 32<<10 || readBytes != size {
		t.Fatalf("stream bounds: read=%d write=%d capacity=%d bytes=%d", maxRead, maxWrite, maxCapacity, readBytes)
	}
	for _, name := range []string{"source", "target"} {
		assertLargeCopyFile(t, filepath.Join(dir, name), size, wantHash)
	}
	after, err := root.Stat(t.Context(), "source")
	if err != nil || after.Version != before.Version {
		t.Fatalf("source metadata changed: before=%+v after=%+v err=%v", before, after, err)
	}
	info, err := os.Stat(filepath.Join(dir, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != p.Permission() {
		t.Fatalf("permission=%v want=%v", info.Mode().Perm(), p.Permission())
	}
}

func TestLargeCopyLimitBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int
		limit int64
		valid bool
	}{
		{"empty", 0, 1, true},
		{"equal", 257, 257, true},
		{"one_byte_below", 257, 256, false},
		{"larger_than_default", 257, 40 << 20, true},
		{"maximum_valid_limit", 257, math.MaxInt64 - 1, true},
		{"zero", 257, 0, false},
		{"zero_with_empty_source", 0, 0, false},
		{"negative", 257, -1, false},
		{"minimum_int64", 257, math.MinInt64, false},
		{"overflowing_limit", 257, math.MaxInt64, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Repeat([]byte{0xff}, tc.size)
			dir, root, legacy := copyFixture(t, data)
			if legacy.Limit() != CopyMaxBytes {
				t.Fatalf("legacy limit=%d", legacy.Limit())
			}
			p, err := root.PrepareCopyWithLimit(t.Context(), "source", "target", legacy.Version(), tc.limit)
			entries, readErr := os.ReadDir(dir)
			if readErr != nil || len(entries) != 1 {
				t.Fatalf("prepare side effects: entries=%v err=%v", entries, readErr)
			}
			if !tc.valid {
				if p != nil || !errors.Is(err, ErrTooLarge) {
					t.Fatalf("plan=%v err=%v want ErrTooLarge", p, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Limit() != tc.limit || p.Size() != int64(tc.size) {
				t.Fatalf("limit=%d size=%d", p.Limit(), p.Size())
			}
			result, err := root.Copy(t.Context(), p)
			wantHash := sha256.Sum256(data)
			want := "sha256:" + hex.EncodeToString(wantHash[:])
			if err != nil || result.Outcome != PublishCompleted || result.ContentHash != want {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			assertLargeCopyFile(t, filepath.Join(dir, "target"), int64(tc.size), want)
			assertLargeCopyFile(t, filepath.Join(dir, "source"), int64(tc.size), want)
		})
	}
}

func TestLargeCopyRejectsInvalidFrozenBudgetAndSize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CopyPlan)
	}{
		{"zero_limit", func(p *CopyPlan) { p.limit = 0 }},
		{"negative_limit", func(p *CopyPlan) { p.limit = -1 }},
		{"overflowing_limit", func(p *CopyPlan) { p.limit = math.MaxInt64 }},
		{"limit_below_size", func(p *CopyPlan) { p.limit = p.Size() - 1 }},
		{"negative_size", func(p *CopyPlan) { p.entry.Size = -1 }},
		{"size_above_limit", func(p *CopyPlan) { p.entry.Size = p.limit + 1 }},
		{"overflowing_size", func(p *CopyPlan) { p.entry.Size = math.MaxInt64 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, root, legacy := copyFixture(t, []byte("original"))
			p, err := root.PrepareCopyWithLimit(t.Context(), "source", "target", legacy.Version(), 40<<20)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(p)
			result, err := root.Copy(t.Context(), p)
			if !errors.Is(err, ErrTooLarge) || result.Outcome != PublishUnchanged || result.ContentHash != "" {
				t.Fatalf("invalid frozen budget accepted: result=%+v err=%v", result, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("invalid plan side effects: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestLargeCopyRejectsStaleVersionAndExistingTarget(t *testing.T) {
	for _, mode := range []string{"stale_version", "target_exists"} {
		t.Run(mode, func(t *testing.T) {
			dir, root, legacy := copyFixture(t, []byte("original"))
			name, want := "source", ErrChanged
			if mode == "target_exists" {
				name, want = "target", ErrAlreadyExists
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("external"+mode), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := root.PrepareCopyWithLimit(t.Context(), "source", "target", legacy.Version(), 40<<20)
			if p != nil || !errors.Is(err, want) {
				t.Fatalf("plan=%v err=%v want=%v", p, err, want)
			}
		})
	}
}

func TestLargeCopyPreservesCommitSafety(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome PublishOutcome
		err     error
	}{
		{"source_changed_before", PublishUnchanged, ErrChanged},
		{"source_grown_before", PublishUnchanged, ErrChanged},
		{"source_over_limit_before", PublishUnchanged, ErrTooLarge},
		{"source_grown_read", PublishUnchanged, ErrChanged},
		{"source_shrunk_read", PublishUnchanged, ErrChanged},
		{"cancel_before", PublishUnchanged, context.Canceled},
		{"cancel_read", PublishUnchanged, context.Canceled},
		{"cancel_published", PublishCompleted, nil},
		{"target_before", PublishUnchanged, ErrAlreadyExists},
		{"target_race", PublishUnchanged, ErrAlreadyExists},
		{"rename_unknown", PublishUnknown, ErrOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Repeat([]byte("binary\x00\xff"), 9000)
			dir, root, legacy := copyFixture(t, data)
			limit := int64(40 << 20)
			if tc.name == "source_over_limit_before" {
				limit = int64(len(data))
			}
			p, err := root.PrepareCopyWithLimit(t.Context(), "source", "target", legacy.Version(), limit)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantSource := data
			changeSource := func(changed []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "source"), changed, 0640); err != nil {
					t.Fatal(err)
				}
				wantSource = changed
			}
			createCompetitor := func() {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "target"), []byte("competitor"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "source_changed_before":
				changeSource([]byte("external change"))
			case "source_grown_before", "source_over_limit_before":
				changeSource(append(bytes.Clone(data), 'x'))
			case "cancel_before":
				cancel()
			case "target_before":
				createCompetitor()
			}
			read, rename := copyRead, copyRenameUnix
			defer func() { copyRead, copyRenameUnix = read, rename }()
			first := true
			var readBytes int64
			copyRead = func(f *os.File, b []byte) (int, error) {
				n, err := read(f, b)
				readBytes += int64(n)
				if first {
					first = false
					switch tc.name {
					case "source_grown_read":
						changeSource(append(bytes.Clone(data), 'x'))
					case "source_shrunk_read":
						changeSource(data[:8])
					case "cancel_read":
						cancel()
					}
				}
				return n, err
			}
			copyRenameUnix = func(a int, name string, b int, target string) error {
				if tc.name == "target_race" {
					createCompetitor()
				}
				err := rename(a, name, b, target)
				if tc.name == "cancel_published" {
					cancel()
				}
				if tc.name == "rename_unknown" && err == nil {
					return unix.EIO
				}
				return err
			}
			result, err := root.Copy(ctx, p)
			if result.Outcome != tc.outcome || !errors.Is(err, tc.err) {
				t.Fatalf("result=%+v err=%v want outcome=%s err=%v", result, err, tc.outcome, tc.err)
			}
			wantHash := sha256.Sum256(data)
			if tc.outcome == PublishCompleted {
				if result.ContentHash != "sha256:"+hex.EncodeToString(wantHash[:]) {
					t.Fatalf("wrong completed hash: %s", result.ContentHash)
				}
			} else if result.ContentHash != "" {
				t.Fatalf("non-completed hash: %s", result.ContentHash)
			}
			if tc.name == "source_grown_read" && readBytes != p.Size()+1 {
				t.Fatalf("growth probe bytes=%d want=%d", readBytes, p.Size()+1)
			}
			source, err := os.ReadFile(filepath.Join(dir, "source"))
			if err != nil || !bytes.Equal(source, wantSource) {
				t.Fatalf("copy modified source: err=%v", err)
			}
			target, err := os.ReadFile(filepath.Join(dir, "target"))
			switch {
			case strings.HasPrefix(tc.name, "target_"):
				if err != nil || string(target) != "competitor" {
					t.Fatalf("competitor overwritten: err=%v", err)
				}
			case tc.outcome != PublishUnchanged:
				if err != nil || !bytes.Equal(target, data) {
					t.Fatalf("published target lost: err=%v", err)
				}
			default:
				if !os.IsNotExist(err) {
					t.Fatalf("unexpected target: err=%v", err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".edu-agent-") {
					t.Fatalf("temporary file leaked: %s", entry.Name())
				}
			}
		})
	}
}
