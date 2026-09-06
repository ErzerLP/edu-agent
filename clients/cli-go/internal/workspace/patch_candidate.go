package workspace

import (
	"bytes"
	"context"
	"strings"
)

func buildPatchCandidate(ctx context.Context, raw []byte, hunks []patchHunk, limit int64) ([]byte, []replacementRange, error) {
	bom := 0
	if bytes.HasPrefix(raw, utf8BOM) {
		bom = len(utf8BOM)
	}
	lines, err := indexCompleteDiffLines(ctx, raw[bom:])
	if err != nil {
		return nil, nil, err
	}
	newline := dominantNewline(raw)
	missingLF := len(raw) > bom && raw[len(raw)-1] != '\n'
	ranges := make([]replacementRange, 0, len(hunks))
	for _, hunk := range hunks {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		start, end, err := matchPatchHunk(ctx, lines, hunk)
		if err != nil {
			return nil, nil, err
		}
		current := replacementRange{start: bom + lines.starts[start], end: bom + lines.starts[end]}
		if len(ranges) > 0 {
			prior := ranges[len(ranges)-1]
			if current.start < prior.end || current.start <= prior.start {
				return nil, nil, operationFailure(CodeReplacementOverlap, "补丁 hunk 在同一原文中重叠、重复或乱序")
			}
		}
		current.text, err = patchHunkText(ctx, lines, hunk, start, end, newline, missingLF, limit)
		if err != nil {
			return nil, nil, err
		}
		ranges = append(ranges, current)
	}
	// Subtract all replaced raw bytes before adding candidates; one hunk can
	// grow while another shrinks. Include the original BOM in the byte budget.
	size := int64(len(raw))
	for _, r := range ranges {
		size -= int64(r.end - r.start)
	}
	if size > limit {
		return nil, nil, patchCandidateTooLarge()
	}
	for _, r := range ranges {
		if int64(len(r.text)) > limit-size {
			return nil, nil, patchCandidateTooLarge()
		}
		size += int64(len(r.text))
	}
	candidate := make([]byte, 0, int(size))
	previousEnd := 0
	for _, r := range ranges {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		candidate = append(candidate, raw[previousEnd:r.start]...)
		candidate = append(candidate, r.text...)
		previousEnd = r.end
	}
	candidate = append(candidate, raw[previousEnd:]...)
	if bytes.Equal(candidate, raw) {
		return nil, nil, operationFailure(CodeNoChanges, "补丁没有实际文本变化")
	}
	return candidate, ranges, ctx.Err()
}

func patchCandidateTooLarge() error {
	return operationFailure(CodeFileTooLarge, "补丁候选超过处理字节上限")
}

func patchHunkText(ctx context.Context, lines completeDiffLines, hunk patchHunk, start, end int, newline string, missingLF bool, limit int64) (string, error) {
	lastOutput := -1
	for i, line := range hunk.lines {
		if line[0] != '-' {
			lastOutput = i
		}
	}
	var out strings.Builder
	write := func(s string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if int64(len(s)) > limit-int64(out.Len()) {
			return patchCandidateTooLarge()
		}
		out.WriteString(s)
		return nil
	}
	oldAt := start
	for i, line := range hunk.lines {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		switch line[0] {
		case '-':
			oldAt++
		case ' ':
			if err := write(string(lines.line(oldAt))); err != nil {
				return "", err
			}
			oldAt++
		case '+':
			// Appending logical lines to an unterminated original needs a
			// separator, but the original/context bytes themselves are copied.
			if out.Len() > 0 && out.String()[out.Len()-1] != '\n' ||
				out.Len() == 0 && start == lines.count() && missingLF {
				if err := write(newline); err != nil {
					return "", err
				}
			}
			if err := write(line[1:]); err != nil {
				return "", err
			}
			if !(missingLF && end == lines.count() && i == lastOutput) {
				if err := write(newline); err != nil {
					return "", err
				}
			}
		}
	}
	return out.String(), nil
}
