package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type statArguments struct {
	Path string `json:"path"`
	Hash bool   `json:"hash"`
}

func (w *Workspace) executeStat(ctx context.Context, raw string) Result {
	var args statArguments
	if err := decodeArguments(raw, &args); err != nil {
		return resultForError(err, "入口检查参数无效")
	}
	path, err := normalizeModelPath(args.Path, true)
	if err != nil {
		return resultForError(err, "入口检查路径无效")
	}
	publishProgress(ctx, Progress{Tool: ToolStat, Path: path})
	entry, err := w.root.Stat(ctx, path)
	if ctx.Err() != nil {
		return contextFailure(ctx.Err())
	}
	value := map[string]any{"path": path, "exists": false, "complete": true}
	if err != nil && !errors.Is(err, securefile.ErrNotFound) {
		return resultForError(err, "入口无法安全检查")
	}
	if err == nil {
		value["exists"] = true
		value["entry_type"] = string(entry.Kind)
		value["mtime"] = entry.ModTime.Format(time.RFC3339Nano)
		value["entry_version"] = entry.Version
		if entry.Kind == securefile.EntryFile {
			value["size"] = entry.Size
		}
		if args.Hash {
			if entry.Kind != securefile.EntryFile {
				result := failureResult(CodeUnsupportedType, "只有普通文件可计算内容 hash")
				result.Reference = &Reference{Path: path, Kind: "entry_metadata", InvalidateObserved: true}
				return result
			}
			hash, err := w.root.HashEntry(ctx, path, entry, min(w.limits.FileBytes, 1<<20))
			if ctx.Err() != nil {
				return contextFailure(ctx.Err())
			}
			if err != nil {
				result := resultForError(err, "文件内容 hash 无法安全计算")
				if errors.Is(err, securefile.ErrTooLarge) {
					result.Value.(map[string]any)["suggestion"] = "使用 hash=false 仅检查元数据；内容 hash 上限为 1 MiB"
				}
				// Refusing a hash must not leave older entry/content evidence
				// current after observing an incompatible type, size or version.
				result.Reference = &Reference{Path: path, Kind: "entry_metadata", InvalidateObserved: true}
				return result
			}
			value["content_hash"] = hash
		}
	}
	if safeResultJSONSize(value) > w.limits.ResultBytes {
		return failureResult(CodeInvalidPath, "入口路径过长，无法在结果预算内完整展示")
	}
	// Keep metadata in the evidence digest even when raw bytes were hashed:
	// chmod/touch must supersede metadata without pretending content changed.
	return Result{Value: value, Summary: "已检查 " + path, Reference: &Reference{Path: path, Kind: "entry_metadata", ContentHash: hashProjection(value)}}
}
