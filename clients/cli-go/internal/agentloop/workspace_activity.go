package agentloop

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

const maxFileActivityPreviewBytes = 6 << 10

func initialWorkspaceFileActivity(tool, rawArguments string) *FileActivityDetail {
	progress, ok := workspace.InitialProgress(tool, rawArguments)
	if !ok {
		return nil
	}
	return fileActivityDetailFromProgress(progress)
}

func workspaceToolContextFailureEvent(id, tool string, err error) Event {
	code := workspace.CodeCancelled
	summary := "工作区文件操作已取消"
	if errors.Is(err, context.DeadlineExceeded) {
		code = workspace.CodeTimeout
		summary = "工作区文件操作已超时"
	}
	return Event{ID: id, Tool: tool, Summary: summary, Status: EventFailed, Detail: code}
}

func fileActivityDetailFromProgress(progress workspace.Progress) *FileActivityDetail {
	path := activityPathForTool(progress.Tool, progress.Path)
	if path == "" {
		return nil
	}
	detail := &FileActivityDetail{Path: path, DestinationPath: activityPathForTool(workspace.ToolCopy, progress.DestinationPath), Operation: safeActivityToken(progress.Operation, 64)}
	switch progress.Tool {
	case workspace.ToolList, workspace.ToolFind:
		detail.Returned, detail.HasReturned = progress.Returned, true
	case workspace.ToolRead:
		detail.Bytes, detail.HasBytes = progress.Bytes, true
		if progress.StartLine > 0 {
			detail.StartLine, detail.EndLine, detail.HasRange = progress.StartLine, progress.EndLine, true
		}
	case workspace.ToolSearch:
		detail.ScannedFiles, detail.ScannedBytes, detail.HasScanned = progress.ScannedFiles, progress.ScannedBytes, true
		detail.Matches, detail.HasMatches = progress.Matches, true
	}
	detail.TruncationReason = safeActivityToken(progress.TruncationReason, 64)
	if progress.HasContinuation {
		detail.NextOffset, detail.NextByteOffset, detail.HasContinuation = progress.NextOffset, progress.NextByteOffset, true
	}
	return detail
}

func fileActivityDetailFromResult(tool string, result workspace.Result) *FileActivityDetail {
	value, ok := result.Value.(map[string]any)
	if !ok {
		return nil
	}
	detail := &FileActivityDetail{}
	if result.Effect != nil && result.Effect.Operation == workspace.ToolMkdir {
		detail.CreationAnchor = result.Effect.Directories.Anchor
		detail.PlannedDirectories = result.Effect.Directories.Count
		detail.CreatedDirectories = result.Effect.Directories.Created
		detail.HasDirectoryPlan = true
	}
	detail.Path = activityPathForTool(tool, stringValue(value["path"]))
	detail.DestinationPath = activityPathForTool(workspace.ToolCopy, stringValue(value["destination"]))
	detail.ArchivePath = safeActivityWorkspacePath(stringValue(value["archive_path"]))
	detail.EntryKind = safeActivityToken(stringValue(value["entry_type"]), 32)
	detail.Operation = safeActivityToken(stringValue(value["operation"]), 64)
	detail.PreviewKind = safeActivityToken(stringValue(value["preview_kind"]), 32)
	detail.Preview = safeActivityPreview(stringValue(value["preview"]))
	detail.PreviewTruncated, _ = value["preview_truncated"].(bool)
	detail.FirstChangedLine, _ = activityInt(value["first_changed_line"])
	detail.PublicationOutcome = safeActivityToken(stringValue(value["publication_outcome"]), 32)
	if detail.PublicationOutcome == "" && result.Publication != "" {
		detail.PublicationOutcome = string(result.Publication)
	}
	if returned, exists := activityInt(value["returned"]); exists {
		detail.Returned, detail.HasReturned = returned, true
	}
	if start, exists := activityInt(value["start_line"]); exists && start > 0 {
		detail.StartLine, detail.HasRange = start, true
		detail.EndLine, _ = activityInt(value["end_line"])
	}
	if content, exists := value["content"].(string); exists {
		detail.Bytes, detail.HasBytes = int64(len(content)), true
	}
	if scannedFiles, exists := activityInt(value["scanned_files"]); exists {
		detail.ScannedFiles, detail.HasScanned = scannedFiles, true
		if scannedBytes, ok := activityInt64(value["scanned_bytes"]); ok {
			detail.ScannedBytes = scannedBytes
		}
	}
	if tool == workspace.ToolSearch {
		// Count output reports matching lines; files output's returned value
		// is the number of listed files, never an inferred body occurrence.
		matches, exists := activityInt(value["matched_lines"])
		if !exists {
			matches, exists = activityInt(value["returned"])
		}
		if exists {
			detail.Matches, detail.HasMatches = matches, true
		}
	}
	detail.TruncationReason = safeActivityToken(stringValue(value["truncation_reason"]), 64)
	if nextOffset, exists := activityInt(value["next_offset"]); exists {
		detail.NextOffset, detail.HasContinuation = nextOffset, true
		if nextByteOffset, ok := activityInt(value["next_byte_offset"]); ok {
			detail.NextByteOffset = nextByteOffset
		}
	}
	if detail.Path == "" && detail.Operation == "" && detail.Preview == "" && detail.PublicationOutcome == "" &&
		!detail.HasReturned && !detail.HasRange && !detail.HasBytes && !detail.HasScanned && !detail.HasMatches && detail.TruncationReason == "" {
		return nil
	}
	return detail
}

