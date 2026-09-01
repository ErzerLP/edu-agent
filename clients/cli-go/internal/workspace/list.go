package workspace

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type listArguments struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
}

func (w *Workspace) executeList(ctx context.Context, raw string) Result {
	var args listArguments
	if err := decodeArguments(raw, &args); err != nil {
		return resultForError(err, "目录列表参数无效")
	}
	if args.Path == "" {
		args.Path = "."
	}
	path, err := normalizeModelPath(args.Path, true)
	if err != nil || args.Offset < 0 || args.Offset > w.limits.DirectoryScanEntries {
		if err == nil {
			err = argumentError("offset is outside the bounded directory scan")
		}
		return resultForError(err, "目录列表参数无效")
	}
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	entries, skipped, scanComplete, err := w.root.ReadDir(path, w.limits.DirectoryScanEntries)
	if err != nil {
		if isDirectoryError(err) {
			if inspectErr := inspectNonDirectory(w.root, path, w.limits.FileBytes); errors.Is(inspectErr, securefile.ErrLink) {
				return failureResult(CodeLinkNotAllowed, "目录链接不允许访问")
			}
			return failureResult(CodeNotDirectory, "目标不是目录")
		}
		if isPermissionError(err) {
			return failureResult(CodePermissionDenied, "目录不可读取")
		}
		return resultForError(err, "目录无法安全列出")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	start := min(args.Offset, len(entries))
	value := map[string]any{
		"path": path, "offset": start, "entries": []map[string]any{},
		"returned": 0, "skipped": skipped, "complete": true,
	}
	projected := value["entries"].([]map[string]any)
	reason := ""
	for index := start; index < len(entries); index++ {
		if len(projected) >= w.limits.ListEntries {
			reason = "entry_limit"
			break
		}
		entry := entries[index]
		candidate := map[string]any{"path": joinRelative(path, entry.Name), "type": string(entry.Type)}
		projected = append(projected, candidate)
		value["entries"] = projected
		value["returned"] = len(projected)
		if safeResultJSONSize(value) > w.limits.ResultBytes {
			projected = projected[:len(projected)-1]
			value["entries"] = projected
			value["returned"] = len(projected)
			reason = "result_bytes"
			break
		}
	}
	next := start + len(projected)
	complete := scanComplete && next >= len(entries)
	if !scanComplete && reason == "" {
		reason = "scan_entry_limit"
	}
	if !complete {
		value["complete"] = false
		value["truncation_reason"] = reason
		value["suggestion"] = "使用 next_offset 继续，或指定更窄的目录"
		if next < len(entries) && reason != "scan_entry_limit" {
			value["next_offset"] = next
		}
	}
	dataHash := hashProjection(value)
	return Result{
		Value:     value,
		Summary:   fmt.Sprintf("已列出 %s 的 %d 个条目", path, len(projected)),
		Reference: &Reference{Path: path, ContentHash: dataHash, Kind: "directory_listing"},
	}
}
