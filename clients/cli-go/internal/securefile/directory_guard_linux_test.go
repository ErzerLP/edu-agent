//go:build linux

package securefile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func directoryGuardTestEvent(wd int, mask uint32) []byte {
	event := make([]byte, unix.SizeofInotifyEvent)
	binary.NativeEndian.PutUint32(event[:4], uint32(wd))
	binary.NativeEndian.PutUint32(event[4:8], mask)
	return event
}

func directoryGuardTestPool() (*linuxDirectoryGuardPool, *linuxDirectoryGuard, *linuxDirectoryGuard) {
	watch := &linuxDirectoryWatch{wd: 7, active: true, members: make(map[*linuxDirectoryGuard]struct{})}
	p := &linuxDirectoryGuardPool{fd: -1, watches: map[int]*linuxDirectoryWatch{7: watch}, guards: make(map[*linuxDirectoryGuard]struct{})}
	first := &linuxDirectoryGuard{pool: p, watch: watch}
	second := &linuxDirectoryGuard{pool: p, watch: watch}
	for _, guard := range []*linuxDirectoryGuard{first, second} {
		p.guards[guard] = struct{}{}
		watch.members[guard] = struct{}{}
	}
	return p, first, second
}

func TestDirectoryGuardLinuxOverflowAndLostEventsFailClosed(t *testing.T) {
	for _, name := range []string{"overflow", "unknown-watch", "short-header", "short-name", "read-error", "ended", "busy", "interrupted"} {
		t.Run(name, func(t *testing.T) {
			pool, first, second := directoryGuardTestPool()
			calls := 0
			err := pool.pollLocked(func(buffer []byte) (int, error) {
				calls++
				event := directoryGuardTestEvent(7, unix.IN_CREATE)
				switch name {
				case "overflow":
					event = directoryGuardTestEvent(-1, unix.IN_Q_OVERFLOW)
				case "unknown-watch":
					event = directoryGuardTestEvent(8, unix.IN_CREATE)
				case "short-header":
					event = event[:3]
				case "short-name":
					binary.NativeEndian.PutUint32(event[12:16], 1024)
				case "read-error":
					return 0, unix.EIO
				case "ended":
					return 0, nil
				case "interrupted":
					return 0, unix.EINTR
				}
				return copy(buffer, event), nil
			})
			if !errors.Is(err, ErrChanged) || !first.stale || !second.stale {
				t.Fatalf("lost observation did not invalidate everyone: %v, %v, %v", err, first.stale, second.stale)
			}
			if calls > directoryGuardPollLimit || (name == "busy" || name == "interrupted") && calls != directoryGuardPollLimit {
				t.Fatalf("unbounded/wrong poll budget: %d", calls)
			}
			if err := pool.pollLocked(func([]byte) (int, error) {
				t.Fatal("failed pool tried to regain success")
				return 0, unix.EAGAIN
			}); !errors.Is(err, ErrChanged) {
				t.Fatalf("lost observation was not sticky: %v", err)
			}
		})
	}
}

func TestDirectoryGuardLinuxIgnoredWatchCannotAffectReusedDescriptor(t *testing.T) {
	pool, first, second := directoryGuardTestPool()
	oldWatch := first.watch
	if err := pool.consumeLocked(directoryGuardTestEvent(7, unix.IN_IGNORED)); err != nil {
		t.Fatal(err)
	}
	if !first.stale || !second.stale || oldWatch.active || pool.watches[7] != nil {
		t.Fatal("ignored watch remained valid")
	}
	freshWatch := &linuxDirectoryWatch{wd: 7, active: true, members: make(map[*linuxDirectoryGuard]struct{})}
	fresh := &linuxDirectoryGuard{pool: pool, watch: freshWatch}
	freshWatch.members[fresh] = struct{}{}
	pool.guards[fresh] = struct{}{}
	pool.watches[7] = freshWatch
	if fresh.stale || first.watch == fresh.watch {
		t.Fatal("reused descriptor inherited old guard identity")
	}
	if err := pool.consumeLocked(directoryGuardTestEvent(7, unix.IN_ATTRIB)); err != nil || !fresh.stale {
		t.Fatalf("fresh descriptor did not receive its event: %v", err)
	}
}

func TestDirectoryGuardLinuxSharesOneInstanceAndCleansUp(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 140; i++ {
		if err := os.Mkdir(filepath.Join(base, fmt.Sprint(i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	root := directoryScanTestRoot(t, base)
	guards := make([]*DirectoryChangeGuard, 0, 140)
	for i := 0; i < 140; i++ {
		guards = append(guards, directoryGuardTestWatch(t, root, fmt.Sprint(i)))
	}
	directoryGuards.mu.Lock()
	fd, count := directoryGuards.fd, len(directoryGuards.guards)
	flags, flagErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	status, statusErr := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	directoryGuards.mu.Unlock()
	if count != len(guards) || flagErr != nil || statusErr != nil || flags&unix.FD_CLOEXEC == 0 || status&unix.O_NONBLOCK == 0 {
		t.Fatalf("shared instance state: guards=%d, flags=%d/%v, status=%d/%v", count, flags, flagErr, status, statusErr)
	}
	for _, guard := range guards {
		backend := guard.backend.(*linuxDirectoryGuard)
		if backend.pool != &directoryGuards {
			t.Fatal("guard allocated another instance")
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	directoryGuards.mu.Lock()
	defer directoryGuards.mu.Unlock()
	if directoryGuards.fd != -1 || len(directoryGuards.guards) != 0 || len(directoryGuards.watches) != 0 {
		t.Fatal("last guard did not release shared resources")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("shared fd remained open: %v", err)
	}
}

func TestDirectoryGuardLinuxWatchRemovalDrainsBeforeNewObserver(t *testing.T) {
	path := t.TempDir()
	if err := os.Mkdir(filepath.Join(path, "peer"), 0700); err != nil {
		t.Fatal(err)
	}
	root := directoryScanTestRoot(t, path)
	peer := directoryGuardTestWatch(t, root, "peer") // Keep the instance alive.
	for round := 0; round < 8; round++ {
		old := directoryGuardTestWatch(t, root, ".")
		name := filepath.Join(path, "transient")
		if err := os.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		fresh := directoryGuardTestWatch(t, root, ".")
		if err := fresh.Close(); err != nil {
			t.Fatal(err)
		}
		if err := peer.Check(); err != nil {
			t.Fatalf("unrelated removal invalidated peer: %v", err)
		}
	}
}
