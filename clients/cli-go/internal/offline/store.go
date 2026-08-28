package offline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const objectSuffix = ".obj"
const purgeBackendMarker = "purge.backend"

type Store struct {
	root         string
	binding      Binding
	trustRoot    TrustState
	trust        TrustState
	dek          []byte
	leaseTimeout time.Duration
	mu           sync.Mutex
	closed       bool
}

type counterDetail struct {
	NoncePrefix string `json:"nonce_prefix"`
	HighWater   string `json:"high_water"`
}

type preparePublishStage string

const (
	preparePublishAfterJournalDurable  preparePublishStage = "after_journal_durable"
	preparePublishAfterPackDurable     preparePublishStage = "after_pack_durable"
	preparePublishAfterTrustDurable    preparePublishStage = "after_trust_durable"
	preparePublishAfterIntentCompleted preparePublishStage = "after_intent_completed"
	preparePublishAfterJournalDeleted  preparePublishStage = "after_journal_deleted"
)

type replaceDetail struct {
	Purpose         string   `json:"purpose"`
	OperationIDs    []string `json:"operation_ids"`
	DeleteOperation bool     `json:"delete_operation"`
}

type discardTarget struct {
	Kind ObjectKind `json:"kind"`
	ID   string     `json:"id"`
}

type discardDetail struct {
	All     bool            `json:"all"`
	Targets []discardTarget `json:"targets"`
}

func CreatePassphrase(ctx context.Context, root string, options CreateOptions, passphrase []byte) (*Store, error) {
	return createWithBackend(ctx, root, options, passphrase, KeyBackendPassphrase)
}

func CreateSystem(ctx context.Context, root string, options CreateOptions, wrappingKey []byte) (*Store, error) {
	return createWithBackend(ctx, root, options, wrappingKey, KeyBackendSystem)
}

func createWithBackend(ctx context.Context, root string, options CreateOptions, wrappingMaterial []byte, backend uint8) (*Store, error) {
	if err := options.Binding.validate(false); err != nil {
		return nil, err
	}
	if !options.TrustState.valid() || len(wrappingMaterial) == 0 {
		return nil, ErrKeyUnavailable
	}
	if backend != KeyBackendPassphrase && backend != KeyBackendSystem {
		return nil, ErrKeyUnavailable
	}
	binding := options.Binding
	if binding.ProfileID != ([16]byte{}) {
		return nil, errors.New("new offline profile binding must not preselect a profile UUID")
	}
	profileID, err := randomUUID()
	if err != nil {
		return nil, fmt.Errorf("generate offline profile identity: %w", err)
	}
	binding.ProfileID = profileID
	timeout := options.LeaseTimeout
	if timeout <= 0 {
		timeout = DefaultLeaseTimeout
	}
	lease, err := AcquireLease(ctx, root, LeaseExclusive, timeout)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	if exists, err := Exists(root); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrProfileExists
	}
	if entriesPresent(root) {
		return nil, fmt.Errorf("%w: offline root contains an incomplete profile", ErrCorruptStore)
	}
	keyFile, dek, err := createWrappedKeyForBackend(binding, wrappingMaterial, backend)
	if err != nil {
		return nil, err
	}
	createdKey := false
	cleanup := func() {
		zeroBytes(dek)
		if createdKey {
			_ = deleteManaged(root, "profile.key")
		}
		_ = removeAllManagedObjects(root)
	}
	if err := atomicWriteManaged(root, "profile.key", keyFile, false); err != nil {
		zeroBytes(keyFile)
		cleanup()
		if errors.Is(err, os.ErrExist) {
			return nil, ErrProfileExists
		}
		return nil, err
	}
	zeroBytes(keyFile)
	createdKey = true
	prefix, err := randomNoncePrefix()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("generate offline nonce prefix: %w", err)
	}
	store := &Store{root: root, binding: binding, trustRoot: TrustState{canonical: options.TrustState.Bytes()}, trust: TrustState{canonical: options.TrustState.Bytes()}, dek: dek, leaseTimeout: timeout}
	if err := store.bootstrapCounterLocked(prefix); err != nil {
		cleanup()
		return nil, err
	}
	profile := ProfileRecord{Format: Format, ProfileVersion: 2, ProfileID: binding.ProfileUUID(), TrustRoot: options.TrustState.Bytes(), TrustState: options.TrustState.Bytes()}
	if err := profile.validate(binding, options.TrustState); err != nil {
		cleanup()
		return nil, err
	}
	if err := store.writeRecordLocked(ObjectProfile, binding.ProfileUUID(), profile, false); err != nil {
		cleanup()
		return nil, err
	}
	createdKey = false
	return store, nil
}

func OpenPassphrase(ctx context.Context, root string, expected Binding, expectedTrust TrustState, passphrase []byte) (*Store, error) {
	return OpenWithBackend(ctx, root, expected, expectedTrust, passphrase, 0)
}

func OpenWithBackend(ctx context.Context, root string, expected Binding, expectedTrust TrustState, wrappingMaterial []byte, expectedBackend uint8) (*Store, error) {
	if err := expected.validate(false); err != nil {
		return nil, err
	}
	if !expectedTrust.valid() || len(wrappingMaterial) == 0 {
		return nil, ErrKeyUnavailable
	}
	if expectedBackend != 0 && expectedBackend != KeyBackendPassphrase && expectedBackend != KeyBackendSystem {
		return nil, ErrKeyUnavailable
	}
	exists, err := Exists(root)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	lease, err := AcquireLease(ctx, root, LeaseExclusive, DefaultLeaseTimeout)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	store, _, err := openStoreLocked(root, expected, expectedTrust, wrappingMaterial, expectedBackend, false)
	return store, err
}

