package workspace

import (
	"context"
	"fmt"
	"strings"
)

const (
	CodeInvalidPatch      = "invalid_patch"
	CodePatchTooLarge     = "patch_too_large"
	CodePatchPathConflict = "patch_path_conflict"
)

// IsPatch identifies the private frozen plan, not mutable presentation fields.
func (p *PreparedMutation) IsPatch() bool { return p != nil && len(p.patchItems) > 0 }

// ClaimPatchItems consumes the outer token even on validation failure. The
// caller must authorize first, then WAL/commit/settle each child in order and
// stop on non-completion. This is deliberately not a multi-file transaction.
func (p *PreparedMutation) ClaimPatchItems() ([]*PreparedMutation, error) {
	if !p.IsPatch() {
		return nil, argumentError("candidate is not a patch plan")
	}
	p.commitMu.Lock()
	defer p.commitMu.Unlock()
	if p.committed {
		return nil, argumentError("patch plan was already claimed")
	}
	p.committed = true
	if p.Presentation != p.patchPresentation || p.previewHash != hashProjection(p.Presentation.Preview) {
		return nil, argumentError("patch plan differs from its authorization presentation")
	}
	return append([]*PreparedMutation(nil), p.patchItems...), nil
}

func (w *Workspace) preparePatch(ctx context.Context, raw string) (*PreparedMutation, Result) {
	args, err := decodePatchArguments(raw)
	if err != nil {
		return nil, mutationFailureForError(err, "补丁参数必须是严格 JSON 对象且不含重复键或 null")
	}
	files, err := parsePatch(args.Patch)
	if err != nil {
		return nil, mutationFailureForError(err, err.Error())
	}
	if err := validatePatchPaths(files, args.ExpectedHashes); err != nil {
		return nil, mutationFailureForError(err, err.Error())
	}
	out := completeDiffWriter{ctx: ctx, limit: w.limits.DiffBytes}
	items := make([]*PreparedMutation, 0, len(files))
	var oldBytes, candidateBytes int64
	var summary strings.Builder
	fmt.Fprintf(&summary, "补丁：%d 个文件；全部预检，一次授权后按顺序逐文件发布；无自动回滚。\n", len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, mutationContextFailure(err)
		}
		remainingDiff := w.limits.DiffBytes - int64(out.text.Len())
		if remainingDiff < 1 {
			return nil, completeDiffFailure(errCompleteDiffTooLarge, w.limits.DiffBytes)
		}
		item, originalBytes, failure := w.preparePatchFile(ctx, file, args.ExpectedHashes[file.path],
			w.limits.PatchBytes-oldBytes, w.limits.PatchBytes-candidateBytes, remainingDiff)
		if item == nil {
			return nil, failure
		}
		oldBytes += originalBytes
		candidateBytes += int64(len(item.candidate))
		out.writeString(item.FullDiff())
		if out.err != nil {
			return nil, completeDiffFailure(out.err, w.limits.DiffBytes)
		}
		fmt.Fprintf(&summary, "%s %s", file.action, file.path)
		if item.archivePath != "" {
			fmt.Fprintf(&summary, " → %s", item.archivePath)
		}
		summary.WriteByte('\n')
		items = append(items, item)
	}
	preview := summary.String()
	truncated := len(preview) > w.limits.MutationPreviewBytes
	if truncated {
		const marker = "\n…计划摘要已截断；完整动作、路径和归档位置见完整差异。"
		preview = truncateUTF8Bytes(preview, w.limits.MutationPreviewBytes-len(marker)) + marker
	}
	presentation := MutationPresentation{Tool: ToolPatch, Operation: ToolPatch, Path: ".", PreviewKind: "diff", Preview: preview, Truncated: truncated}
	if err := ctx.Err(); err != nil {
		return nil, mutationContextFailure(err)
	}
	return &PreparedMutation{Presentation: presentation, path: ".", patchPresentation: presentation,
		patchItems: items, previewHash: hashProjection(preview), fullDiff: out.text.String()}, Result{}
}

func patchTooLarge(limit int64) Result {
	result := mutationFailure(CodePatchTooLarge, fmt.Sprintf("补丁原文总量或候选总量超过 %d 字节处理上限，未准备任何发布", limit))
	result.Value.(map[string]any)["patch_byte_limit"] = limit
	return result
}
