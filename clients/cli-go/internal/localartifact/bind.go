package localartifact

import (
	"context"
	"strings"
)

// Bind loads and authenticates a complete catalog before replacing a binding.
// Its independent five-second context includes gate wait and catalog I/O. A
// failed bind changes neither records nor the existing backend. Missing known
// saved records are an incomplete catalog, not an empty replacement history.
// Passing nil removes saved directory records, but preserves memory results.
// Distinct memory results merge with a catalog; an identical ID can be promoted
// to saved only after full verification. Conflicting immutable IDs are corrupt.
func (m *Manager) Bind(owner string, backend Store) error {
	if err := m.validate(owner); err != nil {
		return err
	}
	ctx, cancel := boundedContext(context.Background(), bindTimeout)
	defer cancel()
	state, err := m.acquire(ctx, owner, true)
	if err != nil {
		return err
	}
	defer m.release(owner, state)
	loaded := make(map[string]*record)
	if backend != nil {
		callCtx, callCancel := boundedContext(ctx, callTimeout)
		names, err := backend.ListArtifacts(callCtx, metadataPrefix)
		callExpired := callCtx.Err() != nil
		callCancel()
		if err != nil {
			return storeError(err, "artifact_unavailable")
		}
		if callExpired {
			return failure("artifact_canceled")
		}
		if len(names) > m.options.MaxRecords {
			return failure("artifact_limit")
		}
		for _, name := range names {
			id, ok := strings.CutPrefix(name, metadataPrefix)
			if !ok || !validID(id) || loaded[id] != nil {
				return failure("artifact_corrupt")
			}
			data, err := readBlob(ctx, backend, name)
			if err != nil {
				return err
			}
			meta, err := decodeMetadata(data, owner, id)
			if err != nil {
				return err
			}
			if meta.Bytes > m.options.MaxArtifactBytes {
				return failure("artifact_limit")
			}
			if err := verifyBody(ctx, backend, meta); err != nil {
				return err
			}
			loaded[id] = &record{owner: owner, info: meta.info(true), meta: meta}
		}
	}
	if ctx.Err() != nil {
		return failure("artifact_canceled")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if backend != nil {
		for id, r := range m.records {
			if r.owner == owner && r.info.Saved && loaded[id] == nil {
				return failure("artifact_unavailable")
			}
		}
		added := 0
		for id, r := range loaded {
			if previous := m.records[id]; previous != nil {
				if previous.owner != owner || !sameMetadata(previous.meta, r.meta) {
					return failure("artifact_corrupt")
				}
			} else {
				added++
			}
		}
		if len(m.records)+m.pending+added > m.options.MaxRecords {
			return failure("artifact_limit")
		}
	}
	state.backend, state.hasBackend = backend, backend != nil
	if backend == nil {
		for id, r := range m.records {
			if r.owner == owner && r.info.Saved {
				delete(m.records, id)
				state.count--
			}
		}
	} else {
		for id, r := range loaded {
			if previous := m.records[id]; previous == nil {
				state.count++
			} else if !previous.info.Saved {
				m.memory -= previous.info.Bytes
			}
			m.records[id] = r
		}
	}
	return nil
}