func (s *Store) Binding() Binding {
	if s == nil {
		return Binding{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Binding{}
	}
	return s.binding
}

func (s *Store) TrustState() TrustState {
	if s == nil {
		return TrustState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return TrustState{}
	}
	return TrustState{canonical: s.trust.Bytes()}
}

func (s *Store) UpdateTrustState(ctx context.Context, next TrustState) error {
	if !next.valid() {
		return ErrBindingMismatch
	}
	return s.withLease(ctx, LeaseExclusive, func() error {
		return s.updateTrustStateLocked(next)
	})
}

func (s *Store) updateTrustStateLocked(next TrustState) error {
	if !next.valid() {
		return ErrBindingMismatch
	}
	if bytes.Equal(s.trust.canonical, next.canonical) {
		return nil
	}
	profile := ProfileRecord{
		Format: Format, ProfileVersion: 2, ProfileID: s.binding.ProfileUUID(),
		TrustRoot: s.trustRoot.Bytes(), TrustState: next.Bytes(),
	}
	if err := profile.validate(s.binding, s.trustRoot); err != nil {
		return err
	}
	if err := s.writeRecordLocked(ObjectProfile, s.binding.ProfileUUID(), profile, true); err != nil {
		return fmt.Errorf("update offline trust checkpoint: %w", err)
	}
	s.trust = TrustState{canonical: next.Bytes()}
	return nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	zeroBytes(s.dek)
	s.dek = nil
	s.trustRoot.canonical = nil
	s.trust.canonical = nil
	s.closed = true
	return nil
}

func (s *Store) SavePack(ctx context.Context, pack Pack) error {
	record, err := packToRecord(pack)
	if err != nil {
		return err
	}
	return s.withLease(ctx, LeaseExclusive, func() error {
		exists, err := s.packRecordExistsLocked(record)
		if err != nil || exists {
			return err
		}
		journalID, err := randomUUID()
		if err != nil {
			return err
		}
		detail, _ := marshalCanonical(prepareDetail{Purpose: "save_pack", Request: json.RawMessage("null")})
		journal := newJournal(formatUUID(journalID), JournalPreparePublish, "publishing", record.PackID, record.PackID, 1, detail)
		if err := s.writeRecordLocked(ObjectJournal, journal.JournalID, journal, false); err != nil {
			return err
		}
		if err := s.writeRecordLocked(ObjectPack, record.PackID, record, false); err != nil {
			return err
		}
		return deleteManaged(s.root, objectRelative(ObjectJournal, journal.JournalID))
	})
}

func (s *Store) packRecordExistsLocked(record PackRecord) (bool, error) {
	if existing, err := s.rawRecordLocked(ObjectPack, record.PackID); err == nil {
		candidate, marshalErr := marshalCanonical(record)
		if marshalErr != nil {
			return false, marshalErr
		}
		if bytes.Equal(existing, candidate) {
			return true, nil
		}
		return false, ErrInvalidState
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return false, nil
}

func packRecordDigest(record PackRecord) (string, error) {
	canonical, err := marshalCanonical(record)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) ListAvailablePacks(ctx context.Context, now time.Time) ([]PackInfo, error) {
	now = now.UTC()
	var result []PackInfo
	err := s.withLease(ctx, LeaseShared, func() error {
		records, err := s.packRecordsLocked()
		if err != nil {
			return err
		}
		for _, record := range records {
			eligible, _ := parseRecordTime(record.EligibleUntil)
			archive, _ := parseRecordTime(record.ArchiveUntil)
			available := now.Before(eligible)
			result = append(result, PackInfo{ID: record.PackID, EligibleUntil: eligible, ArchiveUntil: archive, ItemCount: record.ItemCount, Available: available})
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].EligibleUntil.Equal(result[j].EligibleUntil) {
				return result[i].ID < result[j].ID
			}
			return result[i].EligibleUntil.Before(result[j].EligibleUntil)
		})
		return nil
	})
	return result, err
}

func (s *Store) GetPack(ctx context.Context, packID string) (Pack, error) {
	var result Pack
	err := s.withLease(ctx, LeaseShared, func() error {
		var record PackRecord
		if err := s.readRecordLocked(ObjectPack, packID, &record); err != nil {
			return err
		}
		var err error
		result, err = record.public()
		return err
	})
	return result, err
}

