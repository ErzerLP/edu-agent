package localartifact

import (
	"bytes"
	"context"
	"unicode/utf8"
)

const maxSearchBytes = 1 << 20

// readable requires the owner's gate. Saved records always reauthenticate
// metadata, even for an empty artifact or EOF, so retained metadata cannot
// bypass key revocation or a generation change. Body reads verify segment hashes.
func (m *Manager) readable(ctx context.Context, owner, id string, offset int64, state *ownerState) (*record, error) {
	m.mu.Lock()
	r := m.records[id]
	m.mu.Unlock()
	if r == nil || r.owner != owner {
		return nil, failure("artifact_not_found")
	}
	if offset < 0 || offset > r.info.Bytes {
		return nil, failure("artifact_invalid_arguments")
	}
	if r.info.Saved {
		if state.backend == nil {
			return nil, failure("artifact_unavailable")
		}
		data, err := readBlob(ctx, state.backend, metadataName(id))
		if err != nil {
			return nil, err
		}
		meta, err := decodeMetadata(data, owner, id)
		if err != nil {
			return nil, err
		}
		if !sameMetadata(meta, r.meta) {
			return nil, failure("artifact_corrupt")
		}
	}
	return r, nil
}

// readRange is bounded by the caller's page/scan budget, never by a metadata
// allocation request. Segment blobs can add at most two edge-segment reads to
// the logical scan. Returned data never aliases manager or Store memory.
func readRange(ctx context.Context, backend Store, r *record, offset, count int64) ([]byte, error) {
	data := make([]byte, int(count))
	if !r.info.Saved {
		copy(data, r.body[offset:offset+count])
		if ctx.Err() != nil {
			return nil, failure("artifact_canceled")
		}
		return data, nil
	}
	for copied := int64(0); copied < count; {
		position := offset + copied
		index := int(position / segmentBytes)
		segment, err := readSegment(ctx, backend, r.meta, index)
		if err != nil {
			return nil, err
		}
		start := position % segmentBytes
		keep := min(count-copied, int64(len(segment))-start)
		copy(data[copied:copied+keep], segment[start:start+keep])
		copied += keep
	}
	return data, nil
}

// Read returns exact raw bytes; rendering/UTF-8/control-character policy belongs
// to the caller. Each reader supplies its own independent byte cursor.
func (m *Manager) Read(ctx context.Context, owner, id string, offset int64, limit int) (Page, error) {
	if err := m.validate(owner); err != nil {
		return Page{}, err
	}
	if !validID(id) || offset < 0 || limit < 1 || limit > 65536 {
		return Page{}, failure("artifact_invalid_arguments")
	}
	ctx, cancel := boundedContext(ctx, operationTimeout)
	defer cancel()
	state, err := m.acquire(ctx, owner, false)
	if err != nil {
		return Page{}, err
	}
	defer m.release(owner, state)
	r, err := m.readable(ctx, owner, id, offset, state)
	if err != nil {
		return Page{}, err
	}
	count := min(int64(limit), r.info.Bytes-offset)
	data, err := readRange(ctx, state.backend, r, offset, count)
	if err != nil {
		return Page{}, err
	}
	next := offset + int64(len(data))
	return Page{Info: r.info, Data: data, Offset: offset, NextOffset: next, More: next < r.info.Bytes}, nil
}

// Search scans at most 1 MiB of raw bytes for a case-sensitive literal UTF-8
// needle. Offsets are byte positions (including overlapping matches). At the
// hit limit continuation starts at last match+1; at the scan limit it retains
// needle-length-1 bytes as candidate starts for cross-page matches. Scanned is
// the examined logical byte range, not backend I/O or number of returned hits.
func (m *Manager) Search(ctx context.Context, owner, id, needle string, offset int64, limit int) (SearchPage, error) {
	if err := m.validate(owner); err != nil {
		return SearchPage{}, err
	}
	if !validID(id) || offset < 0 || len(needle) < 1 || len(needle) > 512 || !utf8.ValidString(needle) || limit < 1 || limit > 100 {
		return SearchPage{}, failure("artifact_invalid_arguments")
	}
	ctx, cancel := boundedContext(ctx, operationTimeout)
	defer cancel()
	state, err := m.acquire(ctx, owner, false)
	if err != nil {
		return SearchPage{}, err
	}
	defer m.release(owner, state)
	r, err := m.readable(ctx, owner, id, offset, state)
	if err != nil {
		return SearchPage{}, err
	}
	count := min(int64(maxSearchBytes), r.info.Bytes-offset)
	data, err := readRange(ctx, state.backend, r, offset, count)
	if err != nil {
		return SearchPage{}, err
	}
	page := SearchPage{Info: r.info, Offsets: make([]int64, 0), Offset: offset, NextOffset: offset}
	literal := []byte(needle)
	for candidate := 0; candidate+len(literal) <= len(data); {
		if ctx.Err() != nil {
			return SearchPage{}, failure("artifact_canceled")
		}
		match := bytes.Index(data[candidate:], literal)
		if match < 0 {
			break
		}
		match += candidate
		page.Offsets = append(page.Offsets, offset+int64(match))
		candidate = match + 1
		if len(page.Offsets) == limit {
			page.Scanned = int64(match + len(literal))
			page.NextOffset = offset + int64(candidate)
			page.More = page.NextOffset <= r.info.Bytes-int64(len(literal))
			return page, nil
		}
	}
	page.Scanned = count
	if offset+count == r.info.Bytes {
		page.NextOffset = r.info.Bytes
	} else {
		page.NextOffset = offset + count - int64(len(literal)) + 1
		page.More = true
	}
	return page, nil
}
