//go:build linux || darwin

package securefile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func copyTreeTestLimits() CopyTreeLimits {
	return CopyTreeLimits{Bytes: 8 << 20, Entries: 10000, PlanBytes: 32 << 20}
}

func copyTreeFixture(t *testing.T) (string, *Root) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return dir, root
}

func copyTreeWrite(t *testing.T, dir, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, path), data, 0751); err != nil {
		t.Fatal(err)
	}
}

func copyTreePlan(t *testing.T, root *Root) *CopyTreePlan {
	t.Helper()
	entry, err := root.Stat(t.Context(), "source")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := root.PrepareCopyTree(t.Context(), "source", "target", entry.Version, copyTreeTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func copyTreeObserver() CopyTreeObserver {
	return CopyTreeObserver{
		Before: func(context.Context, int, CopyTreeItem) error { return nil },
		After:  func(context.Context, int, CopyTreeItem, CopyTreeItemResult) error { return nil },
	}
}

func copyTreeAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected entry %q: %v", path, err)
	}
}

func TestCopyTreeNestedBinaryEmptyAndImmutablePlan(t *testing.T) {
	dir, root := copyTreeFixture(t)
	for _, path := range []string{"source/empty", "source/nested", "source/nested/deeper"} {
		if err := os.Mkdir(filepath.Join(dir, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := map[string][]byte{
		"source/a":                  bytes.Repeat([]byte{0, 255, 1, 254, 10}, 15000),
		"source/nested/b":           []byte("nested"),
		"source/nested/deeper/zero": {},
	}
	var total int64
	for path, body := range data {
		copyTreeWrite(t, dir, path, body)
		total += int64(len(body))
	}
	plan := copyTreePlan(t, root)
	copyTreeAbsent(t, filepath.Join(dir, "target"))
	if plan.Source() != "source" || plan.Destination() != "target" || plan.Version() == "" || plan.Bytes() != total {
		t.Fatal("invalid frozen summary", plan.Source(), plan.Destination(), plan.Bytes())
	}
	items := plan.Items()
	if len(items) != 7 || items[0].Kind != EntryDirectory {
		t.Fatal(items)
	}
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Source
	}
	if !sort.StringsAreSorted(names) {
		t.Fatal("unstable traversal", names)
	}
	items[0].Destination = "tampered"
	if plan.Items()[0].Destination != "target" {
		t.Fatal("Items exposed mutable plan")
	}
	before, after := 0, 0
	observer := CopyTreeObserver{
		Before: func(ctx context.Context, index int, item CopyTreeItem) error {
			if index != before || before != after {
				t.Fatal("unpaired Before", index, before, after)
			}
			copyTreeAbsent(t, filepath.Join(dir, item.Destination))
			before++
			return nil
		},
		After: func(ctx context.Context, index int, item CopyTreeItem, result CopyTreeItemResult) error {
			if index != after || before != after+1 || !result.Attempted || result.Outcome != PublishCompleted || result.Err != nil {
				t.Fatal("incorrect actual settlement", index, result)
			}
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
				t.Fatal("unbounded/cancelled settlement context")
			}
			after++
			return nil
		},
	}
	result, err := root.CopyTree(t.Context(), plan, observer)
	if err != nil || result.Outcome != PublishCompleted || result.Completed != len(items) || result.Unknown != 0 || result.CleanupIncomplete || before != len(items) || after != before {
		t.Fatal(result, err, before, after)
	}
	for i, item := range plan.Items() {
		info, err := os.Stat(filepath.Join(dir, item.Destination))
		if err != nil {
			t.Fatal(err)
		}
		if item.Kind == EntryDirectory {
			if !info.IsDir() || info.Mode().Perm() != 0700 || result.Items[i].Bytes != 0 || result.Items[i].ContentHash != "" {
				t.Fatal("directory metadata", info, result.Items[i])
			}
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, item.Destination))
		original, originalErr := os.ReadFile(filepath.Join(dir, item.Source))
		hash := sha256.Sum256(data[item.Source])
		if err != nil || originalErr != nil || !bytes.Equal(body, data[item.Source]) || !bytes.Equal(original, body) || info.Mode().Perm() != 0751 || result.Items[i].ContentHash != "sha256:"+hex.EncodeToString(hash[:]) || result.Items[i].Bytes != int64(len(body)) {
			t.Fatal("file contents/permissions/receipt", item, result.Items[i], err, originalErr)
		}
	}
	if repeated, err := root.CopyTree(t.Context(), plan, observer); !errors.Is(err, ErrChanged) || repeated.Outcome != PublishUnchanged || before != len(items) {
		t.Fatal("plan was reused", repeated, err)
	}
}

