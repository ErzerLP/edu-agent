package workspace

import (
	"context"
	"fmt"
	"sort"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type findArguments struct {
	Path             string `json:"path"`
	Pattern          string `json:"pattern"`
	Type             string `json:"type"`
	Limit            *int   `json:"limit"`
	RespectGitignore *bool  `json:"respect_gitignore"`
}

func decodeFindArguments(raw string) (findArguments, error) {
	respect := false
	args := findArguments{RespectGitignore: &respect}
	if err := decodeArguments(raw, &args); err != nil {
		return args, err
	}
	if args.RespectGitignore == nil {
		return args, argumentError("respect_gitignore must be boolean")
	}
	return args, nil
}

type findState struct {
	args        findArguments
	entries     []map[string]any
	visited     int
	directories int
	links       int
	other       int
	complete    bool
	reason      string
	stop        bool
	reports     int
	ignore      *gitignoreState
}

func (s *findState) incomplete(reason string, stop bool) {
	s.complete = false
	if s.reason == "" {
		s.reason = reason
	}
	s.stop = s.stop || stop
}

func (s *findState) value() map[string]any {
	value := map[string]any{
		"path": s.args.Path, "pattern": s.args.Pattern, "type": s.args.Type,
		"entries": s.entries, "returned": len(s.entries), "visited_entries": s.visited,
		"scanned_directories": s.directories, "skipped": map[string]int{"links": s.links, "other": s.other},
		"complete": s.complete,
	}
	s.ignore.addValue(value)
	if !s.complete {
		value["truncation_reason"] = s.reason
		value["suggestion"] = "缩小 path 或 pattern 后重新查找；没有全量结果或续页游标"
	}
	return value
}

func (w *Workspace) executeFind(ctx context.Context, raw string) Result {
	args, err := decodeFindArguments(raw)
	if err != nil {
		return resultForError(err, "路径查找参数无效")
	}
	if args.Path == "" {
		args.Path = "."
	}
	if args.Type == "" {
		args.Type = "any"
	}
	scope, err := normalizeModelPath(args.Path, true)
	if err != nil {
		return resultForError(err, "查找范围无效")
	}
	args.Path = scope
	glob, err := compilePathGlob(args.Pattern)
	if err != nil {
		return resultForError(err, "查找模式无效")
	}
	limit := min(200, w.limits.ListEntries)
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > limit {
			return failureResult(CodeInvalidArguments, "查找结果数量超出范围")
		}
		limit = *args.Limit
	}
	if args.Type != "any" && args.Type != "file" && args.Type != "directory" {
		return failureResult(CodeInvalidArguments, "查找类型必须是 file、directory 或 any")
	}
	state := &findState{args: args, entries: []map[string]any{}, complete: true}
	if *args.RespectGitignore {
		state.ignore = w.newGitignoreState(state.incomplete)
	}
	if safeResultJSONSize(state.value()) > w.limits.ResultBytes-256 {
		return failureResult(CodeInvalidPath, "查找范围或模式过长，无法在结果预算内展示")
	}
	publishProgress(ctx, Progress{Tool: ToolFind, Path: scope})
	entry, err := w.root.Stat(ctx, scope)
	if ctx.Err() != nil {
		return contextFailure(ctx.Err())
	}
	if err != nil {
		return resultForError(err, "查找范围无法安全检查")
	}
	switch entry.Kind {
	case securefile.EntryDirectory:
		w.findDirectories(ctx, glob, limit, state)
	case securefile.EntryFile:
		state.visited = 1
		w.findEntry(glob, scope, entry.Kind, limit, state)
	case securefile.EntryLink:
		return failureResult(CodeLinkNotAllowed, "不能跟随查找范围链接")
	default:
		return failureResult(CodeUnsupportedType, "查找范围不是普通文件或目录")
	}
	if ctx.Err() != nil {
		return contextFailure(ctx.Err())
	}
	sort.Slice(state.entries, func(i, j int) bool { return state.entries[i]["path"].(string) < state.entries[j]["path"].(string) })
	for safeResultJSONSize(state.value()) > w.limits.ResultBytes && len(state.entries) > 0 {
		state.entries = state.entries[:len(state.entries)-1]
		state.incomplete("result_bytes", true)
	}
	publishProgress(ctx, Progress{Tool: ToolFind, Path: scope, Returned: len(state.entries), TruncationReason: state.reason})
	summary := fmt.Sprintf("已在 %s 找到 %d 个文件或目录", scope, len(state.entries))
	if !state.complete {
		summary += "（结果不完整）"
	}
	value := state.value()
	return Result{Value: value, Summary: summary, Reference: &Reference{Path: scope, Kind: "find_result", ContentHash: hashProjection(value)}}
}