func (s *Store) SavePrepareIntent(ctx context.Context, intent PrepareIntent) error {
	if _, err := parseUUID(intent.RequestID); err != nil {
		return errors.New("prepare request ID must be a canonical UUID")
	}
	created, err := normalizeTime(intent.CreatedAt)
	if err != nil {
		return err
	}
	request, err := requireCanonicalObject(intent.Canonical)
	if err != nil {
		return errors.New("prepare request must be exact canonical JSON")
	}
	trustState := intent.TrustState.Bytes()
	if len(trustState) != 0 && !intent.TrustState.valid() {
		return errors.New("prepare trust checkpoint must be canonical JSON")
	}
	detail, _ := marshalCanonical(prepareDetail{Purpose: "prepare_intent", Request: request, TrustState: trustState})
	journal := newJournal(intent.RequestID, JournalPreparePublish, "prepared", intent.RequestID, intent.RequestID, 1, detail)
	journal.CreatedAt = formatRecordTime(created)
	return s.withLease(ctx, LeaseExclusive, func() error {
		if existing, err := s.rawRecordLocked(ObjectJournal, intent.RequestID); err == nil {
			candidate, _ := marshalCanonical(journal)
			if bytes.Equal(existing, candidate) {
				return nil
			}
			return ErrImmutableOperation
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return s.writeRecordLocked(ObjectJournal, intent.RequestID, journal, false)
	})
}

func (s *Store) PendingPrepareIntent(ctx context.Context) (PrepareIntent, error) {
	var result PrepareIntent
	err := s.withLease(ctx, LeaseShared, func() error {
		journals, err := s.journalRecordsLocked()
		if err != nil {
			return err
		}
		for _, journal := range journals {
			if journal.Kind != JournalPreparePublish {
				continue
			}
			var detail prepareDetail
			if err := decodeClosed(journal.Detail, &detail); err != nil || detail.validate(journal.State) != nil || (detail.Purpose != "prepare_intent" && detail.Purpose != "prepare_publish") {
				return fmt.Errorf("%w: invalid prepare journal detail", ErrCorruptStore)
			}
			request, err := requireCanonicalObject(detail.Request)
			if err != nil {
				return fmt.Errorf("%w: invalid prepare request", ErrCorruptStore)
			}
			var trustState TrustState
			if len(detail.TrustState) != 0 {
				trustState, err = NewTrustState(detail.TrustState)
				if err != nil {
					return fmt.Errorf("%w: invalid prepare trust checkpoint", ErrCorruptStore)
				}
			}
			created, _ := parseRecordTime(journal.CreatedAt)
			result = PrepareIntent{RequestID: journal.SourceID, CreatedAt: created, Canonical: append(json.RawMessage(nil), request...), TrustState: trustState}
			return nil
		}
		return ErrNotFound
	})
	return result, err
}

func (s *Store) ClearPrepareIntent(ctx context.Context, requestID string) error {
	if _, err := parseUUID(requestID); err != nil {
		return err
	}
	return s.withLease(ctx, LeaseExclusive, func() error {
		var journal JournalRecord
		if err := s.readRecordLocked(ObjectJournal, requestID, &journal); err != nil {
			return err
		}
		if journal.Kind != JournalPreparePublish || journal.State != "prepared" {
			return ErrInvalidState
		}
		return deleteManaged(s.root, objectRelative(ObjectJournal, requestID))
	})
}

func (s *Store) PublishPreparedPack(ctx context.Context, requestID string, expectedCurrent, next TrustState, pack Pack) error {
	return s.publishPreparedPack(ctx, requestID, expectedCurrent, next, pack, nil)
}

func newPreparePublicationDetail(intent prepareDetail, expectedCurrent, next TrustState, record PackRecord) (prepareDetail, error) {
	requestDigest, err := canonicalDigest(intent.Request)
	if err != nil {
		return prepareDetail{}, err
	}
	baseDigest, err := canonicalDigest(expectedCurrent.canonical)
	if err != nil {
		return prepareDetail{}, err
	}
	nextDigest, err := canonicalDigest(next.canonical)
	if err != nil {
		return prepareDetail{}, err
	}
	recordDigest, err := packRecordDigest(record)
	if err != nil {
		return prepareDetail{}, err
	}
	trustDigest := ""
	if len(intent.TrustState) != 0 {
		trustDigest, err = canonicalDigest(intent.TrustState)
		if err != nil {
			return prepareDetail{}, err
		}
	}
	record.CanonicalBytes = append(json.RawMessage(nil), record.CanonicalBytes...)
	return prepareDetail{
		Purpose: "prepare_publish", Request: append(json.RawMessage(nil), intent.Request...), TrustState: append(json.RawMessage(nil), intent.TrustState...),
		PublicationVersion: preparePublicationVersion, RequestDigest: requestDigest, TrustStateDigest: trustDigest,
		BaseTrustState: expectedCurrent.Bytes(), BaseTrustStateDigest: baseDigest,
		NextTrustState: next.Bytes(), NextTrustStateDigest: nextDigest,
		PackID: record.PackID, PackRecord: &record, PackRecordDigest: recordDigest,
	}, nil
}

func (s *Store) publishPreparedPack(ctx context.Context, requestID string, expectedCurrent, next TrustState, pack Pack, failpoint func(preparePublishStage) error) error {
	if _, err := parseUUID(requestID); err != nil {
		return errors.New("prepare request ID must be a canonical UUID")
	}
	if !expectedCurrent.valid() || !next.valid() {
		return ErrBindingMismatch
	}
	record, err := packToRecord(pack)
	if err != nil {
		return err
	}
	recordDigest, err := packRecordDigest(record)
	if err != nil {
		return err
	}
	return s.withLease(ctx, LeaseExclusive, func() error {
		var journal JournalRecord
		if err := s.readRecordLocked(ObjectJournal, requestID, &journal); err != nil {
			return err
		}
		if journal.Kind != JournalPreparePublish {
			return ErrInvalidState
		}
		var detail prepareDetail
		if err := decodeClosed(journal.Detail, &detail); err != nil || detail.validate(journal.State) != nil {
			return fmt.Errorf("%w: invalid prepare publish journal", ErrCorruptStore)
		}
		switch detail.Purpose {
		case "prepare_intent":
			if !bytes.Equal(s.trust.canonical, expectedCurrent.canonical) && !bytes.Equal(s.trust.canonical, next.canonical) {
				return ErrInvalidState
			}
			publication, err := newPreparePublicationDetail(detail, expectedCurrent, next, record)
			if err != nil {
				return err
			}
			publicationBytes, err := marshalCanonical(publication)
			if err != nil {
				return err
			}
			journal.State = "publishing"
			journal.TargetID = record.PackID
			journal.Revision = "2"
			journal.Detail = publicationBytes
			if err := s.writeRecordLocked(ObjectJournal, journal.JournalID, journal, true); err != nil {
				return err
			}
			if failpoint != nil {
				if err := failpoint(preparePublishAfterJournalDurable); err != nil {
					return err
				}
			}
			detail = publication
		case "prepare_publish":
			journalRecord, err := marshalCanonical(detail.PackRecord)
			if err != nil {
				return err
			}
			candidateRecord, err := marshalCanonical(record)
			if err != nil {
				return err
			}
			if detail.PackID != record.PackID || detail.PackRecordDigest != recordDigest || !bytes.Equal(journalRecord, candidateRecord) || !bytes.Equal(detail.BaseTrustState, expectedCurrent.canonical) || !bytes.Equal(detail.NextTrustState, next.canonical) {
				return ErrImmutableOperation
			}
		default:
			return ErrInvalidState
		}
		return s.completePreparePublishLocked(journal, detail, failpoint)
	})
}

func (s *Store) completePreparePublishLocked(journal JournalRecord, detail prepareDetail, failpoint func(preparePublishStage) error) error {
	if journal.Kind != JournalPreparePublish || detail.Purpose != "prepare_publish" || detail.validate(journal.State) != nil || (journal.State != "publishing" && journal.State != "completed") {
		return fmt.Errorf("%w: invalid prepare publication recovery journal", ErrCorruptStore)
	}
	base, err := NewTrustState(detail.BaseTrustState)
	if err != nil {
		return fmt.Errorf("%w: invalid prepare base trust checkpoint", ErrCorruptStore)
	}
	next, err := NewTrustState(detail.NextTrustState)
	if err != nil {
		return fmt.Errorf("%w: invalid prepare next trust checkpoint", ErrCorruptStore)
	}
	currentIsBase := bytes.Equal(s.trust.canonical, base.canonical)
	currentIsNext := bytes.Equal(s.trust.canonical, next.canonical)
	if (journal.State == "publishing" && !currentIsBase && !currentIsNext) || (journal.State == "completed" && !currentIsNext) {
		return fmt.Errorf("%w: prepare publication trust checkpoint conflicts with the durable profile", ErrCorruptStore)
	}
	record := *detail.PackRecord
	record.CanonicalBytes = append(json.RawMessage(nil), detail.PackRecord.CanonicalBytes...)
	exists, err := s.packRecordExistsLocked(record)
	if errors.Is(err, ErrInvalidState) {
		return fmt.Errorf("%w: prepare publication conflicts with an existing pack", ErrCorruptStore)
	}
	if err != nil {
		return err
	}
	if !exists {
		if err := s.writeRecordLocked(ObjectPack, record.PackID, record, false); err != nil {
			return err
		}
	}
	if journal.State == "publishing" {
		if failpoint != nil {
			if err := failpoint(preparePublishAfterPackDurable); err != nil {
				return err
			}
		}
		if currentIsBase && !currentIsNext {
			if err := s.updateTrustStateLocked(next); err != nil {
				return err
			}
		}
		if failpoint != nil {
			if err := failpoint(preparePublishAfterTrustDurable); err != nil {
				return err
			}
		}
		journal.State = "completed"
		journal.Revision = "3"
		if err := s.writeRecordLocked(ObjectJournal, journal.JournalID, journal, true); err != nil {
			return err
		}
		if failpoint != nil {
			if err := failpoint(preparePublishAfterIntentCompleted); err != nil {
				return err
			}
		}
	}
	if err := deleteManaged(s.root, objectRelative(ObjectJournal, journal.JournalID)); err != nil {
		return err
	}
	if failpoint != nil {
		if err := failpoint(preparePublishAfterJournalDeleted); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SaveImmutableOperation(ctx context.Context, operation QueuedOperation) error {
	record, err := operationToRecord(operation)
	if err != nil {
		return err
	}
	return s.withLease(ctx, LeaseExclusive, func() error {
		if existing, err := s.rawRecordLocked(ObjectOperation, record.OperationID); err == nil {
			candidate, _ := marshalCanonical(record)
			if bytes.Equal(existing, candidate) {
				return nil
			}
			return ErrImmutableOperation
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := s.ensureSequenceUnusedLocked(record.OperationID, record.DeviceSequence); err != nil {
			return err
		}
		return s.writeRecordLocked(ObjectOperation, record.OperationID, record, false)
	})
}

func (s *Store) ListQueuedOperations(ctx context.Context, limit int) ([]QueuedOperation, error) {
	if limit <= 0 {
		limit = 50
	}
	var result []QueuedOperation
	err := s.withLease(ctx, LeaseShared, func() error {
		operations, err := s.operationRecordsLocked()
		if err != nil {
			return err
		}
		receipts, err := s.receiptMapLocked()
		if err != nil {
			return err
		}
		for _, record := range operations {
			if receipt, exists := receipts[record.OperationID]; exists && receipt.State != StateQueued {
				continue
			}
			operation, err := record.public()
			if err != nil {
				return err
			}
			result = append(result, operation)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].DeviceSequence < result[j].DeviceSequence })
		if len(result) > limit {
			result = result[:limit]
		}
		return nil
	})
	return result, err
}

func (s *Store) GetOperation(ctx context.Context, operationID string) (QueuedOperation, error) {
	var result QueuedOperation
	err := s.withLease(ctx, LeaseShared, func() error {
		var record OperationRecord
		if err := s.readRecordLocked(ObjectOperation, operationID, &record); err != nil {
			return err
		}
		var err error
		result, err = record.public()
		return err
	})
	return result, err
}

func (s *Store) BeginSync(ctx context.Context, journalID string, operationIDs []string) (SyncBatch, error) {
	if _, err := parseUUID(journalID); err != nil {
		return SyncBatch{}, errors.New("sync journal ID must be a canonical UUID")
	}
	ids, err := sortedUniqueUUIDs(operationIDs)
	if err != nil || len(ids) == 0 || len(ids) > 50 {
		return SyncBatch{}, errors.New("sync operation IDs are invalid")
	}
	var result SyncBatch
	err = s.withLease(ctx, LeaseExclusive, func() error {
		operations := make([]QueuedOperation, 0, len(ids))
		for _, id := range ids {
			var record OperationRecord
			if err := s.readRecordLocked(ObjectOperation, id, &record); err != nil {
				return err
			}
			state, err := s.currentStateLocked(id)
			if err != nil || state != StateQueued {
				return ErrInvalidState
			}
			operation, _ := record.public()
			operations = append(operations, operation)
		}
		sort.Slice(operations, func(i, j int) bool { return operations[i].DeviceSequence < operations[j].DeviceSequence })
		detail, _ := marshalCanonical(replaceDetail{Purpose: "sync_upload", OperationIDs: ids, DeleteOperation: false})
		journal := newJournal(journalID, JournalObjectReplace, "pending", journalID, journalID, 1, detail)
		if err := s.writeRecordLocked(ObjectJournal, journalID, journal, false); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			return ErrInvalidState
		}
		for _, operation := range operations {
			resultRecord, err := receiptFromResult(SyncResult{OperationID: operation.ID, SubmissionID: operation.SubmissionID, State: StateUploading, Receipt: json.RawMessage("null"), Status: json.RawMessage("null"), UpdatedAt: time.Now().UTC()}, operation.DeviceSequence)
			if err != nil {
				return err
			}
			if err := s.writeRecordLocked(ObjectReceipt, operation.ID, resultRecord, true); err != nil {
				return err
			}
		}
		result = SyncBatch{JournalID: journalID, Operations: operations}
		return nil
	})
	return result, err
}

func (s *Store) FinishSync(ctx context.Context, journalID string) error {
	if _, err := parseUUID(journalID); err != nil {
		return err
	}
	return s.withLease(ctx, LeaseExclusive, func() error {
		var journal JournalRecord
		if err := s.readRecordLocked(ObjectJournal, journalID, &journal); err != nil {
			return err
		}
		if journal.Kind != JournalObjectReplace {
			return ErrInvalidState
		}
		var detail replaceDetail
		if err := decodeClosed(journal.Detail, &detail); err != nil || detail.Purpose != "sync_upload" {
			return fmt.Errorf("%w: invalid sync journal", ErrCorruptStore)
		}
		for _, id := range detail.OperationIDs {
			state, err := s.currentStateLocked(id)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if state == StateUploading {
				return ErrInvalidState
			}
		}
		return deleteManaged(s.root, objectRelative(ObjectJournal, journalID))
	})
}

func (s *Store) ApplySyncResult(ctx context.Context, result SyncResult) error {
	return s.applyStatus(ctx, result)
}

func (s *Store) SaveStatus(ctx context.Context, result SyncResult) error {
	return s.applyStatus(ctx, result)
}

func (s *Store) GetStatus(ctx context.Context, operationID string) (OperationStatus, error) {
	var result OperationStatus
	err := s.withLease(ctx, LeaseShared, func() error {
		var receipt ReceiptRecord
		if err := s.readRecordLocked(ObjectReceipt, operationID, &receipt); err == nil {
			var publicErr error
			result, publicErr = receipt.public()
			return publicErr
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var operation OperationRecord
		if err := s.readRecordLocked(ObjectOperation, operationID, &operation); err != nil {
			return err
		}
		queuedAt, _ := parseRecordTime(operation.QueuedAt)
		result = OperationStatus{OperationID: operation.OperationID, SubmissionID: operation.SubmissionID, State: StateQueued, ReasonCodes: []string{}, UpdatedAt: queuedAt}
		return nil
	})
	return result, err
}

func (s *Store) applyStatus(ctx context.Context, result SyncResult) error {
	return s.withLease(ctx, LeaseExclusive, func() error {
		sequence, submission, current, err := s.operationIdentityLocked(result.OperationID)
		if err != nil {
			return err
		}
		if submission != result.SubmissionID || !allowedTransition(current, result.State) {
			return ErrInvalidState
		}
		record, err := receiptFromResult(result, sequence)
		if err != nil {
			return err
		}
		deleteOperation := result.State == StateArchivedPendingEvidence || result.State == StateTerminal
		return s.replaceReceiptLocked(record, deleteOperation)
	})
}

func (s *Store) Summary(ctx context.Context, now time.Time) (Summary, error) {
	var summary Summary
	err := s.withLease(ctx, LeaseShared, func() error {
		packs, err := s.packRecordsLocked()
		if err != nil {
			return err
		}
		now = now.UTC()
		for _, pack := range packs {
			summary.PackCount++
			eligible, _ := parseRecordTime(pack.EligibleUntil)
			if now.Before(eligible) {
				summary.AvailablePackCount++
				summary.AvailableItemCount += pack.ItemCount
				if summary.EarliestExpiry == nil || eligible.Before(*summary.EarliestExpiry) {
					copyTime := eligible
					summary.EarliestExpiry = &copyTime
				}
			}
		}
		receipts, err := s.receiptMapLocked()
		if err != nil {
			return err
		}
		operations, err := s.operationRecordsLocked()
		if err != nil {
			return err
		}
		for _, operation := range operations {
			if _, exists := receipts[operation.OperationID]; !exists {
				summary.QueuedCount++
			}
		}
		for _, receipt := range receipts {
			switch receipt.State {
			case StateQueued:
				summary.QueuedCount++
			case StateUploading:
				summary.UploadingCount++
			case StateArchivedPendingEvidence:
				summary.ArchivedPendingCount++
			case StateTerminal:
				summary.TerminalCount++
			case StateConflict:
				summary.ConflictCount++
			case StateBlocked:
				summary.BlockedCount++
			}
			if (receipt.State == StateArchivedPendingEvidence || receipt.State == StateTerminal) && !bytes.Equal(receipt.Receipt, []byte("null")) {
				updated, _ := parseRecordTime(receipt.UpdatedAt)
				if summary.LastSuccessfulSync == nil || updated.After(*summary.LastSuccessfulSync) {
					copyTime := updated
					summary.LastSuccessfulSync = &copyTime
				}
			}
		}
		journals, err := s.journalRecordsLocked()
		if err != nil {
			return err
		}
		summary.PendingJournalCount = len(journals)
		return nil
	})
	return summary, err
}

func (s *Store) PreflightLogout(ctx context.Context) (LogoutPreflight, error) {
	summary, err := s.Summary(ctx, time.Now().UTC())
	if err != nil {
		return LogoutPreflight{}, err
	}
	nonterminal := summary.QueuedCount + summary.UploadingCount + summary.ArchivedPendingCount + summary.ConflictCount + summary.BlockedCount
	return LogoutPreflight{Nonterminal: nonterminal != 0, PendingJournals: summary.PendingJournalCount != 0, NonterminalCount: nonterminal, JournalCount: summary.PendingJournalCount}, nil
}

func (s *Store) Discard(ctx context.Context, kind ObjectKind, id string) error {
	if kind != ObjectPack && kind != ObjectOperation && kind != ObjectReceipt {
		return errors.New("only pack, operation, or receipt objects can be discarded individually")
	}
	if _, err := parseUUID(id); err != nil {
		return err
	}
	return s.withLease(ctx, LeaseExclusive, func() error {
		targets := []discardTarget{{Kind: kind, ID: id}}
		if kind == ObjectOperation {
			targets = append(targets, discardTarget{Kind: ObjectReceipt, ID: id})
		}
		journalID, err := randomUUID()
		if err != nil {
			return err
		}
		detail, _ := marshalCanonical(discardDetail{All: false, Targets: targets})
		journal := newJournal(formatUUID(journalID), JournalCryptoDiscard, "deleting", id, id, 1, detail)
		if err := s.writeRecordLocked(ObjectJournal, journal.JournalID, journal, false); err != nil {
			return err
		}
		if err := s.applyDiscardTargetsLocked(targets); err != nil {
			return err
		}
		return deleteManaged(s.root, objectRelative(ObjectJournal, journal.JournalID))
	})
}

func (s *Store) DiscardAll(ctx context.Context) error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.dek) != 32 {
		return ErrClosed
	}
	lease, err := AcquireLease(ctx, s.root, LeaseExclusive, s.leaseTimeout)
	if err != nil {
		return err
	}
	defer lease.Close()
	journalID, err := randomUUID()
	if err != nil {
		return err
	}
	detail, _ := marshalCanonical(discardDetail{All: true, Targets: []discardTarget{}})
	journal := newJournal(formatUUID(journalID), JournalCryptoDiscard, "deleting", s.binding.ProfileUUID(), s.binding.ProfileUUID(), 1, detail)
	if err := s.writeRecordLocked(ObjectJournal, journal.JournalID, journal, false); err != nil {
		return err
	}
	if err := removeAllManagedObjects(s.root); err != nil {
		return err
	}
	if err := deleteManaged(s.root, keyMigrationStagingFile); err != nil {
		return err
	}
	if err := deleteManaged(s.root, keyMigrationSourceBackendFile); err != nil {
		return err
	}
	if err := deleteManaged(s.root, "profile.key"); err != nil {
		return err
	}
	if exists, err := Exists(s.root); err != nil || exists {
		if err != nil {
			return err
		}
		return errors.New("offline profile key still exists after discard")
	}
	zeroBytes(s.dek)
	s.dek = nil
	s.trustRoot.canonical = nil
	s.trust.canonical = nil
	s.closed = true
	return nil
}

