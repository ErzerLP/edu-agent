package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const CodeDiffTooLarge = "diff_too_large"

const completeDiffContext = 3
const completeDiffChunk = 32 << 10
const noFinalNewline = "\n\\ No newline at end of file\n"

var errCompleteDiffTooLarge = errors.New("complete diff exceeds byte limit")

// buildCompleteDiff renders raw file bytes, including BOM, CRLF and missing LF.
// ranges, when supplied, are sorted, non-overlapping replacements in oldRaw byte
// coordinates that produced newRaw (not offsets in decoded, BOM-free text).
// Nil ranges use one common-prefix/suffix change region for a whole-file write.
// Callers retain responsibility for matching, versions and candidate validation.
// Indexes scale with line count; this is bounded full-file processing, not a
// streaming or minimal diff algorithm. Errors never return a partial diff.
func buildCompleteDiff(ctx context.Context, path string, oldRaw, newRaw []byte, create bool, ranges []replacementRange, limit int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if limit < 1 || limit > int64(^uint(0)>>1)-1 {
		return "", operationFailure(CodeInvalidArguments, "complete diff byte limit is invalid")
	}
	out := completeDiffWriter{ctx: ctx, limit: limit}
	if create {
		out.writeString("--- /dev/null\n")
	} else {
		out.writeString("--- a/")
		out.writeString(path)
		out.writeString("\n")
	}
	out.writeString("+++ b/")
	out.writeString(path)
	out.writeString("\n")
	if out.err != nil {
		return "", out.err
	}
	oldLines, err := indexCompleteDiffLines(ctx, oldRaw)
	if err != nil {
		return "", err
	}
	newLines, err := indexCompleteDiffLines(ctx, newRaw)
	if err != nil {
		return "", err
	}
	if len(ranges) == 0 {
		out.hunk(oldLines, newLines, 0, oldLines.count(), 0, newLines.count())
	} else {
		// Expand known edits in original line coordinates, then merge touching
		// windows before mapping. Thus every replacement contributes its delta
		// exactly once, including multiple replacements on the same line.
		type window struct{ start, end, first, last int }
		windows := make([]window, 0, len(ranges))
		for i, r := range ranges {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			start := sort.Search(len(oldLines.starts), func(j int) bool { return oldLines.starts[j] > r.start }) - 1
			if r.start == len(oldRaw) && len(oldRaw) > 0 && oldRaw[len(oldRaw)-1] != '\n' {
				// Appending to an unterminated line changes that line, rather
				// than inserting after it. Keep three preceding context lines.
				start--
			}
			end := sort.SearchInts(oldLines.starts, r.end)
			end += min(completeDiffContext, oldLines.count()-end)
			current := window{max(0, start-completeDiffContext), end, i, i + 1}
			if len(windows) > 0 && current.start <= windows[len(windows)-1].end {
				previous := &windows[len(windows)-1]
				previous.end = max(previous.end, current.end)
				previous.last = current.last
			} else {
				windows = append(windows, current)
			}
		}
		delta := 0
		for _, current := range windows {
			newStartByte := oldLines.starts[current.start] + delta
			for _, r := range ranges[current.first:current.last] {
				delta += len(r.text) - (r.end - r.start)
			}
			newEndByte := oldLines.starts[current.end] + delta
			newStart := sort.SearchInts(newLines.starts, newStartByte)
			newEnd := sort.SearchInts(newLines.starts, newEndByte)
			if newStart >= len(newLines.starts) || newEnd >= len(newLines.starts) ||
				newLines.starts[newStart] != newStartByte || newLines.starts[newEnd] != newEndByte {
				return "", operationFailure(CodeInternalError, "complete diff range mapping is inconsistent")
			}
			out.hunk(oldLines, newLines, current.start, current.end, newStart, newEnd)
			if out.err != nil {
				return "", out.err
			}
		}
	}
	if out.err != nil {
		return "", out.err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return out.text.String(), nil
}

type completeDiffLines struct {
	raw    []byte
	starts []int // actual line starts followed by len(raw); empty text is {0}
}

func indexCompleteDiffLines(ctx context.Context, raw []byte) (completeDiffLines, error) {
	lines := completeDiffLines{raw: raw, starts: []int{0}}
	for i, b := range raw {
		if i%completeDiffChunk == 0 {
			if err := ctx.Err(); err != nil {
				return completeDiffLines{}, err
			}
		}
		if b == '\n' && i+1 < len(raw) {
			lines.starts = append(lines.starts, i+1)
		}
	}
	if len(raw) > 0 {
		lines.starts = append(lines.starts, len(raw))
	}
	return lines, ctx.Err()
}

func (l completeDiffLines) count() int { return len(l.starts) - 1 }
func (l completeDiffLines) line(i int) []byte {
	return l.raw[l.starts[i]:l.starts[i+1]]
}

type completeDiffWriter struct {
	ctx   context.Context
	limit int64
	text  strings.Builder
	err   error
}

func (w *completeDiffWriter) reserve(size int) bool {
	if w.err != nil {
		return false
	}
	if w.err = w.ctx.Err(); w.err != nil {
		return false
	}
	// Subtract, rather than add lengths, so budgets near maxInt cannot wrap.
	if int64(size) > w.limit-int64(w.text.Len()) {
		w.err = errCompleteDiffTooLarge
		return false
	}
	return true
}

func (w *completeDiffWriter) writeString(text string) {
	if !w.reserve(len(text)) {
		return
	}
	for len(text) > 0 {
		if w.err = w.ctx.Err(); w.err != nil {
			return
		}
		n := min(len(text), completeDiffChunk)
		w.text.WriteString(text[:n])
		text = text[n:]
	}
}

func (w *completeDiffWriter) line(marker string, raw []byte) {
	w.writeString(marker)
	if !w.reserve(len(raw)) {
		return
	}
	remaining := raw
	for len(remaining) > 0 {
		if w.err = w.ctx.Err(); w.err != nil {
			return
		}
		n := min(len(remaining), completeDiffChunk)
		w.text.Write(remaining[:n])
		remaining = remaining[n:]
	}
	if raw[len(raw)-1] != '\n' {
		// Add a diff record separator, then explicitly exclude it from the
		// original/candidate. A trailing CR remains part of the file's bytes.
		w.writeString(noFinalNewline)
	}
}

func (w *completeDiffWriter) equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for len(a) > 0 {
		if w.err = w.ctx.Err(); w.err != nil {
			return false
		}
		n := min(len(a), completeDiffChunk)
		if !bytes.Equal(a[:n], b[:n]) {
			return false
		}
		a, b = a[n:], b[n:]
	}
	return true
}

