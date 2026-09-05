package securefile

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
)

// MkdirPlan is an opaque, root-bound, single-use plan. Preparation opens only
// directories; it creates nothing. Ancestor identity is not a content hash.
type MkdirPlan struct {
	root       *Root
	components []string
	anchor     int
	identity   string
	mu         sync.Mutex
	consumed   bool
}

func (p *MkdirPlan) Path() string { return strings.Join(p.components, "/") }
func (p *MkdirPlan) Anchor() string {
	if p.anchor == 0 {
		return "."
	}
	return strings.Join(p.components[:p.anchor], "/")
}
func (p *MkdirPlan) Count() int { return len(p.components) - p.anchor }

type MkdirResult struct {
	Outcome PublishOutcome
	Created int // Known successful prefix of the frozen missing chain.
}

func (r *Root) PrepareMkdir(ctx context.Context, relative string, parents bool) (*MkdirPlan, error) {
	components, err := r.archiveSourceComponents(ctx, relative)
	if err != nil {
		return nil, err
	}
	if len(components) > 64 || len(relative) > 4096 {
		return nil, ErrTooLarge
	}
	if err = r.CheckArchiveWritePath(ctx, relative); err != nil {
		return nil, err
	}
	plan := &MkdirPlan{root: r, components: components}
	// Each opened prefix is protected and closed; only the deepest existing
	// parent's identity is retained. Commit reopens the original full spelling.
	for depth := 0; depth <= len(components); depth++ {
		identity, openErr := inspectMkdirDirectory(ctx, r, components[:depth])
		if errors.Is(openErr, ErrNotFound) {
			if !parents && depth != len(components) {
				return nil, ErrNotFound
			}
			return plan, nil
		}
		if openErr != nil {
			return nil, openErr
		}
		plan.anchor, plan.identity = depth, identity
	}
	return plan, nil // Existing directory, Count=0; no mutation or authorization.
}
func (r *Root) Mkdir(ctx context.Context, plan *MkdirPlan) (MkdirResult, error) {
	unchanged := MkdirResult{Outcome: PublishUnchanged}
	if r == nil || r.file == nil || plan == nil || plan.root != r || plan.identity == "" {
		return unchanged, ErrChanged
	}
	plan.mu.Lock()
	used := plan.consumed
	plan.consumed = true
	plan.mu.Unlock()
	if used {
		return unchanged, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return unchanged, err
	}
	if err := r.CheckArchiveWritePath(ctx, plan.Path()); err != nil {
		return unchanged, err
	}
	if plan.Count() == 0 {
		return unchanged, nil
	}
	return mkdirWithinRoot(ctx, r, plan)
}
func mkdirHandleIdentity(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", ErrNotDirectory
	}
	return snapshotFileIdentityForPlatform(file, info)
}
func closeMkdirHandles(files []*os.File, result *MkdirResult, err *error) {
	for i := len(files) - 1; i >= 0; i-- {
		*err = errors.Join(*err, files[i].Close())
	}
	if *err != nil && result.Created > 0 {
		result.Outcome = PublishUnknown
	}
	if result.Outcome == PublishUnknown {
		*err = errors.Join(ErrOutcomeUnknown, *err)
	}
}
