package securefile

import (
	"context"
	"errors"
	"os"
	"sync"
)

// DirectoryScan retains one root-confined directory's OS enumeration position.
// It must not be copied. Entry is immutable; Next, DetachChangeGuard and Close
// serialize access to owned resources. Close must be called even after EOF
// unless DetachChangeGuard successfully transfers observation to the caller.
//
// Metadata and native notifications detect changes, but do not provide a content
// snapshot, external lock or CAS.
// The caller must not close Root concurrently with OpenDirectoryScan. Once open,
// a scan owns an independent root anchor and does not access the original Root.
type DirectoryScan struct {
	mu         sync.Mutex
	root       *Root
	directory  *os.File
	components []string
	entry      EntryInfo
	guard      *DirectoryChangeGuard
	eof        bool
}

// OpenDirectoryScan opens a non-following scan of relative (including ".").
// Linux and macOS are supported; other platforms return an unsupported error.
func (r *Root) OpenDirectoryScan(ctx context.Context, relative string) (*DirectoryScan, error) {
	components, err := r.statComponents(ctx, relative)
	if err != nil {
		return nil, err
	}
	return openDirectoryScanWithinRoot(ctx, r, components)
}

// Entry returns the original frozen directory metadata, including after Close.
func (s *DirectoryScan) Entry() EntryInfo {
	if s == nil {
		return EntryInfo{}
	}
	return s.entry
}

// Next returns at most limit entries (1..65536) in OS enumeration order. The
// boolean is true only on observed EOF; even a short page may require another
// call to observe EOF. It never consumes and discards a lookahead name.
//
// Every name is classified without following links, so skipped is currently
// zero. A vanished name or changed directory invalidates the entire page.
// Before and after reading, the retained directory must remain inside the root
// and the original relative path must still match the frozen metadata.
//
// Invalid limits and an already-canceled context do not consume or close the
// scan. Any subsequent error discards the entire page and closes the scan,
// including cancellation after enumeration starts. A closed scan returns
// os.ErrClosed. No error returns entries, a skipped count or successful EOF.
func (s *DirectoryScan) Next(ctx context.Context, limit int) ([]DirEntry, int, bool, error) {
	if s == nil {
		return nil, 0, false, os.ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.directory == nil || s.root == nil {
		return nil, 0, false, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	if limit < 1 || limit > 65536 {
		return nil, 0, false, errors.New("secure directory scan entry limit is invalid")
	}
	entries, skipped, complete, err := s.next(ctx, limit)
	if err != nil {
		return nil, 0, false, errors.Join(err, s.closeLocked())
	}
	s.eof = complete
	return entries, skipped, complete, nil
}

// DetachChangeGuard transfers directory change observation only after Next has
// observed real EOF. It verifies once more, then closes the scanner handles.
// Next and repeated detachment subsequently return os.ErrClosed; scan.Close
// does not close the transferred guard. A premature attempt leaves the scan open.
func (s *DirectoryScan) DetachChangeGuard() (*DirectoryChangeGuard, error) {
	if s == nil {
		return nil, os.ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.directory == nil || s.root == nil || s.guard == nil {
		return nil, os.ErrClosed
	}
	if !s.eof {
		return nil, errors.New("secure directory scan has not reached EOF")
	}
	return s.detachChangeGuardLocked(context.Background())
}

func (s *DirectoryScan) detachChangeGuardLocked(ctx context.Context) (*DirectoryChangeGuard, error) {
	if err := s.verify(ctx); err != nil {
		return nil, errors.Join(err, s.closeLocked())
	}
	guard := s.guard
	s.guard = nil
	if err := s.closeLocked(); err != nil {
		return nil, errors.Join(err, guard.Close())
	}
	return guard, nil
}

// Close releases owned handles and any guard not transferred to the caller.
// It is idempotent and safe alongside Next and DetachChangeGuard.
func (s *DirectoryScan) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *DirectoryScan) closeLocked() error {
	var err error
	if s.directory != nil {
		err = s.directory.Close()
		s.directory = nil
	}
	if s.root != nil {
		err = errors.Join(err, s.root.Close())
		s.root = nil
	}
	if s.guard != nil {
		err = errors.Join(err, s.guard.Close())
		s.guard = nil
	}
	return err
}
