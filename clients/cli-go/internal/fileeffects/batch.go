package fileeffects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

// BatchOptions bounds plans, retained records (including identity-only calls),
// and aggregate charged memory. Zero selects the documented defaults. There
// is no eviction; invalid options fail closed at every public boundary.
type BatchOptions struct {
	PlanBytes   int64
	Entries     int
	MemoryBytes int64
	MaxRecords  int
}
type BatchItem struct {
	Source Endpoint `json:"source"`
	Target Endpoint `json:"target"`
	Bytes  int64    `json:"bytes"`
}
type BatchPlan struct {
	Root  Effect
	Items []BatchItem
}
type BatchActual struct {
	Outcome     string `json:"outcome"`
	Code        string `json:"code"`
	ContentHash string `json:"content_hash"`
	Bytes       int64  `json:"bytes"`
}
type BatchStatus struct {
	ID                                            string
	Items, Started, Completed, Unchanged, Unknown int
	Bytes, SavedBytes                             int64
	Finished, Restored                            bool
	PersistenceError                              string
}
type BatchError struct{ Code string }

func (e *BatchError) Error() string { return e.Code }
func batchError(code string) error  { return &BatchError{Code: "file_batch_" + code} }

const (
	batchSegmentBytes           = 256 << 10
	batchMetadataBytes          = 512 << 10
	batchLineBytes              = 64 << 10
	batchEventBytes             = 512
	batchIdentityMemory   int64 = 1024
	batchOperationTimeout       = 30 * time.Second
	batchBlobTimeout            = 2 * time.Second
)

type batchEntry struct {
	File  bool
	Purge bool
	Bytes int64
}
type batchCall struct {
	marker                           batchMarker
	confirmed, attempted, persistent bool
}
type batchRecord struct {
	meta                         batchMetadata
	saved                        batchMetadata
	entries                      []batchEntry
	hasher                       hash.Hash
	tail                         []byte // only the open event segment, never the complete saved body
	memoryPlan, memoryEvents     [][]byte
	suffix                       []byte // one failed append, strictly after the last confirmed prefix
	charge                       int64
	active, restored, persistent bool
	pending                      bool
	settled                      bool
	persistenceError             string
}
type batchOwner struct {
	owner    string
	overhead int64
	gate     chan struct{}
	refs     int                 // manager mutex
	backend  localartifact.Store // all remaining fields: owner gate
	calls    map[string]*batchCall
	records  map[string]*batchRecord
	charge   int64
}

// BatchManager has no executor, background worker, cleanup, or retry loop.
// Store I/O holds only the cancelable owner gate, never the manager mutex.
// A Store must honor deadlines and must not call back into this manager.
type BatchManager struct {
	options BatchOptions
	invalid bool
	mu      sync.Mutex
	owners  map[string]*batchOwner
	memory  int64
	count   int
}

