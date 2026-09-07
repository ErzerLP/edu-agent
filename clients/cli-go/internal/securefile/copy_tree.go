package securefile

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"unicode"
)

// CopyTreeLimits bounds retained metadata and total ordinary-file bytes. These
// are logical accounting limits, not a promise about peak Go heap or OS handles.
type CopyTreeLimits struct {
	Bytes     int64
	Entries   int
	PlanBytes int64
}

type CopyTreeItem struct {
	Source, Destination string
	Kind                EntryType
	Version             string
	Size                int64
}

type copyTreeNode struct {
	item           CopyTreeItem
	entry          EntryInfo
	permission     os.FileMode
	sourceParentID string
}

// CopyTreePlan is an immutable, root-bound, single-use metadata plan. It retains
// no descriptors across authorization, and is neither a content snapshot nor CAS.
type CopyTreePlan struct {
	root                         *Root
	source, destination, version string
	destinationParentID          string
	limits                       CopyTreeLimits
	bytes, planBytes             int64
	nodes                        []copyTreeNode
	mu                           sync.Mutex
	consumed                     bool
}

func (p *CopyTreePlan) Source() string      { return p.source }
func (p *CopyTreePlan) Destination() string { return p.destination }
func (p *CopyTreePlan) Version() string     { return p.version }
func (p *CopyTreePlan) Bytes() int64        { return p.bytes }
func (p *CopyTreePlan) Items() []CopyTreeItem {
	items := make([]CopyTreeItem, len(p.nodes))
	for i := range p.nodes {
		items[i] = p.nodes[i].item
	}
	return items
}

type CopyTreeItemResult struct {
	Attempted   bool
	Outcome     PublishOutcome
	ContentHash string
	Bytes       int64 // Confirmed ordinary-file bytes; zero for directories/unknown.
	Err         error // Execution error, not an observer's settlement error.
}

type CopyTreeObserver struct {
	Before func(context.Context, int, CopyTreeItem) error
	After  func(context.Context, int, CopyTreeItem, CopyTreeItemResult) error
}

type CopyTreeResult struct {
	Outcome           PublishOutcome
	Items             []CopyTreeItemResult
	Completed         int
	Unknown           int
	CleanupIncomplete bool
}

var errCopyTreeCleanup = errors.New("secure tree copy cleanup is incomplete")

func validCopyTreeLimits(limits CopyTreeLimits) bool {
	return limits.Bytes > 0 && limits.Bytes < math.MaxInt64 &&
		limits.Entries > 0 && limits.Entries <= 1000000 &&
		limits.PlanBytes > 0 && limits.PlanBytes <= 1<<30
}

func (r *Root) PrepareCopyTree(ctx context.Context, source, destination, expectedVersion string, limits CopyTreeLimits) (*CopyTreePlan, error) {
	if !validCopyTreeLimits(limits) {
		return nil, ErrTooLarge
	}
	if !validArchiveVersion(expectedVersion) || strings.ToLower(expectedVersion) != expectedVersion {
		return nil, ErrChanged
	}
	for _, path := range []string{source, destination} {
		parts, err := r.archiveSourceComponents(ctx, path)
		if err != nil {
			return nil, err
		}
		if len(parts) > 64 || len(path) > 4096 {
			return nil, ErrTooLarge
		}
	}
	src, dst := copyTreeFold(source), copyTreeFold(destination)
	if dst == src || strings.HasPrefix(dst, src+"/") {
		return nil, ErrAlreadyExists
	}
	return prepareCopyTreeWithinRoot(ctx, r, source, destination, expectedVersion, limits)
}

// CopyTree requires durable-intent/settlement observers even for no-save callers
// (who can record in memory). Before succeeds before any item-side effect. After
// receives the actual result in a fresh bounded cancellation domain. Neither a
// callback failure nor cancellation rolls back a published prefix.
func (r *Root) CopyTree(ctx context.Context, plan *CopyTreePlan, observer CopyTreeObserver) (CopyTreeResult, error) {
	result := CopyTreeResult{Outcome: PublishUnchanged}
	if r == nil || r.file == nil || plan == nil || plan.root != r || len(plan.nodes) == 0 {
		return result, ErrChanged
	}
	plan.mu.Lock()
	used := plan.consumed
	plan.consumed = true
	plan.mu.Unlock()
	result.Items = make([]CopyTreeItemResult, len(plan.nodes))
	for i := range result.Items {
		result.Items[i].Outcome = PublishUnchanged
	}
	if used {
		return result, ErrChanged
	}
	if !validCopyTreeLimits(plan.limits) {
		return result, ErrTooLarge
	}
	if observer.Before == nil || observer.After == nil {
		return result, errors.New("secure tree copy requires both item observers")
	}
	return copyTreeWithinRoot(ctx, r, plan, observer, result)
}

// Canonicalize Unicode simple-fold classes, including K/Kelvin and sigma; plain
// ToLower misses some EqualFold aliases. Conservative on case-sensitive volumes.
func copyTreeFold(path string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < lowest {
				lowest = next
			}
		}
		return lowest
	}, path)
}

func copyTreeParent(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[:index]
	}
	return "."
}

func copyTreeParts(path string) []string {
	if path == "." {
		return nil
	}
	return strings.Split(path, "/")
}