func fileActivityDetailFromPrepared(prepared *workspace.PreparedMutation) *FileActivityDetail {
	if prepared == nil {
		return nil
	}
	presentation := prepared.Presentation
	return &FileActivityDetail{
		Path: activityPathForTool(presentation.Tool, presentation.Path), Operation: safeActivityToken(presentation.Operation, 64),
		DestinationPath: activityPathForTool(workspace.ToolCopy, presentation.DestinationPath),
		ArchivePath:     safeActivityWorkspacePath(presentation.ArchivePath), EntryKind: safeActivityToken(presentation.EntryKind, 32),
		PreviewKind: safeActivityToken(presentation.PreviewKind, 32), Preview: safeActivityPreview(presentation.Preview),
		PreviewTruncated: presentation.Truncated,
	}
}

func mergePreparedFileActivity(detail *FileActivityDetail, prepared *workspace.PreparedMutation) *FileActivityDetail {
	preparedDetail := fileActivityDetailFromPrepared(prepared)
	if detail == nil {
		return preparedDetail
	}
	if preparedDetail == nil {
		return detail
	}
	if detail.Path == "" {
		detail.Path = preparedDetail.Path
	}
	if detail.Operation == "" {
		detail.Operation = preparedDetail.Operation
	}
	if detail.DestinationPath == "" {
		detail.DestinationPath = preparedDetail.DestinationPath
	}
	if detail.ArchivePath == "" {
		detail.ArchivePath = preparedDetail.ArchivePath
		detail.EntryKind = preparedDetail.EntryKind
	}
	if detail.PreviewKind == "" {
		detail.PreviewKind = preparedDetail.PreviewKind
	}
	if detail.Preview == "" {
		detail.Preview = preparedDetail.Preview
	}
	detail.PreviewTruncated = detail.PreviewTruncated || preparedDetail.PreviewTruncated
	return detail
}

