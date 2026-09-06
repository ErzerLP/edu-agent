package workspace

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func (w *Workspace) discardQueryLocked(q *workspaceQuery) {
	q.closeScanner()
	for _, guard := range q.guards {
		if err := guard.Close(); err != nil {
			q.note("query_cleanup_failed")
		}
	}
	q.guards = nil
	w.queryBytes -= q.bytes
	delete(w.queries, q.id)
}

func (w *Workspace) expireQueriesLocked(now time.Time) {
	for _, q := range w.queries {
		if now.Sub(q.lastUsed) >= queryIdleTTL {
			w.discardQueryLocked(q)
		}
	}
}

func (w *Workspace) makeQueryRoomLocked() bool {
	if len(w.queries) < w.limits.QueryRecords {
		return true
	}
	var oldest *workspaceQuery
	for _, q := range w.queries {
		if q.done && (oldest == nil || q.lastUsed.Before(oldest.lastUsed)) {
			oldest = q
		}
	}
	if oldest == nil {
		return false
	}
	w.discardQueryLocked(oldest)
	return true
}

func (w *Workspace) newWorkspaceQuery(ctx context.Context, args queryArguments) (*workspaceQuery, error) {
	if !w.makeQueryRoomLocked() {
		return nil, operationFailure(CodeQueryCapacity, "active query limit reached")
	}
	q := &workspaceQuery{w: w, id: newQueryID(), args: args, lastUsed: time.Now(), observed: make(map[string]queryObservation)}
	w.queries[q.id] = q
	if !q.charge(int64(1024+len(args.path)+len(args.fingerprint)+safeResultJSONSize(args.find)+safeResultJSONSize(args.search)), 0) {
		w.discardQueryLocked(q)
		return nil, operationFailure(CodeQueryCapacity, "query retention is full")
	}
	info, err := q.observe(ctx, args.path)
	if err == nil {
		switch info.Kind {
		case securefile.EntryLink:
			err = securefile.ErrLink
		case securefile.EntryFile:
			if args.tool == ToolList {
				err = securefile.ErrNotDirectory
			}
		case securefile.EntryDirectory:
		default:
			err = securefile.ErrNotRegular
		}
	}
	if err != nil {
		w.discardQueryLocked(q)
		return nil, err
	}
	respect := false
	if args.tool == ToolFind {
		q.findGlob, _ = compilePathGlob(args.find.Pattern)
		respect = *args.find.RespectGitignore
	} else if args.tool == ToolSearch {
		q.matcher, _ = compileSearchMatcher(args.search)
		q.searchGlob, _ = compilePathGlob(*args.search.Glob)
		respect = *args.search.RespectGitignore
	}
	if respect {
		q.ignore = w.newGitignoreState(func(reason string, _ bool) { q.note(reason) })
		q.ignore.captureSnapshot = func(path string, snapshot securefile.Snapshot) error {
			return q.rememberFileHash(path, snapshot.Data)
		}
	}
	node := queryNode{path: args.path, kind: info.Kind}
	if info.Kind == securefile.EntryDirectory {
		if q.ignore != nil && args.path != "." {
			q.scopePending, q.scopeParts, q.scopeDirectory = true, strings.Split(args.path, "/"), "."
		}
		q.pendingDirectory = &node
	} else {
		q.pushNode(node)
	}
	return q, nil
}

func (w *Workspace) executeQuery(ctx context.Context, tool, raw string) Result {
	args, err := w.parseQueryArguments(tool, raw)
	if err != nil {
		return resultForError(err, "查询参数无效")
	}
	w.queriesMu.Lock()
	defer w.queriesMu.Unlock()
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	if w.root == nil {
		return failureResult(CodeWorkspaceUnavailable, "工作区不可用")
	}
	if w.queries == nil {
		w.queries = make(map[string]*workspaceQuery)
	}
	w.expireQueriesLocked(time.Now())
	var q *workspaceQuery
	if args.cursor == "" {
		q, err = w.newWorkspaceQuery(ctx, args)
		if err != nil {
			return resultForError(err, "无法建立安全查询")
		}
	} else {
		id, _, _ := parseQueryCursor(args.cursor)
		q = w.queries[id]
		if q == nil {
			return failureResult(CodeCursorExpired, "查询游标已过期；请显式发起新查询")
		}
		if q.args.tool != tool || q.args.fingerprint != args.fingerprint {
			return failureResult(CodeCursorMismatch, "查询游标不属于这些参数；请复用原参数或发起新查询")
		}
		if args.offset > len(q.rows) && !(args.tool == ToolList && args.offset == q.args.offset) {
			return failureResult(CodeInvalidArguments, "查询位置超出已保留结果")
		}
	}
	q.lastUsed = time.Now()
	if err := q.validate(ctx); err != nil {
		return w.queryFailureLocked(ctx, q, err)
	}
	step := &queryStep{}
	if q.ignore != nil {
		q.ignore.usedFiles, q.ignore.usedBytes = 0, 0
	}
	publishProgress(ctx, Progress{Tool: tool, Path: args.path})
	if !q.done && (args.offset+args.limit > len(q.rows) || tool == ToolSearch && *args.search.Output == "count") {
		err = q.advanceQuery(ctx, step, args.offset+args.limit)
	}
	if err == nil {
		err = q.fatal
	}
	var capacity *operationError
	if errors.As(err, &capacity) && capacity.code == CodeQueryCapacity {
		q.stop(CodeQueryCapacity)
		err = nil
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = q.validate(ctx)
	}
	if err != nil {
		return w.queryFailureLocked(ctx, q, err)
	}
	if !q.done && step.pause == "" && args.offset+args.limit <= len(q.rows) {
		step.pause = "entry_limit"
		if tool == ToolSearch {
			step.pause = "match_limit"
		}
	}
	result := q.queryResult(args.offset, args.limit, step.pause)
	count := len(q.rows)
	if tool == ToolSearch && *args.search.Output == "count" {
		count = q.matchedLines
	}
	reason := q.reason
	if reason == "" {
		reason = step.pause
	}
	publishProgress(ctx, Progress{Tool: tool, Path: args.path, ScannedFiles: q.scannedFiles, ScannedBytes: q.scannedBytes, Returned: count, Matches: count, TruncationReason: reason})
	return result
}

func (w *Workspace) queryFailureLocked(ctx context.Context, q *workspaceQuery, err error) Result {
	w.discardQueryLocked(q)
	if ctx.Err() != nil {
		return contextFailure(ctx.Err())
	}
	if errors.Is(err, securefile.ErrChanged) || errors.Is(err, securefile.ErrOutsideRoot) || errors.Is(err, securefile.ErrLink) {
		return failureResult(CodeCursorStale, "查询期间工作区、文件或忽略规则发生变化；旧游标已失效")
	}
	return resultForError(err, "查询已终止，旧游标不可继续")
}
