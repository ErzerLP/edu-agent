package workspace

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	maxIgnoreFileBytes    = 64 << 10
	maxIgnoreBytes        = 256 << 10
	maxIgnoreFiles        = 256
	maxIgnoreRules        = 4096
	maxIgnorePatternBytes = 1024
)

// gitignoreState is invocation-local. Rule layers are immutable and shared by
// queued siblings; neither absent files nor rules outside the visited tree are
// cached or eagerly loaded. Ignoring is discovery filtering, never authority.
type gitignoreState struct {
	root                  *securefile.Root
	limits                Limits
	files, rules, ignored int
	bytes                 int64
	usedFiles             int
	usedBytes             int64
	incomplete            func(string, bool)
}

type ignoreLayer struct {
	parent *ignoreLayer
	base   string
	rules  []ignoreRule
}

type ignoreRule struct {
	negate, directory, basename bool
	parts                       []*regexp.Regexp // nil denotes a whole-component **
}

func (w *Workspace) newGitignoreState(incomplete func(string, bool)) *gitignoreState {
	return &gitignoreState{root: w.root, limits: w.limits, incomplete: incomplete}
}

func (s *gitignoreState) addValue(value map[string]any) {
	if s == nil {
		return
	}
	value["respect_gitignore"] = true
	value["ignore_files"] = s.files
	value["ignore_bytes"] = s.bytes
	value["ignored_entries"] = s.ignored
}

func (s *gitignoreState) fail(reason string) (*ignoreLayer, bool) {
	// Only the affected subtree is unavailable. Already known siblings remain
	// useful, and counts must keep the caller's partial flag.
	s.incomplete(reason, false)
	return nil, false
}

// scopeParents checks every ancestor before entering an explicit directory.
// Explicit files do not use this discovery operation at all.
func (s *gitignoreState) scopeParents(ctx context.Context, scope string) (*ignoreLayer, bool) {
	var layer *ignoreLayer
	if scope == "." {
		return nil, true
	}
	current := "."
	for _, component := range strings.Split(scope, "/") {
		var ok bool
		layer, ok = s.load(ctx, current, layer)
		if !ok {
			return nil, false
		}
		current = joinRelative(current, component)
		if s.excluded(layer, current, true) {
			return nil, false
		}
	}
	return layer, true
}

func (s *gitignoreState) load(ctx context.Context, directory string, parent *ignoreLayer) (*ignoreLayer, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	name := joinRelative(directory, ".gitignore")
	info, err := s.root.Stat(ctx, name)
	if errors.Is(err, securefile.ErrNotFound) {
		return parent, true
	}
	if err != nil {
		return s.fail("gitignore_unavailable")
	}
	if info.Kind != securefile.EntryFile {
		return s.fail("gitignore_unsafe_type")
	}
	if s.files >= maxIgnoreFiles {
		return s.fail("gitignore_file_limit")
	}
	if s.usedFiles >= s.limits.SearchFiles {
		return s.fail("file_limit")
	}
	if info.Size < 0 || info.Size > min(int64(maxIgnoreFileBytes), s.limits.FileBytes) {
		return s.fail("gitignore_file_too_large")
	}
	// ReadSnapshot uses a one-byte growth lookahead. Reserve it as well so a
	// replaced/growing file cannot overrun either total byte budget.
	if info.Size >= int64(maxIgnoreBytes)-s.bytes {
		return s.fail("gitignore_bytes")
	}
	if info.Size >= s.limits.SearchBytes-s.usedBytes {
		return s.fail("scan_bytes")
	}
	s.files++
	snapshot, err := s.readSnapshot(name, info.Size)
	charged := snapshot.Size
	if err != nil {
		charged = info.Size + 1
	}
	s.bytes += charged
	if err != nil {
		return s.fail("gitignore_unavailable")
	}
	if ctx.Err() != nil {
		return nil, false
	}
	if !utf8.Valid(snapshot.Data) || strings.ContainsRune(string(snapshot.Data), 0) {
		return s.fail("gitignore_invalid_text")
	}
	layer := &ignoreLayer{parent: parent, base: directory}
	text := strings.TrimPrefix(string(snapshot.Data), "\ufeff")
	for _, line := range strings.Split(text, "\n") {
		if ctx.Err() != nil {
			return nil, false
		}
		rule, exists, err := parseIgnoreRule(strings.TrimSuffix(line, "\r"))
		if err != nil {
			return s.fail("gitignore_pattern_unsupported")
		}
		if !exists {
			continue
		}
		if s.rules >= maxIgnoreRules {
			return s.fail("gitignore_rule_limit")
		}
		s.rules++
		layer.rules = append(layer.rules, rule)
	}
	if len(layer.rules) == 0 {
		return parent, true
	}
	return layer, true
}

// readSnapshot shares the original invocation's file/byte allowance with
// content search. On an I/O failure the charged bytes are a conservative upper
// bound, since securefile intentionally returns no partially read body.
// The caller must reserve limit+1 bytes and one file before calling.
func (s *gitignoreState) readSnapshot(relative string, limit int64) (securefile.Snapshot, error) {
	s.usedFiles++
	snapshot, err := s.root.ReadSnapshot(relative, limit, false)
	if err != nil {
		s.usedBytes += limit + 1
	} else {
		s.usedBytes += snapshot.Size
	}
	return snapshot, err
}

