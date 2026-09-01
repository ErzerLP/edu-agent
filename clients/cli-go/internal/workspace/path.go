package workspace

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxModelPathBytes      = 4096
	maxModelPathRunes      = 1024
	maxModelPathComponents = 64
)

var windowsReservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

func normalizeModelPath(value string, allowRoot bool) (string, error) {
	if !utf8.ValidString(value) || len(value) == 0 || len(value) > maxModelPathBytes || utf8.RuneCountInString(value) > maxModelPathRunes {
		return "", operationFailure(CodeInvalidPath, "workspace path is invalid")
	}
	if value == "." {
		if allowRoot {
			return value, nil
		}
		return "", operationFailure(CodeInvalidPath, "workspace file path cannot be root")
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") || looksLikeWindowsAbsolutePath(value) {
		return "", operationFailure(CodePathOutsideWorkspace, "workspace path must be relative")
	}
	if strings.Contains(value, "\\") {
		return "", operationFailure(CodeInvalidPath, "workspace path must use slash separators")
	}
	components := strings.Split(value, "/")
	if len(components) > maxModelPathComponents {
		return "", operationFailure(CodeInvalidPath, "workspace path has too many components")
	}
	for _, component := range components {
		if component == ".." {
			return "", operationFailure(CodePathOutsideWorkspace, "workspace path cannot escape its root")
		}
		if component == "" || component == "." || invalidPathComponent(component) {
			return "", operationFailure(CodeInvalidPath, "workspace path contains an invalid component")
		}
	}
	return strings.Join(components, "/"), nil
}

func looksLikeWindowsAbsolutePath(value string) bool {
	if len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' {
		return true
	}
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, `//?/`) || strings.HasPrefix(lower, `//./`) || strings.HasPrefix(lower, `\\?\\`) || strings.HasPrefix(lower, `\\.\\`)
}

func invalidPathComponent(component string) bool {
	if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || strings.ContainsAny(component, `:<>"|?*`) {
		return true
	}
	base := component
	if index := strings.IndexByte(base, '.'); index >= 0 {
		base = base[:index]
	}
	if _, reserved := windowsReservedNames[strings.ToUpper(base)]; reserved {
		return true
	}
	for _, current := range component {
		if current == 0 || current == '/' || unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return true
		}
	}
	return false
}

func safeLabel(path string) string {
	label := filepath.Base(filepath.Clean(path))
	if label == "" || label == "." || label == string(filepath.Separator) {
		label = "root"
	}
	var builder strings.Builder
	for _, current := range label {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			continue
		}
		builder.WriteRune(current)
		if utf8.RuneCountInString(builder.String()) >= 48 {
			break
		}
	}
	label = strings.TrimSpace(builder.String())
	if label == "" {
		return "workspace"
	}
	return label
}

func joinRelative(directory, name string) string {
	if directory == "." {
		return name
	}
	return directory + "/" + name
}
