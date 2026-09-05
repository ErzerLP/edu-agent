package workspace

import (
	"context"
	"errors"
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
	Query            string   `json:"query"`
	Path             string   `json:"path"`
	Mode             string   `json:"mode"`
	Case             string   `json:"case"`
	Include          []string `json:"include"`
	Exclude          []string `json:"exclude"`
	Output           *string  `json:"output"`
	Glob             *string  `json:"glob"`
	Context          *int     `json:"context"`
	RespectGitignore *bool    `json:"respect_gitignore"`
}

type searchState struct {
	scope          string
	output         string
	context        int
	files          []string
	contextLines   []map[string]any
	matchedLines   int
	matchedFiles   int
	stop           bool
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
	glob           *pathGlob
	ignore         *gitignoreState
}

type searchDirectory struct {
	path   string
	depth  int
	ignore *ignoreLayer
}

func (w *Workspace) executeSearch(ctx context.Context, raw string) Result {
	args, err := decodeSearchArguments(raw)
	if err != nil {
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
	glob, err := compilePathGlob(*args.Glob)
	if err != nil {
		return resultForError(err, "文件搜索路径模式无效")
	}
	state := searchState{
		glob:  &glob,
		scope: scope, output: *args.Output, context: *args.Context, complete: true,
		matches: []map[string]any{}, files: []string{}, contextLines: []map[string]any{},
	}
	if *args.RespectGitignore {
		state.ignore = w.newGitignoreState(state.incomplete)
	}
	if safeResultJSONSize(searchResultValue(scope, state)) > w.limits.ResultBytes-256 {
		return failureResult(CodeInvalidPath, "搜索范围过长，无法在结果预算内展示")
	}
	state.progress = newSearchProgressEmitter(ctx, scope)
	state.progress.initial()
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	var dirErr error
	if state.ignore != nil {
		// Classify without enumerating an explicit directory before its ignored
		// ancestors have been checked. Explicit files bypass discovery rules.
		entry, err := w.root.Stat(ctx, scope)
		if ctx.Err() != nil {
			return contextFailure(ctx.Err())
		}
		if err != nil {
			return resultForError(err, "搜索范围无法安全检查")
		}
		switch entry.Kind {
		case securefile.EntryDirectory:
		case securefile.EntryFile:
			dirErr = securefile.ErrNotDirectory
		case securefile.EntryLink:
			dirErr = securefile.ErrLink
		default:
			return failureResult(CodeUnsupportedType, "搜索范围不是普通文件或目录")
		}
	} else {
		_, _, _, dirErr = w.root.ReadDir(scope, 1)
	}
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
	sort.Strings(state.files)
	sort.Slice(state.contextLines, func(i, j int) bool {
		left, right := state.contextLines[i], state.contextLines[j]
		if left["path"] != right["path"] {
			return left["path"].(string) < right["path"].(string)
		}
		return left["line"].(int) < right["line"].(int)
	})
	value := searchResultValue(scope, state)
	for safeResultJSONSize(value) > w.limits.ResultBytes && state.trimResult() {
		state.incomplete("result_bytes", true)
		value = searchResultValue(scope, state)
	}
	state.progress.final(state)
	return Result{
		Value:     value,
		Summary:   state.summary(),
		Reference: &Reference{Path: scope, ContentHash: hashProjection(value), Kind: "search_result"},
	}
}

func (w *Workspace) searchDirectories(ctx context.Context, scope string, matcher *regexp.Regexp, include, exclude []string, state *searchState) {
	var parent *ignoreLayer
	if state.ignore != nil {
		var ok bool
		parent, ok = state.ignore.scopeParents(ctx, scope)
		if !ok {
			return
		}
	}
	queue := []searchDirectory{{path: scope, depth: 0, ignore: parent}}
	for len(queue) > 0 && !state.stop {
		if ctx.Err() != nil {
			return
		}
		current := queue[0]
		queue = queue[1:]
		if state.ignore != nil {
			if state.visitedEntries >= w.limits.SearchEntries {
				state.incomplete("entry_limit", true)
				return
			}
			var ok bool
			current.ignore, ok = state.ignore.load(ctx, current.path, current.ignore)
			if !ok {
				continue
			}
		}
		entries, skipped, complete, err := w.root.ReadDir(current.path, w.limits.DirectoryScanEntries)
		if err != nil {
			state.skippedOther++
			state.incomplete("directory_unavailable", false)
			continue
		}
		state.skippedOther += skipped
		state.visitedEntries += skipped
		if skipped > 0 {
			state.incomplete("invalid_entry_path", false)
		}
		if !complete {
			state.incomplete("directory_entry_limit", true)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		for _, entry := range entries {
			if ctx.Err() != nil || state.stop {
				return
			}
			state.visitedEntries++
			if state.visitedEntries > w.limits.SearchEntries {
				state.incomplete("entry_limit", true)
				return
			}
			relative := joinRelative(current.path, entry.Name)
			if entry.Type == securefile.EntryDirectory && !isArchivePath(scope) && isArchivePath(relative) {
				continue
			}
			if state.ignore.excluded(current.ignore, relative, entry.Type == securefile.EntryDirectory) {
				continue
			}
			switch entry.Type {
			case securefile.EntryLink:
				state.skippedLinks++
			case securefile.EntryDirectory:
				if matchesAnyGlob(relative, exclude) {
					continue
				}
				if current.depth >= w.limits.SearchDepth {
					state.incomplete("depth_limit", true)
					return
				}
				queue = append(queue, searchDirectory{path: relative, depth: current.depth + 1, ignore: current.ignore})
			case securefile.EntryFile:
				w.searchFile(ctx, relative, matcher, include, exclude, state)
			default:
				state.skippedOther++
			}
		}
	}
}

func (w *Workspace) searchFile(ctx context.Context, relative string, matcher *regexp.Regexp, include, exclude []string, state *searchState) {
	if ctx.Err() != nil || state.stop || !includedPath(relative, include, exclude) || state.glob != nil && !state.glob.Match(relative) {
		return
	}
	if state.scannedFiles >= w.limits.SearchFiles || state.ignore != nil && state.ignore.usedFiles >= w.limits.SearchFiles {
		state.incomplete("file_limit", true)
		return
	}
	var snapshot securefile.Snapshot
	var err error
	if state.ignore != nil {
		remaining := w.limits.SearchBytes - state.ignore.usedBytes
		if remaining <= 0 {
			state.incomplete("scan_bytes", true)
			return
		}
		limit := min(w.limits.FileBytes, remaining-1)
		snapshot, err = state.ignore.readSnapshot(relative, limit)
		if errors.Is(err, securefile.ErrTooLarge) && limit < w.limits.FileBytes {
			state.incomplete("scan_bytes", true)
			return
		}
	} else {
		snapshot, err = w.root.ReadSnapshot(relative, w.limits.FileBytes, false)
	}
	if err != nil {
		switch {
		case errors.Is(err, securefile.ErrLink):
			state.skippedLinks++
		case errors.Is(err, securefile.ErrTooLarge):
			state.skippedLarge++
			state.incomplete("file_too_large", false)
		default:
			state.skippedOther++
			state.incomplete("file_unavailable", false)
		}
		return
	}
	if state.scannedBytes+snapshot.Size > w.limits.SearchBytes {
		state.incomplete("scan_bytes", true)
		return
	}
	decoded, err := decodeText(snapshot.Data)
	if err != nil {
		var operation *operationError
		if errors.As(err, &operation) && (operation.code == CodeBinaryFile || operation.code == CodeInvalidUTF8) {
			state.skippedBinary++
		} else {
			state.skippedOther++
			state.incomplete("file_unavailable", false)
		}
		return
	}
	state.scannedFiles++
	state.scannedBytes += snapshot.Size
	state.progress.maybe(*state)
	lines := splitTextLines(decoded.Text)
	fileMatched, lastContextLine := false, -1
	for lineIndex, rawLine := range lines {
		if ctx.Err() != nil || state.stop {
			return
		}
		line := trimLineEnding(rawLine)
		if state.output != "content" {
			if matcher.FindStringIndex(line) == nil {
				continue
			}
			if state.matchesCount() >= w.limits.SearchMatches {
				state.incomplete("match_limit", true)
				return
			}
			if !fileMatched {
				state.matchedFiles++
				fileMatched = true
			}
			if state.output == "files" {
				state.files = append(state.files, relative)
				if !w.searchResultFits(state) {
					state.files = state.files[:len(state.files)-1]
				}
				state.progress.maybe(*state)
				return // File discovery needs no further body matches.
			}
			state.matchedLines++ // Count lines, not occurrences on this line.
			state.progress.maybe(*state)
			continue
		}
		// One lookahead detects truncation without allocating an index for
		// every occurrence in a long line (including empty regex matches).
		indices := matcher.FindAllStringIndex(line, w.limits.SearchMatches-len(state.matches)+1)
		for _, match := range indices {
			if ctx.Err() != nil {
				return
			}
			if len(state.matches) >= w.limits.SearchMatches {
				state.incomplete("match_limit", true)
				return
			}
			column := utf8.RuneCountInString(line[:match[0]]) + 1
			entry := map[string]any{
				"path": relative, "line": lineIndex + 1, "column": column,
				"preview": boundedSearchPreview(line, match[0], match[1], w.limits.SearchPreviewBytes),
			}
			state.matches = append(state.matches, entry)
			if !w.searchResultFits(state) {
				state.matches = state.matches[:len(state.matches)-1]
				return
			}
			state.progress.maybe(*state)
		}
		if state.context > 0 && len(indices) > 0 {
			for index := max(lastContextLine+1, lineIndex-state.context); index <= min(len(lines)-1, lineIndex+state.context); index++ {
				if ctx.Err() != nil {
					return
				}
				text := trimLineEnding(lines[index])
				entry := map[string]any{
					"path": relative, "line": index + 1,
					"content": truncateUTF8Bytes(text, w.limits.SearchPreviewBytes),
				}
				if len(text) > w.limits.SearchPreviewBytes {
					entry["truncated"] = true
				}
				state.contextLines = append(state.contextLines, entry)
				if !w.searchResultFits(state) {
					state.contextLines = state.contextLines[:len(state.contextLines)-1]
					return
				}
				lastContextLine = index
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
		"path": scope, "output": state.output, "returned": state.matchesCount(),
		"scanned_files": state.scannedFiles, "scanned_bytes": state.scannedBytes,
		"visited_entries": state.visitedEntries,
		"skipped": map[string]any{
			"links": state.skippedLinks, "binary_or_invalid_utf8": state.skippedBinary,
			"too_large": state.skippedLarge, "other": state.skippedOther,
		},
		"complete": state.complete,
	}
	state.ignore.addValue(value)
	switch state.output {
	case "files":
		value["files"] = state.files
		value["matched_files"] = state.matchedFiles
		value["counts_partial"] = !state.complete
	case "count":
		value["matched_lines"] = state.matchedLines
		value["matched_files"] = state.matchedFiles
		value["counts_partial"] = !state.complete
	default:
		value["matches"] = state.matches
		if state.context > 0 {
			value["context"] = state.context
			value["context_lines"] = state.contextLines
		}
	}
	if !state.complete {
		value["truncation_reason"] = state.reason
		value["suggestion"] = "缩小 path，或使用 include/exclude 收窄搜索"
	}
	return value
}
