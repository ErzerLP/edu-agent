package workspace

import (
	"fmt"
	"strings"
)

func buildMutationPreview(path, oldText, newText, kind string, limit int) (string, bool, int) {
	if kind == "content" {
		prefix := fmt.Sprintf("+++ %s\n", path)
		value, truncated := boundedPreview(prefix+newText, limit)
		return value, truncated, firstChangedLine(oldText, newText)
	}
	oldLines := splitPreviewLines(oldText)
	newLines := splitPreviewLines(newText)
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	oldSuffix, newSuffix := len(oldLines), len(newLines)
	for oldSuffix > prefix && newSuffix > prefix && oldLines[oldSuffix-1] == newLines[newSuffix-1] {
		oldSuffix--
		newSuffix--
	}
	start := max(0, prefix-2)
	oldEnd := min(len(oldLines), oldSuffix+2)
	newEnd := min(len(newLines), newSuffix+2)
	var builder strings.Builder
	fmt.Fprintf(&builder, "--- %s\n+++ %s\n@@ -%d,%d +%d,%d @@\n", path, path, start+1, oldEnd-start, start+1, newEnd-start)
	for index := start; index < prefix; index++ {
		builder.WriteString(" ")
		builder.WriteString(oldLines[index])
	}
	for index := prefix; index < oldSuffix; index++ {
		builder.WriteString("-")
		builder.WriteString(oldLines[index])
	}
	for index := prefix; index < newSuffix; index++ {
		builder.WriteString("+")
		builder.WriteString(newLines[index])
	}
	for index := oldSuffix; index < oldEnd; index++ {
		builder.WriteString(" ")
		builder.WriteString(oldLines[index])
	}
	preview, truncated := boundedPreview(builder.String(), limit)
	return preview, truncated, prefix + 1
}

func splitPreviewLines(value string) []string {
	if value == "" {
		return nil
	}
	lines := strings.SplitAfter(value, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func boundedPreview(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	marker := "\n… preview truncated …"
	return truncateUTF8Bytes(value, max(0, limit-len(marker))) + marker, true
}

func firstChangedLine(oldText, newText string) int {
	oldLines := splitPreviewLines(oldText)
	newLines := splitPreviewLines(newText)
	limit := min(len(oldLines), len(newLines))
	for index := 0; index < limit; index++ {
		if oldLines[index] != newLines[index] {
			return index + 1
		}
	}
	return limit + 1
}
