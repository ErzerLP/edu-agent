package workspace

import (
	"context"
	"sync"
)

type mutationWaiter struct {
	ready chan struct{}
}

type mutationQueue struct {
	held    bool
	waiters []*mutationWaiter
}

type mutationQueues struct {
	mu      sync.Mutex
	targets map[string]*mutationQueue
}

func (q *mutationQueues) acquire(ctx context.Context, path string) (func(), error) {
	waiter := &mutationWaiter{ready: make(chan struct{})}
	q.mu.Lock()
	if q.targets == nil {
		q.targets = make(map[string]*mutationQueue)
	}
	target := q.targets[path]
	if target == nil {
		target = &mutationQueue{}
		q.targets[path] = target
	}
	if !target.held {
		target.held = true
		q.mu.Unlock()
		return q.releaseFunc(path), nil
	}
	target.waiters = append(target.waiters, waiter)
	q.mu.Unlock()

	select {
	case <-waiter.ready:
		if err := ctx.Err(); err != nil {
			q.release(path)
			return nil, err
		}
		return q.releaseFunc(path), nil
	case <-ctx.Done():
		q.mu.Lock()
		target := q.targets[path]
		removed := false
		if target != nil {
			for index, current := range target.waiters {
				if current == waiter {
					target.waiters = append(target.waiters[:index], target.waiters[index+1:]...)
					removed = true
					break
				}
			}
		}
		q.mu.Unlock()
		if !removed {
			q.release(path)
		}
		return nil, ctx.Err()
	}
}

func (q *mutationQueues) releaseFunc(path string) func() {
	var once sync.Once
	return func() { once.Do(func() { q.release(path) }) }
}

func (q *mutationQueues) release(path string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	target := q.targets[path]
	if target == nil || !target.held {
		return
	}
	if len(target.waiters) == 0 {
		target.held = false
		delete(q.targets, path)
		return
	}
	next := target.waiters[0]
	target.waiters = target.waiters[1:]
	close(next.ready)
}
