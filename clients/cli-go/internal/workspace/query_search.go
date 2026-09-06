package workspace

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	// Include the match pair, its outer slice slot and slice growth. The fixed
	// allowance also covers FindAllStringIndex's minimum outer allocation.
	querySearchIndexBytes    int64 = 64
	querySearchIndexOverhead int64 = 256
)

// openSearchFile consumes no file allowance when it pauses. The caller keeps
// its pending node in that case; only this method owns body-read accounting.
func (q *workspaceQuery) openSearchFile(ctx context.Context, node queryNode, step *queryStep) (paused bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	args := q.args.search
	if q.done || !includedPath(node.path, args.Include, args.Exclude) || !q.searchGlob.Match(node.path) {
		return false, nil
	}
	usedFiles, usedBytes := step.files, step.bytes
	if q.ignore != nil {
		// used* includes content reads as well as ignore reads, so summing
		// these counters would charge the same content twice.
		usedFiles = max(usedFiles, q.ignore.usedFiles)
		usedBytes = max(usedBytes, q.ignore.usedBytes)
	}
	if usedFiles >= q.w.limits.SearchFiles {
		step.pause = "file_limit"
		return true, nil
	}
	if usedBytes >= q.w.limits.SearchBytes {
		step.pause = "scan_bytes"
		return true, nil
	}
	info, err := q.observe(ctx, node.path)
	if err != nil {
		return false, q.searchReadError(ctx, err)
	}
	switch info.Kind {
	case securefile.EntryLink:
		q.links++
		return false, nil
	case securefile.EntryFile:
	default:
		q.other++
		q.note("file_unavailable")
		return false, nil
	}
	if info.Size < 0 || info.Size > q.w.limits.FileBytes {
		q.large++
		q.note("file_too_large")
		return false, nil
	}
	// ReadSnapshot is limited to the frozen size, plus its one-byte growth
	// probe. A file that cannot fit even a fresh invocation is a permanent
	// gap, not an endlessly resumable zero-progress page.
	if info.Size >= q.w.limits.SearchBytes {
		q.note("scan_bytes")
		return false, nil
	}
	if info.Size >= q.w.limits.SearchBytes-usedBytes {
		step.pause = "scan_bytes"
		return true, nil
	}

	// Reserve before allocating the read buffer or decoded string. This is a
	// conservative logical budget, not a claim about exact Go peak heap use.
	temporary := 4*(info.Size+1) + 512 + int64(len(node.path))
	if !q.charge(temporary, 0) {
		return false, nil
	}
	defer func() { q.release(temporary) }()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	step.files++
	q.scannedFiles++
	var snapshot securefile.Snapshot
	if q.ignore != nil {
		snapshot, err = q.ignore.readSnapshot(node.path, info.Size)
	} else {
		snapshot, err = q.w.root.ReadSnapshot(node.path, info.Size, false)
	}
	charged := snapshot.Size
	if err != nil {
		// The safe reader does not expose a partial body on failure. Account
		// its bounded possible consumption, just like ignore.readSnapshot.
		charged = info.Size + 1
	}
	step.bytes += charged
	q.scannedBytes += charged
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// Reinspect even a failed read: replacement/disappearance is stale, not
	// an ordinary unavailable-file skip. observe compares the full version.
	if _, observeErr := q.observe(ctx, node.path); observeErr != nil {
		return false, q.searchReadError(ctx, observeErr)
	}
	if err != nil {
		return false, q.searchReadError(ctx, err)
	}
	if snapshot.Size != info.Size || snapshot.Identity != info.Identity || !snapshot.ModTime.Equal(info.ModTime) {
		return false, securefile.ErrChanged
	}
	if err := q.rememberFileHash(node.path, snapshot.Data); err != nil {
		return false, err
	}
	decoded, err := decodeText(snapshot.Data)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		var operation *operationError
		if errors.As(err, &operation) && (operation.code == CodeBinaryFile || operation.code == CodeInvalidUTF8) {
			q.binary++
			return false, nil
		}
		return false, q.searchReadError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// SplitAfter allocates a slot for a trailing empty string even when
	// splitTextLines drops it. Charge that backing array before splitting.
	lineBytes := int64(strings.Count(decoded.Text, "\n")+1) * 16
	if !q.charge(lineBytes, 0) {
		return false, nil
	}
	temporary += lineBytes
	lines := splitTextLines(decoded.Text)
	retained := int64(len(decoded.Text)+len(node.path)) + 512 + lineBytes
	q.searchFile = &querySearchFile{path: node.path, lines: lines, retained: retained}
	temporary -= retained
	if len(lines) == 0 {
		q.finishSearchFile()
	}
	return false, nil
}

