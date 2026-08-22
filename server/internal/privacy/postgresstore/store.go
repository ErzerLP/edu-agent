package postgresstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool       *pgxpool.Pool
	permits    *privacy.ReadPermitManager
	owners     map[privacy.OwnerKind]privacy.LocalOwnerPort
	backupKeys privacy.BackupKeyDestroyer
	beforeStep func(privacy.StoreKind) error
}

type Option func(*Store)

func WithReadPermits(manager *privacy.ReadPermitManager) Option {
	return func(store *Store) { store.permits = manager }
}
func WithBeforeStep(hook func(privacy.StoreKind) error) Option {
	return func(store *Store) { store.beforeStep = hook }
}
func WithLocalOwner(port privacy.LocalOwnerPort) Option {
	return func(store *Store) {
		if privacy.ValidateOwnerPort(port) == nil {
			store.owners[port.Owner()] = port
		}
	}
}
func WithBackupKeyDestroyer(destroyer privacy.BackupKeyDestroyer) Option {
	return func(store *Store) { store.backupKeys = destroyer }
}
func New(pool *pgxpool.Pool, options ...Option) *Store {
	store := &Store{
		pool: pool, permits: privacy.DefaultReadPermits,
		owners: make(map[privacy.OwnerKind]privacy.LocalOwnerPort), backupKeys: newBackupKeyStore(),
	}
	for _, option := range options {
		option(store)
	}
	return store
}

func (s *Store) ownerPort(owner privacy.OwnerKind) (privacy.LocalOwnerPort, error) {
	port, ok := s.owners[owner]
	if !ok {
		return nil, &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: string(owner) + "_owner_port_missing"}
	}
	return port, nil
}

func (s *Store) syncReadPermits(ctx context.Context, generation int64) error {
	rows, err := s.pool.Query(ctx, `SELECT owner_kind,learner_generation,read_open FROM privacy_owner_generation_gates ORDER BY owner_kind`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var owner privacy.OwnerKind
		var current int64
		var readOpen bool
		if err := rows.Scan(&owner, &current, &readOpen); err != nil {
			return err
		}
		if current != generation || !owner.Valid() {
			return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "owner_read_gate_generation_mismatch"}
		}
		if readOpen {
			s.permits.Open(generation, owner)
		} else if err := s.permits.CloseAndDrain(ctx, generation, owner); err != nil {
			return err
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if seen != len(privacy.AllOwners) {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "owner_read_gate_set_incomplete"}
	}
	return nil
}

func (s *Store) CommitBarrier(ctx context.Context, request privacy.ErasureRequest) (privacy.ErasureReceipt, error) {
	return s.commitBarrier(ctx, request, nil)
}

func (s *Store) CommitBarrierAuthorized(ctx context.Context, request privacy.ErasureRequest, authorization privacy.ErasureGrantAuthorization) (privacy.ErasureReceipt, error) {
	return s.commitBarrier(ctx, request, &authorization)
}

