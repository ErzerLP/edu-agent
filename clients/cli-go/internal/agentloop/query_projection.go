package agentloop

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func hasQueryProjection(tool string, object map[string]any) bool {
	if tool != workspace.ToolList && tool != workspace.ToolFind && tool != workspace.ToolSearch {
		return false
	}
	cursor := firstProjectionString(object, "cursor", "next_cursor")
	_, ok := workspace.QueryCursorAt(cursor, 0)
	return ok
}

func queryProjectionItems(object map[string]any) (string, []any) {
	key := "entries"
	if output, _ := object["output"].(string); output != "" {
		if output == "count" {
			return "", nil
		}
		if output == "files" {
			key = "files"
		} else {
			key = "matches"
		}
	}
	items, _ := object[key].([]any)
	return key, items
}

func compactQueryProjection(object map[string]any, keep, previewBytes int, reason string, minimal bool) map[string]any {
	result := preserveFields(object, "path", "offset", "returned", "cursor", "next_cursor", "next_offset", "more", "complete", "scan_complete", "scan_finished", "scan_state", "scan_error", "retained_results", "truncation_reason", "source_truncation_reason", "source_complete", "projection_omitted", "path_omitted", "output", "matched_lines", "matched_files", "counts_partial", "visited_entries", "scanned_directories", "scanned_files", "scanned_bytes", "skipped", "respect_gitignore", "ignore_files", "ignore_bytes", "ignored_entries")
	key, items := queryProjectionItems(object)
	keep = min(max(0, keep), len(items))
	if key != "" {
		kept := make([]any, 0, keep)
		for _, item := range items[:keep] {
			if fields, ok := item.(map[string]any); ok {
				row := preserveFields(fields, "path", "type", "line", "column", "preview", "preview_omitted")
				if text, ok := row["preview"].(string); ok && len(text) > previewBytes {
					row["preview"], row["preview_omitted"] = truncateUTF8(text, max(0, previewBytes)), true
					result["projection_omitted"] = true
				}
				kept = append(kept, row)
			} else {
				kept = append(kept, item)
			} // paths remain whole strings
		}
		result[key], result["returned"] = kept, keep
	}
	if lines, present := object["context_lines"].([]any); present {
		if keep == len(items) && !minimal && previewBytes > 0 {
			contextLines := make([]any, 0, len(lines))
			for _, line := range lines {
				fields, ok := line.(map[string]any)
				if !ok {
					continue
				}
				row := preserveFields(fields, "path", "line", "content", "truncated")
				if content, ok := row["content"].(string); ok && len(content) > previewBytes {
					row["content"], row["truncated"] = truncateUTF8(content, previewBytes), true
					result["projection_omitted"] = true
				}
				contextLines = append(contextLines, row)
			}
			result["context"], result["context_lines"] = object["context"], contextLines
		} else {
			result["context_omitted"], result["projection_omitted"] = true, true
		}
	}
	if keep < len(items) {
		offset := int(projectionInt64(object["offset"]))
		cursor, _ := workspace.QueryCursorAt(firstProjectionString(object, "cursor", "next_cursor"), offset+keep)
		result["next_cursor"], result["next_offset"] = cursor, offset+keep
		result["more"], result["complete"], result["projection_omitted"] = true, false, true
		if object["output"] == "files" {
			result["counts_partial"] = true
		}
		if complete, _ := object["complete"].(bool); complete {
			result["source_complete"] = true
		}
		if original, exists := object["truncation_reason"]; exists {
			result["source_truncation_reason"] = original
		}
		result["truncation_reason"] = reason
	}
	if minimal {
		result = preserveFields(result, "offset", "returned", "cursor", "next_cursor", "more", "complete", "scan_complete", "scan_error", "projection_omitted", "output", "matched_lines", "matched_files", "counts_partial", key)
		if _, exists := result["next_cursor"]; exists {
			delete(result, "cursor")
		}
		if key != "" {
			// The retained array is authoritative; omit its duplicate count at
			// the smallest budget rather than losing the actual continuation.
			delete(result, "returned")
		}
		result["path_omitted"] = true
	}
	return result
}

func boundedQueryProjectionJSON(object map[string]any, limit int, reason string) string {
	if data, err := json.Marshal(object); err == nil && len(data) <= limit {
		return string(data)
	}
	_, items := queryProjectionItems(object)
	for _, minimal := range []bool{false, true} {
		for _, preview := range []int{512, 128, 32, 0} {
			best, low, high := "", 0, len(items)
			for low <= high {
				middle := low + (high-low)/2
				data, err := json.Marshal(compactQueryProjection(object, middle, preview, reason, minimal))
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
	}
	return `{"error":"query_projection","projection_omitted":true}`
}