func (s *Store) Recover(ctx context.Context) error {
	return s.withLease(ctx, LeaseExclusive, s.recoverLocked)
}

func (s *Store) recoverLocked() error {
	return s.recoverLockedAllowKeyMigration(false)
}

func (s *Store) recoverLockedAllowKeyMigration(allowKeyMigration bool) error {
	if err := s.cleanTemporaryFilesLocked(); err != nil {
		return err
	}
	if _, err := s.scanNonceStateLocked(); err != nil {
		return err
	}
	journals, err := s.journalRecordsLocked()
	if err != nil {
		return err
	}
	keyMigrationCount := 0
	for _, journal := range journals {
		if journal.Kind == JournalKeyMigration {
			keyMigrationCount++
		}
	}
	if keyMigrationCount > 1 {
		return fmt.Errorf("%w: multiple key migration journals", ErrCorruptStore)
	}
	if keyMigrationCount == 1 && !allowKeyMigration {
		return ErrKeyMigrationPending
	}
	for _, journal := range journals {
		switch journal.Kind {
		case JournalPreparePublish:
			var detail prepareDetail
			if err := decodeClosed(journal.Detail, &detail); err != nil || detail.validate(journal.State) != nil {
				return fmt.Errorf("%w: invalid prepare journal", ErrCorruptStore)
			}
			switch detail.Purpose {
			case "prepare_intent":
				continue
			case "prepare_publish":
				if err := s.completePreparePublishLocked(journal, detail, nil); err != nil {
					return err
				}
			case "save_pack":
				if err := deleteManaged(s.root, objectRelative(ObjectJournal, journal.JournalID)); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%w: unknown prepare journal purpose", ErrCorruptStore)
			}
		case JournalObjectReplace:
			var detail replaceDetail
			if err := decodeClosed(journal.Detail, &detail); err != nil {
				return fmt.Errorf("%w: invalid replace journal", ErrCorruptStore)
			}
			switch detail.Purpose {
			case "sync_upload":
				for _, id := range detail.OperationIDs {
					var receipt ReceiptRecord
					if err := s.readRecordLocked(ObjectReceipt, id, &receipt); errors.Is(err, ErrNotFound) {
						continue
					} else if err != nil {
						return err
					}
					if receipt.State == StateUploading {
						receipt.State = StateQueued
						receipt.UpdatedAt = formatRecordTime(time.Now().UTC())
						if err := s.writeRecordLocked(ObjectReceipt, id, receipt, true); err != nil {
							return err
						}
					}
				}
			case "receipt_replace":
				if detail.DeleteOperation {
					var receipt ReceiptRecord
					if err := s.readRecordLocked(ObjectReceipt, journal.TargetID, &receipt); err == nil && (receipt.State == StateArchivedPendingEvidence || receipt.State == StateTerminal) {
						if err := deleteManaged(s.root, objectRelative(ObjectOperation, journal.TargetID)); err != nil {
							return err
						}
					} else if err != nil && !errors.Is(err, ErrNotFound) {
						return err
					}
				}
			default:
				return fmt.Errorf("%w: unknown object replace purpose", ErrCorruptStore)
			}
			if err := deleteManaged(s.root, objectRelative(ObjectJournal, journal.JournalID)); err != nil {
				return err
			}
		case JournalCryptoDiscard:
			var detail discardDetail
			if err := decodeClosed(journal.Detail, &detail); err != nil {
				return fmt.Errorf("%w: invalid discard journal", ErrCorruptStore)
			}
			if detail.All {
				return errors.New("offline discard-all was interrupted; retry DiscardAll with the same passphrase")
			}
			if err := s.applyDiscardTargetsLocked(detail.Targets); err != nil {
				return err
			}
			if err := deleteManaged(s.root, objectRelative(ObjectJournal, journal.JournalID)); err != nil {
				return err
			}
		case JournalKeyMigration:
			if !allowKeyMigration {
				return ErrKeyMigrationPending
			}
			continue
		default:
			return fmt.Errorf("%w: unsupported recovery journal", ErrCorruptStore)
		}
	}
	// An uploading state is never durable without its sync journal. Fail-safe
	// recovery returns it to queued using the exact immutable operation bytes.
	receipts, err := s.receiptMapLocked()
	if err != nil {
		return err
	}
	for id, receipt := range receipts {
		if receipt.State == StateUploading {
			receipt.State = StateQueued
			receipt.UpdatedAt = formatRecordTime(time.Now().UTC())
			if err := s.writeRecordLocked(ObjectReceipt, id, receipt, true); err != nil {
				return err
			}
		}
	}
	_, err = s.scanNonceStateLocked()
	return err
}