func (s *Store) commitBarrier(ctx context.Context, request privacy.ErasureRequest, authorization *privacy.ErasureGrantAuthorization) (privacy.ErasureReceipt, error) {
	hash, err := request.OperationHash()
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if existing, found, err := s.replayOperation(ctx, request.DeviceID, request.OperationID, hash); err != nil || found {
		return existing, err
	}

	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('privacy-erasure-barrier',0))`); err != nil {
		connection.Release()
		return privacy.ErasureReceipt{}, err
	}
	releaseConnection := func() {
		if connection == nil {
			return
		}
		var unlocked bool
		if err := connection.QueryRow(context.Background(), `SELECT pg_advisory_unlock(hashtextextended('privacy-erasure-barrier',0))`).Scan(&unlocked); err != nil || !unlocked {
			raw := connection.Hijack()
			_ = raw.Close(context.Background())
			connection = nil
			return
		}
		connection.Release()
		connection = nil
	}
	defer releaseConnection()

	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	committed := false
	permitsClosing := false
	var current, target int64
	defer func() {
		_ = tx.Rollback(context.Background())
		if permitsClosing && !committed {
			s.permits.AbortClose(target, current, privacy.AllOwners...)
		}
	}()

	if id, storedHash, found, err := lookupOperationTx(ctx, tx, request.DeviceID, request.OperationID); err != nil {
		return privacy.ErasureReceipt{}, err
	} else if found {
		if storedHash != hash {
			return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeIdempotencyConflict, Reason: "operation_hash_mismatch"}
		}
		_ = tx.Rollback(ctx)
		releaseConnection()
		existing, err := s.Receipt(ctx, id)
		if err != nil {
			return privacy.ErasureReceipt{}, err
		}
		if err := s.syncReadPermits(ctx, existing.LearnerGeneration); err != nil {
			return privacy.ErasureReceipt{}, fmt.Errorf("synchronize replayed privacy read permits: %w", err)
		}
		return existing, nil
	}
	if authorization != nil {
		if authorization.DeviceID != request.DeviceID {
			return privacy.ErasureReceipt{}, privacy.ErrErasureGrantInvalid
		}
		if err := consumeErasureGrantTx(ctx, tx, *authorization); err != nil {
			if errors.Is(err, privacy.ErrErasureGrantInvalid) {
				if commitErr := tx.Commit(ctx); commitErr != nil {
					return privacy.ErasureReceipt{}, privacy.ErrErasureGrantStoreUnavailable
				}
				return privacy.ErasureReceipt{}, privacy.ErrErasureGrantInvalid
			}
			return privacy.ErasureReceipt{}, err
		}
	}

	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM privacy_erasure_heads WHERE status<>'verified'`).Scan(&active); err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if active != 0 {
		return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "active_erasure_exists"}
	}
	for _, owner := range privacy.AllOwners {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('privacy-owner:'||$1,0))`, owner); err != nil {
			return privacy.ErasureReceipt{}, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT owner_kind,learner_generation,read_open,write_open FROM privacy_owner_generation_gates ORDER BY owner_kind FOR UPDATE`)
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	current = -1
	count := 0
	for rows.Next() {
		var owner privacy.OwnerKind
		var generation int64
		var readOpen, writeOpen bool
		if err := rows.Scan(&owner, &generation, &readOpen, &writeOpen); err != nil {
			rows.Close()
			return privacy.ErasureReceipt{}, err
		}
		if !owner.Valid() || !readOpen || !writeOpen || current != -1 && generation != current {
			rows.Close()
			return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "owner_gate_not_open_at_current_generation"}
		}
		if current == -1 {
			current = generation
		}
		count++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return privacy.ErasureReceipt{}, err
	}
	rows.Close()
	if count != len(privacy.AllOwners) || current < 1 {
		return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "owner_gate_set_incomplete"}
	}
	if request.ExpectedCurrentLearnerGeneration != 0 && request.ExpectedCurrentLearnerGeneration != current {
		return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeIdempotencyConflict, Reason: "learner_generation_changed"}
	}
	target = current + 1
	permitsClosing = true
	if err := s.permits.CloseAndDrain(ctx, target, privacy.AllOwners...); err != nil {
		return privacy.ErasureReceipt{}, fmt.Errorf("drain privacy read permits: %w", err)
	}
	requestedAt := request.RequestedAt.UTC()
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return privacy.ErasureReceipt{}, err
	}
	now = now.UTC()
	erasureID := uuid.NewString()
	hashBytes, _ := hex.DecodeString(hash)
	if _, err := tx.Exec(ctx, `INSERT INTO privacy_erasures(id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,target_learner_generation,managed_backup_scheduled_unrecoverable_after,managed_backup_verified_unrecoverable_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, erasureID, request.DeviceID, request.OperationID, hashBytes, request.ReasonCode, request.ActorDeviceID, requestedAt, target, request.ManagedBackupUnrecoverableAfter.UTC(), now); err != nil {
		return privacy.ErasureReceipt{}, fmt.Errorf("insert privacy erasure: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at) VALUES($1,'barrier_committed',1,$2,$3)`, erasureID, request.ReasonCode, now); err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if s.backupKeys == nil {
		return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: "backup_key_destroyer_missing"}
	}
	if _, err := s.backupKeys.DestroyGenerationKeysTx(ctx, tx, privacy.BackupKeyDestroyRequest{
		ErasureID: erasureID, LearnerGeneration: target, RequestedAt: requestedAt,
		Deadline: request.ManagedBackupUnrecoverableAfter.UTC(), At: now,
	}); err != nil {
		return privacy.ErasureReceipt{}, err
	}
	learningPort, err := s.ownerPort(privacy.OwnerLearning)
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	appender, ok := learningPort.(privacy.RedactionEventAppender)
	if !ok {
		return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: "learning_redaction_event_appender_missing"}
	}
	event, err := appender.AppendEventRedactedTx(ctx, tx, privacy.RedactionEventAppendRequest{
		ErasureID: erasureID, LearnerGeneration: target, ReasonCode: request.ReasonCode,
		ActorDeviceID: request.ActorDeviceID, OperationID: request.OperationID, At: now,
	})
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO privacy_redaction_barriers(erasure_id,learner_generation,redacted_through_event_seq,policy_version,reason_code,event_id,committed_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, erasureID, target, event.RedactedThroughEvent, privacy.PolicyVersion, request.ReasonCode, event.EventID, now); err != nil {
		return privacy.ErasureReceipt{}, err
	}
	for _, owner := range privacy.AllOwners {
		port, err := s.ownerPort(owner)
		if err != nil {
			return privacy.ErasureReceipt{}, err
		}
		transition := privacy.GenerationTransition{ErasureID: erasureID, FromGeneration: current, TargetGeneration: target, At: now}
		if err := port.CloseGenerationTx(ctx, tx, transition); err != nil {
			return privacy.ErasureReceipt{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO privacy_owner_redaction_audit(id,erasure_id,owner_kind,learner_generation,action,created_at) VALUES($1,$2,$3,$4,'gate_closed',$5)`, uuid.NewString(), erasureID, owner, target, now); err != nil {
			return privacy.ErasureReceipt{}, err
		}
	}
	for _, store := range privacy.ReceiptSlots {
		status := privacy.StepPending
		completed := any(nil)
		reason := "awaiting_erasure_step"
		method := "pending_verification"
		if store == privacy.StoreProcessCache {
			status, completed, reason, method = privacy.StepSucceeded, now, "in_process_reads_drained", "read_permit_manager"
		} else if store == privacy.StoreExternalProvider {
			status, completed, reason, method = privacy.StepUnsupported, now, "no_external_provider_configured", "unsupported_by_local_core"
		}
		receiptID := uuid.NewString()
		scope := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d", erasureID, store, target)))
		if _, err := tx.Exec(ctx, `INSERT INTO privacy_erasure_step_receipts(id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method) VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,$9)`, receiptID, erasureID, store, scope[:], now, completed, status, reason, method); err != nil {
			return privacy.ErasureReceipt{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO privacy_erasure_receipt_heads(erasure_id,store_kind,current_receipt_id,current_version,updated_at) VALUES($1,$2,$3,1,$4)`, erasureID, store, receiptID, now); err != nil {
			return privacy.ErasureReceipt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return privacy.ErasureReceipt{}, err
	}
	committed = true
	releaseConnection()
	return s.Receipt(ctx, erasureID)
}

