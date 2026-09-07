package fileeffects

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

// verifyRecord authenticates every referenced prefix, its entire raw log hash,
// and the complete state machine. It never discovers event segments or derives
// a higher watermark from a tail, orphan, or an execution object's memory.
func (m *BatchManager) verifyRecord(ctx context.Context, backend localartifact.Store, meta batchMetadata, charge int64) (*batchRecord, error) {
	r := &batchRecord{meta: meta, saved: batchCloneMeta(meta), persistent: true, restored: true, charge: charge, entries: make([]batchEntry, 0, meta.Items)}
	whole := sha256.New()
	var validator *batchPlanValidator
	rootSeen := false
	for i := range meta.Plan {
		data, err := batchReadSegment(ctx, backend, meta, true, i)
		if err != nil {
			return nil, err
		}
		_, _ = whole.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			return nil, batchError("corrupt")
		}
		for _, line := range bytes.Split(data[:len(data)-1], []byte{'\n'}) {
			if ctx.Err() != nil {
				return nil, batchError("canceled")
			}
			if len(line) > batchLineBytes {
				return nil, batchError("corrupt")
			}
			if !rootSeen {
				var v batchRootLine
				if err = batchDecode(line, &v); err != nil {
					return nil, err
				}
				if v.Type != "root" {
					return nil, batchError("corrupt")
				}
				validator, err = newBatchPlanValidator(v.Root)
				if err != nil {
					return nil, batchError("corrupt")
				}
				rootSeen = true
				continue
			}
			var v batchPlanLine
			if err = batchDecode(line, &v); err != nil {
				return nil, err
			}
			if v.Type != "plan" || v.Index != len(r.entries) || v.Index >= meta.Items || validator.add(v.Index, v.Item) != nil {
				return nil, batchError("corrupt")
			}
			r.entries = append(r.entries, batchEntry{File: v.Item.Source.Kind == "file", Bytes: v.Item.Bytes})
		}
	}
	if !rootSeen || len(r.entries) != meta.Items || batchHashState(whole) != meta.PlanHash {
		return nil, batchError("corrupt")
	}
	sequence, started, completed, unchanged, unknown := 0, 0, 0, 0, 0
	pending, finished, complete := false, false, false
	code := ""
	for i := range meta.Events {
		data, err := batchReadSegment(ctx, backend, meta, false, i)
		if err != nil {
			return nil, err
		}
		_, _ = whole.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			return nil, batchError("corrupt")
		}
		for _, line := range bytes.Split(data[:len(data)-1], []byte{'\n'}) {
			if ctx.Err() != nil {
				return nil, batchError("canceled")
			}
			if len(line)+1 > batchEventBytes || finished {
				return nil, batchError("corrupt")
			}
			var kind struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &kind) != nil {
				return nil, batchError("corrupt")
			}
			sequence++
			switch kind.Type {
			case "pending":
				var v batchPendingLine
				if err = batchDecode(line, &v); err != nil {
					return nil, err
				}
				if v.Sequence != sequence || v.Index != started || started >= meta.Items || pending || completed != started {
					return nil, batchError("corrupt")
				}
				started++
				unknown++
				pending = true
			case "actual":
				var v batchActualLine
				if err = batchDecode(line, &v); err != nil {
					return nil, err
				}
				if v.Sequence != sequence || !pending || v.Index != started-1 || !batchValidActual(r.entries[v.Index], v.Actual) {
					return nil, batchError("corrupt")
				}
				pending = false
				switch v.Actual.Outcome {
				case "completed":
					completed++
					unknown--
				case "unchanged":
					unchanged++
					unknown--
				}
			case "end":
				var v batchEndLine
				if err = batchDecode(line, &v); err != nil {
					return nil, err
				}
				if v.Sequence != sequence || !batchCode(v.Code) || v.Complete && completed != meta.Items {
					return nil, batchError("corrupt")
				}
				finished = true
				complete = v.Complete
				code = v.Code
			default:
				// Preserve explicit future-version errors, even for a new line type.
				var v batchEndLine
				if err = batchDecode(line, &v); err != nil {
					return nil, err
				}
				return nil, batchError("corrupt")
			}
		}
	}
	if batchHashState(whole) != meta.Hash || sequence != meta.Sequence || started != meta.Started || completed != meta.Completed || unchanged != meta.Unchanged || unknown != meta.Unknown || finished != meta.Finished || complete != meta.Complete || code != meta.Code {
		return nil, batchError("corrupt")
	}
	return r, nil
}

