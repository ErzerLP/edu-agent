package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	pathpkg "path"
	"runtime"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	ArchiveDirectory       = securefile.ArchiveDirectory
	CodeArchiveProtected   = "archive_protected"
	CodeArchiveCrossDevice = "archive_cross_device"
	CodeArchiveUnsupported = "archive_unsupported"
)

type archiveArguments struct {
	Path string `json:"path"`
}

func isArchivePath(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return strings.EqualFold(first, ArchiveDirectory)
}

func (w *Workspace) checkArchiveWritePath(ctx context.Context, path string) error {
	if isArchivePath(path) {
		return securefile.ErrArchiveProtected
	}
	return w.root.CheckArchiveWritePath(ctx, path)
}

func (w *Workspace) prepareArchive(ctx context.Context, raw string) (*PreparedMutation, Result) {
	var args archiveArguments
	if err := decodeArguments(raw, &args); err != nil {
		return nil, mutationFailureForError(err, "归档参数无效")
	}
	source, err := normalizeModelPath(args.Path, false)
	if err != nil {
		return nil, mutationFailureForError(err, "归档源路径无效；不能归档工作区根")
	}
	entry, err := w.root.InspectArchiveSource(ctx, source)
	if err != nil {
		return nil, archiveFailure(ctx, nil, err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, mutationFailure(CodeInternalError, "无法生成唯一归档目标")
	}
	container := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(id[:])
	destination, err := normalizeModelPath(ArchiveDirectory+"/"+container+"/"+source, false)
	if err != nil {
		return nil, mutationFailure(CodeInvalidPath, "归档后的路径超过长度或深度上限")
	}
	preview := fmt.Sprintf("源：%s\n归档到：%s\n类型：%s\n仅整体移动，不永久删除。归档由用户手动恢复或清理。", source, destination, entry.Kind)
	if entry.Kind == securefile.EntryDirectory {
		preview += "\n目录整体归档，不跟随内部链接；这不是子树内容快照。"
	}
	prepared := &PreparedMutation{
		Presentation: MutationPresentation{
			Tool: ToolArchive, Operation: ToolArchive, Path: source,
			ArchivePath: destination, EntryKind: string(entry.Kind),
			PreviewKind: "archive", Preview: preview, BaseVersion: entry.Version,
		},
		path: source, archivePath: destination, archiveEntry: &entry,
		baseVersion: entry.Version, previewHash: hashProjection(preview),
	}
	if len(preview) > w.limits.MutationPreviewBytes || safeResultJSONSize(archiveResult(prepared, PublicationCompleted, false).Value) > w.limits.ResultBytes {
		return nil, mutationFailure(CodeInvalidPath, "归档路径过长，无法在安全预算内完整展示源和目标")
	}
	return prepared, Result{}
}

// commitArchive is called only after CommitMutation consumes the prepared token.
func (w *Workspace) commitArchive(ctx context.Context, prepared *PreparedMutation) Result {
	if prepared.archiveEntry == nil || prepared.archivePath == "" ||
		prepared.Presentation.Tool != ToolArchive || prepared.Presentation.Operation != ToolArchive ||
		prepared.Presentation.Path != prepared.path || prepared.Presentation.ArchivePath != prepared.archivePath ||
		prepared.Presentation.EntryKind != string(prepared.archiveEntry.Kind) ||
		prepared.Presentation.BaseVersion != prepared.baseVersion || prepared.archiveEntry.Version != prepared.baseVersion ||
		prepared.previewHash != hashProjection(prepared.Presentation.Preview) {
		return mutationFailure(CodeInvalidArguments, "归档候选与授权预览不一致")
	}
	release, err := w.queues.acquire(ctx, "file:"+prepared.archiveEntry.Identity)
	if err != nil {
		return mutationContextFailure(err)
	}
	defer release()
	result, err := w.root.Archive(ctx, prepared.path, prepared.archivePath, *prepared.archiveEntry)
	if result.Outcome == securefile.PublishUnknown || errors.Is(err, securefile.ErrOutcomeUnknown) {
		return archiveResult(prepared, PublicationUnknown, result.DirectoriesCreated)
	}
	if err != nil {
		return archiveFailure(ctx, prepared, err)
	}
	if result.Outcome != securefile.PublishCompleted {
		return archiveResult(prepared, PublicationUnknown, result.DirectoriesCreated)
	}
	return archiveResult(prepared, PublicationCompleted, result.DirectoriesCreated)
}

func archiveResult(prepared *PreparedMutation, outcome PublicationOutcome, directoriesCreated bool) Result {
	value := map[string]any{
		"path": prepared.path, "archive_path": prepared.archivePath,
		"entry_type": string(prepared.archiveEntry.Kind), "operation": ToolArchive,
		"publication_outcome": string(outcome), "complete": outcome == PublicationCompleted,
		"preview_kind": "archive", "preview": prepared.Presentation.Preview,
		"manual_cleanup": true, "directories_created": directoriesCreated,
	}
	summary := "已归档 " + prepared.path + " → " + prepared.archivePath
	if outcome == PublicationUnknown {
		summary = "归档结果无法确认；请检查源路径及归档目标，客户端不会自动重试或清理"
		value["error"], value["code"] = CodeOutcomeUnknown, CodeOutcomeUnknown
		value["message"] = summary
		value["suggestion"] = "检查 path 和 archive_path；可能仅创建了归档目录，禁止自动重试或清理"
	}
	return Result{
		Value: value, Summary: summary, Publication: outcome,
		Reference: &Reference{Path: prepared.path, Kind: "archive_" + string(prepared.archiveEntry.Kind), InvalidateObserved: true},
	}
}

func archiveFailure(ctx context.Context, prepared *PreparedMutation, err error) Result {
	if ctx.Err() != nil {
		return mutationContextFailure(ctx.Err())
	}
	result := mutationFailureForSecureError(err, "归档未完成；没有永久删除或覆盖文件")
	value := result.Value.(map[string]any)
	value["operation"] = ToolArchive
	if prepared != nil {
		value["path"], value["archive_path"] = prepared.path, prepared.archivePath
		if errors.Is(err, securefile.ErrChanged) {
			result.Reference = &Reference{Path: prepared.path, Kind: "archive_" + string(prepared.archiveEntry.Kind), InvalidateObserved: true}
		}
	}
	value["suggestion"] = "检查源路径和归档目标；源入口变化时需重新准备并授权，不能覆盖已有归档或改用永久删除"
	return result
}

func (r *Reference) IsArchive() bool {
	return r != nil && (r.Kind == "archive_file" || r.Kind == "archive_directory")
}

func archiveAffectsReference(source string, directory bool, previous *Reference) bool {
	if previous.IsArchive() {
		return false // Operation receipts remain historical facts, not file contents.
	}
	path := previous.Path
	if runtime.GOOS == "windows" {
		source, path = strings.ToLower(source), strings.ToLower(path)
	}
	insideSource := path == source || directory && strings.HasPrefix(path, source+"/")
	switch previous.Kind {
	case "file":
		return insideSource
	case "directory_listing":
		return insideSource || path == pathpkg.Dir(source) || path == "."
	case "search_result":
		return insideSource || path == "." || strings.HasPrefix(source, path+"/")
	default:
		return false
	}
}