func mergeFileActivityDetail(detail, fallback *FileActivityDetail) *FileActivityDetail {
	if detail == nil {
		if fallback == nil {
			return nil
		}
		copy := *fallback
		return &copy
	}
	if fallback == nil {
		return detail
	}
	if detail.Path == "" {
		detail.Path = fallback.Path
	}
	if detail.Operation == "" {
		detail.Operation = fallback.Operation
	}
	if detail.DestinationPath == "" {
		detail.DestinationPath = fallback.DestinationPath
	}
	if detail.ArchivePath == "" {
		detail.ArchivePath = fallback.ArchivePath
	}
	if detail.EntryKind == "" {
		detail.EntryKind = fallback.EntryKind
	}
	if !detail.HasReturned && fallback.HasReturned {
		detail.Returned, detail.HasReturned = fallback.Returned, true
	}
	if !detail.HasRange && fallback.HasRange {
		detail.StartLine, detail.EndLine, detail.HasRange = fallback.StartLine, fallback.EndLine, true
	}
	if !detail.HasBytes && fallback.HasBytes {
		detail.Bytes, detail.HasBytes = fallback.Bytes, true
	}
	if !detail.HasScanned && fallback.HasScanned {
		detail.ScannedFiles, detail.ScannedBytes, detail.HasScanned = fallback.ScannedFiles, fallback.ScannedBytes, true
	}
	if !detail.HasMatches && fallback.HasMatches {
		detail.Matches, detail.HasMatches = fallback.Matches, true
	}
	return detail
}

func workspaceProgressSummary(tool string, detail *FileActivityDetail) string {
	if detail == nil {
		return toolRunningSummary(tool)
	}
	switch tool {
	case workspace.ToolRead:
		if detail.HasRange && detail.EndLine >= detail.StartLine {
			return "正在读取 " + detail.Path + " 第 " + intText(detail.StartLine) + "-" + intText(detail.EndLine) + " 行"
		}
		if detail.HasRange {
			return "正在读取 " + detail.Path + " 第 " + intText(detail.StartLine) + " 行起"
		}
		return "正在读取 " + detail.Path
	case workspace.ToolStat:
		return "正在检查 " + detail.Path
	case workspace.ToolSearch:
		return "正在搜索 " + detail.Path + "：已扫描 " + intText(detail.ScannedFiles) + " 个文件"
	case workspace.ToolMove:
		return "正在准备移动 " + detail.Path + " → " + detail.DestinationPath
	case workspace.ToolCopy:
		return "正在准备复制 " + detail.Path + " → " + detail.DestinationPath
	case workspace.ToolMkdir:
		return "正在准备创建目录 " + detail.Path
	case workspace.ToolArchive:
		return "正在准备归档 " + detail.Path
	case workspace.ToolList:
		return "正在列出 " + detail.Path
	default:
		return toolRunningSummary(tool)
	}
}

func activityPathForTool(tool, value string) string {
	if tool == workspace.ToolCopy || tool == workspace.ToolMove {
		if fileeffects.ValidPath(value, false) {
			return value
		}
		return ""
	}
	return safeActivityWorkspacePath(value)
}

func safeActivityWorkspacePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "." {
		return value
	}
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.ContainsAny(value, `\:`) {
		return ""
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return ""
		}
	}
	for _, current := range value {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return ""
		}
	}
	return value
}

func safeActivityToken(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) {
		return ""
	}
	for _, current := range value {
		if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '_' || current == '-' {
			continue
		}
		return ""
	}
	return truncateActivityUTF8(value, limit)
}

func safeActivityPreview(value string) string {
	value = safeActivityDelta(value)
	return truncateActivityUTF8(value, maxFileActivityPreviewBytes)
}

func truncateActivityUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func activityInt(value any) (int, bool) {
	switch current := value.(type) {
	case int:
		return current, current >= 0
	case int64:
		return int(current), current >= 0
	case float64:
		return int(current), current >= 0 && current == float64(int(current))
	default:
		return 0, false
	}
}

func activityInt64(value any) (int64, bool) {
	switch current := value.(type) {
	case int:
		return int64(current), current >= 0
	case int64:
		return current, current >= 0
	case float64:
		return int64(current), current >= 0 && current == float64(int64(current))
	default:
		return 0, false
	}
}

func intText(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [24]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
