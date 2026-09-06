package workspace

import (
	"context"
	"strings"
)

func patchLogicalLine(raw []byte) string {
	text := string(raw)
	if strings.HasSuffix(text, "\n") {
		text = strings.TrimSuffix(text, "\n")
		text = strings.TrimSuffix(text, "\r")
	}
	return text
}

func matchPatchHunk(ctx context.Context, lines completeDiffLines, hunk patchHunk) (int, int, error) {
	var old []string
	for _, line := range hunk.lines {
		if line[0] != '+' {
			old = append(old, line[1:])
		}
	}
	if len(old) == 0 {
		if !hunk.eof {
			return 0, 0, invalidPatch("纯插入 hunk 必须有原文上下文或明确 End of File")
		}
		return lines.count(), lines.count(), nil
	}
	if hunk.eof {
		start := lines.count() - len(old)
		if start >= 0 {
			matched := true
			for i, want := range old {
				if err := ctx.Err(); err != nil {
					return 0, 0, err
				}
				if patchLogicalLine(lines.line(start+i)) != want {
					matched = false
					break
				}
			}
			if matched {
				return start, lines.count(), nil
			}
		}
		return 0, 0, operationFailure(CodeReplacementMissing, "补丁 EOF 上下文与原文末尾不精确匹配")
	}
	// KMP compares logical lines exactly, with no trim/Unicode/fuzzy matching.
	// Its linear scan remains bounded even for repetitive large source files.
	prefix := make([]int, len(old))
	for i, j := 1, 0; i < len(old); i++ {
		for j > 0 && old[i] != old[j] {
			j = prefix[j-1]
		}
		if old[i] == old[j] {
			j++
		}
		prefix[i] = j
	}
	found := -1
	for i, j := 0, 0; i < lines.count(); i++ {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		line := patchLogicalLine(lines.line(i))
		for j > 0 && line != old[j] {
			j = prefix[j-1]
		}
		if line == old[j] {
			j++
		}
		if j != len(old) {
			continue
		}
		if found >= 0 {
			return 0, 0, operationFailure(CodeReplacementNotUnique, "补丁旧行上下文在原文中不唯一")
		}
		found = i + 1 - len(old)
		j = prefix[j-1]
	}
	if found < 0 {
		return 0, 0, operationFailure(CodeReplacementMissing, "补丁旧行上下文在原文中不存在")
	}
	return found, found + len(old), nil
}
