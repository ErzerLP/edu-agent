package securefile

import (
	"context"
	"os"
	"sync"
)

// DirectoryChangeGuard retains native directory change notifications without a
// background reader. Check is bounded and fails permanently on a change or lost
// observation. A guard is not a filesystem snapshot or a file-content hash (in
// particular, macOS directory notifications do not cover child content writes).
// It owns its resources independently of Root and DirectoryScan, must not be
// copied, and must be closed. Check and Close may be called concurrently.
type DirectoryChangeGuard struct {
	mu      sync.Mutex
	backend directoryChangeBackend
}

type directoryChangeBackend interface {
	check() error
	close() error
}

// Check returns ErrChanged after a notification or loss of reliable observation,
// and os.ErrClosed after Close. A successful check never clears a prior change.
func (g *DirectoryChangeGuard) Check() error {
	if g == nil {
		return os.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.backend == nil {
		return os.ErrClosed
	}
	return g.backend.check()
}

// Close releases this guard's notification resources. It is idempotent.
func (g *DirectoryChangeGuard) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.backend == nil {
		return nil
	}
	err := g.backend.close()
	g.backend = nil
	return err
}

// WatchDirectory watches a safely opened directory, including ".", without
// enumerating it or reading file content. User path links are never followed.
// The caller must not close Root concurrently with this call; after it returns,
// the guard does not use Root or retain the temporary scanner handles.
func (r *Root) WatchDirectory(ctx context.Context, relative string) (*DirectoryChangeGuard, error) {
	scan, err := r.OpenDirectoryScan(ctx, relative)
	if err != nil {
		return nil, err
	}
	// This unpublished scan needs no lock. Unlike public DetachChangeGuard,
	// watching an ignore-file ancestor deliberately does not enumerate it.
	return scan.detachChangeGuardLocked(ctx)
}
