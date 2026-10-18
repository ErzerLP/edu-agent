package modelusage

import (
	"context"
	"sync"
)

// TaskContext is business attribution, never a counter. Empty SpaceID means global.
// Generation is required for persisted background work; synchronous callers
// retain their read-permit context and freeze it at the first outbound attempt.
type TaskContext struct {
	Kind, OriginID, Stage, SpaceID string
	Generation                     int64
}

type taskKey struct{}
type taskRoot struct {
	TaskContext
	mu sync.Mutex
}
type taskContext struct {
	root  *taskRoot
	stage string
}

func WithTask(ctx context.Context, task TaskContext) context.Context {
	return context.WithValue(ctx, taskKey{}, taskContext{root: &taskRoot{TaskContext: task}, stage: task.Stage})
}

// ForTask supplies an adapter's default purpose, retaining an enclosing root
// task (for example all activities of one offline preparation operation).
func ForTask(ctx context.Context, task TaskContext) context.Context {
	if current, ok := ctx.Value(taskKey{}).(taskContext); ok {
		current.stage = task.Stage
		return context.WithValue(ctx, taskKey{}, current)
	}
	return WithTask(ctx, task)
}
