//go:build linux || darwin

package securefile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const restoreTestSource = ArchiveDirectory + "/old-container/nested/entry"

func restoreTestFixture(t *testing.T, directory bool) (string, *Root, *RestorePlan) {
	t.Helper()
	root, dir := archiveTestRoot(t)
	payload := restoreTestSource
	if directory {
		payload += "/child/file"
	}
	archiveTestWrite(t, filepath.Join(dir, filepath.FromSlash(payload)), []byte("original\x00\xff"))
	if err := os.Mkdir(filepath.Join(dir, "destination"), 0700); err != nil {
		t.Fatal(err)
	}
	stat, err := root.Stat(t.Context(), restoreTestSource)
	if err != nil {
		t.Fatal(err)
	}
	p, err := root.PrepareRestore(t.Context(), restoreTestSource, "destination/restored", stat.Version)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source() != restoreTestSource || p.Destination() != "destination/restored" || p.Version() != stat.Version || p.Kind() != stat.Kind || p.Identity() != stat.Identity || p.Size() != stat.Size {
		t.Fatal("plan did not freeze selected entry", p, stat)
	}
	return dir, root, p
}

func restoreTestAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", path, err)
	}
}

func restoreTestBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("wrong bytes at %s: length=%d want=%d err=%v", path, len(got), len(want), err)
	}
}

func TestArchiveRestoreActualArchiveBinaryWithoutSizeLimit(t *testing.T) {
	for _, size := range []int{0, 19, (32 << 20) + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root, dir := archiveTestRoot(t)
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i * 37)
			}
			archiveTestWrite(t, filepath.Join(dir, "original.bin"), data)
			original := archiveTestInspect(t, root, "original.bin")
			archived, err := root.Archive(t.Context(), "original.bin", restoreTestSource, original)
			if err != nil || archived.Outcome != PublishCompleted {
				t.Fatal(archived, err)
			}
			stat, err := root.Stat(t.Context(), restoreTestSource)
			if err != nil || stat.Identity != original.Identity {
				t.Fatal(stat, err)
			}
			p, err := root.PrepareRestore(t.Context(), restoreTestSource, "explicit-target.bin", stat.Version)
			if err != nil {
				t.Fatal(err)
			}
			restoreTestAbsent(t, filepath.Join(dir, "explicit-target.bin"))
			result, err := root.Restore(t.Context(), p)
			if err != nil || result.Outcome != PublishCompleted {
				t.Fatal(result, err)
			}
			restoreTestBytes(t, filepath.Join(dir, "explicit-target.bin"), data)
			restoreTestAbsent(t, filepath.Join(dir, restoreTestSource))
			restoreTestAbsent(t, filepath.Join(dir, "original.bin"))
			after, err := root.Stat(t.Context(), "explicit-target.bin")
			if err != nil || after.Identity != original.Identity || after.Kind != EntryFile || after.Size != int64(size) {
				t.Fatal("not the selected renamed entry", after, err)
			}
			// Empty containers and nested archive parents are deliberately kept.
			if _, err := os.Stat(filepath.Dir(filepath.Join(dir, restoreTestSource))); err != nil {
				t.Fatal("cleaned archive parents", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 2 {
				t.Fatal("unexpected temporary entries", entries, err)
			}
			if result, err := root.Restore(t.Context(), p); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
				t.Fatal("reused plan", result, err)
			}
		})
	}
}

