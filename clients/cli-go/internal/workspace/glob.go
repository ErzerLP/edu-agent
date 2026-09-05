package workspace

import (
	pathpkg "path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pathGlob matches slash paths, with ** only acting recursively as a whole
// component. A slash-free pattern matches the basename at any depth.
type pathGlob struct {
	parts    []string
	basename bool
}

func compilePathGlob(pattern string) (pathGlob, error) {
	if pattern == "" || len(pattern) > 256 || !utf8.ValidString(pattern) || strings.Contains(pattern, "\\") || strings.HasPrefix(pattern, "/") {
		return pathGlob{}, argumentError("path glob is invalid")
	}
	for _, r := range pattern {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return pathGlob{}, argumentError("path glob contains controls")
		}
	}
	parts := strings.Split(pattern, "/")
	if len(parts) > maxModelPathComponents {
		return pathGlob{}, argumentError("path glob is too deep")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return pathGlob{}, argumentError("path glob has invalid components")
		}
		if _, err := pathpkg.Match(part, ""); err != nil {
			return pathGlob{}, argumentError("path glob syntax is invalid")
		}
	}
	return pathGlob{parts: parts, basename: len(parts) == 1}, nil
}

func (g pathGlob) Match(relative string) bool {
	if g.basename {
		matched, _ := pathpkg.Match(g.parts[0], pathpkg.Base(relative))
		return matched
	}
	parts := strings.Split(relative, "/")
	// Dynamic programming avoids exponential recursion with repeated **.
	previous := make([]bool, len(parts)+1)
	previous[0] = true
	for _, pattern := range g.parts {
		next := make([]bool, len(parts)+1)
		if pattern == "**" {
			next[0] = previous[0]
			for index := 1; index <= len(parts); index++ {
				next[index] = previous[index] || next[index-1]
			}
		} else {
			for index := 1; index <= len(parts); index++ {
				matched, _ := pathpkg.Match(pattern, parts[index-1])
				next[index] = previous[index-1] && matched
			}
		}
		previous = next
	}
	return previous[len(parts)]
}
