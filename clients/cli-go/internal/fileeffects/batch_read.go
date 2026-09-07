package fileeffects

import (
	"bytes"
	"context"
	"sort"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

func batchInfo(r *batchRecord) localartifact.Info {
	return localartifact.Info{ID: r.meta.ID, Kind: "receipt", Hash: r.meta.Hash, Bytes: r.meta.Bytes, Saved: r.persistent && r.saved.Bytes == r.meta.Bytes && r.persistenceError == ""}
}

// Even EOF reauthenticates metadata. A failed writer may observe the exact
// attempted metadata after an unknown publication, but that does NOT advance
// its confirmed watermark. Reading still uses the old authenticated prefix
// plus its one bounded, locally known append. No fallback after fence failure.
func (m *BatchManager) readable(ctx context.Context, owner string, s *batchOwner, r *batchRecord) error {
	if !r.persistent {
		return nil
	}
	if s.backend == nil || r.saved.Bytes == 0 {
		return batchError("unavailable")
	}
	data, err := batchReadBlob(ctx, s.backend, batchMetaName(r.meta.ID))
	if err != nil {
		return err
	}
	observed, err := m.decodeMeta(data, owner, r.meta.ID)
	if err != nil {
		return err
	}
	if !batchSameMeta(observed, r.saved) && (r.persistenceError == "" || !batchSameMeta(observed, r.meta)) {
		return batchError("corrupt")
	}
	return nil
}
func batchReadRange(ctx context.Context, s *batchOwner, r *batchRecord, offset, count int64) ([]byte, error) {
	result := make([]byte, int(count))
	copied := int64(0)
	refs := r.meta
	if r.persistent {
		refs = r.saved
	}
	base := int64(0)
	for _, plan := range []bool{true, false} {
		segments := refs.Events
		if plan {
			segments = refs.Plan
		}
		for index, ref := range segments {
			end := base + ref.Bytes
			if copied < count && offset+copied < end && offset+count > base {
				var data []byte
				var err error
				if r.persistent {
					data, err = batchReadSegment(ctx, s.backend, refs, plan, index)
					if err != nil {
						return nil, err
					}
				} else if plan {
					data = r.memoryPlan[index]
				} else {
					data = r.memoryEvents[index]
				}
				start := max(int64(0), offset+copied-base)
				take := min(ref.Bytes-start, count-copied)
				copy(result[copied:copied+take], data[start:start+take])
				copied += take
			}
			base = end
			if copied == count {
				break
			}
		}
		if copied == count {
			break
		}
	}
	if copied < count && r.persistent && r.persistenceError != "" {
		start := offset + copied - r.saved.Bytes
		if start < 0 || start > int64(len(r.suffix)) || count-copied > int64(len(r.suffix))-start {
			return nil, batchError("corrupt")
		}
		copy(result[copied:], r.suffix[start:start+count-copied])
		copied = count
	}
	if copied != count {
		return nil, batchError("corrupt")
	}
	if ctx.Err() != nil {
		return nil, batchError("canceled")
	}
	return result, nil
}
func (m *BatchManager) List(ctx context.Context, owner string) ([]localartifact.Info, error) {
	if err := m.validate(owner); err != nil {
		return nil, err
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, true)
	if err != nil {
		return nil, err
	}
	defer m.release(s)
	if s.backend != nil {
		if _, err = batchNames(ctx, s.backend, "result_bm_"); err != nil {
			return nil, err
		}
	}
	result := make([]localartifact.Info, 0, len(s.records))
	for _, r := range s.records {
		if err = m.readable(ctx, owner, s, r); err != nil {
			return nil, err
		}
		result = append(result, batchInfo(r))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (m *BatchManager) Read(ctx context.Context, owner, id string, offset int64, limit int) (localartifact.Page, error) {
	if err := m.validate(owner); err != nil {
		return localartifact.Page{}, err
	}
	if !batchValidID(id) || offset < 0 || limit < 1 || limit > 65536 {
		return localartifact.Page{}, batchError("invalid_arguments")
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return localartifact.Page{}, err
	}
	defer m.release(s)
	r, err := batchLookup(s, id)
	if err != nil {
		return localartifact.Page{}, err
	}
	if err = m.readable(ctx, owner, s, r); err != nil {
		return localartifact.Page{}, err
	}
	if offset > r.meta.Bytes {
		return localartifact.Page{}, batchError("invalid_arguments")
	}
	data, err := batchReadRange(ctx, s, r, offset, min(int64(limit), r.meta.Bytes-offset))
	if err != nil {
		return localartifact.Page{}, err
	}
	next := offset + int64(len(data))
	return localartifact.Page{Info: batchInfo(r), Data: data, Offset: offset, NextOffset: next, More: next < r.meta.Bytes}, nil
}
func (m *BatchManager) Search(ctx context.Context, owner, id, needle string, offset int64, limit int) (localartifact.SearchPage, error) {
	if err := m.validate(owner); err != nil {
		return localartifact.SearchPage{}, err
	}
	if !batchValidID(id) || offset < 0 || len(needle) < 1 || len(needle) > 512 || !utf8.ValidString(needle) || limit < 1 || limit > 100 {
		return localartifact.SearchPage{}, batchError("invalid_arguments")
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return localartifact.SearchPage{}, err
	}
	defer m.release(s)
	r, err := batchLookup(s, id)
	if err != nil {
		return localartifact.SearchPage{}, err
	}
	if err = m.readable(ctx, owner, s, r); err != nil {
		return localartifact.SearchPage{}, err
	}
	if offset > r.meta.Bytes {
		return localartifact.SearchPage{}, batchError("invalid_arguments")
	}
	count := min(int64(1<<20), r.meta.Bytes-offset)
	data, err := batchReadRange(ctx, s, r, offset, count)
	if err != nil {
		return localartifact.SearchPage{}, err
	}
	page := localartifact.SearchPage{Info: batchInfo(r), Offset: offset, Offsets: make([]int64, 0)}
	literal := []byte(needle)
	for candidate := 0; candidate+len(literal) <= len(data); {
		if ctx.Err() != nil {
			return localartifact.SearchPage{}, batchError("canceled")
		}
		hit := bytes.Index(data[candidate:], literal)
		if hit < 0 {
			break
		}
		hit += candidate
		page.Offsets = append(page.Offsets, offset+int64(hit))
		candidate = hit + 1
		if len(page.Offsets) == limit {
			page.Scanned = int64(hit + len(literal))
			page.NextOffset = offset + int64(candidate)
			page.More = page.NextOffset <= r.meta.Bytes-int64(len(literal))
			return page, nil
		}
	}
	page.Scanned = count
	if offset+count < r.meta.Bytes {
		page.NextOffset = offset + count - int64(len(literal)) + 1
		page.More = true
	} else {
		page.NextOffset = r.meta.Bytes
		// An active append-only stream can later complete a literal whose first
		// bytes are already visible. Keep those candidate starts without claiming
		// that another page is available at the current watermark.
		if r.active {
			page.NextOffset = max(offset, r.meta.Bytes-int64(len(literal))+1)
		}
	}
	return page, nil
}