func TestCopyTree2501Files(t *testing.T) {
	dir, root := copyTreeFixture(t)
	for i := 0; i < 2501; i++ {
		copyTreeWrite(t, dir, fmt.Sprintf("source/f%04d", i), []byte{byte(i), byte(i >> 8), 0, 255})
	}
	plan := copyTreePlan(t, root)
	if len(plan.Items()) != 2502 || plan.Bytes() != 2501*4 {
		t.Fatal("truncated plan", len(plan.Items()), plan.Bytes())
	}
	count := 0
	observer := copyTreeObserver()
	observer.Before = func(_ context.Context, i int, item CopyTreeItem) error {
		if i != count {
			t.Fatal("missing/duplicate item", i, count)
		}
		if i > 0 && item.Source != fmt.Sprintf("source/f%04d", i-1) {
			t.Fatal("order", item)
		}
		count++
		return nil
	}
	result, err := root.CopyTree(t.Context(), plan, observer)
	if err != nil || result.Outcome != PublishCompleted || result.Completed != 2502 || count != 2502 {
		t.Fatalf("outcome=%s completed=%d before=%d err=%v", result.Outcome, result.Completed, count, err)
	}
	for i := 0; i < 2501; i++ {
		body, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("target/f%04d", i)))
		want := []byte{byte(i), byte(i >> 8), 0, 255}
		hash := sha256.Sum256(want)
		if err != nil || !bytes.Equal(body, want) || result.Items[i+1].ContentHash != "sha256:"+hex.EncodeToString(hash[:]) {
			t.Fatal("incomplete copy", i, err)
		}
	}
}