func (s *Store) replayOperation(ctx context.Context, deviceID, operationID, hash string) (privacy.ErasureReceipt, bool, error) {
	existing, storedHash, found, err := s.lookupOperation(ctx, deviceID, operationID)
	if err != nil || !found {
		return existing, found, err
	}
	if storedHash != hash {
		return privacy.ErasureReceipt{}, true, &privacy.Error{Code: privacy.CodeIdempotencyConflict, Reason: "operation_hash_mismatch"}
	}
	if err := s.syncReadPermits(ctx, existing.LearnerGeneration); err != nil {
		return privacy.ErasureReceipt{}, true, fmt.Errorf("synchronize replayed privacy read permits: %w", err)
	}
	return existing, true, nil
}

func lookupOperationTx(ctx context.Context, tx pgx.Tx, deviceID, operationID string) (string, string, bool, error) {
	var id string
	var hash []byte
	err := tx.QueryRow(ctx, `SELECT id,request_hash FROM privacy_erasures WHERE device_id=$1 AND operation_id=$2`, deviceID, operationID).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return id, hex.EncodeToString(hash), true, nil
}

func (s *Store) lookupOperation(ctx context.Context, deviceID, operationID string) (privacy.ErasureReceipt, string, bool, error) {
	var id string
	var hash []byte
	err := s.pool.QueryRow(ctx, `SELECT id,request_hash FROM privacy_erasures WHERE device_id=$1 AND operation_id=$2`, deviceID, operationID).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return privacy.ErasureReceipt{}, "", false, nil
	}
	if err != nil {
		return privacy.ErasureReceipt{}, "", false, err
	}
	receipt, err := s.Receipt(ctx, id)
	return receipt, hex.EncodeToString(hash), true, err
}

