package workspace

import "fmt"

func (q *workspaceQuery) queryValue(offset, count int, pause string, omitContext bool) map[string]any {
	end := offset + count
	scanComplete := q.done && q.reason == ""
	more := end < len(q.rows) || !q.done
	value := map[string]any{
		"path": q.args.path, "cursor": q.cursor(offset), "offset": offset,
		"returned": max(0, end-offset), "retained_results": len(q.rows), "more": more,
		"scan_finished": q.done, "scan_complete": scanComplete, "complete": scanComplete && !more,
		"visited_entries": q.visited, "scanned_directories": q.directories,
	}
	if more {
		value["next_cursor"], value["next_offset"] = q.cursor(end), end
	}
	if q.reason != "" {
		value["scan_error"] = q.reason
	}
	if !q.done {
		value["scan_state"] = "scanning"
	} else if scanComplete {
		value["scan_state"] = "complete"
	} else {
		value["scan_state"] = "incomplete"
	}
	if !scanComplete || more {
		reason := pause
		if end < len(q.rows) {
			reason = "result_bytes"
		}
		if reason == "" {
			reason = q.reason
		}
		if reason == "" {
			reason = "scan_pending"
		}
		value["truncation_reason"] = reason
		if more {
			value["suggestion"] = "复用原查询参数与next_cursor继续；游标仅当前工作区实例有效"
		} else {
			value["suggestion"] = "查询范围不完整；检查scan_error或调整资源后显式新建查询"
		}
	}
	q.ignore.addValue(value)
	rows := []map[string]any{}
	neighbors := []map[string]any{}
	files := []string{}
	seenContext := make(map[string]bool)
	for i := offset; i < end; i++ {
		row := q.rows[i]
		rows = append(rows, copyQueryObject(row.value))
		if path, ok := row.value["path"].(string); ok {
			files = append(files, path)
		}
		if !omitContext {
			for _, line := range row.context {
				key := fmt.Sprint(line["path"], "\x00", line["line"])
				if !seenContext[key] {
					neighbors = append(neighbors, copyQueryObject(line))
					seenContext[key] = true
				}
			}
		}
	}
	if q.args.tool != ToolSearch {
		value["entries"] = rows
		value["skipped"] = map[string]int{"links": q.links, "other": q.other}
		if q.args.tool == ToolFind {
			value["pattern"], value["type"] = q.args.find.Pattern, q.args.find.Type
		}
		return value
	}
	value["output"], value["scanned_files"], value["scanned_bytes"] = *q.args.search.Output, q.scannedFiles, q.scannedBytes
	value["skipped"] = map[string]any{"links": q.links, "binary_or_invalid_utf8": q.binary, "too_large": q.large, "other": q.other}
	switch *q.args.search.Output {
	case "files":
		value["files"], value["matched_files"], value["counts_partial"] = files, q.matchedFiles, !scanComplete || more
	case "count":
		value["matched_lines"], value["matched_files"], value["counts_partial"] = q.matchedLines, q.matchedFiles, !scanComplete
		value["returned"] = q.matchedLines
	default:
		value["matches"] = rows
		if *q.args.search.Context > 0 {
			value["context"], value["context_lines"] = *q.args.search.Context, neighbors
			if omitContext {
				value["context_omitted"] = true
			}
		}
	}
	return value
}

func (q *workspaceQuery) queryResult(offset, limit int, pause string) Result {
	if q.done {
		offset = min(offset, len(q.rows))
	}
	count := max(0, min(limit, len(q.rows)-offset))
	var value map[string]any
	for _, omitContext := range []bool{false, true} {
		for n := count; n >= 0; n-- {
			value = q.queryValue(offset, n, pause, omitContext)
			if safeResultJSONSize(value) <= q.w.limits.ResultBytes {
				if n == 0 && count > 0 {
					break
				}
				return q.queryResultWithValue(value)
			}
		}
	}
	// A zero-row success here would loop forever on the same oversized path.
	return failureResult("query_result_too_small", "结果预算无法容纳一个完整定位条目；请收窄查询或增大结果预算")
}

func (q *workspaceQuery) queryResultWithValue(value map[string]any) Result {
	returned, _ := value["returned"].(int)
	summary := fmt.Sprintf("%s 已返回 %d 项", q.args.tool, returned)
	if value["more"] == true {
		summary += "；仍有结果或扫描待继续"
	}
	if q.reason != "" {
		summary += "；查询范围不完整（" + q.reason + "）"
	}
	if value["scan_complete"] == true && len(q.rows) == 0 && q.matchedLines == 0 {
		summary += "；已完整扫描，无匹配"
	}
	kind := "directory_listing"
	if q.args.tool == ToolFind {
		kind = "find_result"
	}
	if q.args.tool == ToolSearch {
		kind = "search_result"
	}
	return Result{Value: value, Summary: summary, Reference: &Reference{Path: q.args.path, Kind: kind, ContentHash: hashProjection(q.id)}}
}