func (s *Store) ensureNoPendingPreparePublicationLocked() error {
	journals, err := s.journalRecordsLocked()
	if err != nil {
		return err
	}
	for _, journal := range journals {
		if journal.Kind != JournalPreparePublish {
			continue
		}
		var detail prepareDetail
		if err := decodeClosed(journal.Detail, &detail); err != nil || detail.validate(journal.State) != nil {
			return fmt.Errorf("%w: invalid prepare publication journal", ErrCorruptStore)
		}
		if detail.Purpose == "prepare_publish" {
			return fmt.Errorf("%w: prepare publication recovery is incomplete", ErrCorruptStore)
		}
	}
	return nil
}

func (s *Store) withLease(ctx context.Context, mode LeaseMode, action func() error) error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.dek) != 32 {
		return ErrClosed
	}
	lease, err := AcquireLease(ctx, s.root, mode, s.leaseTimeout)
	if err != nil {
		return err
	}
	defer lease.Close()
	if mode == LeaseShared {
		if err := s.ensureNoPendingPreparePublicationLocked(); err != nil {
			return err
		}
	}
	return action()
}

func (s *Store) bootstrapCounterLocked(prefix [4]byte) error {
	nonce, _ := nonceFor(prefix, 1)
	detailBytes, _ := marshalCanonical(counterDetail{NoncePrefix: hex.EncodeToString(prefix[:]), HighWater: "2"})
	journal := newJournal(s.binding.ProfileUUID(), JournalCounterReservation, "reserved", s.binding.ProfileUUID(), s.binding.ProfileUUID(), 2, detailBytes)
	plain, err := marshalCanonical(journal)
	if err != nil {
		return err
	}
	header := NewObjectHeader(s.binding, ObjectJournal, s.binding.ProfileID, nonce, uint64(len(plain)+16))
	container, err := sealContainer(s.dek, header, plain)
	zeroBytes(plain)
	if err != nil {
		return err
	}
	defer zeroBytes(container)
	return atomicWriteManaged(s.root, objectRelative(ObjectJournal, s.binding.ProfileUUID()), container, false)
}