func (s *Store) Receipt(ctx context.Context, erasureID string) (privacy.ErasureReceipt, error) {
	var value privacy.ErasureReceipt
	err := s.pool.QueryRow(ctx, `SELECT e.id,h.status,h.summary_version,e.target_learner_generation,b.redacted_through_event_seq,b.policy_version,e.reason_code,e.requested_at,h.updated_at FROM privacy_erasures e JOIN privacy_erasure_heads h ON h.erasure_id=e.id JOIN privacy_redaction_barriers b ON b.erasure_id=e.id WHERE e.id=$1`, erasureID).Scan(&value.ErasureID, &value.Status, &value.SummaryVersion, &value.LearnerGeneration, &value.RedactedThroughEventSeq, &value.PolicyVersion, &value.ReasonCode, &value.RequestedAt, &value.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &privacy.Error{Code: privacy.CodeNotFound, Reason: "erasure_receipt_not_found"}
	}
	if err != nil {
		return value, err
	}
	rows, err := s.pool.Query(ctx, `SELECT r.id,r.store_kind,r.version,r.status,r.stable_reason,r.verification_method,r.started_at,r.completed_at,r.evidence_digest FROM privacy_erasure_receipt_heads h JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id WHERE h.erasure_id=$1`, erasureID)
	if err != nil {
		return value, err
	}
	defer rows.Close()
	for rows.Next() {
		var step privacy.StepReceipt
		var digest []byte
		if err := rows.Scan(&step.ID, &step.Store, &step.Version, &step.Status, &step.StableReason, &step.VerificationMethod, &step.StartedAt, &step.CompletedAt, &digest); err != nil {
			return value, err
		}
		step.EvidenceDigest = hex.EncodeToString(digest)
		value.Steps = append(value.Steps, step)
	}
	value.SortSteps()
	return value, rows.Err()
}

func (s *Store) RunLocalScrub(ctx context.Context, erasureID string) (privacy.ErasureReceipt, error) {
	for _, store := range privacy.LocalManagedSlots {
		if store == privacy.StoreProcessCache {
			continue
		}
		if err := s.RedactTx(ctx, erasureID, store); err != nil {
			receipt, receiptErr := s.Receipt(ctx, erasureID)
			if receiptErr != nil {
				return privacy.ErasureReceipt{}, errors.Join(err, receiptErr)
			}
			return receipt, err
		}
	}
	return s.Receipt(ctx, erasureID)
}

