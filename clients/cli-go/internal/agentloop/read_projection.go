package agentloop

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Read content is a contiguous byte prefix, never prose to abbreviate. Every
// projection level recomputes its continuation from the bytes it actually keeps.
func compactReadProjection(object map[string]any, payloadLimit int, reason string) map[string]any {
	result := preserveFields(object, "path", "content_hash", "expected_hash", "hash_scope", "file_bytes", "read_byte_limit", "offset", "byte_offset", "start_line", "end_line", "total_lines", "returned_lines", "complete", "next_offset", "next_byte_offset", "truncation_reason", "source_truncation_reason", "source_complete", "projection_omitted", "path_omitted", "error", "code", "message", "suggestion", "publication_outcome")
	result["tool"] = "read"
	content, hasContent := object["content"].(string)
	if !hasContent {
		return result
	}
	prefix := readContentPrefix(content, payloadLimit)
	result["content"] = prefix
	if len(prefix) == len(content) {
		return result
	}
	line := max(int64(1), projectionInt64(firstProjectionValue(object, "offset", "start_line")))
	byteOffset := max(int64(0), projectionInt64(object["byte_offset"]))
	nextLine, nextByte := line, byteOffset
	if last := strings.LastIndexByte(prefix, '\n'); last >= 0 {
		nextLine += int64(strings.Count(prefix, "\n"))
		nextByte = int64(len(prefix) - last - 1)
	} else {
		nextByte += int64(len(prefix))
	}
	endLine, returned := int64(0), int64(0)
	if len(prefix) != 0 {
		endLine = nextLine
		if strings.HasSuffix(prefix, "\n") {
			endLine--
		}
		returned = endLine - line + 1
	}
	result["offset"], result["byte_offset"], result["start_line"] = line, byteOffset, line
	result["end_line"], result["returned_lines"] = endLine, returned
	result["next_offset"], result["next_byte_offset"] = nextLine, nextByte
	if complete, _ := object["complete"].(bool); complete {
		result["source_complete"] = true
	}
	if _, exists := result["source_truncation_reason"]; !exists {
		if old, ok := object["truncation_reason"]; ok {
			result["source_truncation_reason"] = old
		}
	}
	result["complete"], result["projection_omitted"], result["truncation_reason"] = false, true, reason
	return result
}

func readContentPrefix(content string, limit int) string {
	limit = min(len(content), max(0, limit))
	for limit > 0 && limit < len(content) && !utf8.RuneStart(content[limit]) {
		limit--
	}
	return content[:limit]
}

func minimalReadProjection(object map[string]any, reason string) map[string]any {
	full := compactReadProjection(object, 0, reason)
	result := preserveFields(full, "tool", "content_hash", "expected_hash", "next_offset", "next_byte_offset", "complete", "content", "projection_omitted", "truncation_reason", "error", "code", "publication_outcome", "read_byte_limit")
	if _, ok := object["path"]; ok {
		result["path_omitted"] = true
	}
	return result
}

func boundedReadProjectionJSON(value any, limit int, reason string) string {
	if data, err := json.Marshal(value); err == nil && len(data) <= limit {
		return string(data)
	}
	object := normalizedProjectionObject(value)
	if object == nil {
		return `{"error":"read_projection"}`
	}
	content, _ := object["content"].(string)
	// Prefer retaining the exact path. When metadata alone is too large, omit
	// that field explicitly rather than inventing a truncated filesystem path.
	for _, omitPath := range []bool{false, true} {
		var best string
		low, high := 0, min(len(content), max(0, limit))
		for low <= high {
			middle := low + (high-low)/2
			candidate := compactReadProjection(object, middle, reason)
			if omitPath {
				delete(candidate, "path")
				candidate["path_omitted"] = true
				delete(candidate, "suggestion")
			}
			data, err := json.Marshal(candidate)
			if err == nil && len(data) <= limit {
				best, low = string(data), middle+1
			} else {
				high = middle - 1
			}
		}
		if best != "" {
			return best
		}
	}
	if data, err := json.Marshal(minimalReadProjection(object, reason)); err == nil && len(data) <= limit {
		return string(data)
	}
	return `{"error":"read_projection"}`
}
