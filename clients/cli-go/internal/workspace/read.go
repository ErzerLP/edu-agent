package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type readArguments struct {
	Path         string `json:"path"`
	Offset       int    `json:"offset"`
	Limit        int    `json:"limit"`
	ByteOffset   int    `json:"byte_offset"`
	ExpectedHash string `json:"expected_hash"`
}

func readOffsetLimit(limits Limits) int {
	return max(1_000_000, int(limits.ReadFileBytes)+1)
}

func (w *Workspace) executeRead(ctx context.Context, raw string) Result {
	var args readArguments
	if err := decodeArguments(raw, &args); err != nil {
		return resultForError(err, "文件读取参数无效")
	}
	path, err := normalizeModelPath(args.Path, false)
	if err != nil {
		return resultForError(err, "文件读取路径无效")
	}
	if args.Offset == 0 {
		args.Offset = 1
	}
	if args.Limit == 0 {
		args.Limit = w.limits.ReadLines
	}
	if args.Offset < 1 || args.Offset > readOffsetLimit(w.limits) || args.Limit < 1 || args.Limit > w.limits.ReadLines || args.ByteOffset < 0 || int64(args.ByteOffset) > w.limits.ReadFileBytes || args.ExpectedHash != "" && !validContentHash(args.ExpectedHash) {
		return failureResult(CodeInvalidArguments, "文件读取参数无效")
	}
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	publishProgress(ctx, Progress{Tool: ToolRead, Path: path, StartLine: args.Offset})
	// Keep the secure whole-file snapshot and its change/link checks. This is
	// bounded full-file I/O, not a streaming or partial-hash read.
	snapshot, err := w.root.ReadSnapshot(path, w.limits.ReadFileBytes, false)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextFailure(contextErr)
	}
	if err != nil {
		if errors.Is(err, securefile.ErrLink) {
			return failureResult(CodeLinkNotAllowed, "文件链接不允许读取")
		}
		if isPermissionError(err) {
			return failureResult(CodePermissionDenied, "文件不可读取")
		}
		if errors.Is(err, securefile.ErrChanged) {
			return readContentChangedResult(path, args.ExpectedHash, "文件读取期间内容版本已变化")
		}
		if errors.Is(err, securefile.ErrTooLarge) {
			result := failureResult(CodeFileTooLarge, fmt.Sprintf("文件超过完整读取预算 %d 字节", w.limits.ReadFileBytes))
			value := result.Value.(map[string]any)
			value["read_byte_limit"] = w.limits.ReadFileBytes
			value["suggestion"] = "增大 --file-read-limit，或使用 Shell 按范围处理；未取得完整文件 hash"
			return result
		}
		return resultForError(err, "文件无法安全读取")
	}
	publishProgress(ctx, Progress{Tool: ToolRead, Path: path, StartLine: args.Offset, Bytes: int64(len(snapshot.Data))})
	decoded, err := decodeText(snapshot.Data)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextFailure(contextErr)
	}
	if err != nil {
		return resultForError(err, "文件不是可读取的 UTF-8 文本")
	}
	if args.ExpectedHash != "" && args.ExpectedHash != decoded.Hash {
		return readContentChangedResult(path, args.ExpectedHash, "文件内容版本已变化")
	}
	lines, totalLines, err := readLineWindow(ctx, decoded.Text, args.Offset, args.Limit)
	if err != nil {
		return contextFailure(err)
	}
	startIndex := min(args.Offset-1, totalLines)
	// A caller may point exactly past a line's bytes. Canonicalize the actual
	// content origin before projecting it, without extending the requested
	// line window. Otherwise a later shortened page could point into that
	// already-consumed line and skip or reject the following bytes.
	if len(lines) > 0 && args.ByteOffset == len(lines[0]) {
		startIndex++
		args.Offset, args.ByteOffset = startIndex+1, 0
		lines = lines[1:]
	}
	contentBudget := max(64, w.limits.ResultBytes-len(path)-1400)
	var builder strings.Builder
	builder.Grow(min(contentBudget, len(decoded.Text)))
	returnedLines := 0
	endLine := 0
	nextOffset, nextByteOffset := 0, 0
	reason := ""
	for localIndex, line := range lines {
		if err := ctx.Err(); err != nil {
			return contextFailure(err)
		}
		index := startIndex + localIndex
		lineByteOffset := 0
		if localIndex == 0 {
			lineByteOffset = args.ByteOffset
			if lineByteOffset > len(line) || lineByteOffset < len(line) && !utf8.RuneStart(line[lineByteOffset]) {
				return failureResult(CodeInvalidArguments, "文件读取 byte_offset 无效")
			}
		}
		remaining := line[lineByteOffset:]
		available := contentBudget - builder.Len()
		if len(remaining) > available {
			prefix := truncateUTF8Bytes(remaining, available)
			if prefix == "" {
				reason = "result_bytes"
				nextOffset, nextByteOffset = index+1, lineByteOffset
				break
			}
			builder.WriteString(prefix)
			returnedLines++
			endLine = index + 1
			reason = "result_bytes"
			nextOffset, nextByteOffset = index+1, lineByteOffset+len(prefix)
			break
		}
		builder.WriteString(remaining)
		returnedLines++
		endLine = index + 1
		if builder.Len() >= contentBudget && index+1 < totalLines {
			reason = "result_bytes"
			nextOffset = index + 2
			break
		}
	}
	if reason == "" && startIndex+returnedLines < totalLines {
		reason = "line_limit"
		nextOffset = startIndex + returnedLines + 1
	}
	complete := reason == "" && startIndex+returnedLines >= totalLines
	content := builder.String()
	value := map[string]any{
		"path": path, "content": content, "content_hash": decoded.Hash,
		"hash_scope": "whole_file", "file_bytes": len(snapshot.Data), "read_byte_limit": w.limits.ReadFileBytes,
		"offset": args.Offset, "byte_offset": args.ByteOffset,
		"start_line": args.Offset, "end_line": endLine, "returned_lines": returnedLines,
		"total_lines": totalLines, "complete": complete,
	}
	const continuationSuggestion = "使用 next_offset 和 next_byte_offset 继续读取，并携带 expected_hash"
	if !complete {
		value["truncation_reason"] = reason
		value["next_offset"] = nextOffset
		value["next_byte_offset"] = nextByteOffset
		value["suggestion"] = continuationSuggestion
	}
	if safeResultJSONSize(value) > w.limits.ResultBytes && len(content) > 0 {
		// Escaping can expand one byte to six JSON bytes. Search for a fitting
		// rune-safe prefix rather than subtracting that expansion from the raw
		// bytes, which could produce an empty, permanently stuck page.
		setPrefix := func(prefix string) {
			endLine, returnedLines, nextOffset, nextByteOffset = readPrefixPosition(lines, 0, args.ByteOffset, len(prefix))
			if endLine > 0 {
				endLine += startIndex
			}
			nextOffset += startIndex
			value["content"] = prefix
			value["end_line"] = endLine
			value["returned_lines"] = returnedLines
			value["complete"] = false
			value["truncation_reason"] = "result_bytes"
			value["next_offset"] = nextOffset
			value["next_byte_offset"] = nextByteOffset
			value["suggestion"] = continuationSuggestion
		}
		setPrefix("")
		if safeResultJSONSize(value) > w.limits.ResultBytes {
			return readResultBudgetFailure(w.limits.ResultBytes)
		}
		low, high, best := 1, len(content)-1, 0
		for low <= high {
			middle := low + (high-low)/2
			prefix := truncateUTF8Bytes(content, middle)
			setPrefix(prefix)
			if safeResultJSONSize(value) <= w.limits.ResultBytes {
				best = len(prefix)
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		if best == 0 {
			return readResultBudgetFailure(w.limits.ResultBytes)
		}
		content = content[:best]
		setPrefix(content)
	}
	if safeResultJSONSize(value) > w.limits.ResultBytes {
		return readResultBudgetFailure(w.limits.ResultBytes)
	}
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	progress := Progress{
		Tool: ToolRead, Path: path, StartLine: args.Offset, EndLine: endLine,
		Bytes: int64(len(content)),
	}
	if truncationReason, ok := value["truncation_reason"].(string); ok {
		progress.TruncationReason = truncationReason
	}
	if continuationOffset, ok := value["next_offset"].(int); ok {
		progress.NextOffset, progress.HasContinuation = continuationOffset, true
		progress.NextByteOffset, _ = value["next_byte_offset"].(int)
	}
	publishProgress(ctx, progress)
	return Result{
		Value:     value,
		Summary:   fmt.Sprintf("已读取 %s 第 %d-%d 行", path, args.Offset, endLine),
		Reference: &Reference{Path: path, ContentHash: decoded.Hash, Kind: "file"},
	}
}

// readLineWindow counts every line but retains only the requested window's
// string references. Empty text has zero lines; a trailing LF adds no line.
func readLineWindow(ctx context.Context, text string, offset, limit int) ([]string, int, error) {
	var lines []string
	totalLines := 0
	for start := 0; start < len(text); {
		if totalLines%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		end := len(text)
		if newline := strings.IndexByte(text[start:], '\n'); newline >= 0 {
			end = start + newline + 1
		}
		totalLines++
		if totalLines >= offset && len(lines) < limit {
			lines = append(lines, text[start:end])
		}
		start = end
	}
	return lines, totalLines, ctx.Err()
}

func readResultBudgetFailure(limit int) Result {
	result := failureResult(CodeInvalidArguments, "文件读取结果预算不足以返回可续读内容")
	value := result.Value.(map[string]any)
	value["result_byte_limit"] = limit
	value["suggestion"] = "缩短路径或增大结果预算；也可使用 Shell 按范围处理"
	return result
}

// readPrefixPosition maps a byte prefix of the contiguous read stream back to
// the next line/byte cursor. Lines include their newline terminators, so a
// prefix ending exactly at a line boundary continues at the next line.
func readPrefixPosition(lines []string, startIndex, startByteOffset, consumed int) (endLine, returnedLines, nextOffset, nextByteOffset int) {
	if consumed == 0 {
		return 0, 0, startIndex + 1, startByteOffset
	}
	index := startIndex
	byteOffset := startByteOffset
	for index < len(lines) {
		if byteOffset >= len(lines[index]) {
			index++
			byteOffset = 0
			continue
		}
		if consumed <= 0 {
			break
		}
		remaining := len(lines[index]) - byteOffset
		take := min(consumed, remaining)
		if take > 0 {
			endLine = index + 1
			returnedLines++
			byteOffset += take
			consumed -= take
		}
		if byteOffset == len(lines[index]) {
			index++
			byteOffset = 0
			continue
		}
		break
	}
	return endLine, returnedLines, index + 1, byteOffset
}

func readContentChangedResult(path, expectedHash, summary string) Result {
	result := failureResult(CodeContentChanged, summary)
	result.Reference = &Reference{Path: path, ContentHash: expectedHash, Kind: "file", InvalidateObserved: true}
	result.Publication = PublicationUnchanged
	return result
}

func validContentHash(value string) bool {
	if len(value) != len("sha256:")+64 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, current := range value[len("sha256:"):] {
		if current < '0' || current > '9' && current < 'a' || current > 'f' {
			return false
		}
	}
	return true
}
