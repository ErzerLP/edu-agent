package workspace

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

// ready=false is a resumable per-call allowance boundary. allowed=false is an
// unavailable rule layer: never silently traverse that subtree without it.
func (q *workspaceQuery) loadQueryIgnore(ctx context.Context, directory string, parent *ignoreLayer, step *queryStep) (layer *ignoreLayer, ready, allowed bool, err error) {
	if q.ignore == nil {
		return parent, true, true, nil
	}
	if err := q.ensureDirectoryGuard(ctx, directory); err != nil {
		return nil, true, false, err
	}
	name := joinRelative(directory, ".gitignore")
	info, err := q.observe(ctx, name)
	if errors.Is(err, securefile.ErrNotFound) {
		return parent, true, true, nil
	}
	if err != nil {
		if errors.Is(err, securefile.ErrChanged) || errors.Is(err, securefile.ErrOutsideRoot) || ctx.Err() != nil {
			return nil, true, false, err
		}
		q.note("gitignore_unavailable")
		return nil, true, false, nil
	}
	if info.Kind == securefile.EntryFile && info.Size >= 0 && info.Size <= min(int64(maxIgnoreFileBytes), q.w.limits.FileBytes) {
		if step.files >= q.w.limits.SearchFiles || q.ignore.usedFiles >= q.w.limits.SearchFiles {
			step.pause = "file_limit"
			return nil, false, false, nil
		}
		if info.Size+1 > q.w.limits.SearchBytes {
			q.note("scan_bytes")
			return nil, true, false, nil
		}
		if info.Size+1 > q.w.limits.SearchBytes-step.bytes || info.Size+1 > q.w.limits.SearchBytes-q.ignore.usedBytes {
			step.pause = "scan_bytes"
			return nil, false, false, nil
		}
	}
	cost := max(int64(0), min(info.Size, int64(maxIgnoreFileBytes)))*8 + 1024
	if !q.charge(cost, 0) {
		return nil, true, false, nil
	}
	beforeFiles, beforeBytes := q.ignore.usedFiles, q.ignore.usedBytes
	layer, allowed = q.ignore.load(ctx, directory, parent)
	step.files += q.ignore.usedFiles - beforeFiles
	step.bytes += q.ignore.usedBytes - beforeBytes
	if ctx.Err() != nil {
		return nil, true, false, ctx.Err()
	}
	if _, err := q.observe(ctx, name); err != nil {
		return nil, true, false, err
	}
	return layer, true, allowed, nil
}

func (q *workspaceQuery) queryScopeParents(ctx context.Context, step *queryStep) (bool, error) {
	for q.scopeIndex < len(q.scopeParts) {
		layer, ready, allowed, err := q.loadQueryIgnore(ctx, q.scopeDirectory, q.scopeIgnore, step)
		if err != nil || !ready {
			return ready, err
		}
		if !allowed || q.done {
			q.done, q.scopePending = true, false
			return true, nil
		}
		q.scopeDirectory = joinRelative(q.scopeDirectory, q.scopeParts[q.scopeIndex])
		q.scopeIndex++
		q.scopeIgnore = layer
		if q.ignore.excluded(layer, q.scopeDirectory, true) {
			q.done, q.scopePending = true, false
			return true, nil
		}
	}
	q.scopePending = false
	q.pendingDirectory.ignore = q.scopeIgnore
	return true, nil
}