func TestCopyTreePreflightRejectsWithoutEffects(t *testing.T) {
	for _, name := range []string{"existing", "missing_parent", "source_root", "destination_root", "source_archive", "destination_archive", "source_link", "nested_link", "parent_link", "fifo", "regular_root", "case_target", "case_items", "unicode_case_items", "self", "descendant", "case_descendant", "bytes", "entries", "plan_bytes", "max_bytes", "zero_bytes", "max_entries", "zero_entries", "max_plan_bytes", "zero_plan_bytes", "invalid_version", "long_path"} {
		t.Run(name, func(t *testing.T) {
			dir, root := copyTreeFixture(t)
			copyTreeWrite(t, dir, "source/file", []byte("body"))
			source, destination := "source", "target"
			limits := copyTreeTestLimits()
			switch name {
			case "existing":
				copyTreeWrite(t, dir, "target", []byte("competitor"))
			case "missing_parent":
				destination = "missing/target"
			case "source_root":
				source = "."
			case "destination_root":
				destination = "."
			case "source_archive", "destination_archive":
				if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
					t.Fatal(err)
				}
				if name == "source_archive" {
					source = ArchiveDirectory
				} else {
					destination = ArchiveDirectory + "/target"
				}
			case "source_link", "nested_link", "parent_link":
				path := "link"
				if name == "nested_link" {
					path = "source/link"
				}
				if err := os.Symlink(filepath.Join(dir, "source"), filepath.Join(dir, path)); err != nil {
					t.Fatal(err)
				}
				if name == "source_link" {
					source = "link"
				}
				if name == "parent_link" {
					destination = "link/target"
				}
			case "fifo":
				if err := unix.Mkfifo(filepath.Join(dir, "source/fifo"), 0600); err != nil {
					t.Fatal(err)
				}
			case "regular_root":
				source = "source/file"
			case "case_target":
				copyTreeWrite(t, dir, "TARGET", []byte("competitor"))
			case "case_items", "unicode_case_items":
				a, b := "A", "a"
				if name == "unicode_case_items" {
					a, b = "Σ", "ς"
				}
				copyTreeWrite(t, dir, "source/"+a, []byte("a"))
				copyTreeWrite(t, dir, "source/"+b, []byte("b"))
				x, _ := os.Stat(filepath.Join(dir, "source/"+a))
				y, _ := os.Stat(filepath.Join(dir, "source/"+b))
				if os.SameFile(x, y) {
					t.Skip("case-insensitive filesystem cannot construct conflicting source entries")
				}
			case "self":
				destination = "source"
			case "descendant":
				destination = "source/target"
			case "case_descendant":
				destination = "SOURCE/target"
			case "bytes":
				limits.Bytes = 3
			case "entries":
				limits.Entries = 1
			case "plan_bytes":
				limits.PlanBytes = 1
			case "max_bytes":
				limits.Bytes = math.MaxInt64
			case "zero_bytes":
				limits.Bytes = 0
			case "max_entries":
				limits.Entries = 1000001
			case "zero_entries":
				limits.Entries = 0
			case "max_plan_bytes":
				limits.PlanBytes = 1<<30 + 1
			case "zero_plan_bytes":
				limits.PlanBytes = 0
			case "long_path":
				destination = strings.Repeat("p/", 65) + "target"
			}
			info, err := root.Stat(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			version := info.Version
			if name == "invalid_version" {
				version = "sha256:" + strings.Repeat("a", 64)
			}
			before, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := root.PrepareCopyTree(t.Context(), source, destination, version, limits)
			if err == nil || plan != nil {
				t.Fatal("accepted invalid tree", plan, err)
			}
			after, err := os.ReadDir(dir)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("preflight had effects", err, before, after)
			}
		})
	}
}

func TestCopyTreeFrozenChangesRejectBeforeObserver(t *testing.T) {
	for _, change := range []string{"add", "modify", "replace_file", "root_parent", "target_appeared"} {
		t.Run(change, func(t *testing.T) {
			dir, root := copyTreeFixture(t)
			copyTreeWrite(t, dir, "source/file", []byte("body"))
			plan := copyTreePlan(t, root)
			switch change {
			case "add":
				copyTreeWrite(t, dir, "source/new", []byte("new"))
			case "modify":
				copyTreeWrite(t, dir, "source/file", []byte("different"))
			case "replace_file":
				if err := os.Rename(filepath.Join(dir, "source/file"), filepath.Join(dir, "original")); err != nil {
					t.Fatal(err)
				}
				copyTreeWrite(t, dir, "source/file", []byte("body"))
			case "root_parent":
				// Freeze a non-root destination parent, then replace its identity.
				if err := os.Mkdir(filepath.Join(dir, "parent"), 0700); err != nil {
					t.Fatal(err)
				}
				entry, _ := root.Stat(t.Context(), "source")
				var err error
				plan, err = root.PrepareCopyTree(t.Context(), "source", "parent/target", entry.Version, copyTreeTestLimits())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(dir, "parent"), filepath.Join(dir, "old-parent")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "parent"), 0700); err != nil {
					t.Fatal(err)
				}
			case "target_appeared":
				copyTreeWrite(t, dir, "target", []byte("competitor"))
			}
			observer := copyTreeObserver()
			observer.Before = func(context.Context, int, CopyTreeItem) error { t.Fatal("stale plan reached observer"); return nil }
			result, err := root.CopyTree(t.Context(), plan, observer)
			if err == nil || result.Outcome != PublishUnchanged || result.Completed != 0 {
				t.Fatal(result, err)
			}
			for _, item := range result.Items {
				if item.Attempted {
					t.Fatal("stale plan attempted", item)
				}
			}
			if change != "target_appeared" {
				copyTreeAbsent(t, filepath.Join(dir, plan.Destination()))
			}
		})
	}
}

