package securefile

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// RestorePlan selects an explicit archive entry and destination. It freezes
// entry metadata, the archive root and both parent identities, not contents or
// a directory subtree. No handles are retained across authorization. Plans are
// opaque, root-bound and single-use.
type RestorePlan struct {
	root                                *Root
	source, destination                 []string
	entry                               ArchiveEntry
	archiveID                           string
	sourceParentID, destinationParentID string
	mu                                  sync.Mutex
	consumed                            bool
}

func (p *RestorePlan) Source() string      { return strings.Join(p.source, "/") }
func (p *RestorePlan) Destination() string { return strings.Join(p.destination, "/") }
func (p *RestorePlan) Version() string     { return p.entry.Version }
func (p *RestorePlan) Kind() EntryType     { return p.entry.Kind }
func (p *RestorePlan) Size() int64         { return p.entry.Size }
func (p *RestorePlan) Identity() string    { return p.entry.Identity }

// PrepareRestore creates no entries and closes every opened handle before
// returning. The destination parent must already exist; no original path is
// inferred from the archive layout. expectedVersion is a current Stat version.
// Linux and macOS implement restoration; other platforms fail closed.
func (r *Root) PrepareRestore(ctx context.Context, source, destination, expectedVersion string) (*RestorePlan, error) {
	if r == nil || r.file == nil {
		return nil, errors.New("secure root is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validArchiveVersion(expectedVersion) || strings.ToLower(expectedVersion) != expectedVersion {
		return nil, ErrChanged
	}
	if len(source) > 4096 || len(destination) > 4096 {
		return nil, ErrTooLarge
	}
	if source == "." {
		return nil, ErrArchiveProtected
	}
	src, err := relativeComponents(source)
	if err != nil {
		return nil, err
	}
	if len(src) < 3 || src[0] != ArchiveDirectory {
		return nil, ErrArchiveProtected
	}
	dst, err := r.archiveSourceComponents(ctx, destination)
	if err != nil {
		return nil, err
	}
	if len(src) > 64 || len(dst) > 64 {
		return nil, ErrTooLarge
	}
	p := &RestorePlan{root: r, source: src, destination: dst}
	if err := prepareRestore(ctx, r, p, expectedVersion); err != nil {
		return nil, err
	}
	return p, nil
}

// Restore performs one same-filesystem no-replace rename. It never copies,
// merges, creates parents, deletes conflicts, cleans containers or rolls back.
// Final Unix checks are not a cross-process CAS; uncertainty is reported rather
// than retried. Cancellation after rename cannot undo the filesystem operation.
func (r *Root) Restore(ctx context.Context, p *RestorePlan) (MoveResult, error) {
	unchanged := MoveResult{Outcome: PublishUnchanged}
	if r == nil || r.file == nil || p == nil || p.root != r || p.archiveID == "" || p.sourceParentID == "" || p.destinationParentID == "" {
		return unchanged, ErrChanged
	}
	p.mu.Lock()
	used := p.consumed
	p.consumed = true
	p.mu.Unlock()
	if used {
		return unchanged, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return unchanged, err
	}
	return restoreWithinRoot(ctx, r, p)
}