func (s *gitignoreState) excluded(layer *ignoreLayer, relative string, directory bool) bool {
	if s == nil {
		return false
	}
	for current := layer; current != nil; current = current.parent {
		path := relative
		if current.base != "." {
			var found bool
			path, found = strings.CutPrefix(relative, current.base+"/")
			if !found {
				continue
			}
		}
		for i := len(current.rules) - 1; i >= 0; i-- {
			rule := current.rules[i]
			if (!rule.directory || directory) && rule.match(path) {
				if !rule.negate {
					s.ignored++
				}
				return !rule.negate
			}
		}
	}
	return false
}

func parseIgnoreRule(line string) (ignoreRule, bool, error) {
	rule := ignoreRule{}
	// Only unescaped trailing spaces are insignificant; leading spaces and
	// escaped #, !, whitespace and glob metacharacters remain literal.
	for strings.HasSuffix(line, " ") {
		backslashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			break
		}
		line = line[:len(line)-1]
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return rule, false, nil
	}
	if len(line) > maxIgnorePatternBytes {
		return rule, false, argumentError("ignore pattern too long")
	}
	if strings.HasPrefix(line, "!") {
		rule.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		rule.directory = true
		line = line[:len(line)-1]
	}
	anchored := strings.HasPrefix(line, "/")
	line = strings.TrimPrefix(line, "/")
	parts := strings.Split(line, "/")
	if len(parts) > maxModelPathComponents {
		return rule, false, argumentError("ignore pattern too deep")
	}
	rule.basename = !anchored && len(parts) == 1
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return rule, false, argumentError("ignore component unsupported")
		}
		if part == "**" {
			rule.parts = append(rule.parts, nil)
			continue
		}
		compiled, err := compileIgnoreComponent(part)
		if err != nil {
			return rule, false, err
		}
		rule.parts = append(rule.parts, compiled)
	}
	return rule, true, nil
}

// Translate Git-style component wildcards, not model path globs. RE2 bounds
// matching time. POSIX named/collating/equivalence classes are deliberately
// rejected (fail closed), not misinterpreted as wider or narrower rules.
func compileIgnoreComponent(pattern string) (*regexp.Regexp, error) {
	runes := []rune(pattern)
	var out strings.Builder
	out.WriteString("^(?:")
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '\\':
			i++
			if i == len(runes) {
				return nil, argumentError("dangling ignore escape")
			}
			out.WriteString(regexp.QuoteMeta(string(runes[i])))
		case '*':
			out.WriteString("[^/]*")
		case '?':
			out.WriteString("[^/]")
		case '[':
			out.WriteByte('[')
			i++
			if i < len(runes) && (runes[i] == '!' || runes[i] == '^') {
				out.WriteByte('^')
				i++
			}
			start := i
			// A ] first in the class is literal in Git patterns.
			if i < len(runes) && runes[i] == ']' {
				out.WriteString(`\]`)
				i++
			}
			for ; i < len(runes) && runes[i] != ']'; i++ {
				if runes[i] == '[' && i+1 < len(runes) && strings.ContainsRune(":.=", runes[i+1]) {
					return nil, argumentError("named ignore class unsupported")
				}
				if runes[i] == '\\' {
					i++
					if i == len(runes) {
						return nil, argumentError("dangling ignore class escape")
					}
					if runes[i] == '-' {
						out.WriteString(`\-`)
					} else {
						out.WriteString(regexp.QuoteMeta(string(runes[i])))
					}
				} else if runes[i] == '[' || runes[i] == '^' {
					out.WriteString(`\` + string(runes[i]))
				} else {
					out.WriteRune(runes[i])
				}
			}
			if i == len(runes) || i == start {
				return nil, argumentError("invalid ignore class")
			}
			out.WriteByte(']')
		default:
			out.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	out.WriteString(")$")
	compiled, err := regexp.Compile(out.String())
	if err != nil {
		return nil, argumentError("invalid ignore pattern")
	}
	return compiled, nil
}

func (r ignoreRule) match(relative string) bool {
	parts := strings.Split(relative, "/")
	if r.basename {
		return r.parts[0] == nil || r.parts[0].MatchString(parts[len(parts)-1])
	}
	previous := make([]bool, len(parts)+1)
	previous[0] = true
	for i, pattern := range r.parts {
		next := make([]bool, len(parts)+1)
		if pattern == nil {
			// abc/** applies inside abc, not to abc itself. Interior **
			// matches zero or more directories.
			if i != len(r.parts)-1 {
				next[0] = previous[0]
			}
			for j := 1; j <= len(parts); j++ {
				if i == len(r.parts)-1 {
					next[j] = previous[j-1] || next[j-1]
				} else {
					next[j] = previous[j] || next[j-1]
				}
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				next[j] = previous[j-1] && pattern.MatchString(parts[j-1])
			}
		}
		previous = next
	}
	return previous[len(parts)]
}
