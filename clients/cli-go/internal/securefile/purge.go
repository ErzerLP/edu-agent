package securefile

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// PurgeLimits bounds entry count and retained logical metadata, not file contents
// or peak heap/descriptor usage. Large files are never read by purge.
type PurgeLimits struct {
	Entries   int
	PlanBytes int64
}

type PurgeItem struct {
	Path    string
	Kind    EntryType
	Version string
	Size    int64
}

type purgeNode struct {
	item     PurgeItem
	entry    EntryInfo
	parentID string
}

type purgeUse struct {
	mu       sync.Mutex
	consumed bool
}

// PurgePlan is root-bound, immutable and single-use, including when its value
// is copied. It retains no handles or watches across authorization. Versions
// describe metadata, not contents, an atomic subtree snapshot, or a CAS.
type PurgePlan struct {
	root                    *Root
	path, version, identity string
	kind                    EntryType
	limits                  PurgeLimits
	bytes, planBytes        int64
	nodes                   []purgeNode
	directories             map[string]string // Frozen named directory identities, including ancestors.
	mount                   string
	use                     *purgeUse
}

func (p *PurgePlan) Path() string    { return p.path }
func (p *PurgePlan) Version() string { return p.version }
func (p *PurgePlan) Kind() EntryType { return p.kind }

// Identity is for internal workspace serialization, not model-facing plans.
func (p *PurgePlan) Identity() string { return p.identity }

// Bytes is the sum of ordinary-file logical sizes, never physical space freed.
func (p *PurgePlan) Bytes() int64 { return p.bytes }
func (p *PurgePlan) Items() []PurgeItem {
	items := make([]PurgeItem, len(p.nodes))
	for i := range p.nodes {
		items[i] = p.nodes[i].item
	}
	return items
}

type PurgeItemResult struct {
	Attempted bool
	Outcome   PublishOutcome
	Bytes     int64 // Confirmed deleted ordinary-file logical bytes only.
	Err       error // Actual execution error; never replaced by an After error.
}

type PurgeObserver struct {
	Before func(context.Context, int, PurgeItem) error
	After  func(context.Context, int, PurgeItem, PurgeItemResult) error
}

type PurgeResult struct {
	Outcome           PublishOutcome
	Items             []PurgeItemResult
	Completed         int
	Unknown           int
	CleanupIncomplete bool
}

var errPurgeCleanup = errors.New("secure archive purge resource cleanup is incomplete")

func validPurgeLimits(limits PurgeLimits) bool {
	return limits.Entries > 0 && limits.Entries <= 1000000 && limits.PlanBytes > 0 && limits.PlanBytes <= 1<<30
}

func purgeComponents(path string) ([]string, error) {
	if len(path) > 4096 {
		return nil, ErrTooLarge
	}
	parts, err := relativeComponents(path)
	if err != nil {
		return nil, err
	}
	if len(parts) < 2 || parts[0] != ArchiveDirectory {
		return nil, ErrArchiveProtected
	}
	if len(parts) > 64 {
		return nil, ErrTooLarge
	}
	return parts, nil
}

// PreparePurge completely enumerates an explicit canonical archive entry,
// including links as leaves. Preparation is read-only and closes all resources
// before returning. Unsupported platforms or mount/notification proof fail closed.
func (r *Root) PreparePurge(ctx context.Context, path, expectedVersion string, limits PurgeLimits) (*PurgePlan, error) {
	if r == nil || r.file == nil {
		return nil, errors.New("secure root is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validPurgeLimits(limits) {
		return nil, ErrTooLarge
	}
	if !validArchiveVersion(expectedVersion) || strings.ToLower(expectedVersion) != expectedVersion {
		return nil, ErrChanged
	}
	if _, err := purgeComponents(path); err != nil {
		return nil, err
	}
	return preparePurgeWithinRoot(ctx, r, path, expectedVersion, limits)
}

// Purge requires both intent and settlement observers. It never widens the
// frozen scope, retries, rolls back, or removes an unselected parent. Unix's
// final named-entry checks and unlink are not a cross-process atomic CAS.
// Root must not be closed concurrently (the same contract as other Root APIs).
func (r *Root) Purge(ctx context.Context, p *PurgePlan, observer PurgeObserver) (PurgeResult, error) {
	result := PurgeResult{Outcome: PublishUnchanged}
	if r == nil || r.file == nil || p == nil || p.root != r || p.use == nil || len(p.nodes) == 0 {
		return result, ErrChanged
	}
	result.Items = make([]PurgeItemResult, len(p.nodes))
	for i := range result.Items {
		result.Items[i].Outcome = PublishUnchanged
	}
	p.use.mu.Lock()
	used := p.use.consumed
	p.use.consumed = true
	p.use.mu.Unlock()
	if used {
		return result, ErrChanged
	}
	if !validPurgeLimits(p.limits) {
		return result, ErrTooLarge
	}
	if observer.Before == nil || observer.After == nil {
		return result, errors.New("secure archive purge requires both item observers")
	}
	return purgeWithinRoot(ctx, r, p, observer, result)
}
