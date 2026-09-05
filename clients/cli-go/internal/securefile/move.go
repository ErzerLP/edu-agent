package securefile

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
)

// ErrMovePath rejects ambiguous aliases and directory self-descendants.
var ErrMovePath = errors.New("unsafe move path: alias, case-only rename or self-descendant")

// MovePlan freezes only source entry metadata and both parent identities, not
// content or a directory subtree. It is root-bound, single-use and side-effect free.
type MovePlan struct {
	root                                *Root
	source, destination                 []string
	entry                               ArchiveEntry
	sourceParentID, destinationParentID string
	mu                                  sync.Mutex
	consumed                            bool
}

func (p *MovePlan) Source() string      { return strings.Join(p.source, "/") }
func (p *MovePlan) Destination() string { return strings.Join(p.destination, "/") }
func (p *MovePlan) Version() string     { return p.entry.Version }
func (p *MovePlan) Kind() EntryType     { return p.entry.Kind }
func (p *MovePlan) Size() int64         { return p.entry.Size }
func (p *MovePlan) Identity() string    { return p.entry.Identity }
func (p *MovePlan) Unchanged() bool     { return p.Source() == p.Destination() }

type MoveResult struct{ Outcome PublishOutcome }
type moveState struct {
	root                                    *Root
	sourceParent, destinationParent, source *os.File
	handles                                 []*os.File
	entry                                   ArchiveEntry
}

var moveClose = func(f *os.File) error { return f.Close() }

func (s *moveState) close() error {
	var err error
	for i := len(s.handles) - 1; i >= 0; i-- {
		err = errors.Join(err, moveClose(s.handles[i]))
	}
	return err
}
func (r *Root) PrepareMove(ctx context.Context, source, destination, expectedVersion string) (plan *MovePlan, err error) {
	if !validArchiveVersion(expectedVersion) || strings.ToLower(expectedVersion) != expectedVersion {
		return nil, ErrChanged
	}
	src, err := r.archiveSourceComponents(ctx, source)
	if err != nil {
		return nil, err
	}
	dst, err := r.archiveSourceComponents(ctx, destination)
	if err != nil {
		return nil, err
	}
	if len(src) > 64 || len(dst) > 64 || len(source) > 4096 || len(destination) > 4096 {
		return nil, ErrTooLarge
	}
	// Conservatively reject case-only renames on every platform. Never stage a
	// temporary two-step rename to try to make an ambiguous spelling work.
	if source != destination && strings.EqualFold(source, destination) {
		return nil, ErrMovePath
	}
	for _, path := range []string{source, destination} {
		if err = r.CheckArchiveWritePath(ctx, path); err != nil {
			return nil, err
		}
	}
	p := &MovePlan{root: r, source: src, destination: dst}
	s, err := openMoveState(ctx, r, p, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, s.close())
		if err != nil {
			plan = nil
		}
	}()
	if s.entry.Version != expectedVersion {
		return nil, ErrChanged
	}
	p.entry = s.entry
	p.sourceParentID, err = mkdirHandleIdentity(s.sourceParent)
	if err != nil {
		return nil, err
	}
	p.destinationParentID, err = mkdirHandleIdentity(s.destinationParent)
	if err != nil {
		return nil, err
	}
	if err = s.verify(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Move uses one same-filesystem no-replace rename; no temp, mkdir, body I/O,
// copy/delete fallback or rollback. Cancellation after rename is not rollback.
func (r *Root) Move(ctx context.Context, p *MovePlan) (result MoveResult, err error) {
	result.Outcome = PublishUnchanged
	if r == nil || r.file == nil || p == nil || p.root != r || p.sourceParentID == "" || p.destinationParentID == "" {
		return result, ErrChanged
	}
	p.mu.Lock()
	used := p.consumed
	p.consumed = true
	p.mu.Unlock()
	if used {
		return result, ErrChanged
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	for _, path := range []string{p.Source(), p.Destination()} {
		if err = r.CheckArchiveWritePath(ctx, path); err != nil {
			return result, err
		}
	}
	s, err := openMoveState(ctx, r, p, true)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := s.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
			if result.Outcome != PublishUnchanged {
				result.Outcome = PublishUnknown
			}
		}
		if result.Outcome == PublishUnknown {
			err = errors.Join(ErrOutcomeUnknown, err)
		}
	}()
	if err = s.verify(ctx, p); err != nil {
		return result, err
	}
	if p.Unchanged() {
		return result, nil
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Outcome, err = s.rename(p)
	if err != nil {
		return result, err
	}
	if err = s.verifyPublication(context.WithoutCancel(ctx), p); err != nil {
		result.Outcome = PublishUnknown
		return result, err
	}
	if err = s.syncParents(); err != nil {
		result.Outcome = PublishUnknown
		return result, err
	}
	return result, nil
}