func (s *Store) reserveNonceLocked() ([12]byte, error) {
	var zero [12]byte
	state, err := s.scanNonceStateLocked()
	if err != nil {
		return zero, err
	}
	if state.highWater > math.MaxUint64-2 {
		return zero, ErrCounterOverflow
	}
	journalCounter := state.highWater + 1
	objectCounter := state.highWater + 2
	journalNonce, _ := nonceFor(state.prefix, journalCounter)
	objectNonce, _ := nonceFor(state.prefix, objectCounter)
	detailBytes, _ := marshalCanonical(counterDetail{NoncePrefix: hex.EncodeToString(state.prefix[:]), HighWater: canonicalUint(objectCounter)})
	journal := newJournal(s.binding.ProfileUUID(), JournalCounterReservation, "reserved", s.binding.ProfileUUID(), s.binding.ProfileUUID(), objectCounter, detailBytes)
	plain, err := marshalCanonical(journal)
	if err != nil {
		return zero, err
	}
	header := NewObjectHeader(s.binding, ObjectJournal, s.binding.ProfileID, journalNonce, uint64(len(plain)+16))
	container, err := sealContainer(s.dek, header, plain)
	zeroBytes(plain)
	if err != nil {
		return zero, err
	}
	defer zeroBytes(container)
	if err := atomicWriteManaged(s.root, objectRelative(ObjectJournal, s.binding.ProfileUUID()), container, true); err != nil {
		return zero, err
	}
	return objectNonce, nil
}

type nonceState struct {
	prefix    [4]byte
	highWater uint64
}

func (s *Store) loadCounterStateLocked() (nonceState, error) {
	var result nonceState
	container, err := readManaged(s.root, objectRelative(ObjectJournal, s.binding.ProfileUUID()), maxManagedFile)
	if err != nil {
		return result, err
	}
	defer zeroBytes(container)
	header, plain, err := openContainer(s.dek, container, s.binding, ObjectJournal, s.binding.ProfileID)
	if err != nil {
		return result, err
	}
	defer zeroBytes(plain)
	var journal JournalRecord
	if err := decodeClosed(plain, &journal); err != nil || journal.validate() != nil || journal.Kind != JournalCounterReservation || journal.State != "reserved" {
		return result, fmt.Errorf("%w: invalid counter reservation journal", ErrCorruptStore)
	}
	var detail counterDetail
	if err := decodeClosed(journal.Detail, &detail); err != nil {
		return result, fmt.Errorf("%w: invalid counter reservation detail", ErrCorruptStore)
	}
	prefix, err := decodeNoncePrefix(detail.NoncePrefix)
	if err != nil {
		return result, fmt.Errorf("%w: invalid nonce prefix", ErrCorruptStore)
	}
	highWater, err := parseCanonicalUint(detail.HighWater, true)
	if err != nil || highWater < 2 || journal.Revision != detail.HighWater {
		return result, fmt.Errorf("%w: invalid nonce high-water", ErrCounterRollback)
	}
	headerPrefix, headerCounter := nonceParts(header.Nonce)
	if headerPrefix != prefix || headerCounter == math.MaxUint64 || headerCounter+1 != highWater {
		return result, ErrCounterRollback
	}
	return nonceState{prefix: prefix, highWater: highWater}, nil
}

func (s *Store) scanNonceStateLocked() (nonceState, error) {
	state, err := s.loadCounterStateLocked()
	if err != nil {
		return nonceState{}, err
	}
	seen := map[uint64]string{}
	for _, kind := range []ObjectKind{ObjectProfile, ObjectPack, ObjectOperation, ObjectReceipt, ObjectJournal} {
		ids, err := s.listObjectIDsLocked(kind)
		if err != nil {
			return nonceState{}, err
		}
		for _, id := range ids {
			container, err := readManaged(s.root, objectRelative(kind, id), maxManagedFile)
			if err != nil {
				return nonceState{}, err
			}
			if len(container) < ObjectHeaderSize {
				zeroBytes(container)
				return nonceState{}, fmt.Errorf("%w: truncated managed object", ErrCorruptStore)
			}
			header, err := DecodeObjectHeader(container[:ObjectHeaderSize])
			zeroBytes(container)
			if err != nil {
				return nonceState{}, err
			}
			logical, _ := parseUUID(id)
			if err := header.ValidateBinding(s.binding, kind, logical); err != nil {
				return nonceState{}, err
			}
			prefix, counter := nonceParts(header.Nonce)
			if prefix != state.prefix || counter == 0 {
				return nonceState{}, fmt.Errorf("%w: nonce prefix mismatch", ErrCounterRollback)
			}
			if previous, duplicate := seen[counter]; duplicate {
				_ = previous
				return nonceState{}, fmt.Errorf("%w: duplicate nonce counter", ErrCounterRollback)
			}
			seen[counter] = id
			if counter > state.highWater {
				return nonceState{}, ErrCounterRollback
			}
		}
	}
	return state, nil
}

