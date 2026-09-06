package workspace

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func (q *workspaceQuery) ensureDirectoryGuard(ctx context.Context, path string) error {
	if guard := q.guards[path]; guard != nil {
		return guard.Check()
	}
	cost := int64(len(path) + 256)
	if !q.charge(cost, 0) {
		return operationFailure(CodeQueryCapacity, "query retention is full")
	}
	guard, err := q.w.root.WatchDirectory(ctx, path)
	if err != nil {
		q.release(cost)
		if errors.Is(err, securefile.ErrNotFound) {
			return securefile.ErrChanged
		}
		if ctx.Err() != nil || errors.Is(err, securefile.ErrChanged) || errors.Is(err, securefile.ErrOutsideRoot) || errors.Is(err, securefile.ErrLink) {
			return err
		}
		return operationFailure("query_watch_unavailable", "native directory change observation is unavailable")
	}
	if q.guards == nil {
		q.guards = make(map[string]*securefile.DirectoryChangeGuard)
	}
	q.guards[path] = guard
	return nil
}

func (q *workspaceQuery) finishQueryDirectory() error {
	guard, err := q.scanner.DetachChangeGuard()
	if err != nil {
		return err
	}
	q.scanner = nil
	if previous := q.guards[q.scanning.path]; previous != nil {
		// The earlier guard covers rule loading as well as enumeration. Keep
		// that uninterrupted observation and release only the duplicate.
		return guard.Close()
	}
	if q.guards == nil {
		q.guards = make(map[string]*securefile.DirectoryChangeGuard)
	}
	q.guards[q.scanning.path] = guard
	return nil
}