func TestArchiveRestoreDirectoriesAndLegacyNoManifest(t *testing.T) {
	for _, realArchive := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("archive=%t/empty=%t", realArchive, empty), func(t *testing.T) {
				root, dir := archiveTestRoot(t)
				source := restoreTestSource
				if realArchive {
					source = "original-directory"
				}
				if err := os.MkdirAll(filepath.Join(dir, source), 0700); err != nil {
					t.Fatal(err)
				}
				outside := t.TempDir()
				archiveTestWrite(t, filepath.Join(outside, "sentinel"), []byte("untouched"))
				if !empty {
					archiveTestWrite(t, filepath.Join(dir, source, "child/file"), []byte("initial"))
					for _, link := range []struct{ name, target string }{{"external", outside}, {"dangling", "missing"}, {"relative", "child/file"}} {
						if err := os.Symlink(link.target, filepath.Join(dir, source, link.name)); err != nil {
							t.Fatal(err)
						}
					}
				}
				if realArchive {
					entry := archiveTestInspect(t, root, source)
					result, err := root.Archive(t.Context(), source, restoreTestSource, entry)
					if err != nil || result.Outcome != PublishCompleted {
						t.Fatal(result, err)
					}
				}
				stat, err := root.Stat(t.Context(), restoreTestSource)
				if err != nil {
					t.Fatal(err)
				}
				p, err := root.PrepareRestore(t.Context(), restoreTestSource, "explicit-directory", stat.Version)
				if err != nil {
					t.Fatal(err)
				}
				// Entry metadata is not a descendant content snapshot.
				if !empty {
					archiveTestWrite(t, filepath.Join(dir, restoreTestSource, "child/file"), []byte("latest child data"))
				}
				result, err := root.Restore(t.Context(), p)
				if err != nil || result.Outcome != PublishCompleted {
					t.Fatal(result, err)
				}
				restoreTestAbsent(t, filepath.Join(dir, restoreTestSource))
				after, err := root.Stat(t.Context(), "explicit-directory")
				if err != nil || after.Identity != stat.Identity || after.Kind != EntryDirectory {
					t.Fatal(after, err)
				}
				if !empty {
					restoreTestBytes(t, filepath.Join(dir, "explicit-directory/child/file"), []byte("latest child data"))
					for _, link := range []struct{ name, target string }{{"external", outside}, {"dangling", "missing"}, {"relative", "child/file"}} {
						got, err := os.Readlink(filepath.Join(dir, "explicit-directory", link.name))
						if err != nil || got != link.target {
							t.Fatal("link was followed or changed", got, err)
						}
					}
				}
				restoreTestBytes(t, filepath.Join(outside, "sentinel"), []byte("untouched"))
			})
		}
	}
}

func TestArchiveRestoreRejectsPathsVersionsAndMissingParents(t *testing.T) {
	dir, root, p := restoreTestFixture(t, false)
	for _, tc := range []struct {
		name, src, dst, version string
		want                    error
	}{
		{"workspace-root", ".", p.Destination(), p.Version(), ErrArchiveProtected},
		{"archive-root", ArchiveDirectory, p.Destination(), p.Version(), ErrArchiveProtected},
		{"container", ArchiveDirectory + "/old-container", p.Destination(), p.Version(), ErrArchiveProtected},
		{"ordinary-source", "destination/file", p.Destination(), p.Version(), ErrArchiveProtected},
		{"source-case", strings.ToUpper(ArchiveDirectory) + "/old-container/nested/entry", p.Destination(), p.Version(), ErrArchiveProtected},
		{"target-root", p.Source(), ".", p.Version(), ErrArchiveProtected},
		{"target-archive", p.Source(), ArchiveDirectory + "/new/file", p.Version(), ErrArchiveProtected},
		{"target-archive-case", p.Source(), strings.ToUpper(ArchiveDirectory) + "/new/file", p.Version(), ErrArchiveProtected},
		{"missing-parent", p.Source(), "missing/parent/file", p.Version(), ErrNotFound},
		{"missing-source", p.Source() + "-absent", p.Destination(), p.Version(), ErrNotFound},
		{"source-escape", ArchiveDirectory + "/old-container/../../file", p.Destination(), p.Version(), nil},
		{"target-escape", p.Source(), "../outside", p.Version(), nil},
		{"absolute-source", "/" + p.Source(), p.Destination(), p.Version(), nil},
		{"absolute-target", p.Source(), "/destination", p.Version(), nil},
		{"empty-source", "", p.Destination(), p.Version(), nil},
		{"empty-target", p.Source(), "", p.Version(), nil},
		{"duplicate-slash", ArchiveDirectory + "//old-container/entry", p.Destination(), p.Version(), nil},
		{"dot-component", p.Source(), "destination/./file", p.Version(), nil},
		{"backslash", p.Source(), "destination\\file", p.Version(), nil},
		{"nul", p.Source(), "destination/\x00", p.Version(), nil},
		{"missing-version", p.Source(), p.Destination(), "", ErrChanged},
		{"hash-not-version", p.Source(), p.Destination(), "sha256:" + strings.Repeat("a", 64), ErrChanged},
		{"uppercase-version", p.Source(), p.Destination(), "entry-v1:" + strings.Repeat("A", 64), ErrChanged},
		{"wrong-version", p.Source(), p.Destination(), archiveMetadataVersion("wrong"), ErrChanged},
		{"source-bytes", ArchiveDirectory + "/c/" + strings.Repeat("x", 4096), p.Destination(), p.Version(), ErrTooLarge},
		{"target-bytes", p.Source(), strings.Repeat("x", 4097), p.Version(), ErrTooLarge},
		{"source-depth", ArchiveDirectory + "/" + strings.Repeat("x/", 63) + "file", p.Destination(), p.Version(), ErrTooLarge},
		{"target-depth", p.Source(), strings.Repeat("x/", 64) + "file", p.Version(), ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := root.PrepareRestore(t.Context(), tc.src, tc.dst, tc.version)
			if err == nil || plan != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatal(plan, err, "want", tc.want)
			}
			restoreTestBytes(t, filepath.Join(dir, p.Source()), []byte("original\x00\xff"))
			restoreTestAbsent(t, filepath.Join(dir, p.Destination()))
			restoreTestAbsent(t, filepath.Join(dir, "missing"))
		})
	}
	fresh, freshDir := archiveTestRoot(t)
	if plan, err := fresh.PrepareRestore(t.Context(), restoreTestSource, "target", p.Version()); plan != nil || !errors.Is(err, ErrNotFound) {
		t.Fatal("missing archive", plan, err)
	}
	entries, err := os.ReadDir(freshDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("preparation created entries", entries, err)
	}
}