func (s *Store) writeRecordLocked(kind ObjectKind, id string, value any, replace bool) error {
	logicalID, err := parseUUID(id)
	if err != nil {
		return err
	}
	plain, err := marshalCanonical(value)
	if err != nil {
		return err
	}
	defer zeroBytes(plain)
	nonce, err := s.reserveNonceLocked()
	if err != nil {
		return err
	}
	header := NewObjectHeader(s.binding, kind, logicalID, nonce, uint64(len(plain)+16))
	container, err := sealContainer(s.dek, header, plain)
	if err != nil {
		return err
	}
	defer zeroBytes(container)
	if err := atomicWriteManaged(s.root, objectRelative(kind, id), container, replace); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	return nil
}

func (s *Store) readRecordLocked(kind ObjectKind, id string, target any) error {
	logicalID, err := parseUUID(id)
	if err != nil {
		return err
	}
	container, err := readManaged(s.root, objectRelative(kind, id), maxManagedFile)
	if err != nil {
		return err
	}
	defer zeroBytes(container)
	_, plain, err := openContainer(s.dek, container, s.binding, kind, logicalID)
	if err != nil {
		return err
	}
	defer zeroBytes(plain)
	if err := decodeClosed(plain, target); err != nil {
		return fmt.Errorf("%w: invalid sealed record", ErrCorruptStore)
	}
	switch record := target.(type) {
	case *PackRecord:
		err = record.validate()
	case *OperationRecord:
		err = record.validate()
	case *ReceiptRecord:
		err = record.validate()
	case *JournalRecord:
		err = record.validate()
	case *ProfileRecord:
		err = record.validate(s.binding, TrustState{})
	}
	if err != nil {
		return fmt.Errorf("%w: invalid sealed record", ErrCorruptStore)
	}
	return nil
}

func (s *Store) rawRecordLocked(kind ObjectKind, id string) ([]byte, error) {
	logicalID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	container, err := readManaged(s.root, objectRelative(kind, id), maxManagedFile)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(container)
	_, plain, err := openContainer(s.dek, container, s.binding, kind, logicalID)
	if err != nil {
		return nil, err
	}
	canonical, err := requireCanonicalObject(plain)
	if err != nil {
		zeroBytes(plain)
		return nil, fmt.Errorf("%w: invalid record bytes", ErrCorruptStore)
	}
	result := append([]byte(nil), canonical...)
	zeroBytes(plain)
	return result, nil
}

func objectRelative(kind ObjectKind, id string) string {
	return "objects/" + kind.directory() + "/" + id + objectSuffix
}

func (s *Store) listObjectIDsLocked(kind ObjectKind) ([]string, error) {
	entries, err := listManaged(s.root, "objects/"+kind.directory())
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".edu-offline-") {
			return nil, fmt.Errorf("%w: unrecovered temporary object", ErrCorruptStore)
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(name, objectSuffix) {
			return nil, ErrUnsafePath
		}
		id := strings.TrimSuffix(name, objectSuffix)
		if _, err := parseUUID(id); err != nil {
			return nil, ErrUnsafePath
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) packRecordsLocked() ([]PackRecord, error) {
	ids, err := s.listObjectIDsLocked(ObjectPack)
	if err != nil {
		return nil, err
	}
	result := make([]PackRecord, 0, len(ids))
	for _, id := range ids {
		var record PackRecord
		if err := s.readRecordLocked(ObjectPack, id, &record); err != nil {
			return nil, err
		}
		if record.PackID != id {
			return nil, ErrBindingMismatch
		}
		result = append(result, record)
	}
	return result, nil
}

func (s *Store) operationRecordsLocked() ([]OperationRecord, error) {
	ids, err := s.listObjectIDsLocked(ObjectOperation)
	if err != nil {
		return nil, err
	}
	result := make([]OperationRecord, 0, len(ids))
	for _, id := range ids {
		var record OperationRecord
		if err := s.readRecordLocked(ObjectOperation, id, &record); err != nil {
			return nil, err
		}
		if record.OperationID != id {
			return nil, ErrBindingMismatch
		}
		result = append(result, record)
	}
	return result, nil
}

func (s *Store) receiptMapLocked() (map[string]ReceiptRecord, error) {
	ids, err := s.listObjectIDsLocked(ObjectReceipt)
	if err != nil {
		return nil, err
	}
	result := make(map[string]ReceiptRecord, len(ids))
	for _, id := range ids {
		var record ReceiptRecord
		if err := s.readRecordLocked(ObjectReceipt, id, &record); err != nil {
			return nil, err
		}
		if record.OperationID != id {
			return nil, ErrBindingMismatch
		}
		result[id] = record
	}
	return result, nil
}

func (s *Store) journalRecordsLocked() ([]JournalRecord, error) {
	ids, err := s.listObjectIDsLocked(ObjectJournal)
	if err != nil {
		return nil, err
	}
	result := make([]JournalRecord, 0, len(ids))
	for _, id := range ids {
		if id == s.binding.ProfileUUID() {
			continue
		}
		var record JournalRecord
		if err := s.readRecordLocked(ObjectJournal, id, &record); err != nil {
			return nil, err
		}
		if record.JournalID != id {
			return nil, ErrBindingMismatch
		}
		result = append(result, record)
	}
	return result, nil
}

func (s *Store) ensureSequenceUnusedLocked(operationID, sequence string) error {
	operations, err := s.operationRecordsLocked()
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if operation.DeviceSequence == sequence && operation.OperationID != operationID {
			return ErrImmutableOperation
		}
	}
	receipts, err := s.receiptMapLocked()
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		if receipt.DeviceSequence == sequence && receipt.OperationID != operationID {
			return ErrImmutableOperation
		}
	}
	return nil
}

