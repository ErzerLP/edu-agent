package workspace

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type searchArguments struct {
	Query   string   `json:"query"`
	Path    string   `json:"path"`
	Mode    string   `json:"mode"`
	Case    string   `json:"case"`
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type searchState struct {
	matches        []map[string]any
	scannedFiles   int
	scannedBytes   int64
	visitedEntries int
	skippedLinks   int
	skippedBinary  int
	skippedLarge   int
	skippedOther   int
	complete       bool
	reason         string
	progress       *searchProgressEmitter
}

type searchDirectory struct {
	path  string
	depth int
}

func (w *Workspace) executeSearch(ctx context.Context, raw string) Result {
	var args searchArguments
	if err := decodeArguments(raw, &args); err != nil {
		return resultForError(err, "文件搜索参数无效")
	}
	if args.Path == "" {
		args.Path = "."
	}
	if args.Mode == "" {
		args.Mode = "literal"
	}
	if args.Case == "" {
		args.Case = "smart"
	}
	scope, err := normalizeModelPath(args.Path, true)
	if err != nil {
		return resultForError(err, "文件搜索路径无效")
	}
	matcher, err := compileSearchMatcher(args)
	if err != nil {
		return resultForError(err, "文件搜索表达式无效")
	}
	if err := validateGlobs(args.Include, args.Exclude); err != nil {
		return resultForError(err, "文件搜索筛选参数无效")
	}
	state := searchState{complete: true, matches: []map[string]any{}}
	state.progress = newSearchProgressEmitter(ctx, scope)
	state.progress.initial()
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	_, _, _, dirErr := w.root.ReadDir(scope, 1)
	switch {
	case dirErr == nil:
		w.searchDirectories(ctx, scope, matcher, args.Include, args.Exclude, &state)
	case isDirectoryError(dirErr):
		w.searchFile(ctx, scope, matcher, args.Include, args.Exclude, &state)
	case errors.Is(dirErr, securefile.ErrLink):
		return failureResult(CodeLinkNotAllowed, "搜索范围链接不允许访问")
	case isPermissionError(dirErr):
		return failureResult(CodePermissionDenied, "搜索范围不可读取")
	default:
		return resultForError(dirErr, "搜索范围无法安全读取")
	}
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	sort.Slice(state.matches, func(i, j int) bool {
		left, right := state.matches[i], state.matches[j]
		if left["path"] != right["path"] {
			return left["path"].(string) < right["path"].(string)
		}
		if left["line"] != right["line"] {
			return left["line"].(int) < right["line"].(int)
		}
		return left["column"].(int) < right["column"].(int)
	})
	value := searchResultValue(scope, state)
	for safeResultJSONSize(value) > w.limits.ResultBytes && len(state.matches) > 0 {
		state.matches = state.matches[:len(state.matches)-1]
		state.complete = false
		state.reason = "result_bytes"
		value = searchResultValue(scope, state)
	}
	state.progress.final(state)
	return Result{
		Value:     value,
		Summary:   fmt.Sprintf("在 %s 中找到 %d 处匹配", scope, len(state.matches)),
		Reference: &Reference{Path: scope, ContentHash: hashProjection(value), Kind: "search_result"},
	}
}

func (w *Workspace) searchDirectories(ctx context.Context, scope string, matcher *regexp.Regexp, include, exclude []string, state *searchState) {
	queue := []searchDirectory{{path: scope, depth: 0}}
	for len(queue) > 0 && state.complete {
		if ctx.Err() != nil {
			return
		}
		current := queue[0]
		queue = queue[1:]
		entries, _, complete, err := w.root.ReadDir(current.path, w.limits.DirectoryScanEntries)
		if err != nil {
			state.skippedOther++
			continue
		}
		if !complete {
			state.complete = false
			state.reason = "directory_entry_limit"
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		for _, entry := range entries {
			if ctx.Err() != nil || !state.complete {
				return
			}
			state.visitedEntries++
			if state.visitedEntries > w.limits.SearchEntries {
				state.complete = false
				state.reason = "entry_limit"
				return
			}
			relative := joinRelative(current.path, entry.Name)
			switch entry.Type {
			case securefile.EntryLink:
				state.skippedLinks++
			case securefile.EntryDirectory:
				if !isArchivePath(scope) && isArchivePath(relative) {
					continue
				}
				if matchesAnyGlob(relative, exclude) {
					continue
				}
				if current.depth >= w.limits.SearchDepth {
					state.complete = false
					state.reason = "depth_limit"
					return
				}
				queue = append(queue, searchDirectory{path: relative, depth: current.depth + 1})
			case securefile.EntryFile:
				w.searchFile(ctx, relative, matcher, include, exclude, state)
			default:
				state.skippedOther++
			}
		}
	}
}

func (w *Workspace) searchFile(ctx context.Context, relative string, matcher *regexp.Regexp, include, exclude []string, state *searchState) {
	if ctx.Err() != nil || !state.complete || !includedPath(relative, include, exclude) {
		return
	}
	if state.scannedFiles >= w.limits.SearchFiles {
		state.complete = false
		state.reason = "file_limit"
		return
	}
	snapshot, err := w.root.ReadSnapshot(relative, w.limits.FileBytes, false)
	if err != nil {
		switch {
		case errors.Is(err, securefile.ErrLink):
			state.skippedLinks++
		case errors.Is(err, securefile.ErrTooLarge):
			state.skippedLarge++
		default:
			state.skippedOther++
		}
		return
	}
	if state.scannedBytes+snapshot.Size > w.limits.SearchBytes {
		state.complete = false
		state.reason = "scan_bytes"
		return
	}
	decoded, err := decodeText(snapshot.Data)
	if err != nil {
		var operation *operationError
		if errors.As(err, &operation) && (operation.code == CodeBinaryFile || operation.code == CodeInvalidUTF8) {
			state.skippedBinary++
		} else {
			state.skippedOther++
		}
		return
	}
	state.scannedFiles++
	state.scannedBytes += snapshot.Size
	state.progress.maybe(*state)
	for lineIndex, rawLine := range splitTextLines(decoded.Text) {
		if ctx.Err() != nil || !state.complete {
			return
		}
		line := trimLineEnding(rawLine)
		indices := matcher.FindAllStringIndex(line, -1)
		for _, match := range indices {
			if len(state.matches) >= w.limits.SearchMatches {
				state.complete = false
				state.reason = "match_limit"
				return
			}
			column := utf8.RuneCountInString(line[:match[0]]) + 1
			entry := map[string]any{
				"path": relative, "line": lineIndex + 1, "column": column,
				"preview": boundedSearchPreview(line, match[0], match[1], w.limits.SearchPreviewBytes),
			}
			state.matches = append(state.matches, entry)
			state.progress.maybe(*state)
			if safeResultJSONSize(searchResultValue(".", *state)) > w.limits.ResultBytes-256 {
				state.matches = state.matches[:len(state.matches)-1]
				state.complete = false
				state.reason = "result_bytes"
				return
			}
		}
	}
}

func compileSearchMatcher(args searchArguments) (*regexp.Regexp, error) {
	query := args.Query
	if query == "" || len(query) > 1000 || !utf8.ValidString(query) {
		return nil, argumentError("query is invalid")
	}
	for _, current := range query {
		if current == '\n' || current == '\r' || unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return nil, argumentError("query contains unsupported controls")
		}
	}
	if args.Mode != "literal" && args.Mode != "regex" || args.Case != "smart" && args.Case != "sensitive" && args.Case != "insensitive" {
		return nil, argumentError("search mode or case policy is invalid")
	}
	pattern := query
	if args.Mode == "literal" {
		pattern = regexp.QuoteMeta(query)
	} else {
		parsed, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			return nil, operationFailure(CodeRegexInvalid, "search regular expression is invalid")
		}
		program, err := syntax.Compile(parsed)
		if err != nil || len(program.Inst) > 10000 {
			return nil, operationFailure(CodeRegexInvalid, "search regular expression is too complex")
		}
	}
	caseSensitive := args.Case == "sensitive" || args.Case == "smart" && hasUpper(query)
	if !caseSensitive {
		pattern = "(?i:" + pattern + ")"
	}
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return nil, operationFailure(CodeRegexInvalid, "search regular expression is invalid")
	}
	return matcher, nil
}