func TestCopyTreeObserverFailuresAndCancellationKeepPrefix(t *testing.T) {
	for _, fault := range []string{"before_first", "before_file", "after_file", "cancel_before", "cancel_after", "cancel_last", "source_between", "source_during", "missing_observer"} {
		t.Run(fault, func(t *testing.T) {
			dir, root := copyTreeFixture(t)
			copyTreeWrite(t, dir, "source/a", []byte("a"))
			copyTreeWrite(t, dir, "source/b", []byte("b"))
			plan := copyTreePlan(t, root)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			before, after := 0, 0
			injected := errors.New("observer failure")
			observer := CopyTreeObserver{
				Before: func(_ context.Context, i int, _ CopyTreeItem) error {
					before++
					if fault == "before_first" && i == 0 || fault == "before_file" && i == 1 {
						return injected
					}
					if fault == "cancel_before" && i == 1 {
						cancel()
					}
					if fault == "source_during" && i == 1 {
						copyTreeWrite(t, dir, "source/a", []byte("changed"))
					}
					return nil
				},
				After: func(ctx context.Context, i int, _ CopyTreeItem, result CopyTreeItemResult) error {
					after++
					if ctx.Err() != nil {
						t.Fatal("cancelled settlement")
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("unbounded settlement")
					}
					if !result.Attempted {
						t.Fatal("lost attempt")
					}
					if fault == "after_file" && i == 1 {
						return injected
					}
					if fault == "cancel_after" && i == 1 || fault == "cancel_last" && i == 2 {
						cancel()
					}
					if fault == "source_between" && i == 1 {
						copyTreeWrite(t, dir, "source/new", []byte("new"))
					}
					return nil
				},
			}
			if fault == "missing_observer" {
				observer.After = nil
			}
			result, err := root.CopyTree(ctx, plan, observer)
			if fault == "cancel_last" {
				if err != nil || result.Outcome != PublishCompleted || result.Completed != 3 {
					t.Fatal(result, err)
				}
				return
			}
			if err == nil {
				t.Fatal("fault ignored", result)
			}
			if fault == "before_first" || fault == "missing_observer" {
				if result.Outcome != PublishUnchanged || result.Completed != 0 || after != 0 {
					t.Fatal(result, err)
				}
				copyTreeAbsent(t, filepath.Join(dir, "target"))
				return
			}
			if result.Outcome != PublishUnknown || result.Completed < 1 {
				t.Fatal("lost real prefix", result, err)
			}
			if fault == "after_file" || fault == "cancel_after" || fault == "source_between" {
				body, readErr := os.ReadFile(filepath.Join(dir, "target/a"))
				if readErr != nil || string(body) != "a" || result.Items[1].Outcome != PublishCompleted || result.Items[1].Err != nil {
					t.Fatal("lost completed item", result, readErr)
				}
			} else {
				copyTreeAbsent(t, filepath.Join(dir, "target/a"))
			}
			copyTreeAbsent(t, filepath.Join(dir, "target/b"))
			if fault == "before_file" && result.Items[1].Attempted {
				t.Fatal("rejected Before counted attempted")
			}
			if fault == "cancel_before" && (!result.Items[1].Attempted || after != 2) {
				t.Fatal("cancelled attempted item not settled", before, after, result)
			}
		})
	}
}

