//go:build linux

package securefile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
)

const directoryGuardPollLimit = 64
const directoryGuardReadSize = 8192

const directoryGuardInotifyMask = unix.IN_CREATE | unix.IN_DELETE |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_MOVE_SELF | unix.IN_DELETE_SELF |
	unix.IN_ATTRIB | unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_UNMOUNT

// One lazily allocated instance avoids the per-user inotify instance limit for
// completed scans. No goroutine consumes events: every pool operation drains
// under mu and delivers each event to every guard sharing its watch descriptor.
var directoryGuards = linuxDirectoryGuardPool{fd: -1}

type linuxDirectoryGuardPool struct {
	mu      sync.Mutex
	fd      int
	watches map[int]*linuxDirectoryWatch
	guards  map[*linuxDirectoryGuard]struct{}
	failed  error
}

type linuxDirectoryWatch struct {
	wd      int
	active  bool
	members map[*linuxDirectoryGuard]struct{}
}

type linuxDirectoryGuard struct {
	pool   *linuxDirectoryGuardPool
	watch  *linuxDirectoryWatch
	stale  bool
	closed bool
}

func newDirectoryChangeGuard(directory *os.File) (*DirectoryChangeGuard, error) {
	p := &directoryGuards
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
		if err != nil {
			return nil, fmt.Errorf("secure directory notifications unavailable: %w", err)
		}
		p.fd = fd
		p.watches = make(map[int]*linuxDirectoryWatch)
		p.guards = make(map[*linuxDirectoryGuard]struct{})
	}
	// An unsuccessful first installation must not leave an idle instance open.
	defer func() {
		if len(p.guards) == 0 {
			_ = p.closeIdleLocked()
		}
	}()
	if err := p.drainLocked(); err != nil {
		return nil, err
	}
	// inotify has only a pathname API. This trusted procfs descriptor bridge
	// resolves the already held, no-follow directory, not a user pathname.
	// IN_DONT_FOLLOW would watch the procfs link rather than the directory.
	wd, err := unix.InotifyAddWatch(p.fd, fmt.Sprintf("/proc/self/fd/%d", directory.Fd()), directoryGuardInotifyMask|unix.IN_ONLYDIR)
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, fmt.Errorf("secure directory notifications unavailable: %w", err)
	}
	watch := p.watches[wd]
	if watch == nil {
		watch = &linuxDirectoryWatch{wd: wd, active: true, members: make(map[*linuxDirectoryGuard]struct{})}
		p.watches[wd] = watch
	}
	backend := &linuxDirectoryGuard{pool: p, watch: watch}
	watch.members[backend] = struct{}{}
	p.guards[backend] = struct{}{}
	// Drain after installation too: queued changes and ignored notifications
	// must be attributed before any caller can use this guard.
	if err := p.drainLocked(); err != nil || backend.stale {
		closeErr := backend.closeLocked()
		return nil, errors.Join(ErrChanged, err, closeErr)
	}
	return &DirectoryChangeGuard{backend: backend}, nil
}

func (g *linuxDirectoryGuard) check() error {
	p := g.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if g.closed {
		return os.ErrClosed
	}
	if g.stale {
		return ErrChanged
	}
	if err := p.drainLocked(); err != nil {
		return err
	}
	if g.stale {
		return ErrChanged
	}
	return nil
}

func (g *linuxDirectoryGuard) close() error {
	g.pool.mu.Lock()
	defer g.pool.mu.Unlock()
	return g.closeLocked()
}

func (g *linuxDirectoryGuard) closeLocked() error {
	if g.closed {
		return nil
	}
	p := g.pool
	// Consume old events before removing a watch, and IN_IGNORED after removal,
	// while its old descriptor still belongs to this watch. Pool failure blocks
	// additions until all guards close, so an undrained descriptor cannot be
	// reused and incorrectly reported as a fresh, unchanged observation.
	_ = p.drainLocked()
	g.closed = true
	delete(p.guards, g)
	delete(g.watch.members, g)
	var err error
	if len(g.watch.members) == 0 && g.watch.active {
		_, err = unix.InotifyRmWatch(p.fd, uint32(g.watch.wd))
		if err != nil {
			p.failLocked(err)
			// The kernel may have removed the watch concurrently. Observation
			// is invalidated above, but there is no remaining watch to release.
			if errors.Is(err, unix.EINVAL) {
				err = nil
			}
		}
		_ = p.drainLocked()
		g.watch.active = false
		if p.watches[g.watch.wd] == g.watch {
			delete(p.watches, g.watch.wd)
		}
	}
	if len(p.guards) == 0 {
		err = errors.Join(err, p.closeIdleLocked())
	}
	return err
}

func (p *linuxDirectoryGuardPool) closeIdleLocked() error {
	if p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	p.watches = nil
	p.guards = nil
	p.failed = nil
	return err
}

func (p *linuxDirectoryGuardPool) failLocked(cause error) error {
	if p.failed == nil {
		p.failed = errors.Join(ErrChanged, cause)
	}
	for guard := range p.guards {
		guard.stale = true
	}
	return p.failed
}

func (p *linuxDirectoryGuardPool) drainLocked() error {
	return p.pollLocked(func(buffer []byte) (int, error) { return unix.Read(p.fd, buffer) })
}

// pollLocked's reader parameter permits bounded tests of overflow and an
// endlessly busy source without changing process-global syscall hooks.
func (p *linuxDirectoryGuardPool) pollLocked(read func([]byte) (int, error)) error {
	if p.failed != nil {
		return p.failed
	}
	var buffer [directoryGuardReadSize]byte
	for attempt := 0; attempt < directoryGuardPollLimit; attempt++ {
		n, err := read(buffer[:])
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return p.failLocked(err)
		}
		if n <= 0 || n > len(buffer) {
			return p.failLocked(errors.New("directory notification stream ended"))
		}
		if err := p.consumeLocked(buffer[:n]); err != nil {
			return err
		}
	}
	return p.failLocked(errors.New("directory notification poll budget exhausted"))
}

func (p *linuxDirectoryGuardPool) consumeLocked(events []byte) error {
	for len(events) != 0 {
		if len(events) < unix.SizeofInotifyEvent {
			return p.failLocked(errors.New("incomplete directory notification"))
		}
		wd := int(int32(binary.NativeEndian.Uint32(events[:4])))
		mask := binary.NativeEndian.Uint32(events[4:8])
		length := uint64(binary.NativeEndian.Uint32(events[12:16]))
		if length > uint64(len(events)-unix.SizeofInotifyEvent) {
			return p.failLocked(errors.New("incomplete directory notification name"))
		}
		if mask&unix.IN_Q_OVERFLOW != 0 {
			return p.failLocked(errors.New("directory notification queue overflow"))
		}
		watch := p.watches[wd]
		if watch == nil {
			return p.failLocked(errors.New("directory notification watch identity lost"))
		}
		for guard := range watch.members {
			guard.stale = true
		}
		if mask&unix.IN_IGNORED != 0 {
			watch.active = false
			delete(p.watches, wd)
		}
		events = events[unix.SizeofInotifyEvent+int(length):]
	}
	return nil
}
