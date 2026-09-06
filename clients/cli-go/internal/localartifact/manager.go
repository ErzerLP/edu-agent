// Package localartifact retains immutable, session-owned diff and receipt data.
// It does not interpret results, restore execution objects, or access files.
package localartifact

import (
	"context"
	"crypto/rand"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	DefaultMaxArtifactBytes int64 = 128 << 20
	DefaultMemoryBytes      int64 = 256 << 20
	DefaultMaxRecords             = 256

	// Safety ceilings also bound metadata and integer arithmetic on 32-bit hosts.
	maxConfiguredBytes   int64 = 1 << 30
	maxConfiguredRecords       = 8192
	segmentBytes               = 256 << 10
	metadataBytes              = 512 << 10
	callTimeout                = 2 * time.Second
	bindTimeout                = 5 * time.Second
	operationTimeout           = 30 * time.Second
)

// Store owns authentication, session isolation, quotas and generation checks.
// Implementations must honor context deadlines and must not call back into the
// Manager or its controller. The manager cannot interrupt blocking OS I/O.
// Writes are attempted once; an error may mean publication is unknown. No blob
// is deleted or retried, and only a later authenticated Bind can recover it.
type Store interface {
	ReadArtifact(context.Context, string) ([]byte, error)
	WriteArtifact(context.Context, string, []byte) error
	ListArtifacts(context.Context, string) ([]string, error)
}

// Options are manager-wide budgets, without eviction. Zero selects defaults.
// Byte budgets must be at most 1 GiB and MaxRecords at most 8192. Invalid
// configuration is retained by New and fails closed at the public boundary.
type Options struct {
	MaxArtifactBytes int64
	MemoryBytes      int64
	MaxRecords       int
}

type Info struct {
	ID    string
	Kind  string
	Hash  string
	Bytes int64
	Saved bool
}

type Page struct {
	Info       Info
	Data       []byte
	Offset     int64
	NextOffset int64
	More       bool
}

type SearchPage struct {
	Info       Info
	Offsets    []int64
	Offset     int64
	NextOffset int64
	Scanned    int64
	More       bool
}

// Error contains only a stable, non-sensitive code, never backend error text.
type Error struct{ Code string }

func (e *Error) Error() string  { return e.Code }
func failure(code string) error { return &Error{Code: code} }

func storeError(err error, fallback string) error {
	var stable *Error
	if errors.As(err, &stable) && stable != nil {
		switch stable.Code {
		case "artifact_limit", "artifact_unavailable", "artifact_corrupt", "artifact_version_unsupported", "artifact_store_failed", "artifact_canceled":
			return failure(stable.Code)
		}
	}
	return failure(fallback)
}

type record struct {
	owner string
	info  Info
	meta  metadata
	body  []byte // non-nil storage is used only by memory records; Saved is authoritative
}

type ownerState struct {
	gate       chan struct{}
	backend    Store // protected by gate
	refs       int   // protected by Manager.mu, including gate waiters
	count      int   // protected by Manager.mu
	hasBackend bool  // protected by Manager.mu
}

// Manager serializes each owner's operations with a context-aware gate. Lock
// order is owner gate, then mu; gate lookup never waits while holding mu. Store
// I/O never holds mu. Records are immutable after publication; List needs only
// mu and cannot hide a failed catalog load (Bind returns that failure).
type Manager struct {
	options Options
	invalid bool
	mu      sync.Mutex
	owners  map[string]*ownerState
	records map[string]*record
	pending int
	memory  int64 // retained memory bodies plus outstanding memory reservations
}

func New(options Options) *Manager {
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if options.MemoryBytes == 0 {
		options.MemoryBytes = DefaultMemoryBytes
	}
	if options.MaxRecords == 0 {
		options.MaxRecords = DefaultMaxRecords
	}
	invalid := options.MaxArtifactBytes < 1 || options.MaxArtifactBytes > maxConfiguredBytes ||
		options.MemoryBytes < 1 || options.MemoryBytes > maxConfiguredBytes ||
		options.MaxRecords < 1 || options.MaxRecords > maxConfiguredRecords
	return &Manager{options: options, invalid: invalid, owners: make(map[string]*ownerState), records: make(map[string]*record)}
}

func (m *Manager) validate(owner string) error {
	if m == nil || m.invalid || m.owners == nil {
		return failure("artifact_invalid_configuration")
	}
	if owner == "" {
		return failure("artifact_invalid_arguments")
	}
	return nil
}