func TestCopyTreeOwnedDirectoryRelocationStops(t *testing.T) {
	for _, place := range []string{"replace", "inside", "outside", "archive", "nested_replace", "stream_outside"} {
		t.Run(place, func(t *testing.T) {
			dir, root := copyTreeFixture(t)
			if err := os.Mkdir(filepath.Join(dir, "source/sub"), 0700); err != nil {
				t.Fatal(err)
			}
			copyTreeWrite(t, dir, "source/sub/a", bytes.Repeat([]byte("a"), 70000))
			copyTreeWrite(t, dir, "source/sub/b", []byte("b"))
			plan := copyTreePlan(t, root)
			moved := filepath.Join(dir, "moved")
			if place == "outside" || place == "stream_outside" {
				moved = filepath.Join(t.TempDir(), "moved")
			}
			if place == "archive" {
				if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
					t.Fatal(err)
				}
				moved = filepath.Join(dir, ArchiveDirectory, "moved")
			}
			move := func() {
				path := filepath.Join(dir, "target")
				if place == "nested_replace" {
					path = filepath.Join(path, "sub")
				}
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				if place == "replace" || place == "nested_replace" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			observer := copyTreeObserver()
			observer.Before = func(_ context.Context, i int, _ CopyTreeItem) error {
				if place != "stream_outside" && i == 2 {
					move()
				}
				return nil
			}
			write := copyWrite
			defer func() { copyWrite = write }()
			if place == "stream_outside" {
				once := false
				copyWrite = func(f *os.File, b []byte) (int, error) {
					n, err := write(f, b)
					if !once {
						once = true
						move()
					}
					return n, err
				}
			}
			result, err := root.CopyTree(t.Context(), plan, observer)
			if err == nil || result.Outcome != PublishUnknown || result.Completed != 2 || !result.Items[2].Attempted || result.Items[3].Attempted {
				t.Fatal(result, err)
			}
			copyTreeAbsent(t, filepath.Join(dir, "target/sub/a"))
			copyTreeAbsent(t, filepath.Join(moved, "sub/a"))
			copyTreeAbsent(t, filepath.Join(moved, "a"))
			if place == "stream_outside" && (!result.CleanupIncomplete || result.Items[2].Outcome != PublishUnknown) {
				t.Fatal("lost staging uncertainty", result)
			}
		})
	}
}