func (w *Workspace) findEntry(glob pathGlob, path string, kind securefile.EntryType, limit int, state *findState) {
	if state.args.Type != "any" && state.args.Type != string(kind) || !glob.Match(path) {
		return
	}
	if len(state.entries) >= limit {
		state.incomplete("entry_limit", true)
		return
	}
	state.entries = append(state.entries, map[string]any{"path": path, "type": string(kind)})
	if safeResultJSONSize(state.value()) > w.limits.ResultBytes-256 {
		state.entries = state.entries[:len(state.entries)-1]
		state.incomplete("result_bytes", true)
	}
}

func (w *Workspace) findDirectories(ctx context.Context, glob pathGlob, limit int, state *findState) {
	var parent *ignoreLayer
	if state.ignore != nil {
		var ok bool
		parent, ok = state.ignore.scopeParents(ctx, state.args.Path)
		if !ok {
			return
		}
	}
	queue := []searchDirectory{{path: state.args.Path, ignore: parent}}
	for len(queue) > 0 && !state.stop && ctx.Err() == nil {
		current := queue[0]
		queue = queue[1:]
		remaining := w.limits.SearchEntries - state.visited
		if remaining <= 0 {
			state.incomplete("scan_entry_limit", true)
			return
		}
		if state.ignore != nil {
			var ok bool
			current.ignore, ok = state.ignore.load(ctx, current.path, current.ignore)
			if !ok {
				continue
			}
		}
		entries, skipped, complete, err := w.root.ReadDir(current.path, min(w.limits.DirectoryScanEntries, remaining))
		state.directories++
		if err != nil {
			state.other++
			state.incomplete("directory_unavailable", false)
			continue
		}
		state.visited += skipped
		state.other += skipped
		if !complete {
			state.incomplete("directory_entry_limit", false)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		for _, entry := range entries {
			if state.stop || ctx.Err() != nil {
				return
			}
			if state.visited >= w.limits.SearchEntries {
				state.incomplete("scan_entry_limit", true)
				return
			}
			state.visited++
			path := joinRelative(current.path, entry.Name)
			if _, err := normalizeModelPath(path, false); err != nil {
				state.other++
				state.incomplete("invalid_entry_path", false)
				continue
			}
			if !isArchivePath(state.args.Path) && isArchivePath(path) {
				continue
			}
			if state.ignore.excluded(current.ignore, path, entry.Type == securefile.EntryDirectory) {
				continue
			}
			switch entry.Type {
			case securefile.EntryLink:
				state.links++
			case securefile.EntryFile:
				w.findEntry(glob, path, entry.Type, limit, state)
			case securefile.EntryDirectory:
				w.findEntry(glob, path, entry.Type, limit, state)
				if current.depth >= min(64, w.limits.SearchDepth) {
					state.incomplete("depth_limit", false)
				} else {
					queue = append(queue, searchDirectory{path: path, depth: current.depth + 1, ignore: current.ignore})
				}
			default:
				state.other++
			}
		}
		if state.reports < maxSearchProgressReports-2 && (state.directories == 1 || state.directories%128 == 0) {
			publishProgress(ctx, Progress{Tool: ToolFind, Path: state.args.Path, Returned: len(state.entries), TruncationReason: state.reason})
			state.reports++
		}
	}
}