func (w *completeDiffWriter) hunk(old, next completeDiffLines, oldStart, oldEnd, newStart, newEnd int) {
	oldChange, newChange := oldStart, newStart
	for oldChange < oldEnd && newChange < newEnd && w.err == nil && w.equal(old.line(oldChange), next.line(newChange)) {
		oldChange++
		newChange++
	}
	if w.err != nil || oldChange == oldEnd && newChange == newEnd {
		return
	}
	oldSuffix, newSuffix := oldEnd, newEnd
	for oldSuffix > oldChange && newSuffix > newChange && w.err == nil && w.equal(old.line(oldSuffix-1), next.line(newSuffix-1)) {
		oldSuffix--
		newSuffix--
	}
	if w.err != nil {
		return
	}
	contextBefore := min(completeDiffContext, oldChange-oldStart)
	contextAfter := min(completeDiffContext, oldEnd-oldSuffix)
	oldStart, newStart = oldChange-contextBefore, newChange-contextBefore
	oldEnd, newEnd = oldSuffix+contextAfter, newSuffix+contextAfter
	oldNumber, newNumber := oldStart, newStart
	if oldEnd > oldStart {
		oldNumber++
	}
	if newEnd > newStart {
		newNumber++
	}
	w.writeString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", oldNumber, oldEnd-oldStart, newNumber, newEnd-newStart))
	for i := oldStart; i < oldChange && w.err == nil; i++ {
		w.line(" ", old.line(i))
	}
	for i := oldChange; i < oldSuffix && w.err == nil; i++ {
		w.line("-", old.line(i))
	}
	for i := newChange; i < newSuffix && w.err == nil; i++ {
		w.line("+", next.line(i))
	}
	for i := oldSuffix; i < oldEnd && w.err == nil; i++ {
		w.line(" ", old.line(i))
	}
}

func completeDiffFailure(err error, limit int64) Result {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return mutationContextFailure(err)
	}
	if errors.Is(err, errCompleteDiffTooLarge) {
		result := mutationFailure(CodeDiffTooLarge, fmt.Sprintf("完整差异超过 %d 字节处理上限，未准备文件修改", limit))
		value := result.Value.(map[string]any)
		value["diff_byte_limit"] = limit
		value["suggestion"] = "请分次操作、由用户调整客户端差异资源上限，或使用 Shell/脚本处理"
		return result
	}
	return mutationFailureForError(err, "无法生成完整文件差异")
}
