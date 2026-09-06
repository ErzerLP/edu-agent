package workspace

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	DefaultQueryMemoryBytes int64 = 64 << 20
	DefaultQueryEntries           = 100000
	DefaultQueryRecords           = 16
	maxQueryEntries               = 1000000
	queryIdleTTL                  = 10 * time.Minute
	CodeCursorExpired             = "cursor_expired"
	CodeCursorStale               = "cursor_stale"
	CodeCursorMismatch            = "cursor_mismatch"
	CodeQueryCapacity             = "query_capacity"
)

// A cursor identifies a retained query plus a record position, not an OS seek
// cookie. It is deliberately unusable in another Workspace instance.
func QueryCursorAt(cursor string, offset int) (string, bool) {
	id, _, ok := parseQueryCursor(cursor)
	if !ok || offset < 0 || offset > maxQueryEntries {
		return "", false
	}
	return id + "." + strconv.Itoa(offset), true
}

func parseQueryCursor(cursor string) (string, int, bool) {
	id, number, ok := strings.Cut(cursor, ".")
	if !ok || !strings.HasPrefix(id, "q1_") || len(id) < 20 || len(id) > 80 || len(number) == 0 || len(number) > 7 {
		return "", 0, false
	}
	for _, c := range id[3:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", 0, false
		}
	}
	n, err := strconv.Atoi(number)
	return id, n, err == nil && n >= 0 && n <= maxQueryEntries && strconv.Itoa(n) == number
}

type queryArguments struct {
	tool, path, fingerprint, cursor string
	offset, limit                   int
	find                            findArguments
	search                          searchArguments
}

type queryRecord struct {
	value   map[string]any
	context []map[string]any
}

type queryObservation struct {
	info    securefile.EntryInfo
	missing bool
}

type queryNode struct {
	path   string
	kind   securefile.EntryType
	depth  int
	ignore *ignoreLayer
}

type querySearchFile struct {
	path                                            string
	lines                                           []string
	line, index                                     int
	indices                                         [][]int
	lineReady, lineCounted, matched, indicesPartial bool
	retained, indexBytes                            int64
}

// workspaceQuery owns its scanner, immutable prefix records and all discovery
// state. Workspace.queriesMu serializes advancement, expiry and Close.
type workspaceQuery struct {
	w                                  *Workspace
	id                                 string
	args                               queryArguments
	lastUsed                           time.Time
	bytes                              int64
	units                              int
	rows                               []queryRecord
	observed                           map[string]queryObservation
	hashes                             map[string]string
	guards                             map[string]*securefile.DirectoryChangeGuard
	frontier                           queryHeap
	scanner                            *securefile.DirectoryScan
	scanning                           queryNode
	pendingDirectory                   *queryNode
	pendingFile                        *queryNode
	searchFile                         *querySearchFile
	ignore                             *gitignoreState
	scopePending                       bool
	scopeParts                         []string
	scopeIndex                         int
	scopeDirectory                     string
	scopeIgnore                        *ignoreLayer
	findGlob                           pathGlob
	matcher                            *regexp.Regexp
	searchGlob                         pathGlob
	done                               bool
	reason                             string
	fatal                              error
	visited, directories, scannedFiles int
	scannedBytes                       int64
	links, binary, large, other        int
	matchedLines, matchedFiles         int
}

type queryStep struct {
	entries, files, matches int
	bytes                   int64
	directory               string
	directoryEntries        int
	pause                   string
}

func (q *workspaceQuery) cursor(offset int) string { return q.id + "." + strconv.Itoa(offset) }

func (q *workspaceQuery) note(reason string) {
	if q.reason == "" {
		q.reason = reason
	}
}

func (q *workspaceQuery) stop(reason string) {
	q.note(reason)
	q.done = true
	q.closeScanner()
}

func (q *workspaceQuery) closeScanner() {
	if q.scanner != nil {
		if err := q.scanner.Close(); err != nil {
			q.note("directory_close_failed")
		}
		q.scanner = nil
	}
}

func (q *workspaceQuery) charge(bytes int64, units int) bool {
	if bytes < 0 || units < 0 || units > q.w.limits.QueryEntries-q.units || bytes > q.w.limits.QueryMemoryBytes-q.w.queryBytes {
		q.stop(CodeQueryCapacity)
		return false
	}
	q.units += units
	q.bytes += bytes
	q.w.queryBytes += bytes
	return true
}

func (q *workspaceQuery) release(bytes int64) {
	bytes = min(max(0, bytes), q.bytes)
	q.bytes -= bytes
	q.w.queryBytes -= bytes
}

func (q *workspaceQuery) appendRecord(value map[string]any, neighbors []map[string]any) bool {
	// Charge retained encoded data plus conservative map/slice overhead. This
	// is a logical retention budget, not an assertion about Go peak heap size.
	cost := int64(safeResultJSONSize(value) + safeResultJSONSize(neighbors) + 512 + len(neighbors)*256)
	if !q.charge(cost, 1) {
		return false
	}
	q.rows = append(q.rows, queryRecord{value: value, context: neighbors})
	return true
}

func (q *workspaceQuery) observe(ctx context.Context, path string) (securefile.EntryInfo, error) {
	info, err := q.w.root.Stat(ctx, path)
	missing := errors.Is(err, securefile.ErrNotFound)
	if err != nil && !missing {
		return info, err
	}
	current := queryObservation{info: info, missing: missing}
	if previous, exists := q.observed[path]; exists {
		if previous != current {
			return info, securefile.ErrChanged
		}
	} else {
		if !q.charge(int64(len(path)+384), 1) {
			return info, operationFailure(CodeQueryCapacity, "query retention is full")
		}
		q.observed[path] = current
	}
	return info, err
}

func (q *workspaceQuery) validate(ctx context.Context) error {
	for _, guard := range q.guards {
		if err := guard.Check(); err != nil {
			return err
		}
	}
	for path, expected := range q.observed {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := q.w.root.Stat(ctx, path)
		if expected.missing && errors.Is(err, securefile.ErrNotFound) {
			continue
		}
		if err != nil || expected.missing || info != expected.info {
			return securefile.ErrChanged
		}
		if expectedHash, exists := q.hashes[path]; exists {
			hash, err := q.w.root.HashEntry(ctx, path, expected.info, q.w.limits.FileBytes)
			if err != nil {
				return err
			}
			if hash != expectedHash {
				return securefile.ErrChanged
			}
		}
	}
	for _, guard := range q.guards {
		if err := guard.Check(); err != nil {
			return err
		}
	}
	return nil
}

func newQueryID() string { return "q1_" + rand.Text() }

func copyQueryObject(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func queryFingerprint(value any) string {
	data, _ := json.Marshal(value)
	return hashProjection(string(data))
}