func (s *Store) RedactTx(ctx context.Context, erasureID string, store privacy.StoreKind) error {
	owner, ok := privacy.OwnerForStore(store)
	if !ok {
		return &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: string(store)}
	}
	port, err := s.ownerPort(owner)
	if err != nil {
		return err
	}
	if s.beforeStep != nil {
		if err := s.beforeStep(store); err != nil {
			return err
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var generation, through int64
	var receiptID string
	var version int64
	var status privacy.StepStatus
	var scope []byte
	err = tx.QueryRow(ctx, `SELECT e.target_learner_generation,b.redacted_through_event_seq,r.id,r.version,r.status,r.scope_digest FROM privacy_erasures e JOIN privacy_erasure_heads eh ON eh.erasure_id=e.id AND eh.status<>'verified' JOIN privacy_redaction_barriers b ON b.erasure_id=e.id JOIN privacy_erasure_receipt_heads rh ON rh.erasure_id=e.id AND rh.store_kind=$2 JOIN privacy_erasure_step_receipts r ON r.id=rh.current_receipt_id WHERE e.id=$1 FOR UPDATE OF eh,rh`, erasureID, store).Scan(&generation, &through, &receiptID, &version, &status, &scope)
	if err != nil {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: string(store), Cause: err}
	}
	request := privacy.LocalRedactionRequest{ErasureID: erasureID, Store: store, ReceiptID: receiptID, LearnerGeneration: generation, RedactedThroughEvent: through}
	if err := request.Validate(owner); err != nil {
		return err
	}
	if status == privacy.StepPending {
		if err := port.RedactTx(ctx, request); err != nil {
			return err
		}
		remaining, err := port.VerifyRedacted(ctx, request)
		if err != nil {
			return err
		}
		if remaining != 0 {
			return &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: fmt.Sprintf("%s_remaining_%d", store, remaining)}
		}
		now := time.Now().UTC()
		evidence := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\nverified", erasureID, store, generation)))
		newReceipt := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO privacy_erasure_step_receipts(id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method,evidence_digest) VALUES($1,$2,$3,$4,$5,$6,$6,'succeeded','local_content_redacted','zero_residual_body_scan',$7)`, newReceipt, erasureID, store, version+1, scope, now, evidence[:]); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE privacy_erasure_receipt_heads SET current_receipt_id=$3,current_version=$4,updated_at=$5 WHERE erasure_id=$1 AND store_kind=$2 AND current_receipt_id=$6`, erasureID, store, newReceipt, version+1, now, receiptID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "receipt_head_changed"}
		}
		receiptID, version, status = newReceipt, version+1, privacy.StepSucceeded
	} else if status != privacy.StepSucceeded && status != privacy.StepNotApplicable {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "receipt_not_pending"}
	}

	complete, err := ownerComplete(ctx, tx, erasureID, owner)
	if err != nil {
		return err
	}
	reopenOwner := complete && owner != privacy.OwnerMemory
	gateNeedsOpen := false
	if reopenOwner {
		var readOpen, writeOpen bool
		var activeErasure *string
		if err := tx.QueryRow(ctx, `SELECT read_open,write_open,active_erasure_id::text FROM privacy_owner_generation_gates WHERE owner_kind=$1 AND learner_generation=$2 FOR UPDATE`, owner, generation).Scan(&readOpen, &writeOpen, &activeErasure); err != nil {
			return err
		}
		switch {
		case !readOpen && !writeOpen && activeErasure != nil && *activeErasure == erasureID:
			gateNeedsOpen = true
		case readOpen && writeOpen && activeErasure == nil:
			// A resumed local scrub may observe an already-open persistent gate.
		default:
			return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: string(owner) + "_generation_gate_state_invalid"}
		}
	}
	now := time.Now().UTC()
	evidence := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\nverified", erasureID, store, generation)))
	if complete {
		if _, err := tx.Exec(ctx, `INSERT INTO privacy_owner_redaction_audit(id,erasure_id,owner_kind,learner_generation,receipt_id,action,evidence_digest,created_at) VALUES($1,$2,$3,$4,$5,'scrubbed',$6,$7),($8,$2,$3,$4,$5,'verified',$6,$7) ON CONFLICT DO NOTHING`, uuid.NewString(), erasureID, owner, generation, receiptID, evidence[:], now, uuid.NewString()); err != nil {
			return err
		}
	}
	if gateNeedsOpen {
		transition := privacy.GenerationTransition{ErasureID: erasureID, FromGeneration: generation, TargetGeneration: generation, ReceiptID: receiptID, At: now}
		if err := port.OpenGenerationTx(ctx, tx, transition); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO privacy_owner_redaction_audit(id,erasure_id,owner_kind,learner_generation,receipt_id,action,evidence_digest,created_at) VALUES($1,$2,$3,$4,$5,'gate_opened',$6,$7) ON CONFLICT DO NOTHING`, uuid.NewString(), erasureID, owner, generation, receiptID, evidence[:], now); err != nil {
			return err
		}
	}
	var allLocal bool
	if err := tx.QueryRow(ctx, `SELECT bool_and(r.status IN ('succeeded','not_applicable')) FROM privacy_erasure_receipt_heads h JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id WHERE h.erasure_id=$1 AND h.store_kind=ANY($2::text[])`, erasureID, storeStrings(privacy.LocalManagedSlots)).Scan(&allLocal); err != nil {
		return err
	}
	if allLocal {
		if _, err := tx.Exec(ctx, `UPDATE privacy_erasure_heads SET status='local_scrubbed',summary_version=summary_version+1,stable_reason='all_local_managed_steps_verified',updated_at=$2 WHERE erasure_id=$1 AND status='barrier_committed'`, erasureID, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if reopenOwner {
		s.permits.Open(generation, owner)
	}
	return nil
}

func (s *Store) VerifyRedacted(ctx context.Context, erasureID string, store privacy.StoreKind) error {
	owner, ok := privacy.OwnerForStore(store)
	if !ok {
		return &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: string(store)}
	}
	port, err := s.ownerPort(owner)
	if err != nil {
		return err
	}
	var request privacy.LocalRedactionRequest
	request.ErasureID, request.Store = erasureID, store
	if err := s.pool.QueryRow(ctx, `SELECT b.redacted_through_event_seq,b.learner_generation,r.id FROM privacy_redaction_barriers b JOIN privacy_erasure_receipt_heads h ON h.erasure_id=b.erasure_id AND h.store_kind=$2 JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id WHERE b.erasure_id=$1`, erasureID, store).Scan(&request.RedactedThroughEvent, &request.LearnerGeneration, &request.ReceiptID); err != nil {
		return err
	}
	remaining, err := port.VerifyRedacted(ctx, request)
	if err != nil {
		return err
	}
	if remaining != 0 {
		return &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: fmt.Sprintf("%s_remaining_%d", store, remaining)}
	}
	return nil
}

func ownerComplete(ctx context.Context, db privacy.DBTX, erasureID string, owner privacy.OwnerKind) (bool, error) {
	stores := privacy.StoresForOwner(owner)
	var complete bool
	err := db.QueryRow(ctx, `SELECT count(*)=$3 AND bool_and(r.status IN ('succeeded','not_applicable')) FROM privacy_erasure_receipt_heads h JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id WHERE h.erasure_id=$1 AND h.store_kind=ANY($2::text[])`, erasureID, storeStrings(stores), len(stores)).Scan(&complete)
	return complete, err
}
func storeStrings(stores []privacy.StoreKind) []string {
	values := make([]string, len(stores))
	for index, store := range stores {
		values[index] = string(store)
	}
	return values
}
