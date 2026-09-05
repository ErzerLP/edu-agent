package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type writeArguments struct {
	Path         string `json:"path"`
	Mode         string `json:"mode"`
	Content      string `json:"content"`
	ExpectedHash string `json:"expected_hash"`
}

type editArguments struct {
	Path         string            `json:"path"`
	ExpectedHash string            `json:"expected_hash"`
	Edits        []editReplacement `json:"edits"`
}

type editReplacement struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

type replacementRange struct {
	start int
	end   int
	text  string
}

func (w *Workspace) PrepareMutation(ctx context.Context, toolName, rawArguments string) (*PreparedMutation, Result) {
	if w == nil || w.root == nil {
		return nil, mutationFailure(CodeWorkspaceUnavailable, "工作区不可用")
	}
	if err := ctx.Err(); err != nil {
		return nil, mutationContextFailure(err)
	}
	switch toolName {
	case ToolMove:
		return w.prepareMove(ctx, rawArguments)
	case ToolCopy:
		return w.prepareCopy(ctx, rawArguments)
	case ToolMkdir:
		return w.prepareMkdir(ctx, rawArguments)
	case ToolArchive:
		return w.prepareArchive(ctx, rawArguments)
	case ToolWrite:
		return w.prepareWrite(ctx, rawArguments)
	case ToolEdit:
		return w.prepareEdit(ctx, rawArguments)
	default:
		return nil, mutationFailure(CodeInvalidArguments, "未知工作区修改工具")
	}
}

func (w *Workspace) prepareWrite(ctx context.Context, raw string) (*PreparedMutation, Result) {
	var args writeArguments
	if err := decodeArguments(raw, &args); err != nil {
		return nil, mutationFailureForError(err, "文件写入参数无效")
	}
	path, err := normalizeModelPath(args.Path, false)
	if err != nil {
		return nil, mutationFailureForError(err, "文件写入路径无效")
	}
	if err := w.checkArchiveWritePath(ctx, path); err != nil {
		return nil, mutationFailureForSecureError(err, "归档目录仅允许专用归档工具新增条目")
	}
	if args.Mode != "create" && args.Mode != "replace" {
		return nil, mutationFailure(CodeInvalidArguments, "文件写入模式无效")
	}
	if args.Mode == "create" && args.ExpectedHash != "" || args.Mode == "replace" && !validContentHash(args.ExpectedHash) {
		return nil, mutationFailure(CodeInvalidArguments, "文件写入版本前置条件无效")
	}
	if err := validateMutationText(args.Content, w.limits.FileBytes); err != nil {
		return nil, mutationFailureForError(err, "文件写入内容无效")
	}
	if err := ctx.Err(); err != nil {
		return nil, mutationContextFailure(err)
	}

	operation := "write_create"
	previewKind := "content"
	baseText := ""
	candidate := []byte(args.Content)
	permission := os.FileMode(0o644)
	baseVersion := ""
	create := args.Mode == "create"
	if create {
		_, readErr := w.root.ReadSnapshot(path, w.limits.FileBytes, false)
		switch {
		case readErr == nil, errors.Is(readErr, securefile.ErrNotRegular):
			return nil, mutationFailure(CodeAlreadyExists, "文件目标已经存在")
		case errors.Is(readErr, securefile.ErrNotFound):
		case errors.Is(readErr, securefile.ErrLink):
			return nil, mutationFailure(CodeLinkNotAllowed, "文件链接不允许写入")
		case isPermissionError(readErr):
			return nil, mutationFailure(CodePermissionDenied, "文件目标不可写入")
		default:
			return nil, mutationFailureForError(readErr, "文件目标无法安全检查")
		}
	} else {
		operation = "write_replace"
		previewKind = "diff"
		snapshot, readErr := w.root.ReadSnapshot(path, w.limits.FileBytes, false)
		if readErr != nil {
			return nil, mutationFailureForSecureError(readErr, "文件无法安全替换")
		}
		decoded, decodeErr := decodeText(snapshot.Data)
		if decodeErr != nil {
			return nil, mutationFailureForError(decodeErr, "文件不是可替换的 UTF-8 文本")
		}
		if decoded.Hash != args.ExpectedHash {
			return nil, mutationContentChanged(path, args.ExpectedHash, "文件内容版本已变化")
		}
		baseText = decoded.Text
		baseVersion = decoded.Hash
		permission = snapshot.Mode.Perm()
		candidate = encodeForExistingFile(args.Content, snapshot.Data)
		if bytes.Equal(candidate, snapshot.Data) {
			return nil, mutationFailure(CodeNoChanges, "文件内容没有变化")
		}
	}
	if int64(len(candidate)) > w.limits.FileBytes {
		return nil, mutationFailure(CodeFileTooLarge, "文件候选内容超过安全上限")
	}
	candidateText := textWithoutBOM(candidate)
	preview, truncated, firstLine := buildMutationPreview(path, baseText, candidateText, previewKind, w.limits.MutationPreviewBytes)
	prepared := &PreparedMutation{
		Presentation: MutationPresentation{
			Tool: ToolWrite, Operation: operation, Path: path, PreviewKind: previewKind,
			Preview: preview, Truncated: truncated, BaseVersion: baseVersion,
		},
		path: path, candidate: append([]byte(nil), candidate...), candidateHash: contentHash(candidate),
		baseVersion: baseVersion, basePermission: uint32(permission.Perm()), create: create,
		previewHash: hashProjection(preview), firstChangeLine: firstLine,
	}
	return prepared, Result{}
}

