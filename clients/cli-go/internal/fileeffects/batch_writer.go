package fileeffects

import (
	"context"
	"crypto/sha256"
)

// Begin consumes a new call identity only after validating the complete plan
// and reserving all in-process budgets. Every immutable plan segment and the
// initial metadata are confirmed before it exposes a writable journal.
func (m *BatchManager) Begin(ctx context.Context, owner, callID string, plan BatchPlan) (string, error) {
	if err := m.validate(owner); err != nil {
		return "", err
	}
	if !batchValidCall(callID) {
		return "", batchError("invalid_arguments")
	}
	if len(plan.Items) < 1 || len(plan.Items) > m.options.Entries {
		return "", batchError("limit")
	}
	validator, err := newBatchPlanValidator(plan.Root)
	if err != nil {
		return "", err
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, true)
	if err != nil {
		return "", err
	}
	defer m.release(s)
	marker := batchMakeMarker(owner, callID)
	if s.calls[marker.CallHash] != nil {
		return "", batchError("duplicate_call")
	}
	var planBytes int64
	segments, used := 0, 0
	measure := func(line []byte) error {
		if len(line) > batchLineBytes {
			return batchError("limit")
		}
		planBytes += int64(len(line))
		if planBytes > m.options.PlanBytes {
			return batchError("limit")
		}
		if used == 0 || used+len(line) > batchSegmentBytes {
			segments++
			used = 0
		}
		used += len(line)
		return nil
	}
	if err = measure(batchLine(batchRootLine{Version: 1, Type: "root", Root: plan.Root})); err != nil {
		return "", err
	}
	for i, item := range plan.Items {
		if ctx.Err() != nil {
			return "", batchError("canceled")
		}
		// Reject oversized external strings before allocating encoded copies.
		if len(item.Source.Path) > 4096 || len(item.Target.Path) > 4096 || len(item.Source.Version) > 73 || len(item.Source.Kind) > 9 || len(item.Target.Kind) > 9 || item.Target.Version != "" {
			return "", batchError("invalid_plan")
		}
		if err = measure(batchLine(batchPlanLine{Version: 1, Type: "plan", Index: i, Item: item})); err != nil {
			return "", err
		}
	}
	charge, err := batchRecordCharge(planBytes, len(plan.Items), segments, s.backend != nil)
	if err != nil {
		return "", err
	}
	if err = m.reserve(charge, 0); err != nil {
		return "", err
	}
	retained := false
	defer func() {
		if !retained {
			m.refund(charge, 0)
		}
	}()
	entries := make([]batchEntry, len(plan.Items))
	for i, item := range plan.Items {
		if ctx.Err() != nil {
			return "", batchError("canceled")
		}
		if err = validator.add(i, item); err != nil {
			return "", err
		}
		entries[i] = batchEntry{File: item.Source.Kind == "file", Bytes: item.Bytes}
	}
	known, err := m.confirmCall(ctx, owner, callID, s)
	if err != nil {
		return marker.ID, err
	}
	if known {
		return marker.ID, batchError("duplicate_call")
	}
	r := &batchRecord{entries: entries, hasher: sha256.New(), charge: charge, persistent: s.backend != nil}
	r.meta = batchMetadata{Version: 1, ID: marker.ID, OwnerHash: marker.OwnerHash, CallHash: marker.CallHash, Items: len(entries), Plan: make([]batchSegment, 0, segments), Events: make([]batchSegment, 0)}
	s.records[marker.ID] = r
	s.charge += charge
	retained = true
	buffer := make([]byte, 0, batchSegmentBytes)
	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		index := len(r.meta.Plan)
		ref := batchSegment{Bytes: int64(len(buffer)), Hash: batchHash(buffer)}
		if s.backend != nil {
			if e := batchWriteBlob(ctx, s.backend, batchSegmentName(marker.ID, true, index), buffer); e != nil {
				return e
			}
		} else {
			r.memoryPlan = append(r.memoryPlan, append([]byte(nil), buffer...))
		}
		r.meta.Plan = append(r.meta.Plan, ref)
		_, _ = r.hasher.Write(buffer)
		r.meta.PlanBytes += ref.Bytes
		r.meta.Bytes += ref.Bytes
		buffer = buffer[:0]
		return nil
	}
	appendPlan := func(line []byte) error {
		if ctx.Err() != nil {
			return batchError("canceled")
		}
		if len(buffer)+len(line) > batchSegmentBytes {
			if e := flush(); e != nil {
				return e
			}
		}
		buffer = append(buffer, line...)
		return nil
	}
	if err = appendPlan(batchLine(batchRootLine{Version: 1, Type: "root", Root: plan.Root})); err == nil {
		for i, item := range plan.Items {
			if err = appendPlan(batchLine(batchPlanLine{Version: 1, Type: "plan", Index: i, Item: item})); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = flush()
	}
	if err != nil {
		return marker.ID, batchFail(r, err)
	}
	r.meta.PlanHash = batchHashState(r.hasher)
	r.meta.Hash = r.meta.PlanHash
	if s.backend != nil {
		if err = batchSaveMeta(ctx, s.backend, r.meta); err != nil {
			return marker.ID, batchFail(r, err)
		}
		r.saved = batchCloneMeta(r.meta)
	}
	r.active = true
	return marker.ID, nil
}