func boundedContext(ctx context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, duration)
}

func (m *Manager) acquire(ctx context.Context, owner string, create bool) (*ownerState, error) {
	if ctx.Err() != nil {
		return nil, failure("artifact_canceled")
	}
	m.mu.Lock()
	state := m.owners[owner]
	if state == nil && create {
		state = &ownerState{gate: make(chan struct{}, 1)}
		state.gate <- struct{}{}
		m.owners[owner] = state
	}
	if state == nil {
		m.mu.Unlock()
		return nil, failure("artifact_not_found")
	}
	state.refs++
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.unref(owner, state)
		return nil, failure("artifact_canceled")
	case <-state.gate:
		if ctx.Err() != nil {
			m.release(owner, state)
			return nil, failure("artifact_canceled")
		}
		return state, nil
	}
}

func (m *Manager) unref(owner string, state *ownerState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state.refs--
	if state.refs == 0 && state.count == 0 && !state.hasBackend {
		delete(m.owners, owner)
	}
}

func (m *Manager) release(owner string, state *ownerState) {
	m.unref(owner, state)
	state.gate <- struct{}{}
}

// Put copies memory-only data, or writes fixed-size segments followed by the
// sole catalog entry. Capacity is reserved before copying, hashing or I/O.
// Saved is true only after every write, including metadata, returns success.
// The caller must not mutate data concurrently with Put.
func (m *Manager) Put(ctx context.Context, owner, kind string, data []byte) (Info, error) {
	if err := m.validate(owner); err != nil {
		return Info{}, err
	}
	if !validKind(kind) {
		return Info{}, failure("artifact_invalid_arguments")
	}
	size := int64(len(data))
	if size > m.options.MaxArtifactBytes {
		return Info{}, failure("artifact_limit")
	}
	ctx, cancel := boundedContext(ctx, operationTimeout)
	defer cancel()
	state, err := m.acquire(ctx, owner, true)
	if err != nil {
		return Info{}, err
	}
	defer m.release(owner, state)
	memory := int64(0)
	if state.backend == nil {
		memory = size
	}
	m.mu.Lock()
	if len(m.records)+m.pending >= m.options.MaxRecords || memory > m.options.MemoryBytes-m.memory {
		m.mu.Unlock()
		return Info{}, failure("artifact_limit")
	}
	m.pending++
	m.memory += memory
	m.mu.Unlock()
	committed := false
	defer func() {
		if !committed {
			m.mu.Lock()
			m.pending--
			m.memory -= memory
			m.mu.Unlock()
		}
	}()

	id := "r_" + rand.Text()
	m.mu.Lock()
	_, collision := m.records[id]
	m.mu.Unlock()
	if collision {
		return Info{}, failure("artifact_store_failed")
	}
	meta := makeMetadata(owner, id, kind, data)
	r := &record{owner: owner, meta: meta, info: meta.info(state.backend != nil)}
	if state.backend == nil {
		r.body = append([]byte(nil), data...)
		if ctx.Err() != nil {
			return Info{}, failure("artifact_canceled")
		}
	} else if err := save(ctx, state.backend, meta, data); err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	m.pending--
	m.records[id] = r
	state.count++
	committed = true
	m.mu.Unlock()
	return r.info, nil
}

// List is a sorted metadata snapshot, with no body or backend I/O. Callers must
// retain and surface Bind errors rather than interpreting a failed load as an
// empty history. A listed saved record can become unavailable after revocation.
func (m *Manager) List(owner string) []Info {
	items := make([]Info, 0)
	if m.validate(owner) != nil {
		return items
	}
	m.mu.Lock()
	for _, r := range m.records {
		if r.owner == owner {
			items = append(items, r.info)
		}
	}
	m.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// DropOwner releases this manager's records and binding, never backend blobs.
// Use it after an owner has closed and its in-flight calls have completed. Like
// other gate acquisitions, it is bounded; it does not cancel another operation.
func (m *Manager) DropOwner(owner string) {
	if m.validate(owner) != nil {
		return
	}
	ctx, cancel := boundedContext(nil, operationTimeout)
	defer cancel()
	state, err := m.acquire(ctx, owner, false)
	if err != nil {
		return
	}
	defer m.release(owner, state)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.records {
		if r.owner == owner {
			if !r.info.Saved {
				m.memory -= r.info.Bytes
			}
			delete(m.records, id)
		}
	}
	state.count, state.hasBackend, state.backend = 0, false, nil
}
