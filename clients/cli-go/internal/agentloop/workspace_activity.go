package agentloop

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

const maxFileActivityPreviewBytes = 6 << 10

func fileActivityDetailFromProgress(progress workspace.Progress) *FileActivityDetail {
	path := safeActivityWorkspacePath(progress.Path)
	if path == "" {
		return nil
	}
	detail := &FileActivityDetail{Path: path}
	switch progress.Tool {
	case workspace.ToolList:
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
	detail.Path = safeActivityWorkspacePath(stringValue(value["path"]))
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
		if matches, exists := activityInt(value["returned"]); exists {
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
		Path: safeActivityWorkspacePath(presentation.Path), Operation: safeActivityToken(presentation.Operation, 64),
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
	if detail.PreviewKind == "" {
		detail.PreviewKind = preparedDetail.PreviewKind
	}
	if detail.Preview == "" {
		detail.Preview = preparedDetail.Preview
	}
	detail.PreviewTruncated = detail.PreviewTruncated || preparedDetail.PreviewTruncated
	return detail
}

func workspaceProgressSummary(tool string, detail *FileActivityDetail) string {
	if detail == nil {
		return toolRunningSummary(tool)
	}
	switch tool {
	case workspace.ToolRead:
		if detail.HasRange {
			return "正在读取 " + detail.Path + " 第 " + intText(detail.StartLine) + "-" + intText(detail.EndLine) + " 行"
		}
		return "正在读取 " + detail.Path
	case workspace.ToolSearch:
		return "正在搜索 " + detail.Path + "：已扫描 " + intText(detail.ScannedFiles) + " 个文件"
	case workspace.ToolList:
		return "正在列出 " + detail.Path
	default:
		return toolRunningSummary(tool)
	}
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
	return truncateActivityUTF8(value, 512)
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