func (s *Store) currentStateLocked(operationID string) (LocalState, error) {
	var receipt ReceiptRecord
	if err := s.readRecordLocked(ObjectReceipt, operationID, &receipt); err == nil {
		return receipt.State, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	var operation OperationRecord
	if err := s.readRecordLocked(ObjectOperation, operationID, &operation); err != nil {
		return "", err
	}
	return StateQueued, nil
}

func (s *Store) operationIdentityLocked(operationID string) (uint64, string, LocalState, error) {
	if _, err := parseUUID(operationID); err != nil {
		return 0, "", "", err
	}
	var receipt ReceiptRecord
	if err := s.readRecordLocked(ObjectReceipt, operationID, &receipt); err == nil {
		sequence, _ := parseCanonicalUint(receipt.DeviceSequence, true)
		return sequence, receipt.SubmissionID, receipt.State, nil
	} else if !errors.Is(err, ErrNotFound) {
		return 0, "", "", err
	}
	var operation OperationRecord
	if err := s.readRecordLocked(ObjectOperation, operationID, &operation); err != nil {
		return 0, "", "", err
	}
	sequence, _ := parseCanonicalUint(operation.DeviceSequence, true)
	return sequence, operation.SubmissionID, StateQueued, nil
}

func (s *Store) replaceReceiptLocked(record ReceiptRecord, deleteOperation bool) error {
	journalID, err := randomUUID()
	if err != nil {
		return err
	}
	detail, _ := marshalCanonical(replaceDetail{Purpose: "receipt_replace", OperationIDs: []string{record.OperationID}, DeleteOperation: deleteOperation})
	journal := newJournal(formatUUID(journalID), JournalObjectReplace, "pending", record.OperationID, record.OperationID, 1, detail)
	if err := s.writeRecordLocked(ObjectJournal, journal.JournalID, journal, false); err != nil {
		return err
	}
	replace := true
	if _, err := readManaged(s.root, objectRelative(ObjectReceipt, record.OperationID), maxManagedFile); errors.Is(err, ErrNotFound) {
		replace = false
	} else if err != nil {
		return err
	}
	if err := s.writeRecordLocked(ObjectReceipt, record.OperationID, record, replace); err != nil {
		return err
	}
	if deleteOperation {
		if err := deleteManaged(s.root, objectRelative(ObjectOperation, record.OperationID)); err != nil {
			return err
		}
	}
	return deleteManaged(s.root, objectRelative(ObjectJournal, journal.JournalID))
}

func (s *Store) applyDiscardTargetsLocked(targets []discardTarget) error {
	for _, target := range targets {
		if target.Kind != ObjectPack && target.Kind != ObjectOperation && target.Kind != ObjectReceipt {
			return fmt.Errorf("%w: invalid discard target", ErrCorruptStore)
		}
		if _, err := parseUUID(target.ID); err != nil {
			return fmt.Errorf("%w: invalid discard identity", ErrCorruptStore)
		}
		if err := deleteManaged(s.root, objectRelative(target.Kind, target.ID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) cleanTemporaryFilesLocked() error {
	for _, kind := range []ObjectKind{ObjectProfile, ObjectPack, ObjectOperation, ObjectReceipt, ObjectJournal} {
		entries, err := listManaged(s.root, "objects/"+kind.directory())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".edu-offline-") && !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				if err := deleteManaged(s.root, "objects/"+kind.directory()+"/"+entry.Name()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func newJournal(id string, kind JournalKind, state, sourceID, targetID string, revision uint64, detail json.RawMessage) JournalRecord {
	return JournalRecord{JournalVersion: 1, JournalID: id, Kind: kind, State: state, SourceID: sourceID, TargetID: targetID, Revision: canonicalUint(revision), CreatedAt: formatRecordTime(time.Now().UTC().Truncate(time.Microsecond)), Detail: detail}
}

func entriesPresent(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() != ".lease" {
			return true
		}
	}
	return false
}

func removeAllManagedObjects(root string) error {
	for _, kind := range []ObjectKind{ObjectProfile, ObjectPack, ObjectOperation, ObjectReceipt, ObjectJournal} {
		entries, err := listManaged(root, "objects/"+kind.directory())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return ErrUnsafePath
			}
			if err := deleteManaged(root, "objects/"+kind.directory()+"/"+entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// PurgeProfile removes all managed encrypted objects and the wrapped key without
// requiring the profile DEK. It is used only after an authenticated privacy task.
// PurgeSession holds the profile-wide exclusive lease while the caller removes
// any platform credential and acknowledges the purge to the server.
type PurgeSession struct {
	root    string
	lease   *Lease
	backend uint8
	closed  bool
}

// KeyBackend reports how the deleted profile.key was wrapped before purge.
// Zero means no profile key existed when the exclusive lease was acquired.
func (p *PurgeSession) KeyBackend() uint8 { return p.backend }

// Close releases the exclusive lease and removes the now-empty managed root.
func (p *PurgeSession) Close() error { return p.finish(true) }

// Release preserves the non-secret purge backend marker so a later invocation
// can safely retry a failed or interrupted server acknowledgment.
func (p *PurgeSession) Release() error { return p.finish(false) }

func (p *PurgeSession) finish(removeRoot bool) error {
	if p == nil || p.closed {
		return nil
	}
	p.closed = true
	if err := p.lease.Close(); err != nil {
		return err
	}
	if !removeRoot {
		return nil
	}
	return removePurgedRoot(p.root)
}

// BeginPurgeProfile deletes every encrypted profile object and profile.key while
// retaining the exclusive profile lease for platform-key deletion and the ACK.
func BeginPurgeProfile(ctx context.Context, root string) (*PurgeSession, error) {
	lease, err := AcquireLease(ctx, root, LeaseExclusive, DefaultLeaseTimeout)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = lease.Close()
		}
	}()
	var backend uint8
	if keyFile, err := readManaged(root, "profile.key", KeyHeaderSize+48); err == nil {
		header, decodeErr := DecodeKeyHeader(keyFile[:KeyHeaderSize])
		zeroBytes(keyFile)
		if decodeErr != nil {
			return nil, decodeErr
		}
		backend = header.Backend
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	} else if marker, markerErr := readManaged(root, purgeBackendMarker, 1); markerErr == nil {
		if len(marker) != 1 || (marker[0] != KeyBackendPassphrase && marker[0] != KeyBackendSystem) {
			return nil, ErrCorruptStore
		}
		backend = marker[0]
	} else if !errors.Is(markerErr, ErrNotFound) {
		return nil, markerErr
	}
	if sourceBackend, markerErr := migrationSourceBackend(root); markerErr == nil {
		if sourceBackend == KeyBackendSystem {
			backend = KeyBackendSystem
		}
	} else if !errors.Is(markerErr, ErrNotFound) {
		return nil, markerErr
	}
	if stage, stageErr := readManaged(root, keyMigrationStagingFile, KeyHeaderSize+48); stageErr == nil {
		if len(stage) != KeyHeaderSize+48 {
			zeroBytes(stage)
			return nil, ErrCorruptStore
		}
		stageHeader, decodeErr := DecodeKeyHeader(stage[:KeyHeaderSize])
		zeroBytes(stage)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if stageHeader.Backend == KeyBackendSystem {
			backend = KeyBackendSystem
		}
	} else if !errors.Is(stageErr, ErrNotFound) {
		return nil, stageErr
	}
	if backend != 0 {
		if err := atomicWriteManaged(root, purgeBackendMarker, []byte{backend}, true); err != nil {
			return nil, fmt.Errorf("persist offline purge backend marker: %w", err)
		}
	}
	if err := removeAllManagedObjects(root); err != nil {
		return nil, err
	}
	if err := deleteManaged(root, keyMigrationStagingFile); err != nil {
		return nil, err
	}
	if err := deleteManaged(root, keyMigrationSourceBackendFile); err != nil {
		return nil, err
	}
	if err := deleteManaged(root, "profile.key"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if exists, err := Exists(root); err != nil || exists {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("offline profile key still exists after privacy purge")
	}
	failed = false
	return &PurgeSession{root: root, lease: lease, backend: backend}, nil
}

func PurgeProfile(ctx context.Context, root string) error {
	purge, err := BeginPurgeProfile(ctx, root)
	if err != nil {
		return err
	}
	return purge.Close()
}

func removePurgedRoot(root string) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return ErrUnsafePath
	}
	var directories []string
	if err := filepath.WalkDir(absolute, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && path == absolute {
				return nil
			}
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(path, info) {
			return ErrUnsafePath
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(directories, func(left, right int) bool { return len(directories[left]) > len(directories[right]) })
	for _, directory := range directories {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
