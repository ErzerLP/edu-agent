//go:build darwin

package securefile

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

const directoryGuardVnodeMask = unix.NOTE_WRITE | unix.NOTE_DELETE |
	unix.NOTE_RENAME | unix.NOTE_ATTRIB | unix.NOTE_EXTEND | unix.NOTE_LINK | unix.NOTE_REVOKE

type darwinDirectoryGuard struct {
	queue     int
	directory int
	stale     error
}

func newDirectoryChangeGuard(directory *os.File) (_ *DirectoryChangeGuard, err error) {
	// Darwin kqueue has no atomic CLOEXEC flag. Coordinate with os/exec's
	// fork lock until the descriptor is protected; the directory dup is atomic.
	syscall.ForkLock.RLock()
	queue, err := unix.Kqueue()
	if err == nil {
		_, err = unix.FcntlInt(uintptr(queue), unix.F_SETFD, unix.FD_CLOEXEC)
		if err != nil {
			_ = unix.Close(queue)
		}
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("secure directory notifications unavailable: %w", err)
	}
	backend := &darwinDirectoryGuard{queue: queue, directory: -1}
	defer func() {
		if err != nil {
			err = errors.Join(err, backend.close())
		}
	}()
	backend.directory, err = unix.FcntlInt(directory.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, fmt.Errorf("secure directory notification handle unavailable: %w", err)
	}
	change := unix.Kevent_t{
		Ident: uint64(backend.directory), Filter: unix.EVFILT_VNODE,
		Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR, Fflags: directoryGuardVnodeMask,
	}
	_, err = unix.Kevent(queue, []unix.Kevent_t{change}, nil, &unix.Timespec{})
	if err != nil {
		return nil, fmt.Errorf("secure directory notifications unavailable: %w", err)
	}
	return &DirectoryChangeGuard{backend: backend}, nil
}

func (g *darwinDirectoryGuard) check() error {
	if g.stale != nil {
		return g.stale
	}
	var events [1]unix.Kevent_t
	for attempt := 0; attempt < 64; attempt++ {
		n, err := unix.Kevent(g.queue, nil, events[:], &unix.Timespec{})
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			g.stale = errors.Join(ErrChanged, err)
		} else if n != 0 {
			// Every returned event, including EV_ERROR/revoke or an unexpected
			// filter, defeats proof that the directory stayed unchanged.
			g.stale = ErrChanged
		}
		return g.stale
	}
	g.stale = errors.Join(ErrChanged, errors.New("directory notification poll budget exhausted"))
	return g.stale
}

func (g *darwinDirectoryGuard) close() error {
	var err error
	if g.queue >= 0 {
		err = unix.Close(g.queue)
		g.queue = -1
	}
	if g.directory >= 0 {
		err = errors.Join(err, unix.Close(g.directory))
		g.directory = -1
	}
	return err
}
