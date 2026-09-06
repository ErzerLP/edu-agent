package workspace

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func (w *Workspace) parseQueryArguments(tool, raw string) (queryArguments, error) {
	q := queryArguments{tool: tool}
	if !utf8.ValidString(raw) || !validPatchJSONUnicode(raw) {
		return q, argumentError("query arguments must be valid UTF-8")
	}
	fields, err := patchJSONObject(raw)
	if err != nil {
		return q, err
	}
	for _, value := range fields {
		if strings.TrimSpace(string(value)) == "null" {
			return q, argumentError("query fields cannot be null")
		}
	}
	if value, exists := fields["cursor"]; exists {
		if json.Unmarshal(value, &q.cursor) != nil || q.cursor == "" || len(q.cursor) > 96 {
			return q, argumentError("query cursor is invalid")
		}
		if _, q.offset, exists = parseQueryCursor(q.cursor); !exists {
			return q, argumentError("query cursor is invalid")
		}
		delete(fields, "cursor")
	}
	data, _ := json.Marshal(fields)
	switch tool {
	case ToolList:
		var args listArguments
		if err := decodeArguments(string(data), &args); err != nil {
			return q, err
		}
		if args.Path == "" {
			args.Path = "."
		}
		q.path, err = normalizeModelPath(args.Path, true)
		if err != nil {
			return q, err
		}
		if args.Offset < 0 || args.Offset > w.limits.QueryEntries {
			return q, argumentError("list offset is invalid")
		}
		if q.cursor == "" {
			q.offset = args.Offset
		} else if _, supplied := fields["offset"]; supplied && args.Offset != q.offset {
			return q, argumentError("offset must match the cursor")
		}
		q.limit = w.limits.ListEntries
		q.fingerprint = queryFingerprint(listArguments{Path: q.path})
	case ToolFind:
		q.find, err = decodeFindArguments(string(data))
		if err != nil {
			return q, err
		}
		if q.find.Path == "" {
			q.find.Path = "."
		}
		if q.find.Type == "" {
			q.find.Type = "any"
		}
		q.path, err = normalizeModelPath(q.find.Path, true)
		if err != nil {
			return q, err
		}
		q.find.Path = q.path
		if q.find.Pattern == "" {
			return q, argumentError("find requires pattern")
		}
		if _, err := compilePathGlob(q.find.Pattern); err != nil {
			return q, err
		}
		if q.find.Type != "any" && q.find.Type != "file" && q.find.Type != "directory" {
			return q, argumentError("find type is invalid")
		}
		q.limit = min(200, w.limits.ListEntries)
		if q.find.Limit != nil {
			if *q.find.Limit < 1 || *q.find.Limit > q.limit {
				return q, argumentError("find limit is invalid")
			}
			q.limit = *q.find.Limit
		}
		q.find.Limit = &q.limit
		q.fingerprint = queryFingerprint(q.find)
	case ToolSearch:
		q.search, err = decodeSearchArguments(string(data))
		if err != nil {
			return q, err
		}
		if q.search.Path == "" {
			q.search.Path = "."
		}
		if q.search.Mode == "" {
			q.search.Mode = "literal"
		}
		if q.search.Case == "" {
			q.search.Case = "smart"
		}
		q.path, err = normalizeModelPath(q.search.Path, true)
		if err != nil {
			return q, err
		}
		q.search.Path = q.path
		if _, err := compileSearchMatcher(q.search); err != nil {
			return q, err
		}
		if err := validateGlobs(q.search.Include, q.search.Exclude); err != nil {
			return q, err
		}
		if _, err := compilePathGlob(*q.search.Glob); err != nil {
			return q, err
		}
		q.limit = w.limits.SearchMatches
		q.fingerprint = queryFingerprint(q.search)
	default:
		return q, argumentError("not a query tool")
	}
	return q, nil
}