func TestArchiveRestoreCancellationRootBindingAndSingleUse(t *testing.T) {
	dir, root, p := restoreTestFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if plan, err := root.PrepareRestore(ctx, p.Source(), p.Destination(), p.Version()); !errors.Is(err, context.Canceled) || plan != nil {
		t.Fatal("canceled preparation", plan, err)
	}
	if result, err := root.Restore(ctx, p); !errors.Is(err, context.Canceled) || result.Outcome != PublishUnchanged {
		t.Fatal(result, err)
	}
	if result, err := root.Restore(t.Context(), p); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
		t.Fatal("canceled plan was reused", result, err)
	}
	restoreTestBytes(t, filepath.Join(dir, p.Source()), []byte("original\x00\xff"))
	restoreTestAbsent(t, filepath.Join(dir, p.Destination()))
	other, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	q, err := root.PrepareRestore(t.Context(), p.Source(), p.Destination(), p.Version())
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []*RestorePlan{nil, {}, {root: root}} {
		if result, err := root.Restore(t.Context(), invalid); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
			t.Fatal(result, err)
		}
	}
	if result, err := other.Restore(t.Context(), q); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
		t.Fatal("plan not root-bound", result, err)
	}
	if result, err := root.Restore(t.Context(), q); err != nil || result.Outcome != PublishCompleted {
		t.Fatal("wrong root consumed plan", result, err)
	}
}

func TestArchiveRestoreOrdinaryMoveAndArchiveRemainProtected(t *testing.T) {
	_, root, p := restoreTestFixture(t, false)
	for _, operation := range []string{"move", "archive", "write"} {
		t.Run(operation, func(t *testing.T) {
			var err error
			switch operation {
			case "move":
				_, err = root.PrepareMove(t.Context(), p.Source(), p.Destination(), p.Version())
			case "archive":
				_, err = root.Archive(t.Context(), p.Source(), ArchiveDirectory+"/other/file", p.entry)
			case "write":
				err = root.CheckArchiveWritePath(t.Context(), p.Source())
			}
			if !errors.Is(err, ErrArchiveProtected) {
				t.Fatal("ordinary operation gained archive privilege", err)
			}
		})
	}
}