// appendEvent is called only after validating and recording the actual local
// state transition. It appends bytes monotonically and advances SavedBytes
// only after the tail AND the metadata prefix acknowledgement both succeed.
func batchAppendEvent(ctx context.Context, s *batchOwner, r *batchRecord, line []byte) error {
	if len(line) > batchEventBytes {
		return batchFail(r, batchError("limit"))
	}
	if len(r.tail) == 0 || len(r.tail)+len(line) > batchSegmentBytes {
		r.tail = make([]byte, 0, batchSegmentBytes)
		r.meta.Events = append(r.meta.Events, batchSegment{})
		if !r.persistent {
			r.memoryEvents = append(r.memoryEvents, nil)
		}
	}
	r.tail = append(r.tail, line...)
	index := len(r.meta.Events) - 1
	r.meta.Events[index] = batchSegment{Bytes: int64(len(r.tail)), Hash: batchHash(r.tail)}
	_, _ = r.hasher.Write(line)
	r.meta.Bytes += int64(len(line))
	r.meta.Hash = batchHashState(r.hasher)
	if !r.persistent {
		r.memoryEvents[index] = r.tail
		return nil
	}
	err := batchWriteBlob(ctx, s.backend, batchSegmentName(r.meta.ID, false, index), r.tail)
	if err == nil {
		err = batchSaveMeta(ctx, s.backend, r.meta)
	}
	if err != nil {
		r.suffix = append([]byte(nil), line...)
		return batchFail(r, err)
	}
	r.saved = batchCloneMeta(r.meta)
	return nil
}

func (m *BatchManager) Pending(ctx context.Context, owner, id string, index int) error {
	if err := m.validate(owner); err != nil {
		return err
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return err
	}
	defer m.release(s)
	r, err := batchLookup(s, id)
	if err != nil {
		return err
	}
	if err = batchWritable(r); err != nil {
		return err
	}
	if index < 0 || index >= r.meta.Items || index != r.meta.Started || r.pending || r.meta.Completed != r.meta.Started {
		return batchError("invalid_state")
	}
	r.meta.Sequence++
	r.meta.Started++
	r.meta.Unknown++
	r.pending = true
	r.settled = false
	return batchAppendEvent(ctx, s, r, batchLine(batchPendingLine{Version: 1, Type: "pending", Sequence: r.meta.Sequence, Index: index}))
}
func (m *BatchManager) Settle(ctx context.Context, owner, id string, index int, actual BatchActual) error {
	if err := m.validate(owner); err != nil {
		return err
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return err
	}
	defer m.release(s)
	r, err := batchLookup(s, id)
	if err != nil {
		return err
	}
	if err = batchWritable(r); err != nil {
		return err
	}
	if index < 0 || index >= len(r.entries) || !r.pending || r.settled || index != r.meta.Started-1 {
		return batchError("invalid_state")
	}
	if !batchValidActual(r.entries[index], actual) {
		return batchError("invalid_actual")
	}
	r.pending = false
	r.settled = true
	r.meta.Sequence++
	switch actual.Outcome {
	case "completed":
		r.meta.Completed++
		r.meta.Unknown--
	case "unchanged":
		r.meta.Unchanged++
		r.meta.Unknown--
	}
	return batchAppendEvent(ctx, s, r, batchLine(batchActualLine{Version: 1, Type: "actual", Sequence: r.meta.Sequence, Index: index, Actual: actual}))
}
func (m *BatchManager) Finish(ctx context.Context, owner, id string, complete bool, code string) error {
	if err := m.validate(owner); err != nil {
		return err
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return err
	}
	defer m.release(s)
	r, err := batchLookup(s, id)
	if err != nil {
		return err
	}
	if err = batchWritable(r); err != nil {
		return err
	}
	// Even an invalid finalization cannot leave an executable writer behind.
	r.active = false
	if !batchCode(code) || complete && r.meta.Completed != r.meta.Items {
		return batchError("invalid_state")
	}
	r.meta.Sequence++
	r.meta.Finished = true
	r.meta.Complete = complete
	r.meta.Code = code
	return batchAppendEvent(ctx, s, r, batchLine(batchEndLine{Version: 1, Type: "end", Sequence: r.meta.Sequence, Complete: complete, Code: code}))
}
