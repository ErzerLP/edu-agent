package privacy

import (
	"context"
	"sync"
)

type ReadPermit struct {
	manager *ReadPermitManager
	id      uint64
	ctx     context.Context
	once    sync.Once
}

func (p *ReadPermit) Context() context.Context {
	if p == nil || p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *ReadPermit) Release() {
	if p == nil || p.manager == nil {
		return
	}
	p.once.Do(func() { p.manager.release(p.id) })
}

// CommitResponse serializes the final response write with privacy closure for
// every owner covered by this permit.
func (p *ReadPermit) CommitResponse(write func()) error {
	if p == nil || p.manager == nil || write == nil {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_read_commit"}
	}
	return p.manager.commitResponse(p.id, p.ctx, write)
}

type activePermit struct {
	owners map[OwnerKind]struct{}
	cancel context.CancelCauseFunc
}

// ResponseCommitGate holds persistent owner read gates while a complete
// buffered response is emitted.
type ResponseCommitGate interface {
	WithReadGates(context.Context, []OwnerKind, func() error) error
}

type ReadPermitManagerOption func(*ReadPermitManager)

func WithResponseCommitGate(gate ResponseCommitGate) ReadPermitManagerOption {
	return func(manager *ReadPermitManager) { manager.responseGate = gate }
}

// ReadPermitManager cancels and drains reads whose response body is currently
// being assembled in this process. Persistent owner gates remain authoritative
// across process restarts.
type ReadPermitManager struct {
	mu           sync.Mutex
	nextID       uint64
	closed       map[OwnerKind]bool
	generation   map[OwnerKind]int64
	active       map[uint64]activePermit
	writeGates   map[OwnerKind]chan struct{}
	changed      chan struct{}
	responseGate ResponseCommitGate
}

func NewReadPermitManager(options ...ReadPermitManagerOption) *ReadPermitManager {
	generations := make(map[OwnerKind]int64, len(AllOwners))
	writeGates := make(map[OwnerKind]chan struct{}, len(AllOwners))
	for _, owner := range AllOwners {
		generations[owner] = 1
		gate := make(chan struct{}, 1)
		gate <- struct{}{}
		writeGates[owner] = gate
	}
	manager := &ReadPermitManager{
		closed: make(map[OwnerKind]bool), generation: generations,
		active: make(map[uint64]activePermit), writeGates: writeGates,
		changed: make(chan struct{}),
	}
	for _, option := range options {
		if option != nil {
			option(manager)
		}
	}
	return manager
}

var DefaultReadPermits = NewReadPermitManager()