func (w *Workspace) prepareEdit(ctx context.Context, raw string) (*PreparedMutation, Result) {
	var args editArguments
	if err := decodeArguments(raw, &args); err != nil {
		return nil, mutationFailureForError(err, "文件编辑参数无效")
	}
	path, err := normalizeModelPath(args.Path, false)
	if err != nil {
		return nil, mutationFailureForError(err, "文件编辑路径无效")
	}
	if err := w.checkArchiveWritePath(ctx, path); err != nil {
		return nil, mutationFailureForSecureError(err, "归档目录内容不能编辑，请由用户手动管理")
	}
	if !validContentHash(args.ExpectedHash) || len(args.Edits) < 1 || len(args.Edits) > w.limits.EditReplacements {
		return nil, mutationFailure(CodeInvalidArguments, "文件编辑版本或替换数量无效")
	}
	if err := ctx.Err(); err != nil {
		return nil, mutationContextFailure(err)
	}
	snapshot, readErr := w.root.ReadSnapshot(path, w.limits.FileBytes, false)
	if readErr != nil {
		return nil, mutationFailureForSecureError(readErr, "文件无法安全编辑")
	}
	decoded, decodeErr := decodeText(snapshot.Data)
	if decodeErr != nil {
		return nil, mutationFailureForError(decodeErr, "文件不是可编辑的 UTF-8 文本")
	}
	if decoded.Hash != args.ExpectedHash {
		return nil, mutationContentChanged(path, args.ExpectedHash, "文件内容版本已变化")
	}
	newline := dominantNewline(snapshot.Data)
	ranges := make([]replacementRange, 0, len(args.Edits))
	for _, replacement := range args.Edits {
		if replacement.OldText == "" || !validMutationFragment(replacement.OldText) || !validMutationFragment(replacement.NewText) {
			return nil, mutationFailure(CodeInvalidArguments, "文件编辑替换文本无效")
		}
		if replacement.OldText == replacement.NewText {
			return nil, mutationFailure(CodeNoChanges, "文件编辑不包含实际变化")
		}
		start, matches := uniqueReplacementIndex(decoded.Text, replacement.OldText)
		if matches == 0 {
			return nil, mutationFailure(CodeReplacementMissing, "文件编辑目标文本不存在")
		}
		if matches != 1 {
			return nil, mutationFailure(CodeReplacementNotUnique, "文件编辑目标文本不唯一")
		}
		ranges = append(ranges, replacementRange{start: start, end: start + len(replacement.OldText), text: normalizeNewlines(replacement.NewText, newline)})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	for index := 1; index < len(ranges); index++ {
		if ranges[index].start < ranges[index-1].end {
			return nil, mutationFailure(CodeReplacementOverlap, "文件编辑替换区域重叠")
		}
	}
	candidateText := decoded.Text
	for index := len(ranges) - 1; index >= 0; index-- {
		current := ranges[index]
		candidateText = candidateText[:current.start] + current.text + candidateText[current.end:]
	}
	candidate := withOriginalBOM(candidateText, snapshot.Data)
	if bytes.Equal(candidate, snapshot.Data) {
		return nil, mutationFailure(CodeNoChanges, "文件编辑不包含实际变化")
	}
	if int64(len(candidate)) > w.limits.FileBytes {
		return nil, mutationFailure(CodeFileTooLarge, "文件编辑候选内容超过安全上限")
	}
	preview, truncated, firstLine := buildMutationPreview(path, decoded.Text, candidateText, "diff", w.limits.MutationPreviewBytes)
	prepared := &PreparedMutation{
		Presentation: MutationPresentation{
			Tool: ToolEdit, Operation: "edit", Path: path, PreviewKind: "diff",
			Preview: preview, Truncated: truncated, BaseVersion: decoded.Hash,
		},
		path: path, candidate: append([]byte(nil), candidate...), candidateHash: contentHash(candidate),
		baseVersion: decoded.Hash, basePermission: uint32(snapshot.Mode.Perm()),
		previewHash: hashProjection(preview), firstChangeLine: firstLine, replacements: len(args.Edits),
	}
	return prepared, Result{}
}

func (w *Workspace) CommitMutation(ctx context.Context, prepared *PreparedMutation) (result Result) {
	defer func() {
		if prepared != nil && (result.Publication == PublicationCompleted || result.Publication == PublicationUnknown) && result.Effect == nil {
			effect := prepared.FileEffect()
			if result.Publication == PublicationCompleted && result.Reference != nil && effect.Operation != ToolArchive {
				effect.Target.Version = result.Reference.ContentHash
			}
			result.Effect = &effect
			if value, ok := result.Value.(map[string]any); ok {
				value["file_effect"] = effect
			}
		}
	}()
	if w == nil || w.root == nil {
		return mutationFailure(CodeWorkspaceUnavailable, "工作区不可用")
	}
	if prepared == nil || prepared.path == "" || !IsMutationTool(prepared.Presentation.Tool) {
		return mutationFailure(CodeInvalidArguments, "文件修改候选无效")
	}
	prepared.commitMu.Lock()
	if prepared.committed {
		prepared.commitMu.Unlock()
		return mutationFailure(CodeInvalidArguments, "文件修改候选已经处理")
	}
	prepared.committed = true
	prepared.commitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return mutationContextFailure(err)
	}
	if prepared.Presentation.Tool == ToolMove {
		return w.commitMove(ctx, prepared)
	}
	if prepared.Presentation.Tool == ToolCopy {
		return w.commitCopy(ctx, prepared)
	}
	if prepared.Presentation.Tool == ToolMkdir {
		return w.commitMkdir(ctx, prepared)
	}
	if prepared.Presentation.Tool == ToolArchive {
		return w.commitArchive(ctx, prepared)
	}
	if err := w.checkArchiveWritePath(ctx, prepared.path); err != nil {
		return mutationFailureForSecureError(err, "归档目录不允许普通文件写入")
	}
	queueKey := "create:" + strings.ToLower(prepared.path)
	if !prepared.create {
		identitySnapshot, readErr := w.root.ReadSnapshot(prepared.path, w.limits.FileBytes, false)
		if readErr != nil {
			return mutationFailureForSecureError(readErr, "文件身份无法安全检查")
		}
		queueKey = "file:" + identitySnapshot.Identity
	}
	release, err := w.queues.acquire(ctx, queueKey)
	if err != nil {
		return mutationContextFailure(err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return mutationContextFailure(err)
	}
	if prepared.candidateHash != contentHash(prepared.candidate) || prepared.previewHash != hashProjection(prepared.Presentation.Preview) {
		return mutationFailure(CodeInternalError, "文件修改候选校验失败")
	}
	permission := os.FileMode(prepared.basePermission)
	if prepared.create {
		_, readErr := w.root.ReadSnapshot(prepared.path, w.limits.FileBytes, false)
		switch {
		case readErr == nil, errors.Is(readErr, securefile.ErrNotRegular):
			return mutationFailure(CodeAlreadyExists, "文件目标已经存在")
		case errors.Is(readErr, securefile.ErrNotFound):
		case errors.Is(readErr, securefile.ErrLink):
			return mutationFailure(CodeLinkNotAllowed, "文件链接不允许写入")
		case isPermissionError(readErr):
			return mutationFailure(CodePermissionDenied, "文件目标不可写入")
		default:
			return mutationFailureForSecureError(readErr, "文件目标无法安全检查")
		}
	} else {
		snapshot, readErr := w.root.ReadSnapshot(prepared.path, w.limits.FileBytes, false)
		if readErr != nil {
			return mutationFailureForSecureError(readErr, "文件内容版本无法重新验证")
		}
		decoded, decodeErr := decodeText(snapshot.Data)
		if decodeErr != nil {
			return mutationFailureForError(decodeErr, "文件内容版本无法重新验证")
		}
		if decoded.Hash != prepared.baseVersion {
			return mutationContentChanged(prepared.path, prepared.baseVersion, "文件内容版本已变化")
		}
		permission = snapshot.Mode.Perm()
	}
	if err := ctx.Err(); err != nil {
		return mutationContextFailure(err)
	}
	mode := securefile.PublishReplace
	if prepared.create {
		mode = securefile.PublishCreate
	}
	publish, publishErr := w.root.Publish(ctx, prepared.path, prepared.candidate, securefile.PublishOptions{
		Mode: mode, Permission: permission, ExpectedHash: prepared.baseVersion, ExpectedLimit: w.limits.FileBytes,
		ProtectArchive: true,
	})
	if publish.Outcome == securefile.PublishUnknown || errors.Is(publishErr, securefile.ErrOutcomeUnknown) {
		return mutationOutcomeUnknown(prepared)
	}
	if publishErr != nil {
		if errors.Is(publishErr, context.Canceled) || errors.Is(publishErr, context.DeadlineExceeded) {
			return mutationContextFailure(publishErr)
		}
		return mutationFailureForSecureError(publishErr, "文件修改未发布")
	}
	if publish.Outcome != securefile.PublishCompleted {
		return mutationFailure(CodeInternalError, "文件修改未完成")
	}
	return mutationSuccess(prepared)
}

func MutationDenied(prepared *PreparedMutation) Result {
	path := ""
	operation := "mutation"
	if prepared != nil {
		path = prepared.Presentation.Path
		operation = prepared.Presentation.Operation
	}
	result := failureResult(CodeAuthorizationDenied, "用户拒绝了文件修改")
	result.Publication = PublicationUnchanged
	if value, ok := result.Value.(map[string]any); ok {
		value["path"] = path
		if prepared != nil && prepared.movePlan != nil {
			value["source"], value["destination"], value["entry_type"] = path, prepared.movePlan.Destination(), string(prepared.movePlan.Kind())
		}
		if prepared != nil && prepared.copyPlan != nil {
			value["source"] = path
			value["destination"] = prepared.copyPlan.Destination()
			value["source_unchanged"] = true
		}
		value["operation"] = operation
		value["publication_outcome"] = string(PublicationUnchanged)
	}
	return result
}

func mutationContentChanged(path, expectedHash, summary string) Result {
	result := mutationFailure(CodeContentChanged, summary)
	result.Reference = &Reference{Path: path, ContentHash: expectedHash, Kind: "file", InvalidateObserved: true}
	return result
}

func mutationSuccess(prepared *PreparedMutation) Result {
	value := map[string]any{
		"path": prepared.path, "operation": prepared.Presentation.Operation,
		"bytes": len(prepared.candidate), "content_hash": prepared.candidateHash,
		"complete": true, "publication_outcome": string(PublicationCompleted),
		"preview_kind": prepared.Presentation.PreviewKind, "preview": prepared.Presentation.Preview,
		"preview_truncated": prepared.Presentation.Truncated, "first_changed_line": prepared.firstChangeLine,
	}
	if prepared.baseVersion != "" {
		value["base_hash"] = prepared.baseVersion
	}
	if prepared.replacements > 0 {
		value["replacements"] = prepared.replacements
	}
	return Result{
		Value: value, Summary: fmt.Sprintf("已完成 %s：%s", prepared.Presentation.Operation, prepared.path),
		Reference:   &Reference{Path: prepared.path, ContentHash: prepared.candidateHash, Kind: "file"},
		Publication: PublicationCompleted,
	}
}

func mutationOutcomeUnknown(prepared *PreparedMutation) Result {
	result := failureResult(CodeOutcomeUnknown, "文件发布结果无法确认")
	result.Reference = &Reference{Path: prepared.path, Kind: "file", InvalidateObserved: true}
	result.Publication = PublicationUnknown
	if value, ok := result.Value.(map[string]any); ok {
		value["path"] = prepared.path
		value["operation"] = prepared.Presentation.Operation
		value["publication_outcome"] = string(PublicationUnknown)
	}
	return result
}

func mutationFailure(code, summary string) Result {
	result := failureResult(code, summary)
	result.Publication = PublicationUnchanged
	if value, ok := result.Value.(map[string]any); ok {
		value["publication_outcome"] = string(PublicationUnchanged)
	}
	return result
}

func mutationFailureForError(err error, summary string) Result {
	result := resultForError(err, summary)
	result.Publication = PublicationUnchanged
	if value, ok := result.Value.(map[string]any); ok {
		value["publication_outcome"] = string(PublicationUnchanged)
	}
	return result
}

func mutationFailureForSecureError(err error, summary string) Result {
	if errors.Is(err, securefile.ErrLink) {
		return mutationFailure(CodeLinkNotAllowed, summary)
	}
	if isPermissionError(err) {
		return mutationFailure(CodePermissionDenied, summary)
	}
	return mutationFailure(codeForSecureError(err), summary)
}

func mutationContextFailure(err error) Result {
	result := contextFailure(err)
	result.Publication = PublicationUnchanged
	if value, ok := result.Value.(map[string]any); ok {
		value["publication_outcome"] = string(PublicationUnchanged)
	}
	return result
}

func validateMutationText(value string, limit int64) error {
	if !utf8.ValidString(value) {
		return operationFailure(CodeInvalidUTF8, "mutation content is not valid UTF-8")
	}
	if looksBinary([]byte(value)) {
		return operationFailure(CodeBinaryFile, "mutation content appears to be binary")
	}
	if int64(len(value)) > limit {
		return operationFailure(CodeFileTooLarge, "mutation content exceeds the file limit")
	}
	return nil
}

func uniqueReplacementIndex(value, oldText string) (int, int) {
	first := strings.Index(value, oldText)
	if first < 0 {
		return -1, 0
	}
	if strings.Index(value[first+1:], oldText) >= 0 {
		return first, 2
	}
	return first, 1
}

func validMutationFragment(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if current == '\n' || current == '\r' || current == '\t' || current == '\f' {
			continue
		}
		if current == 0 || unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func contentHash(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func textWithoutBOM(data []byte) string {
	return string(bytes.TrimPrefix(data, utf8BOM))
}

func withOriginalBOM(text string, original []byte) []byte {
	data := []byte(text)
	if bytes.HasPrefix(original, utf8BOM) {
		return append(append([]byte(nil), utf8BOM...), data...)
	}
	return data
}

func encodeForExistingFile(text string, original []byte) []byte {
	text = strings.TrimPrefix(text, "\ufeff")
	text = normalizeNewlines(text, dominantNewline(original))
	return withOriginalBOM(text, original)
}

func dominantNewline(data []byte) string {
	text := string(bytes.TrimPrefix(data, utf8BOM))
	crlf := strings.Count(text, "\r\n")
	lf := strings.Count(text, "\n") - crlf
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

func normalizeNewlines(value, newline string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	if newline == "\r\n" {
		value = strings.ReplaceAll(value, "\n", "\r\n")
	}
	return value
}
