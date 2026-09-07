//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const purgeTestPath = ArchiveDirectory + "/container/selected"

func purgeTestLimits() PurgeLimits { return PurgeLimits{Entries: 10000, PlanBytes: 32 << 20} }

func purgeTestPlan(t *testing.T, root *Root, path string) *PurgePlan {
	t.Helper()
	info, err := root.Stat(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := root.PreparePurge(t.Context(), path, info.Version, purgeTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if p.Path() != path || p.Version() != info.Version || p.Kind() != info.Kind || p.Identity() != info.Identity {
		t.Fatal("incorrect root metadata", p, info)
	}
	return p
}

func purgeTestObserver() PurgeObserver {
	return PurgeObserver{
		Before: func(context.Context, int, PurgeItem) error { return nil },
		After:  func(context.Context, int, PurgeItem, PurgeItemResult) error { return nil },
	}
}

func purgeTestTree(t *testing.T) (*Root, string, *PurgePlan) {
	t.Helper()
	root, dir := archiveTestRoot(t)
	for _, name := range []string{"a", "b"} {
		archiveTestWrite(t, filepath.Join(dir, purgeTestPath, name), []byte("original"))
	}
	return root, dir, purgeTestPlan(t, root, purgeTestPath)
}

func purgeTestUnstarted(t *testing.T, result PurgeResult, from int) {
	t.Helper()
	for i := from; i < len(result.Items); i++ {
		if result.Items[i].Attempted || result.Items[i].Outcome != PublishUnchanged || result.Items[i].Bytes != 0 {
			t.Fatal("unattempted item was promoted", i, result.Items[i])
		}
	}
}

func TestArchivePurgeCompleteFrozenPostorder2501(t *testing.T) {
	root, dir := archiveTestRoot(t)
	for i := 0; i < 2501; i++ {
		archiveTestWrite(t, filepath.Join(dir, purgeTestPath, fmt.Sprintf("%04d", i)), []byte{byte(i), 0, 255})
	}
	archiveTestWrite(t, filepath.Join(dir, purgeTestPath, "nested/child/data"), []byte("nested"))
	if err := os.Mkdir(filepath.Join(dir, purgeTestPath, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	archiveTestWrite(t, filepath.Join(outside, "sentinel"), []byte("untouched"))
	for _, link := range []struct{ name, target string }{{"outside", outside}, {"relative", "nested"}, {"dangling", "missing"}} {
		if err := os.Symlink(link.target, filepath.Join(dir, purgeTestPath, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	archiveTestWrite(t, filepath.Join(dir, ArchiveDirectory, "unselected"), []byte("keep"))
	before := make(map[string]EntryInfo)
	err := filepath.WalkDir(filepath.Join(dir, purgeTestPath), func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		entry, err := root.Stat(t.Context(), filepath.ToSlash(rel))
		before[filepath.ToSlash(rel)] = entry
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p := purgeTestPlan(t, root, purgeTestPath)
	items := p.Items()
	if len(items) != len(before) || len(items) <= 2501 || items[len(items)-1].Path != purgeTestPath || p.Bytes() != 2501*3+6 {
		t.Fatal("incomplete plan", len(items), len(before), p.Bytes())
	}
	indexes := make(map[string]int)
	for i, item := range items {
		if _, dup := indexes[item.Path]; dup {
			t.Fatal("duplicate", item.Path)
		}
		indexes[item.Path] = i
		entry, err := root.Stat(t.Context(), item.Path)
		if err != nil || entry != before[item.Path] {
			t.Fatal("preview changed entry", item.Path, entry, err)
		}
		if item.Kind != entry.Kind || item.Version != entry.Version || item.Size != entry.Size {
			t.Fatal("wrong item", item, entry)
		}
	}
	for _, item := range items {
		if item.Path != p.Path() && indexes[filepath.ToSlash(filepath.Dir(item.Path))] <= indexes[item.Path] {
			t.Fatal("not postorder", item.Path)
		}
	}
	// Neither mutation of a returned slice nor changing its length changes authority.
	items[0] = PurgeItem{Path: "outside", Kind: EntryFile}
	items = append(items, PurgeItem{Path: ArchiveDirectory})
	if p.Items()[0].Path == "outside" || len(p.Items()) != len(before) {
		t.Fatal("mutable plan")
	}
	observer := purgeTestObserver()
	beforeCalls, afterCalls := 0, 0
	observer.Before = func(_ context.Context, i int, item PurgeItem) error {
		if i != beforeCalls {
			t.Fatal("wrong before order", i, beforeCalls)
		}
		beforeCalls++
		if _, err := os.Lstat(filepath.Join(dir, item.Path)); err != nil {
			t.Fatal("intent after deletion", err)
		}
		return nil
	}
	observer.After = func(ctx context.Context, i int, item PurgeItem, actual PurgeItemResult) error {
		afterCalls++
		if ctx.Err() != nil || !actual.Attempted || actual.Outcome != PublishCompleted || actual.Err != nil {
			t.Fatal(i, actual, ctx.Err())
		}
		want := int64(0)
		if item.Kind == EntryFile {
			want = item.Size
		}
		if actual.Bytes != want {
			t.Fatal("wrong logical bytes", actual, item)
		}
		restoreTestAbsent(t, filepath.Join(dir, item.Path))
		return nil
	}
	result, err := root.Purge(t.Context(), p, observer)
	if err != nil || result.Outcome != PublishCompleted || result.Completed != len(before) || result.Unknown != 0 || result.CleanupIncomplete || beforeCalls != len(before) || afterCalls != len(before) {
		t.Fatal(result, err, beforeCalls, afterCalls)
	}
	restoreTestAbsent(t, filepath.Join(dir, purgeTestPath))
	if _, err := os.Stat(filepath.Join(dir, ArchiveDirectory, "container")); err != nil {
		t.Fatal("removed unselected parent", err)
	}
	restoreTestBytes(t, filepath.Join(dir, ArchiveDirectory, "unselected"), []byte("keep"))
	restoreTestBytes(t, filepath.Join(outside, "sentinel"), []byte("untouched"))
}

func TestArchivePurgeLeavesContainersAndLargeBinary(t *testing.T) {
	for _, kind := range []string{"file", "binary", "empty", "container", "link-file", "link-dir", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := archiveTestRoot(t)
			path := purgeTestPath
			if kind == "container" {
				path = ArchiveDirectory + "/container"
			}
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			archiveTestWrite(t, filepath.Join(outside, "sentinel"), []byte("outside"))
			wantBytes := int64(0)
			switch kind {
			case "file":
				archiveTestWrite(t, full, []byte("small"))
				wantBytes = 5
			case "binary":
				data := make([]byte, (33<<20)+17)
				for i := range data {
					data[i] = byte(i * 31)
				}
				archiveTestWrite(t, full, data)
				wantBytes = int64(len(data))
			case "empty", "container":
				if err := os.Mkdir(full, 0700); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(outside, "sentinel"), full); err != nil {
					t.Fatal(err)
				}
				wantBytes = 7
			default:
				target := outside
				if kind == "link-file" {
					target = filepath.Join(outside, "sentinel")
				}
				if err := os.Symlink(target, full); err != nil {
					t.Fatal(err)
				}
			}
			p := purgeTestPlan(t, root, path)
			if len(p.Items()) != 1 || p.Bytes() != wantBytes {
				t.Fatal("wrong leaf plan", p.Items(), p.Bytes())
			}
			result, err := root.Purge(t.Context(), p, purgeTestObserver())
			if err != nil || result.Outcome != PublishCompleted || result.Completed != 1 || result.Items[0].Bytes != wantBytes {
				t.Fatal(result, err)
			}
			restoreTestAbsent(t, full)
			if _, err := os.Stat(filepath.Dir(full)); err != nil {
				t.Fatal("removed unselected parent", err)
			}
			restoreTestBytes(t, filepath.Join(outside, "sentinel"), []byte("outside"))
		})
	}
}

func TestArchivePurgeRejectsPathsVersionsBudgetsAndSpecialEntries(t *testing.T) {
	root, dir, p := purgeTestTree(t)
	if err := os.Symlink(filepath.Join(dir, ArchiveDirectory), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", ".", ArchiveDirectory, "ordinary", "alias/container/selected", strings.ToUpper(ArchiveDirectory) + "/container/selected", "/" + purgeTestPath, purgeTestPath + "/..", ArchiveDirectory + "//container", ArchiveDirectory + "/../selected", purgeTestPath + "/", purgeTestPath + "\x00", purgeTestPath + "\\a"} {
		if got, err := root.PreparePurge(t.Context(), path, p.Version(), purgeTestLimits()); got != nil || err == nil {
			t.Fatal("accepted path", path, got, err)
		}
	}
	for _, version := range []string{"", strings.ToUpper(p.Version()), "sha256:" + strings.Repeat("0", 64), archiveMetadataVersion("not current")} {
		if got, err := root.PreparePurge(t.Context(), p.Path(), version, purgeTestLimits()); got != nil || !errors.Is(err, ErrChanged) {
			t.Fatal("accepted version", got, err)
		}
	}
	for _, limits := range []PurgeLimits{{0, 10000}, {1000001, 10000}, {100, 0}, {100, 1<<30 + 1}, {2, 32 << 20}, {100, 1}} {
		if got, err := root.PreparePurge(t.Context(), p.Path(), p.Version(), limits); got != nil || !errors.Is(err, ErrTooLarge) {
			t.Fatal("accepted budget", limits, got, err)
		}
	}
	fifo := filepath.Join(dir, purgeTestPath, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := root.Stat(t.Context(), p.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := root.PreparePurge(t.Context(), p.Path(), entry.Version, purgeTestLimits()); got != nil || !errors.Is(err, ErrNotRegular) {
		t.Fatal("accepted special subtree", got, err)
	}
	entry, err = root.Stat(t.Context(), purgeTestPath+"/fifo")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := root.PreparePurge(t.Context(), purgeTestPath+"/fifo", entry.Version, purgeTestLimits()); got != nil || !errors.Is(err, ErrNotRegular) {
		t.Fatal("accepted special root", got, err)
	}
	restoreTestBytes(t, filepath.Join(dir, purgeTestPath, "a"), []byte("original"))
}

func TestArchivePurgeRootBindingConsumptionAndRequiredObservers(t *testing.T) {
	for _, mode := range []string{"success", "before-failure", "nil-before", "nil-after", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			root, dir, p := purgeTestTree(t)
			other, _ := archiveTestRoot(t)
			if result, err := other.Purge(t.Context(), p, purgeTestObserver()); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
				t.Fatal("cross-root", result, err)
			}
			copied := *p // The consumption token is shared even through a value copy.
			observer := purgeTestObserver()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "before-failure":
				observer.Before = func(context.Context, int, PurgeItem) error { return errors.New("intent failed") }
			case "nil-before":
				observer.Before = nil
			case "nil-after":
				observer.After = nil
			case "canceled":
				cancel()
			}
			result, err := root.Purge(ctx, p, observer)
			if mode == "success" {
				if err != nil || result.Outcome != PublishCompleted {
					t.Fatal(result, err)
				}
			} else {
				if err == nil || result.Outcome != PublishUnchanged {
					t.Fatal(result, err)
				}
				purgeTestUnstarted(t, result, 0)
				restoreTestBytes(t, filepath.Join(dir, purgeTestPath, "a"), []byte("original"))
			}
			for _, reused := range []*PurgePlan{p, &copied} {
				result, err := root.Purge(t.Context(), reused, purgeTestObserver())
				if !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
					t.Fatal("reused authority", result, err)
				}
				purgeTestUnstarted(t, result, 0)
			}
		})
	}
}

func TestArchivePurgeChangesRejectBeforeAnyIntent(t *testing.T) {
	for _, change := range []string{"new-child", "new-nested-child", "replace-file", "replace-root", "parent-outside", "parent-renamed", "archive-replaced", "archive-link", "parent-link"} {
		t.Run(change, func(t *testing.T) {
			root, dir := archiveTestRoot(t)
			archiveTestWrite(t, filepath.Join(dir, purgeTestPath, "nested/file"), []byte("original"))
			p := purgeTestPlan(t, root, purgeTestPath)
			outside := t.TempDir()
			full := filepath.Join(dir, purgeTestPath)
			moved := ""
			switch change {
			case "new-child":
				archiveTestWrite(t, filepath.Join(full, "new"), []byte("new"))
			case "new-nested-child":
				archiveTestWrite(t, filepath.Join(full, "nested/new"), []byte("new"))
			case "replace-file":
				if err := os.Rename(filepath.Join(full, "nested/file"), filepath.Join(full, "saved")); err != nil {
					t.Fatal(err)
				}
				archiveTestWrite(t, filepath.Join(full, "nested/file"), []byte("replacement"))
			default:
				old := full
				if strings.HasPrefix(change, "parent") {
					old = filepath.Dir(full)
				}
				if strings.HasPrefix(change, "archive") {
					old = filepath.Join(dir, ArchiveDirectory)
				}
				moved = old + "-saved"
				if change == "parent-outside" {
					moved = filepath.Join(outside, "moved")
				}
				if err := os.Rename(old, moved); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(change, "link") {
					if err := os.Symlink(moved, old); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(old, 0700); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			obs := purgeTestObserver()
			obs.Before = func(context.Context, int, PurgeItem) error { calls++; return nil }
			result, err := root.Purge(t.Context(), p, obs)
			if err == nil || result.Outcome != PublishUnchanged || calls != 0 || result.Completed != 0 {
				t.Fatal("mutated preflight executed", result, err, calls)
			}
			purgeTestUnstarted(t, result, 0)
			if moved != "" {
				if _, err := os.Stat(moved); err != nil {
					t.Fatal("touched relocated data", err)
				}
			}
		})
	}
}

func TestArchivePurgeStopsConfirmedPrefixOnCallbacksAndChanges(t *testing.T) {
	for _, mode := range []string{"before", "after", "cancel-before", "cancel-after", "replace-file", "move-root", "new-child"} {
		t.Run(mode, func(t *testing.T) {
			root, dir, p := purgeTestTree(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			obs := purgeTestObserver()
			sentinel := errors.New("observer failed")
			afterCalls := 0
			moved := filepath.Join(t.TempDir(), "moved")
			obs.Before = func(_ context.Context, i int, item PurgeItem) error {
				if i != 1 {
					return nil
				}
				switch mode {
				case "before":
					return sentinel
				case "cancel-before":
					cancel()
				case "replace-file":
					if err := os.Rename(filepath.Join(dir, item.Path), filepath.Join(dir, item.Path)+"-saved"); err != nil {
						t.Fatal(err)
					}
					archiveTestWrite(t, filepath.Join(dir, item.Path), []byte("replacement"))
				case "move-root":
					if err := os.Rename(filepath.Join(dir, p.Path()), moved); err != nil {
						t.Fatal(err)
					}
				case "new-child":
					archiveTestWrite(t, filepath.Join(dir, p.Path(), "new"), []byte("new"))
				}
				return nil
			}
			obs.After = func(settle context.Context, i int, item PurgeItem, actual PurgeItemResult) error {
				afterCalls++
				if settle.Err() != nil {
					t.Fatal("settlement inherited cancellation", settle.Err())
				}
				if deadline, ok := settle.Deadline(); !ok || time.Until(deadline) > purgeSettleTimeout {
					t.Fatal("unbounded settlement")
				}
				if i == 0 {
					if actual.Outcome != PublishCompleted || actual.Err != nil {
						t.Fatal(actual)
					}
					if mode == "after" {
						return sentinel
					}
					if mode == "cancel-after" {
						cancel()
					}
				}
				return nil
			}
			result, err := root.Purge(ctx, p, obs)
			if err == nil || result.Outcome != PublishUnknown || result.Items[0].Outcome != PublishCompleted || result.Items[0].Bytes != 8 || result.Items[0].Err != nil {
				t.Fatal("lost real prefix", result, err)
			}
			completed := 1
			if mode == "new-child" {
				completed = 2
			}
			if result.Completed != completed || result.Unknown != 0 {
				t.Fatal("incorrect counts", result)
			}
			if mode == "before" || mode == "after" || mode == "cancel-after" {
				purgeTestUnstarted(t, result, 1)
			} else if mode != "new-child" {
				purgeTestUnstarted(t, result, 2)
			}
			if mode == "after" && (afterCalls != 1 || !errors.Is(err, sentinel)) {
				t.Fatal(afterCalls, err)
			}
			if mode == "move-root" {
				restoreTestBytes(t, filepath.Join(moved, "a"), []byte("original"))
			}
			if mode == "new-child" {
				restoreTestBytes(t, filepath.Join(dir, p.Path(), "new"), []byte("new"))
			}
		})
	}
}

func TestArchivePurgeHeldAncestorRelocationRejected(t *testing.T) {
	root, dir, p := purgeTestTree(t)
	opened, err := openPurgePath(t.Context(), root, p, p.nodes[0].item.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(filepath.Join(dir, p.Path()), moved); err != nil {
		t.Fatal(err)
	}
	if err := opened.verify(t.Context(), p.nodes[0], true); err == nil {
		t.Fatal("accepted relocated held parent")
	}
	restoreTestBytes(t, filepath.Join(moved, "b"), []byte("original"))
}

func TestArchivePurgeUnlinkSyncPostcheckCloseAndLateCancellation(t *testing.T) {
	for _, fault := range []string{"denied", "unknown-before", "unknown-after", "sync", "postcheck", "close", "recreated", "late-cancel"} {
		t.Run(fault, func(t *testing.T) {
			root, dir := archiveTestRoot(t)
			archiveTestWrite(t, filepath.Join(dir, purgeTestPath), []byte("original"))
			p := purgeTestPlan(t, root, purgeTestPath)
			unlink, sync, close, post := purgeUnlinkUnix, purgeSyncUnix, purgeClose, purgePostcheckUnix
			defer func() { purgeUnlinkUnix, purgeSyncUnix, purgeClose, purgePostcheckUnix = unlink, sync, close, post }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			deleted := false
			calls := 0
			purgeUnlinkUnix = func(fd int, name string, flags int) error {
				calls++
				if fault == "denied" {
					return unix.EACCES
				}
				if fault == "unknown-before" {
					return unix.EIO
				}
				err := unlink(fd, name, flags)
				deleted = err == nil
				if err == nil {
					if fault == "unknown-after" {
						return unix.EINTR
					}
					if fault == "late-cancel" {
						cancel()
					}
					if fault == "recreated" {
						archiveTestWrite(t, filepath.Join(dir, p.Path()), []byte("replacement"))
					}
				}
				return err
			}
			purgeSyncUnix = func(fd int) error {
				if fault == "sync" {
					return unix.EIO
				}
				return sync(fd)
			}
			purgeClose = func(f *os.File) error {
				err := close(f)
				if deleted && fault == "close" {
					return errors.Join(err, unix.EIO)
				}
				return err
			}
			purgePostcheckUnix = func(ctx context.Context, path *purgePath) error {
				if fault == "postcheck" {
					return unix.EIO
				}
				return post(ctx, path)
			}
			obs := purgeTestObserver()
			var actual PurgeItemResult
			obs.After = func(ctx context.Context, _ int, _ PurgeItem, item PurgeItemResult) error {
				actual = item
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				return nil
			}
			result, err := root.Purge(ctx, p, obs)
			if calls != 1 || !actual.Attempted || actual.Outcome != result.Items[0].Outcome || actual.Bytes != result.Items[0].Bytes {
				t.Fatal("inconsistent settlement", result, actual, err, calls)
			}
			switch fault {
			case "late-cancel":
				if err != nil || result.Outcome != PublishCompleted || result.Completed != 1 || actual.Bytes != 8 {
					t.Fatal("late cancel erased delete", result, err)
				}
			case "denied":
				if err == nil || result.Outcome != PublishUnchanged || actual.Outcome != PublishUnchanged {
					t.Fatal(result, err)
				}
			default:
				if !errors.Is(err, ErrOutcomeUnknown) || result.Unknown != 1 || actual.Outcome != PublishUnknown || actual.Bytes != 0 {
					t.Fatal("lost uncertainty", result, err)
				}
			}
			if fault == "close" && !result.CleanupIncomplete {
				t.Fatal("lost close failure", result)
			}
			if fault == "recreated" {
				restoreTestBytes(t, filepath.Join(dir, p.Path()), []byte("replacement"))
			} else if deleted {
				restoreTestAbsent(t, filepath.Join(dir, p.Path()))
			} else {
				restoreTestBytes(t, filepath.Join(dir, p.Path()), []byte("original"))
			}
		})
	}
}

func TestArchivePurgeMountAndResourceProofFailClosed(t *testing.T) {
	for _, fault := range []string{"mount-unavailable", "mount-different", "mount-after-intent", "watch-unavailable", "fd-exhausted", "prepare-close"} {
		t.Run(fault, func(t *testing.T) {
			root, dir, p := purgeTestTree(t)
			mount, watch, open, close := purgeMountUnix, purgeWatchUnix, purgeOpenUnix, purgeClose
			defer func() { purgeMountUnix, purgeWatchUnix, purgeOpenUnix, purgeClose = mount, watch, open, close }()
			active := fault != "mount-after-intent"
			purgeMountUnix = func(file *os.File) (string, error) {
				if active && fault == "mount-unavailable" {
					return "", ErrArchiveUnsupported
				}
				id, err := mount(file)
				info, e := purgeFDInfo(file)
				if e != nil {
					return "", e
				}
				if active && (fault == "mount-different" || fault == "mount-after-intent") && info.Identity == p.identity {
					id += "-different"
				}
				return id, err
			}
			purgeWatchUnix = func(file *os.File) (*DirectoryChangeGuard, error) {
				if fault == "watch-unavailable" {
					return nil, unix.EMFILE
				}
				return watch(file)
			}
			purgeOpenUnix = func(fd int, path string, flags int, mode uint32) (int, error) {
				if fault == "fd-exhausted" {
					return -1, unix.EMFILE
				}
				return open(fd, path, flags, mode)
			}
			purgeClose = func(f *os.File) error {
				err := close(f)
				if fault == "prepare-close" {
					return errors.Join(err, unix.EIO)
				}
				return err
			}
			obs := purgeTestObserver()
			before := 0
			obs.Before = func(context.Context, int, PurgeItem) error { before++; active = true; return nil }
			result, err := root.Purge(t.Context(), p, obs)
			if err == nil || result.Completed != 0 {
				t.Fatal("failed-open proof", result, err)
			}
			if fault == "mount-after-intent" {
				if before != 1 || !result.Items[0].Attempted {
					t.Fatal(result, before)
				}
			} else if before != 0 {
				t.Fatal("intent before resource preflight", before)
			}
			if fault == "prepare-close" && !result.CleanupIncomplete {
				t.Fatal("lost preflight close failure", result)
			}
			if fault == "mount-different" || fault == "mount-after-intent" {
				if !errors.Is(err, ErrCrossDevice) {
					t.Fatal(err)
				}
			}
			restoreTestBytes(t, filepath.Join(dir, p.Path(), "a"), []byte("original"))
			restoreTestBytes(t, filepath.Join(dir, p.Path(), "b"), []byte("original"))
		})
	}
}

func TestArchivePurgePreparationNativeChangesAndClosesAllHandles(t *testing.T) {
	root, dir, p := purgeTestTree(t)
	open, close, watch := purgeOpenUnix, purgeClose, purgeWatchUnix
	defer func() { purgeOpenUnix, purgeClose, purgeWatchUnix = open, close, watch }()
	live := make(map[int]bool)
	purgeOpenUnix = func(fd int, path string, flags int, mode uint32) (int, error) {
		next, err := open(fd, path, flags, mode)
		if err == nil {
			if live[next] {
				t.Fatal("descriptor reused without close", next)
			}
			live[next] = true
		}
		return next, err
	}
	purgeClose = func(f *os.File) error {
		fd := int(f.Fd())
		if !live[fd] {
			t.Fatal("unexpected/double close", fd)
		}
		delete(live, fd)
		return close(f)
	}
	info, err := root.Stat(t.Context(), p.Path())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := root.PreparePurge(t.Context(), p.Path(), info.Version, purgeTestLimits())
	if err != nil || len(live) != 0 {
		t.Fatal("preparation retained handles", err, live)
	}
	changed := false
	purgeWatchUnix = func(file *os.File) (*DirectoryChangeGuard, error) {
		guard, err := watch(file)
		if err != nil {
			return guard, err
		}
		entry, e := purgeFDInfo(file)
		if e != nil {
			t.Fatal(e)
		}
		if entry.Identity == p.identity && !changed {
			changed = true
			transient := filepath.Join(dir, p.Path(), "transient")
			archiveTestWrite(t, transient, []byte("temporary"))
			if err := os.Remove(transient); err != nil {
				t.Fatal(err)
			}
		}
		return guard, nil
	}
	if got, err := root.PreparePurge(t.Context(), p.Path(), info.Version, purgeTestLimits()); got != nil || !errors.Is(err, ErrChanged) || !changed || len(live) != 0 {
		t.Fatal("accepted mutation or leaked", got, err, changed, live)
	}
	purgeWatchUnix = watch
	// The failed transient mutation can change versions; obtain fresh authority.
	prepared = purgeTestPlan(t, root, p.Path())
	result, err := root.Purge(t.Context(), prepared, purgeTestObserver())
	if err != nil || result.Outcome != PublishCompleted || len(live) != 0 {
		t.Fatal("execution retained handles", result, err, live)
	}
}