func (m *ReadPermitManager) Acquire(ctx context.Context, owners ...OwnerKind) (*ReadPermit, error) {
	if m == nil {
		m = DefaultReadPermits
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ownerSet := make(map[OwnerKind]struct{}, len(owners))
	for _, owner := range owners {
		if !owner.Valid() {
			return nil, &Error{Code: CodeInvalidRequest, Reason: "invalid_read_owner"}
		}
		ownerSet[owner] = struct{}{}
	}
	if len(ownerSet) == 0 {
		return nil, &Error{Code: CodeInvalidRequest, Reason: "read_owner_required"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner := range ownerSet {
		if m.closed[owner] {
			return nil, &Error{Code: CodeContentRedacted, Reason: string(owner) + "_read_gate_closed"}
		}
	}
	permitContext, cancel := context.WithCancelCause(ctx)
	m.nextID++
	id := m.nextID
	m.active[id] = activePermit{owners: ownerSet, cancel: cancel}
	return &ReadPermit{manager: m, id: id, ctx: permitContext}, nil
}

func (m *ReadPermitManager) CloseAndDrain(ctx context.Context, generation int64, owners ...OwnerKind) error {
	if m == nil {
		m = DefaultReadPermits
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ownerSet := normalizeOwners(owners)
	if len(ownerSet) == 0 || generation < 2 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_read_drain"}
	}

	unlockWrites, err := m.lockWriteGates(ctx, ownerSet)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for owner := range ownerSet {
		m.closed[owner] = true
		m.generation[owner] = generation
	}
	for _, permit := range m.active {
		if intersects(permit.owners, ownerSet) {
			permit.cancel(&Error{Code: CodeContentRedacted, Reason: "privacy_barrier_committing"})
		}
	}
	m.mu.Unlock()
	unlockWrites()

	m.mu.Lock()
	for hasActive(m.active, ownerSet) {
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		m.mu.Lock()
	}
	m.mu.Unlock()
	return nil
}

func (m *ReadPermitManager) AbortClose(closedGeneration, restoreGeneration int64, owners ...OwnerKind) {
	if m == nil {
		m = DefaultReadPermits
	}
	ownerSet := normalizeOwners(owners)
	unlockWrites, _ := m.lockWriteGates(context.Background(), ownerSet)
	defer unlockWrites()
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner := range ownerSet {
		if m.closed[owner] && m.generation[owner] == closedGeneration {
			m.generation[owner] = restoreGeneration
			delete(m.closed, owner)
		}
	}
}

func (m *ReadPermitManager) Open(generation int64, owners ...OwnerKind) {
	if m == nil {
		m = DefaultReadPermits
	}
	ownerSet := normalizeOwners(owners)
	unlockWrites, _ := m.lockWriteGates(context.Background(), ownerSet)
	defer unlockWrites()
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner := range ownerSet {
		if generation >= m.generation[owner] {
			m.generation[owner] = generation
			delete(m.closed, owner)
		}
	}
}

func (m *ReadPermitManager) Closed(owner OwnerKind) (bool, int64) {
	if m == nil {
		m = DefaultReadPermits
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed[owner], m.generation[owner]
}

func (m *ReadPermitManager) commitResponse(id uint64, permitContext context.Context, write func()) error {
	m.mu.Lock()
	permit, ok := m.active[id]
	if !ok {
		m.mu.Unlock()
		if cause := context.Cause(permitContext); cause != nil {
			return cause
		}
		return &Error{Code: CodeContentRedacted, Reason: "read_permit_inactive"}
	}
	owners := cloneOwners(permit.owners)
	m.mu.Unlock()

	commit := func() error {
		unlockWrites, err := m.lockWriteGates(permitContext, owners)
		if err != nil {
			if cause := context.Cause(permitContext); cause != nil {
				return cause
			}
			return err
		}
		defer unlockWrites()

		m.mu.Lock()
		permit, ok = m.active[id]
		if !ok {
			m.mu.Unlock()
			if cause := context.Cause(permitContext); cause != nil {
				return cause
			}
			return &Error{Code: CodeContentRedacted, Reason: "read_permit_inactive"}
		}
		if cause := context.Cause(permitContext); cause != nil {
			m.mu.Unlock()
			return cause
		}
		for owner := range permit.owners {
			if m.closed[owner] {
				m.mu.Unlock()
				return &Error{Code: CodeContentRedacted, Reason: string(owner) + "_read_gate_closed"}
			}
		}
		m.mu.Unlock()

		write()
		return nil
	}
	if m.responseGate == nil {
		return commit()
	}
	if err := m.responseGate.WithReadGates(permitContext, orderedOwnerKinds(owners), commit); err != nil {
		if cause := context.Cause(permitContext); cause != nil {
			return cause
		}
		return err
	}
	return nil
}

func (m *ReadPermitManager) lockWriteGates(ctx context.Context, owners map[OwnerKind]struct{}) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ordered := orderedOwnerKinds(owners)
	acquired := make([]chan struct{}, 0, len(ordered))
	for _, owner := range ordered {
		select {
		case <-ctx.Done():
			for index := len(acquired) - 1; index >= 0; index-- {
				acquired[index] <- struct{}{}
			}
			return nil, context.Cause(ctx)
		case <-m.writeGates[owner]:
			acquired = append(acquired, m.writeGates[owner])
		}
	}
	return func() {
		for index := len(acquired) - 1; index >= 0; index-- {
			acquired[index] <- struct{}{}
		}
	}, nil
}

func orderedOwnerKinds(owners map[OwnerKind]struct{}) []OwnerKind {
	ordered := make([]OwnerKind, 0, len(owners))
	for _, owner := range AllOwners {
		if _, ok := owners[owner]; ok {
			ordered = append(ordered, owner)
		}
	}
	return ordered
}

func cloneOwners(owners map[OwnerKind]struct{}) map[OwnerKind]struct{} {
	cloned := make(map[OwnerKind]struct{}, len(owners))
	for owner := range owners {
		cloned[owner] = struct{}{}
	}
	return cloned
}

func (m *ReadPermitManager) release(id uint64) {
	m.mu.Lock()
	permit, ok := m.active[id]
	if ok {
		delete(m.active, id)
		permit.cancel(nil)
		close(m.changed)
		m.changed = make(chan struct{})
	}
	m.mu.Unlock()
}

func normalizeOwners(owners []OwnerKind) map[OwnerKind]struct{} {
	result := make(map[OwnerKind]struct{}, len(owners))
	for _, owner := range owners {
		if owner.Valid() {
			result[owner] = struct{}{}
		}
	}
	return result
}
func intersects(left, right map[OwnerKind]struct{}) bool {
	for owner := range left {
		if _, ok := right[owner]; ok {
			return true
		}
	}
	return false
}
func hasActive(active map[uint64]activePermit, owners map[OwnerKind]struct{}) bool {
	for _, permit := range active {
		if intersects(permit.owners, owners) {
			return true
		}
	}
	return false
}
