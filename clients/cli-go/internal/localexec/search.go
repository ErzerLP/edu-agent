package localexec

import (
	"bytes"
	"context"
	"unicode/utf8"
)

const maxSearchBytes = 1 << 20

// SearchPage is a bounded, independent byte cursor. NextOffset is the next
// candidate start, not the end of the last match; overlapping matches are kept.
// More refers only to candidates at the current readable waterline. An active
// or uncertain stream retains its needle-sized suffix for a later poll.
type SearchPage struct {
	Offsets    []int64
	Offset     int64
	NextOffset int64
	Scanned    int64
	Received   int64
	Retained   int64
	More       bool
	Truncated  bool
	Incomplete bool
}

// Search performs literal UTF-8 needle matching against raw output bytes. At
// most 1 MiB is read per call, with no index and no shared read/search cursor.
func (m *Manager) Search(ctx context.Context, owner, taskID, stream, needle string, offset int64, limit int) (SearchPage, error) {
	if len(needle) < 1 || len(needle) > 512 || !utf8.ValidString(needle) {
		return SearchPage{}, failure("invalid_needle")
	}
	if limit < 1 || limit > 100 {
		return SearchPage{}, failure("invalid_limit")
	}
	if ctx.Err() != nil {
		return SearchPage{}, failure("search_canceled")
	}
	// Freeze the upper scan bound, so concurrent writes do not turn one query
	// into an unbounded moving-target scan.
	m.mu.Lock()
	t, err := m.taskLocked(owner, taskID)
	if err != nil {
		m.mu.Unlock()
		return SearchPage{}, err
	}
	output, err := taskStream(t, stream)
	if err != nil {
		m.mu.Unlock()
		return SearchPage{}, err
	}
	if offset < 0 || offset > output.received {
		m.mu.Unlock()
		return SearchPage{}, failure("invalid_offset")
	}
	received, retained := output.received, m.readableLocked(t, output)
	incomplete, terminal := t.outputIncomplete, t.terminal
	m.mu.Unlock()
	count := min(int64(maxSearchBytes), max(int64(0), retained-offset))
	page := SearchPage{Offsets: make([]int64, 0), Offset: offset, NextOffset: offset,
		Received: received, Retained: retained, Truncated: retained < received, Incomplete: incomplete}
	// Even an empty scan/EOF must pass the same privacy fence as a byte read.
	raw, err := m.readOutput(ctx, owner, taskID, stream, offset, int(max(int64(1), count)))
	if err != nil {
		return SearchPage{}, err
	}
	if count == 0 {
		if offset >= retained {
			page.NextOffset = received
		}
		return page, nil
	}
	// A concurrent detach can reduce the readable range, but cannot invent a
	// prefix. Preserve the original observed waterline and report that gap.
	page.Retained = min(retained, raw.Retained)
	page.Truncated = page.Retained < received
	data := raw.Data
	n := len(needle)
	candidate := 0
	for candidate+n <= len(data) {
		if ctx.Err() != nil {
			return page, failure("search_canceled")
		}
		match := bytes.Index(data[candidate:], []byte(needle))
		if match < 0 {
			break
		}
		match += candidate
		page.Offsets = append(page.Offsets, offset+int64(match))
		candidate = match + 1
		if len(page.Offsets) == limit {
			page.NextOffset = offset + int64(candidate)
			page.Scanned = int64(match + n)
			page.More = page.NextOffset <= page.Retained-int64(n)
			return page, nil
		}
	}
	page.Scanned = int64(len(data))
	page.NextOffset = max(offset, offset+int64(len(data))-int64(n)+1)
	page.More = page.NextOffset <= page.Retained-int64(n)
	if !page.More {
		if page.Truncated || terminal && !incomplete {
			page.NextOffset = received
		}
	}
	return page, nil
}