func NewBatchManager(o BatchOptions) *BatchManager {
	if o.PlanBytes == 0 {
		o.PlanBytes = 64 << 20
	}
	if o.Entries == 0 {
		o.Entries = 100000
	}
	if o.MemoryBytes == 0 {
		o.MemoryBytes = 256 << 20
	}
	if o.MaxRecords == 0 {
		o.MaxRecords = 256
	}
	return &BatchManager{options: o, invalid: o.PlanBytes < 1 || o.PlanBytes > 1<<30 || o.Entries < 1 || o.Entries > 1000000 || o.MemoryBytes < 1 || o.MemoryBytes > 1<<30 || o.MaxRecords < 1 || o.MaxRecords > 8192, owners: make(map[string]*batchOwner)}
}
func (m *BatchManager) validate(owner string) error {
	if m == nil || m.invalid || m.owners == nil {
		return batchError("invalid_configuration")
	}
	if owner == "" || len(owner) > 4096 || !utf8.ValidString(owner) {
		return batchError("invalid_arguments")
	}
	return nil
}
func batchContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d)
}
func (m *BatchManager) acquire(ctx context.Context, owner string, create bool) (*batchOwner, error) {
	if ctx.Err() != nil {
		return nil, batchError("canceled")
	}
	m.mu.Lock()
	s := m.owners[owner]
	if s == nil && create {
		overhead := int64(len(owner)) + 512
		if overhead > m.options.MemoryBytes-m.memory {
			m.mu.Unlock()
			return nil, batchError("limit")
		}
		s = &batchOwner{owner: owner, overhead: overhead, gate: make(chan struct{}, 1), calls: make(map[string]*batchCall), records: make(map[string]*batchRecord)}
		m.memory += overhead
		s.gate <- struct{}{}
		m.owners[owner] = s
	}
	if s == nil {
		m.mu.Unlock()
		return nil, batchError("not_found")
	}
	s.refs++
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.mu.Lock()
		s.refs--
		m.pruneOwner(s)
		m.mu.Unlock()
		return nil, batchError("canceled")
	case <-s.gate:
		if ctx.Err() != nil {
			m.release(s)
			return nil, batchError("canceled")
		}
		return s, nil
	}
}
func (m *BatchManager) pruneOwner(s *batchOwner) {
	// refs==0 excludes a concurrent gate holder or waiter.
	if s.refs == 0 && s.backend == nil && len(s.calls) == 0 && len(s.records) == 0 {
		delete(m.owners, s.owner)
		m.memory -= s.overhead
	}
}
func (m *BatchManager) release(s *batchOwner) {
	m.mu.Lock()
	s.refs--
	m.pruneOwner(s)
	m.mu.Unlock()
	s.gate <- struct{}{}
}
func (m *BatchManager) reserve(bytes int64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if bytes < 0 || bytes > m.options.MemoryBytes-m.memory || count > m.options.MaxRecords-m.count {
		return batchError("limit")
	}
	m.memory += bytes
	m.count += count
	return nil
}
func (m *BatchManager) refund(bytes int64, count int) {
	m.mu.Lock()
	m.memory -= bytes
	m.count -= count
	m.mu.Unlock()
}
func batchHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func batchHashState(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }
func batchLookup(s *batchOwner, id string) (*batchRecord, error) {
	if !batchValidID(id) {
		return nil, batchError("invalid_arguments")
	}
	r := s.records[id]
	if r == nil {
		return nil, batchError("not_found")
	}
	return r, nil
}
func batchWritable(r *batchRecord) error {
	if r.restored {
		return batchError("read_only")
	}
	if !r.active {
		return batchError("writer_stopped")
	}
	return nil
}
func batchFail(r *batchRecord, err error) error {
	r.active = false
	if e, ok := err.(*BatchError); ok {
		r.persistenceError = e.Code
	} else {
		r.persistenceError = "file_batch_store_failed"
	}
	return err
}
func (m *BatchManager) Status(owner, id string) (BatchStatus, error) {
	if err := m.validate(owner); err != nil {
		return BatchStatus{}, err
	}
	ctx, cancel := batchContext(nil, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return BatchStatus{}, err
	}
	defer m.release(s)
	r, err := batchLookup(s, id)
	if err != nil {
		return BatchStatus{}, err
	}
	v := r.meta
	return BatchStatus{ID: id, Items: v.Items, Started: v.Started, Completed: v.Completed, Unchanged: v.Unchanged, Unknown: v.Unknown, Bytes: v.Bytes, SavedBytes: r.saved.Bytes, Finished: v.Finished, Restored: r.restored, PersistenceError: r.persistenceError}, nil
}

// HasCall means a confirmed identity, not a claim that an uncertain marker
// write was durable. Begin also rejects unconfirmed attempted identities.
func (m *BatchManager) HasCall(owner, callID string) bool {
	if m.validate(owner) != nil || !batchValidCall(callID) {
		return false
	}
	ctx, cancel := batchContext(nil, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return false
	}
	defer m.release(s)
	c := s.calls[batchMakeMarker(owner, callID).CallHash]
	return c != nil && c.confirmed
}

// DropOwner is only for a closed owner with no in-flight operations. It never
// removes backend evidence and refuses to discard an active writer.
func (m *BatchManager) DropOwner(owner string) {
	if m.validate(owner) != nil {
		return
	}
	ctx, cancel := batchContext(nil, batchOperationTimeout)
	defer cancel()
	s, err := m.acquire(ctx, owner, false)
	if err != nil {
		return
	}
	defer m.release(s)
	for _, r := range s.records {
		if r.active {
			return
		}
	}
	m.refund(s.charge, len(s.calls))
	s.charge = 0
	s.backend = nil
	s.calls = make(map[string]*batchCall)
	s.records = make(map[string]*batchRecord)
}
