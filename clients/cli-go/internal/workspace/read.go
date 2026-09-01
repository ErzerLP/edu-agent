package workspace

import (
	"context"
	"errors"
	"fmt"
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
	if args.Offset < 1 || args.Offset > 1_000_000 || args.Limit < 1 || args.Limit > w.limits.ReadLines || args.ByteOffset < 0 || args.ByteOffset > int(w.limits.FileBytes) || args.ExpectedHash != "" && !validContentHash(args.ExpectedHash) {
		return failureResult(CodeInvalidArguments, "文件读取参数无效")
	}
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	snapshot, err := w.root.ReadSnapshot(path, w.limits.FileBytes, false)
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
		return resultForError(err, "文件无法安全读取")
	}
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	decoded, err := decodeText(snapshot.Data)
	if err != nil {
		return resultForError(err, "文件不是可读取的 UTF-8 文本")
	}
	if args.ExpectedHash != "" && args.ExpectedHash != decoded.Hash {
		return readContentChangedResult(path, args.ExpectedHash, "文件内容版本已变化")
	}
	lines := splitTextLines(decoded.Text)
	startIndex := min(args.Offset-1, len(lines))
	contentBudget := max(64, w.limits.ResultBytes-len(path)-1400)
	content := ""
	returnedLines := 0
	endLine := 0
	nextOffset, nextByteOffset := 0, 0
	reason := ""
	for index := startIndex; index < len(lines) && returnedLines < args.Limit; index++ {
		if err := ctx.Err(); err != nil {
			return contextFailure(err)
		}
		line := lines[index]
		lineByteOffset := 0
		if index == startIndex {
			lineByteOffset = args.ByteOffset
			if lineByteOffset > len(line) || lineByteOffset < len(line) && !utf8.RuneStart(line[lineByteOffset]) {
				return failureResult(CodeInvalidArguments, "文件读取 byte_offset 无效")
			}
		}
		remaining := line[lineByteOffset:]
		available := contentBudget - len(content)
		if len(remaining) > available {
			prefix := truncateUTF8Bytes(remaining, available)
			if prefix == "" {
				reason = "result_bytes"
				nextOffset, nextByteOffset = index+1, lineByteOffset
				break
			}
			content += prefix
			returnedLines++
			endLine = index + 1
			reason = "result_bytes"
			nextOffset, nextByteOffset = index+1, lineByteOffset+len(prefix)
			break
		}
		content += remaining
		returnedLines++
		endLine = index + 1
		if len(content) >= contentBudget && index+1 < len(lines) {
			reason = "result_bytes"
			nextOffset = index + 2
			break
		}
	}
	if reason == "" && startIndex+returnedLines < len(lines) {
		reason = "line_limit"
		nextOffset = startIndex + returnedLines + 1
	}
	complete := reason == "" && startIndex+returnedLines >= len(lines)
	value := map[string]any{
		"path": path, "content": content, "content_hash": decoded.Hash,
		"offset": args.Offset, "byte_offset": args.ByteOffset,
		"start_line": args.Offset, "end_line": endLine, "returned_lines": returnedLines,
		"total_lines": len(lines), "complete": complete,
	}
	if !complete {
		value["truncation_reason"] = reason
		value["next_offset"] = nextOffset
		value["next_byte_offset"] = nextByteOffset
		value["suggestion"] = "使用 next_offset 和 next_byte_offset 继续读取，并携带 expected_hash"
	}
	for safeResultJSONSize(value) > w.limits.ResultBytes && len(content) > 0 {
		shrink := max(1, safeResultJSONSize(value)-w.limits.ResultBytes+16)
		content = truncateUTF8Bytes(content, max(0, len(content)-shrink))
		value["content"] = content
		value["complete"] = false
		value["truncation_reason"] = "result_bytes"
		value["next_offset"] = args.Offset
		value["next_byte_offset"] = args.ByteOffset + len(content)
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
