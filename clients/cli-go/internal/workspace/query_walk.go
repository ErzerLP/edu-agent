package workspace

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func (q *workspaceQuery) startQueryDirectory(ctx context.Context, step *queryStep) (bool, error) {
	node := *q.pendingDirectory
	if err := q.ensureDirectoryGuard(ctx, node.path); err != nil {
		return true, err
	}
	layer, ready, allowed, err := q.loadQueryIgnore(ctx, node.path, node.ignore, step)
	if err != nil || !ready {
		return ready, err
	}
	if !allowed || q.done {
		q.pendingDirectory = nil
		return true, nil
	}
	info, err := q.observe(ctx, node.path)
	if err != nil {
		return true, err
	}
	if info.Kind != securefile.EntryDirectory {
		return true, securefile.ErrChanged
	}
	scan, err := q.w.root.OpenDirectoryScan(ctx, node.path)
	if err != nil {
		if errors.Is(err, securefile.ErrChanged) || errors.Is(err, securefile.ErrOutsideRoot) || errors.Is(err, securefile.ErrLink) || ctx.Err() != nil {
			return true, err
		}
		q.other++
		q.note("directory_unavailable")
		q.pendingDirectory = nil
		return true, nil
	}
	if scan.Entry() != info {
		_ = scan.Close()
		return true, securefile.ErrChanged
	}
	node.ignore = layer
	q.scanner, q.scanning, q.pendingDirectory = scan, node, nil
	q.directories++
	return true, nil
}

func (q *workspaceQuery) scanQueryDirectory(ctx context.Context, step *queryStep) (bool, error) {
	if step.directory != q.scanning.path {
		step.directory, step.directoryEntries = q.scanning.path, 0
	}
	remaining := min(q.w.limits.DirectoryScanEntries-step.directoryEntries, q.w.limits.SearchEntries-step.entries, 65536)
	if remaining <= 0 {
		step.pause = "directory_entry_limit"
		if step.entries >= q.w.limits.SearchEntries {
			step.pause = "entry_limit"
		}
		return false, nil
	}
	entries, skipped, complete, err := q.scanner.Next(ctx, remaining)
	if err != nil {
		return false, err
	}
	step.entries += len(entries) + skipped
	step.directoryEntries += len(entries) + skipped
	q.visited += len(entries) + skipped
	q.other += skipped
	if skipped > 0 {
		q.note("invalid_entry_path")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		path := joinRelative(q.scanning.path, entry.Name)
		if _, err := normalizeModelPath(path, false); err != nil {
			q.other++
			q.note("invalid_entry_path")
			continue
		}
		if q.args.tool != ToolList && !isArchivePath(q.args.path) && isArchivePath(path) {
			continue
		}
		if q.ignore.excluded(q.scanning.ignore, path, entry.Type == securefile.EntryDirectory) {
			continue
		}
		if q.args.tool == ToolSearch && entry.Type == securefile.EntryDirectory && matchesAnyGlob(path, q.args.search.Exclude) {
			continue
		}
		if q.args.tool != ToolList {
			if entry.Type == securefile.EntryLink {
				q.links++
				continue
			}
			if entry.Type != securefile.EntryDirectory && entry.Type != securefile.EntryFile {
				q.other++
				continue
			}
		}
		if !q.pushNode(queryNode{path: path, kind: entry.Type, depth: q.scanning.depth + 1, ignore: q.scanning.ignore}) {
			return false, nil
		}
	}
	if complete {
		if err := q.finishQueryDirectory(); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (q *workspaceQuery) advanceQuery(ctx context.Context, step *queryStep, target int) error {
	countMode := q.args.tool == ToolSearch && *q.args.search.Output == "count"
	for !q.done && (countMode || len(q.rows) < target) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if q.scopePending {
			ready, err := q.queryScopeParents(ctx, step)
			if err != nil || !ready {
				return err
			}
			continue
		}
		if q.searchFile != nil {
			paused, err := q.advanceSearchFile(ctx, step)
			if err != nil || paused {
				return err
			}
			continue
		}
		if q.pendingFile != nil {
			previous := q.scannedFiles
			paused, err := q.openSearchFile(ctx, *q.pendingFile, step)
			if err != nil || paused {
				return err
			}
			q.pendingFile = nil
			if q.scannedFiles != previous && (q.scannedFiles == 1 || q.scannedFiles%128 == 0) {
				publishProgress(ctx, Progress{Tool: q.args.tool, Path: q.args.path, ScannedFiles: q.scannedFiles, ScannedBytes: q.scannedBytes, Returned: len(q.rows), TruncationReason: q.reason})
			}
			continue
		}
		if q.pendingDirectory != nil {
			ready, err := q.startQueryDirectory(ctx, step)
			if err != nil || !ready {
				return err
			}
			continue
		}
		if q.scanner != nil {
			ready, err := q.scanQueryDirectory(ctx, step)
			if err != nil || !ready {
				return err
			}
			continue
		}
		if q.frontier.Len() == 0 {
			q.done = true
			break
		}
		if step.matches >= q.args.limit {
			step.pause = "entry_limit"
			if q.args.tool == ToolSearch {
				step.pause = "match_limit"
			}
			break
		}
		node := q.popNode()
		if err := q.visitQueryNode(ctx, node, step); err != nil {
			return err
		}
	}
	if q.frontier.Len() == 0 && q.scanner == nil && q.pendingDirectory == nil && q.pendingFile == nil && q.searchFile == nil && !q.scopePending {
		q.done = true
	}
	return nil
}

func (q *workspaceQuery) visitQueryNode(ctx context.Context, node queryNode, step *queryStep) error {
	if node.kind == securefile.EntryDirectory {
		if err := q.ensureDirectoryGuard(ctx, node.path); err != nil {
			return err
		}
	}
	info, err := q.observe(ctx, node.path)
	if err != nil {
		if errors.Is(err, securefile.ErrNotFound) {
			return securefile.ErrChanged
		}
		return err
	}
	if info.Kind != node.kind {
		return securefile.ErrChanged
	}
	if q.args.tool == ToolList {
		if q.appendRecord(map[string]any{"path": node.path, "type": string(node.kind)}, nil) && len(q.rows) > q.args.offset {
			step.matches++
		}
		return nil
	}
	if node.kind == securefile.EntryDirectory {
		if node.depth > q.w.limits.SearchDepth {
			q.note("depth_limit")
		} else {
			q.pendingDirectory = &node
		}
	}
	if q.args.tool == ToolFind {
		if (q.args.find.Type == "any" || q.args.find.Type == string(node.kind)) && q.findGlob.Match(node.path) {
			if q.appendRecord(map[string]any{"path": node.path, "type": string(node.kind)}, nil) {
				step.matches++
			}
		}
	} else if node.kind == securefile.EntryFile {
		q.pendingFile = &node
	}
	return nil
}