// Bind is transactional in memory: failed authentication, budget reservation,
// or state-machine validation retains the previous binding and catalog. All
// restored records are read-only. Identity-only markers survive receipt aging.
func (m *BatchManager) Bind(ctx context.Context, owner string, backend localartifact.Store) error {
	if err := m.validate(owner); err != nil {
		return err
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, true)
	if err != nil {
		return err
	}
	defer m.release(s)
	for _, r := range s.records {
		if r.active {
			return batchError("writer_active")
		}
	}
	if backend == nil {
		var refund int64
		count := 0
		for id, r := range s.records {
			if r.persistent {
				refund += r.charge
				delete(s.records, id)
			}
		}
		for id, c := range s.calls {
			if c.persistent {
				refund += batchIdentityMemory
				count++
				delete(s.calls, id)
			}
		}
		s.backend = nil
		s.charge -= refund
		m.refund(refund, count)
		return nil
	}
	markerNames, err := batchNames(ctx, backend, "result_bi_")
	if err != nil {
		return err
	}
	if len(markerNames) > m.options.MaxRecords {
		return batchError("limit")
	}
	metaNames, err := batchNames(ctx, backend, "result_bm_")
	if err != nil {
		return err
	}
	if len(metaNames) > len(markerNames) {
		return batchError("corrupt")
	}
	calls := make(map[string]*batchCall, len(markerNames))
	records := make(map[string]*batchRecord, len(metaNames))
	var reserved int64
	committed := false
	defer func() {
		if !committed {
			m.refund(reserved, 0)
		}
	}()
	if err = m.reserve(int64(len(markerNames))*batchIdentityMemory, 0); err != nil {
		return err
	}
	reserved += int64(len(markerNames)) * batchIdentityMemory
	for _, name := range markerNames {
		data, e := batchReadBlob(ctx, backend, name)
		if e != nil {
			return e
		}
		v, e := batchDecodeMarker(data, owner, name)
		if e != nil {
			return e
		}
		calls[v.CallHash] = &batchCall{marker: v, confirmed: true, persistent: true}
	}
	for _, name := range metaNames {
		id := strings.TrimPrefix(name, "result_bm_")
		if !batchValidID(id) {
			return batchError("corrupt")
		}
		data, e := batchReadBlob(ctx, backend, name)
		if e != nil {
			return e
		}
		meta, e := m.decodeMeta(data, owner, id)
		if e != nil {
			return e
		}
		c := calls[meta.CallHash]
		if c == nil || c.marker.ID != id || c.marker.OwnerHash != meta.OwnerHash {
			return batchError("corrupt")
		}
		charge, e := batchRecordCharge(meta.PlanBytes, meta.Items, len(meta.Plan), true)
		if e != nil {
			return e
		}
		if e = m.reserve(charge, 0); e != nil {
			return e
		}
		reserved += charge
		r, e := m.verifyRecord(ctx, backend, meta, charge)
		if e != nil {
			return e
		}
		records[id] = r
	}
	if ctx.Err() != nil {
		return batchError("canceled")
	}
	m.mu.Lock()
	if m.count-len(s.calls)+len(calls) > m.options.MaxRecords {
		m.mu.Unlock()
		return batchError("limit")
	}
	m.memory -= s.charge
	m.count += len(calls) - len(s.calls)
	s.charge = reserved
	s.calls = calls
	s.records = records
	s.backend = backend
	committed = true
	m.mu.Unlock()
	return nil
}
