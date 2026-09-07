package fileeffects

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

func batchReadBlob(ctx context.Context, backend localartifact.Store, name string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, batchError("canceled")
	}
	c, cancel := batchContext(ctx, batchBlobTimeout)
	defer cancel()
	data, err := backend.ReadArtifact(c, name)
	if err != nil {
		return nil, batchError("unavailable")
	}
	if c.Err() != nil {
		return nil, batchError("canceled")
	}
	if len(data) > batchMetadataBytes {
		return nil, batchError("corrupt")
	}
	return data, nil
}
func batchWriteBlob(ctx context.Context, backend localartifact.Store, name string, data []byte) error {
	if ctx.Err() != nil {
		return batchError("canceled")
	}
	c, cancel := batchContext(ctx, batchBlobTimeout)
	defer cancel()
	if backend.WriteArtifact(c, name, append([]byte(nil), data...)) != nil {
		return batchError("store_failed")
	}
	return nil
}
func batchNames(ctx context.Context, backend localartifact.Store, prefix string) ([]string, error) {
	if ctx.Err() != nil {
		return nil, batchError("canceled")
	}
	c, cancel := batchContext(ctx, batchBlobTimeout)
	defer cancel()
	names, err := backend.ListArtifacts(c, prefix)
	if err != nil {
		return nil, batchError("unavailable")
	}
	if c.Err() != nil {
		return nil, batchError("canceled")
	}
	if len(names) > 8192 {
		return nil, batchError("limit")
	}
	seen := make(map[string]bool)
	for _, name := range names {
		if len(name) > 160 || !strings.HasPrefix(name, prefix) || seen[name] {
			return nil, batchError("corrupt")
		}
		seen[name] = true
	}
	return names, nil
}
func batchReadSegment(ctx context.Context, backend localartifact.Store, v batchMetadata, plan bool, index int) ([]byte, error) {
	refs := v.Events
	if plan {
		refs = v.Plan
	}
	ref := refs[index]
	data, err := batchReadBlob(ctx, backend, batchSegmentName(v.ID, plan, index))
	if err != nil {
		return nil, err
	}
	// A previously committed event tail may now have an uncommitted suffix.
	// Only its authenticated prefix is visible. Closed segments and plans are
	// immutable and must have exactly their committed lengths.
	if len(data) > batchSegmentBytes || int64(len(data)) < ref.Bytes || (plan || index < len(refs)-1) && int64(len(data)) != ref.Bytes || batchHash(data[:ref.Bytes]) != ref.Hash {
		return nil, batchError("corrupt")
	}
	return data[:ref.Bytes], nil
}
func batchSaveMeta(ctx context.Context, backend localartifact.Store, v batchMetadata) error {
	data, err := json.Marshal(v)
	if err != nil || len(data) > batchMetadataBytes {
		return batchError("limit")
	}
	return batchWriteBlob(ctx, backend, batchMetaName(v.ID), data)
}

// confirmCall never retries a write. If a previous write returned an error,
// only an authenticated identical marker can confirm it on a later invocation.
func (m *BatchManager) confirmCall(ctx context.Context, owner, callID string, s *batchOwner) (bool, error) {
	marker := batchMakeMarker(owner, callID)
	c, known := s.calls[marker.CallHash]
	if !known {
		if err := m.reserve(batchIdentityMemory, 1); err != nil {
			return false, err
		}
		c = &batchCall{marker: marker, persistent: s.backend != nil}
		s.calls[marker.CallHash] = c
		s.charge += batchIdentityMemory
	}
	if s.backend == nil {
		c.confirmed = true
		return known, nil
	}
	names, err := batchNames(ctx, s.backend, batchMarkerName(marker.CallHash))
	if err != nil {
		return known, err
	}
	if len(names) > 1 {
		return known, batchError("corrupt")
	}
	if len(names) == 1 {
		data, e := batchReadBlob(ctx, s.backend, names[0])
		if e != nil {
			return true, e
		}
		observed, e := batchDecodeMarker(data, owner, names[0])
		if e != nil {
			return true, e
		}
		if observed != marker {
			return true, batchError("corrupt")
		}
		c.confirmed = true
		return true, nil
	}
	if c.attempted || c.confirmed {
		return known, batchError("store_failed")
	}
	c.attempted = true
	data, _ := json.Marshal(marker)
	if err = batchWriteBlob(ctx, s.backend, batchMarkerName(marker.CallHash), data); err != nil {
		return known, err
	}
	c.confirmed = true
	return known, nil
}
func (m *BatchManager) RecordCall(ctx context.Context, owner, callID string) error {
	if err := m.validate(owner); err != nil {
		return err
	}
	if !batchValidCall(callID) {
		return batchError("invalid_arguments")
	}
	ctx, cancel := batchContext(ctx, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, true)
	if err != nil {
		return err
	}
	defer m.release(s)
	_, err = m.confirmCall(ctx, owner, callID, s)
	return err
}

// Charge transient validation maps, minimal per-item facts, bounded read/scan
// buffers, open tail, and the worst metadata before accepting a plan. Memory
// records additionally reserve their entire worst-case append-only body.
func batchRecordCharge(planBytes int64, items, planSegments int, persistent bool) (int64, error) {
	eventBudget := (int64(items)*2 + 1) * batchEventBytes
	eventSegments := eventBudget/(batchSegmentBytes-batchEventBytes) + 1
	metaBudget := int64(planSegments)*128 + eventSegments*128 + 4096
	if metaBudget > batchMetadataBytes {
		return 0, batchError("limit")
	}
	charge := planBytes*3 + int64(items)*320 + 3*batchSegmentBytes + 2*batchLineBytes + 4*metaBudget + (1 << 20)
	if !persistent {
		charge += eventBudget + planBytes
	}
	return charge, nil
}