func validateGlobs(groups ...[]string) error {
	for _, patterns := range groups {
		if len(patterns) > 16 {
			return argumentError("too many include or exclude patterns")
		}
		for _, pattern := range patterns {
			if pattern == "" || len(pattern) > 256 || !utf8.ValidString(pattern) || strings.Contains(pattern, "\\") {
				return argumentError("include or exclude pattern is invalid")
			}
			for _, current := range pattern {
				if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
					return argumentError("include or exclude pattern contains controls")
				}
			}
			if _, err := pathpkg.Match(pattern, "probe"); err != nil {
				return argumentError("include or exclude pattern is invalid")
			}
		}
	}
	return nil
}

func includedPath(relative string, include, exclude []string) bool {
	if matchesAnyGlob(relative, exclude) {
		return false
	}
	return len(include) == 0 || matchesAnyGlob(relative, include)
}

func matchesAnyGlob(relative string, patterns []string) bool {
	base := pathpkg.Base(relative)
	for _, pattern := range patterns {
		if matched, _ := pathpkg.Match(pattern, relative); matched {
			return true
		}
		if matched, _ := pathpkg.Match(pattern, base); matched {
			return true
		}
	}
	return false
}

func boundedSearchPreview(line string, start, end, limit int) string {
	if len(line) <= limit {
		return line
	}
	left := max(0, start-limit/3)
	for left < len(line) && !utf8.RuneStart(line[left]) {
		left++
	}
	right := min(len(line), max(end, left+limit))
	for right > left && right < len(line) && !utf8.RuneStart(line[right]) {
		right--
	}
	preview := line[left:right]
	if left > 0 {
		preview = "…" + preview
	}
	if right < len(line) {
		preview += "…"
	}
	return truncateUTF8Bytes(preview, limit)
}

func searchResultValue(scope string, state searchState) map[string]any {
	value := map[string]any{
		"path": scope, "matches": state.matches, "returned": len(state.matches),
		"scanned_files": state.scannedFiles, "scanned_bytes": state.scannedBytes,
		"visited_entries": state.visitedEntries,
		"skipped": map[string]any{
			"links": state.skippedLinks, "binary_or_invalid_utf8": state.skippedBinary,
			"too_large": state.skippedLarge, "other": state.skippedOther,
		},
		"complete": state.complete,
	}
	if !state.complete {
		value["truncation_reason"] = state.reason
		value["suggestion"] = "缩小 path，或使用 include/exclude 收窄搜索"
	}
	return value
}