func TestCopyTreeDirectoryAndFilePublicationFaults(t *testing.T) {
	for _, fault := range []string{"mkdir_unknown", "open_failure", "temp_replace", "directory_rename_unknown", "directory_rename_unsupported", "directory_parent_sync", "file_rename_unknown", "close_failure"} {
		t.Run(fault, func(t *testing.T) {
			dir, root := copyTreeFixture(t)
			copyTreeWrite(t, dir, "source/a", []byte("a"))
			copyTreeWrite(t, dir, "source/b", []byte("b"))
			plan := copyTreePlan(t, root)
			mkdir, open, rename, sync, close, fileRename := copyTreeMkdirUnix, copyTreeOpenUnix, copyTreeRenameUnix, copyTreeSyncUnix, copyTreeClose, copyRenameUnix
			defer func() {
				copyTreeMkdirUnix = mkdir
				copyTreeOpenUnix = open
				copyTreeRenameUnix = rename
				copyTreeSyncUnix = sync
				copyTreeClose = close
				copyRenameUnix = fileRename
			}()
			if fault == "mkdir_unknown" {
				copyTreeMkdirUnix = func(fd int, name string, mode uint32) error {
					if err := mkdir(fd, name, mode); err != nil {
						return err
					}
					return unix.EIO
				}
			}
			if fault == "open_failure" {
				copyTreeOpenUnix = func(int, string, int, uint32) (int, error) { return -1, unix.EIO }
			}
			if fault == "temp_replace" {
				copyTreeOpenUnix = func(fd int, name string, flags int, mode uint32) (int, error) {
					if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, "owned-moved")); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
						t.Fatal(err)
					}
					copyTreeWrite(t, dir, name+"/competitor", []byte("keep"))
					return open(fd, name, flags, mode)
				}
			}
			if fault == "directory_rename_unknown" {
				copyTreeRenameUnix = func(a int, name string, b int, target string) error {
					if err := rename(a, name, b, target); err != nil {
						return err
					}
					return unix.EIO
				}
			}
			if fault == "directory_rename_unsupported" {
				copyTreeRenameUnix = func(int, string, int, string) error { return unix.ENOTSUP }
			}
			if fault == "directory_parent_sync" {
				copyTreeSyncUnix = func(fd int) error {
					if fd == int(root.file.Fd()) {
						return unix.EIO
					}
					if _, err := os.Stat(filepath.Join(dir, "target")); err == nil {
						return unix.EIO
					}
					return sync(fd)
				}
			}
			if fault == "file_rename_unknown" {
				copyRenameUnix = func(a int, name string, b int, target string) error {
					if err := fileRename(a, name, b, target); err != nil {
						return err
					}
					return unix.EIO
				}
			}
			if fault == "close_failure" {
				copyTreeClose = func(f *os.File) error { return errors.Join(close(f), unix.EIO) }
			}
			var receipts []CopyTreeItemResult
			observer := copyTreeObserver()
			observer.After = func(_ context.Context, _ int, _ CopyTreeItem, item CopyTreeItemResult) error {
				receipts = append(receipts, item)
				return nil
			}
			result, err := root.CopyTree(t.Context(), plan, observer)
			if err == nil {
				t.Fatal("fault ignored", result)
			}
			if fault == "directory_rename_unsupported" {
				if result.Outcome != PublishUnchanged || result.CleanupIncomplete || result.Completed != 0 {
					t.Fatal(result, err)
				}
				entries, _ := os.ReadDir(dir)
				if len(entries) != 1 {
					t.Fatal("private temp not cleaned", entries)
				}
				return
			}
			if result.Outcome != PublishUnknown || !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("unknown lost", result, err)
			}
			if fault != "directory_parent_sync" && !result.CleanupIncomplete {
				t.Fatal("cleanup uncertainty hidden", result)
			}
			if fault == "file_rename_unknown" {
				if result.Completed != 1 || result.Unknown != 1 || result.Items[2].Attempted || len(receipts) != 2 || receipts[1].ContentHash != "" {
					t.Fatal(result, receipts)
				}
				body, readErr := os.ReadFile(filepath.Join(dir, "target/a"))
				if readErr != nil || string(body) != "a" {
					t.Fatal("unknown publication deleted", readErr)
				}
			}
			if fault == "directory_rename_unknown" || fault == "directory_parent_sync" {
				if info, statErr := os.Stat(filepath.Join(dir, "target")); statErr != nil || !info.IsDir() {
					t.Fatal("published directory removed", statErr)
				}
				if result.Items[1].Attempted {
					t.Fatal("continued after unknown", result)
				}
			}
			if fault == "temp_replace" {
				entries, _ := os.ReadDir(dir)
				found := false
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".edu-agent-") {
						body, readErr := os.ReadFile(filepath.Join(dir, entry.Name(), "competitor"))
						if readErr != nil || string(body) != "keep" {
							t.Fatal("deleted replacement", readErr)
						}
						found = true
					}
				}
				if !found {
					t.Fatal("replacement disappeared")
				}
			}
		})
	}
}

func TestCopyTreeNoRetainedPreparationOrExecutionFDs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor inventory uses Linux procfs")
	}
	dir, root := copyTreeFixture(t)
	copyTreeWrite(t, dir, "source/a", []byte("a"))
	count := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	for i := 0; i < 6; i++ {
		plan := copyTreePlan(t, root)
		if current := count(); current != before {
			t.Fatalf("prepare retained descriptors: %d -> %d", before, current)
		}
		observer := copyTreeObserver()
		observer.Before = func(context.Context, int, CopyTreeItem) error { return context.Canceled }
		if result, err := root.CopyTree(t.Context(), plan, observer); err == nil || result.Outcome != PublishUnchanged {
			t.Fatal(result, err)
		}
		if current := count(); current != before {
			t.Fatalf("aborted execution leaked descriptors: %d -> %d", before, current)
		}
	}
	if result, err := root.CopyTree(t.Context(), copyTreePlan(t, root), copyTreeObserver()); err != nil || result.Outcome != PublishCompleted {
		t.Fatal(result, err)
	}
	if current := count(); current != before {
		t.Fatalf("completed execution leaked descriptors: %d -> %d", before, current)
	}
}
