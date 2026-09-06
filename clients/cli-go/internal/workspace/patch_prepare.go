package workspace

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func (w *Workspace) preparePatchFile(ctx context.Context, file patchFile, expected string, oldRemaining, candidateRemaining, diffRemaining int64) (*PreparedMutation, int64, Result) {
	if err := w.checkArchiveWritePath(ctx, file.path); err != nil {
		return nil, 0, mutationFailureForSecureError(err, "补丁目标必须是归档树之外的安全普通文件")
	}
	if file.action == "Add" {
		var size int64
		for _, line := range file.added {
			if int64(len(line))+1 > min(w.limits.FileBytes, candidateRemaining)-size {
				if candidateRemaining < w.limits.FileBytes {
					return nil, 0, patchTooLarge(w.limits.PatchBytes)
				}
				return nil, 0, mutationFailure(CodeFileTooLarge, "新增文件超过写入处理上限")
			}
			size += int64(len(line)) + 1
		}
		var content strings.Builder
		content.Grow(int(size))
		for _, line := range file.added {
			content.WriteString(line)
			content.WriteByte('\n')
		}
		item, failure := w.prepareWriteArguments(ctx, writeArguments{Path: file.path, Mode: "create", Content: content.String()}, diffRemaining)
		return item, 0, failure
	}
	var archived *PreparedMutation
	if file.action == "Delete" {
		var failure Result
		archived, failure = w.prepareArchivePath(ctx, file.path)
		if archived == nil {
			return nil, 0, failure
		}
		if archived.archiveEntry.Kind != securefile.EntryFile {
			return nil, 0, mutationFailure(CodeUnsupportedType, "补丁删除只支持普通 UTF-8 文本文件，不支持目录")
		}
	}
	readLimit := min(w.limits.EditFileBytes, oldRemaining)
	snapshot, err := w.root.ReadSnapshot(file.path, max(1, readLimit), false)
	if errors.Is(err, securefile.ErrTooLarge) {
		if oldRemaining < w.limits.EditFileBytes {
			return nil, 0, patchTooLarge(w.limits.PatchBytes)
		}
		return nil, 0, editFileTooLarge(w.limits.EditFileBytes)
	}
	if err != nil {
		return nil, 0, mutationFailureForSecureError(err, "补丁源无法安全读取")
	}
	if int64(len(snapshot.Data)) > oldRemaining {
		return nil, 0, patchTooLarge(w.limits.PatchBytes)
	}
	decoded, err := decodeText(snapshot.Data)
	if err != nil {
		return nil, 0, mutationFailureForError(err, "补丁源必须是普通 UTF-8 文本")
	}
	if !validPatchText(decoded.Text) {
		return nil, 0, mutationFailure(CodeBinaryFile, "补丁源含有不支持的控制字符")
	}
	if decoded.Hash != expected {
		return nil, 0, mutationContentChanged(file.path, expected, "补丁源内容版本已变化")
	}
	if archived != nil {
		entry, err := w.root.InspectArchiveSource(ctx, file.path)
		if err != nil {
			return nil, 0, archiveFailure(ctx, archived, err)
		}
		if entry != *archived.archiveEntry || entry.Identity != snapshot.Identity || entry.Size != int64(len(snapshot.Data)) {
			return nil, 0, mutationContentChanged(file.path, expected, "补丁删除源在预检期间已变化")
		}
		full, err := buildPatchDeleteDiff(ctx, file.path, archived.archivePath, snapshot.Data, diffRemaining)
		if err != nil {
			return nil, 0, completeDiffFailure(err, w.limits.DiffBytes)
		}
		archived.archiveContentHash, archived.fileBytes, archived.fullDiff = decoded.Hash, w.limits.EditFileBytes, full
		return archived, int64(len(snapshot.Data)), Result{}
	}
	candidate, ranges, err := buildPatchCandidate(ctx, snapshot.Data, file.hunks, min(w.limits.EditFileBytes, candidateRemaining))
	if err != nil {
		var operation *operationError
		if errors.As(err, &operation) && operation.code == CodeFileTooLarge {
			if candidateRemaining < w.limits.EditFileBytes {
				return nil, 0, patchTooLarge(w.limits.PatchBytes)
			}
			return nil, 0, editFileTooLarge(w.limits.EditFileBytes)
		}
		if ctx.Err() != nil {
			return nil, 0, mutationContextFailure(ctx.Err())
		}
		return nil, 0, mutationFailureForError(err, err.Error())
	}
	full, err := buildCompleteDiff(ctx, file.path, snapshot.Data, candidate, false, ranges, diffRemaining)
	if err != nil {
		return nil, 0, completeDiffFailure(err, w.limits.DiffBytes)
	}
	preview, truncated, firstLine := buildMutationPreview(file.path, decoded.Text, textWithoutBOM(candidate), "diff", w.limits.MutationPreviewBytes)
	item := &PreparedMutation{
		Presentation: MutationPresentation{Tool: ToolEdit, Operation: "edit", Path: file.path, PreviewKind: "diff", Preview: preview, Truncated: truncated, BaseVersion: decoded.Hash},
		path:         file.path, candidate: candidate, candidateHash: contentHash(candidate), baseVersion: decoded.Hash,
		basePermission: uint32(snapshot.Mode.Perm()), fileBytes: w.limits.EditFileBytes,
		previewHash: hashProjection(preview), fullDiff: full, firstChangeLine: firstLine, replacements: len(file.hunks),
	}
	return item, int64(len(snapshot.Data)), Result{}
}

func buildPatchDeleteDiff(ctx context.Context, path, archivePath string, raw []byte, limit int64) (string, error) {
	out := completeDiffWriter{ctx: ctx, limit: limit}
	out.writeString("--- a/" + path + "\n+++ /dev/null\n")
	old, err := indexCompleteDiffLines(ctx, raw)
	if err != nil {
		return "", err
	}
	next := completeDiffLines{starts: []int{0}}
	out.hunk(old, next, 0, old.count(), 0, 0)
	out.writeString("# Archived to: " + archivePath + " (not permanently deleted)\n")
	if out.err != nil {
		return "", out.err
	}
	return out.text.String(), nil
}
