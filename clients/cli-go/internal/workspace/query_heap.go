package workspace

import (
	"container/heap"
)

// Best-first traversal makes the emitted prefix globally path-ordered even
// when punctuation sorts before a directory separator (a.go before a/file).
type queryHeap []queryNode

func (h queryHeap) Len() int           { return len(h) }
func (h queryHeap) Less(i, j int) bool { return h[i].path < h[j].path }
func (h queryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *queryHeap) Push(x any)        { *h = append(*h, x.(queryNode)) }
func (h *queryHeap) Pop() any {
	old := *h
	n := len(old) - 1
	node := old[n]
	old[n] = queryNode{}
	*h = old[:n]
	return node
}

func (q *workspaceQuery) pushNode(node queryNode) bool {
	if !q.charge(int64(len(node.path)+192), 1) {
		return false
	}
	heap.Push(&q.frontier, node)
	return true
}

func (q *workspaceQuery) popNode() queryNode {
	node := heap.Pop(&q.frontier).(queryNode)
	q.release(int64(len(node.path) + 192))
	return node
}