// advanceSearchFile emits at most one primary record (or one count-mode
// matching line). Its retained indices are independent of per-invocation
// match limits; neither a new page nor a zero-width match restarts the regex.
func (q *workspaceQuery) advanceSearchFile(ctx context.Context, step *queryStep) (paused bool, err error) {
	defer func() {
		if err != nil || q.done {
			q.finishSearchFile()
		}
	}()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if q.done || q.searchFile == nil {
		return false, nil
	}
	file := q.searchFile
	// The enclosing engine validates the entire observation set on resume.
	// Also keep this single-file boundary safe when advanced independently.
	if _, err := q.observe(ctx, file.path); err != nil {
		return false, err
	}
	for file.line < len(file.lines) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if step.matches >= q.w.limits.SearchMatches {
			step.pause = "match_limit"
			return true, nil
		}
		line := trimLineEnding(file.lines[file.line])
		if *q.args.search.Output != "content" {
			if !q.matcher.MatchString(line) {
				file.line++
				continue
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if *q.args.search.Output == "files" {
				if q.appendRecord(map[string]any{"path": file.path}, nil) {
					q.matchedFiles++
					step.matches++
					q.finishSearchFile()
				}
				return false, nil
			}
			// Count progress consumes a cumulative unit without retaining a
			// fake result row or allocating occurrence indices.
			if !q.charge(0, 1) {
				return false, nil
			}
			q.countSearchLine(file)
			step.matches++
			q.finishSearchLine(file)
			return false, nil
		}

		if !file.lineReady {
			ready := q.prepareSearchIndices(file, line)
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if !ready {
				return false, nil
			}
			if len(file.indices) == 0 {
				q.finishSearchLine(file)
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		match := file.indices[file.index]
		column, previousStart := 1, 0
		if file.index > 0 {
			// Rows are an immutable prefix and the previous row is this
			// line's previous match. Count only the new rune span, rather
			// than rescanning a long UTF-8 prefix for every occurrence.
			previousStart = file.indices[file.index-1][0]
			column = q.rows[len(q.rows)-1].value["column"].(int)
		}
		column += utf8.RuneCountInString(line[previousStart:match[0]])
		entry := map[string]any{
			"path": file.path, "line": file.line + 1, "column": column,
			// Clone bounded strings: published rows must not keep the entire
			// decoded file alive after its temporary charge is released.
			"preview": strings.Clone(boundedSearchPreview(line, match[0], match[1], q.w.limits.SearchPreviewBytes)),
		}
		neighbors := q.searchNeighbors(file)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !q.appendRecord(entry, neighbors) {
			return false, nil
		}
		q.countSearchLine(file)
		step.matches++
		file.index++
		if file.index == len(file.indices) {
			if file.indicesPartial {
				// The lookahead proved an unretained suffix. Never turn this
				// bounded prefix into an apparent EOF or rescan the suffix.
				q.stop(CodeQueryCapacity)
			} else {
				q.finishSearchLine(file)
			}
		}
		return false, nil
	}
	q.finishSearchFile()
	return false, nil
}

func (q *workspaceQuery) prepareSearchIndices(file *querySearchFile, line string) bool {
	remaining := q.w.limits.QueryEntries - q.units
	available := q.w.limits.QueryMemoryBytes - q.w.queryBytes
	// Leave half the free retention for primary records/context instead of
	// letting a long empty regex consume every byte merely on indices.
	slots := max(int64(0), (available/2-querySearchIndexOverhead)/querySearchIndexBytes)
	limit := min(remaining, len(line)+1, int(max(int64(0), slots-1)))
	if limit == 0 {
		// A capacity bound alone is not evidence of a match or omission.
		if q.matcher.MatchString(line) {
			q.stop(CodeQueryCapacity)
			return false
		}
		file.lineReady = true
		return true
	}
	reserved := int64(limit+1)*querySearchIndexBytes + querySearchIndexOverhead
	if !q.charge(reserved, 0) {
		return false
	}
	indices := q.matcher.FindAllStringIndex(line, limit+1)
	file.lineReady = true
	file.indicesPartial = len(indices) > limit
	// Keep the charged backing slice (including its lookahead slot) until
	// the line ends; slicing it does not free the underlying allocation.
	file.indexBytes = int64(len(indices)) * querySearchIndexBytes
	if len(indices) > 0 {
		file.indexBytes += querySearchIndexOverhead
	}
	q.release(reserved - file.indexBytes)
	if file.indicesPartial {
		indices = indices[:limit]
	}
	file.indices = indices
	return true
}

func (q *workspaceQuery) countSearchLine(file *querySearchFile) {
	if !file.matched {
		file.matched = true
		q.matchedFiles++
	}
	if !file.lineCounted {
		file.lineCounted = true
		q.matchedLines++
	}
}

func (q *workspaceQuery) searchNeighbors(file *querySearchFile) []map[string]any {
	contextLines := *q.args.search.Context
	if contextLines == 0 {
		return nil
	}
	first := max(0, file.line-contextLines)
	last := min(len(file.lines)-1, file.line+contextLines)
	neighbors := make([]map[string]any, 0, last-first+1)
	for index := first; index <= last; index++ {
		text := trimLineEnding(file.lines[index])
		entry := map[string]any{
			"path": file.path, "line": index + 1,
			"content": strings.Clone(truncateUTF8Bytes(text, q.w.limits.SearchPreviewBytes)),
		}
		if len(text) > q.w.limits.SearchPreviewBytes {
			entry["truncated"] = true
		}
		neighbors = append(neighbors, entry)
	}
	return neighbors
}

func (q *workspaceQuery) finishSearchLine(file *querySearchFile) {
	q.release(file.indexBytes)
	file.indices, file.indexBytes = nil, 0
	file.index, file.lineReady, file.lineCounted, file.indicesPartial = 0, false, false, false
	file.line++
	if file.line == len(file.lines) {
		q.finishSearchFile()
	}
}

func (q *workspaceQuery) finishSearchFile() {
	if q.searchFile != nil {
		q.release(q.searchFile.retained + q.searchFile.indexBytes)
		q.searchFile = nil
	}
}

func (q *workspaceQuery) searchReadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, securefile.ErrChanged) || errors.Is(err, securefile.ErrOutsideRoot) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if q.done {
		return nil
	}
	switch {
	case errors.Is(err, securefile.ErrLink):
		q.links++
	case errors.Is(err, securefile.ErrTooLarge):
		q.large++
		q.note("file_too_large")
	default:
		q.other++
		q.note("file_unavailable")
	}
	return nil
}
