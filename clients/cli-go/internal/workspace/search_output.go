package workspace

import "fmt"

func decodeSearchArguments(raw string) (searchArguments, error) {
	output, contextLines, glob, respect := "content", 0, "*", false
	args := searchArguments{Output: &output, Context: &contextLines, Glob: &glob, RespectGitignore: &respect}
	if err := decodeArguments(raw, &args); err != nil {
		return args, err
	}
	// Initialized pointers distinguish an omitted default from explicit null.
	if args.RespectGitignore == nil {
		return args, argumentError("respect_gitignore must be boolean")
	}
	if args.Output == nil || args.Context == nil || args.Glob == nil ||
		(*args.Output != "content" && *args.Output != "files" && *args.Output != "count") ||
		*args.Context < 0 || *args.Context > 3 || *args.Output != "content" && *args.Context != 0 {
		return args, argumentError("search output or context is invalid")
	}
	return args, nil
}

func (s *searchState) incomplete(reason string, stop bool) {
	s.complete = false
	if s.reason == "" {
		s.reason = reason
	}
	s.stop = s.stop || stop
}

func (w *Workspace) searchResultFits(state *searchState) bool {
	// Reserve the same truncation metadata headroom for all payload modes.
	if safeResultJSONSize(searchResultValue(state.scope, *state)) > w.limits.ResultBytes-256 {
		state.incomplete("result_bytes", true)
		return false
	}
	return true
}

func (s *searchState) trimResult() bool {
	switch {
	case len(s.contextLines) > 0:
		s.contextLines = s.contextLines[:len(s.contextLines)-1]
	case len(s.matches) > 0:
		s.matches = s.matches[:len(s.matches)-1]
	case len(s.files) > 0:
		s.files = s.files[:len(s.files)-1]
	default:
		return false
	}
	return true
}

func (s searchState) summary() string {
	var summary string
	switch s.output {
	case "files":
		summary = fmt.Sprintf("在 %s 中找到 %d 个匹配文件", s.scope, len(s.files))
	case "count":
		summary = fmt.Sprintf("在 %s 中统计到 %d 个匹配行、%d 个匹配文件", s.scope, s.matchedLines, s.matchedFiles)
	default:
		summary = fmt.Sprintf("在 %s 中找到 %d 处匹配", s.scope, len(s.matches))
	}
	if !s.complete {
		summary += "（结果不完整）"
	}
	return summary
}
