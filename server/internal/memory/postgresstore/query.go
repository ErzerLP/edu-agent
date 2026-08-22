package postgresstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type rowScanner interface{ Scan(...any) error }

type pageCursor struct {
	LearnerGeneration int64
	MemoryGeneration  int64
	Time              time.Time
	ID                string
}

type pageCursorWire struct {
	LearnerGeneration int64  `json:"learner_generation"`
	MemoryGeneration  int64  `json:"memory_generation"`
	Time              string `json:"time"`
	ID                string `json:"id"`
}

const candidateColumns = `
	c.id,c.candidate_uri,COALESCE(c.logical_memory_id,lm.id),c.payload_id,c.content_hash,c.source_kind,
	c.source_event_id::text,c.source_operation_id::text,c.source_model_id,c.source_prompt_revision,c.source_hashes,
	c.proposer_id,c.reason,c.category,c.sensitivity,c.stability,c.valid_until,c.admission_policy_version,
	h.status,h.revision,c.created_at,p.content`

func (s *Store) Candidate(ctx context.Context, id string) (memory.CandidateView, error) {
	if !canonicalUUID(id) {
		return memory.CandidateView{}, invalid("invalid_candidate_id")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.CandidateView{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := lockReadableGeneration(ctx, tx)
	if err != nil {
		return memory.CandidateView{}, err
	}
	if _, err := expireCandidatesTx(ctx, tx, id, 1); err != nil {
		return memory.CandidateView{}, err
	}
	value, err := loadCandidate(ctx, tx, id)
	if err != nil {
		return memory.CandidateView{}, err
	}
	value.ReadGeneration = memory.GenerationStamp{
		LearnerGeneration: generation.LearnerGeneration,
		MemoryGeneration:  generation.MemoryGeneration,
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.CandidateView{}, err
	}
	return value, nil
}

func lockReadableGeneration(ctx context.Context, db DBTX) (memory.Generation, error) {
	var value memory.Generation
	if err := db.QueryRow(ctx, `
		SELECT learner_generation,learner_generation,read_open,write_open,updated_at
		FROM privacy_owner_generation_gates WHERE owner_kind='memory' FOR SHARE`).Scan(
		&value.LearnerGeneration, &value.MemoryGeneration, &value.ReadOpen, &value.WriteOpen, &value.UpdatedAt,
	); err != nil {
		return value, fmt.Errorf("lock readable memory generation: %w", err)
	}
	value.UpdatedAt = value.UpdatedAt.UTC()
	if !value.ReadOpen {
		return value, &memory.Error{Code: memory.CodeContentRedacted, Reason: "memory_read_gate_closed"}
	}
	return value, nil
}

func expireCandidatesTx(ctx context.Context, tx pgx.Tx, candidateID string, limit int) (int, error) {
	query := `
		SELECT c.id,h.revision,clock_timestamp()
		FROM memory_candidates c
		JOIN memory_candidate_heads h ON h.candidate_id=c.id
		WHERE h.status='pending_review' AND c.valid_until <= clock_timestamp()
		  AND ($1::uuid IS NULL OR c.id=$1)
		ORDER BY c.valid_until,c.id
		FOR UPDATE OF h`
	args := []any{nullable(candidateID)}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("claim expired candidates: %w", err)
	}
	type expired struct {
		id       string
		revision int64
		dbNow    time.Time
	}
	var values []expired
	for rows.Next() {
		var value expired
		if err := rows.Scan(&value.id, &value.revision, &value.dbNow); err != nil {
			rows.Close()
			return 0, err
		}
		value.dbNow = value.dbNow.UTC()
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, value := range values {
		if err := expireCandidateLocked(ctx, tx, value.id, value.revision, value.dbNow); err != nil {
			return 0, err
		}
	}
	return len(values), nil
}

func expireCandidateLocked(ctx context.Context, tx pgx.Tx, candidateID string, revision int64, dbNow time.Time) error {
	decisionID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_candidate_decisions(
			id,candidate_id,revision,decision,reason,actor_kind,created_at)
		VALUES($1,$2,$3,'expire','candidate_ttl_elapsed','system',$4)`,
		decisionID, candidateID, revision+1, dbNow); err != nil {
		return fmt.Errorf("record candidate lazy expiry: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_candidate_heads
		SET revision=$2,status='expired',current_decision_id=$3,payload_available=FALSE,updated_at=$4
		WHERE candidate_id=$1 AND revision=$5 AND status='pending_review'`,
		candidateID, revision+1, decisionID, dbNow, revision)
	if err != nil {
		return fmt.Errorf("advance candidate lazy expiry: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeCandidateConflict, CandidateID: candidateID}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_candidate_payloads WHERE candidate_id=$1`, candidateID); err != nil {
		return fmt.Errorf("scrub lazy expired candidate: %w", err)
	}
	return nil
}

func loadCandidate(ctx context.Context, db DBTX, id string) (memory.CandidateView, error) {
	row := db.QueryRow(ctx, `SELECT `+candidateColumns+`
		FROM memory_candidates c
		JOIN memory_candidate_heads h ON h.candidate_id=c.id
		LEFT JOIN memory_logical_memories lm ON lm.created_from_candidate_id=c.id
		LEFT JOIN memory_candidate_payloads p ON p.candidate_id=c.id
		WHERE c.id=$1`, id)
	value, err := scanCandidate(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound, CandidateID: id}
	}
	if err != nil {
		return value, fmt.Errorf("load memory candidate: %w", err)
	}
	return value, nil
}

func scanCandidate(row rowScanner) (memory.CandidateView, error) {
	var value memory.CandidateView
	var logicalID, eventID, operationID, modelID, promptRevision, content *string
	var contentHash []byte
	var sourceHashes [][]byte
	var source, category, sensitivity, stability, status string
	err := row.Scan(
		&value.Candidate.ID, &value.Candidate.URI, &logicalID, &value.Candidate.PayloadID, &contentHash, &source,
		&eventID, &operationID, &modelID, &promptRevision, &sourceHashes, &value.Candidate.ProposerID,
		&value.Candidate.Reason, &category, &sensitivity, &stability, &value.Candidate.ValidUntil,
		&value.Candidate.PolicyVersion, &status, &value.Candidate.Revision, &value.Candidate.CreatedAt, &content,
	)
	if err != nil {
		return value, err
	}
	value.Candidate.LogicalMemoryID = deref(logicalID)
	value.Candidate.ContentHash = hex.EncodeToString(contentHash)
	value.Candidate.Source = memory.SourceKind(source)
	value.Candidate.SourceReference = memory.SourceReference{
		EventID: deref(eventID), OperationID: deref(operationID), ModelID: deref(modelID), PromptRevision: deref(promptRevision),
	}
	for _, hash := range sourceHashes {
		value.Candidate.SourceReference.SourceHashes = append(value.Candidate.SourceReference.SourceHashes, hex.EncodeToString(hash))
	}
	value.Candidate.Category = memory.Category(category)
	value.Candidate.Sensitivity = memory.Sensitivity(sensitivity)
	value.Candidate.Stability = memory.Stability(stability)
	value.Candidate.Status = memory.CandidateStatus(status)
	value.Candidate.ValidUntil = value.Candidate.ValidUntil.UTC()
	value.Candidate.CreatedAt = value.Candidate.CreatedAt.UTC()
	if content != nil && status == string(memory.CandidatePending) {
		value.ContentStatus = "available"
		value.ProposedContent = *content
	} else {
		value.ContentStatus = "scrubbed"
	}
	return value, nil
}

func loadOperationResult(ctx context.Context, db DBTX, archived operationRecord) (memory.OperationResult, error) {
	var result memory.OperationResult
	if archived.candidateID != nil {
		candidate, err := loadCandidate(ctx, db, *archived.candidateID)
		if err != nil {
			return result, err
		}
		result.Candidate = candidate
	}
	if archived.recordRevisionID != nil {
		record, err := loadRecord(ctx, db, *archived.recordRevisionID)
		if err != nil {
			return result, err
		}
		result.Record = &record
	}
	if archived.deliveryID != nil {
		delivery, err := loadDelivery(ctx, db, *archived.deliveryID)
		if err != nil {
			return result, err
		}
		result.Delivery = &delivery
	}
	return result, nil
}

func loadRecord(ctx context.Context, db DBTX, id string) (memory.Record, error) {
	var value memory.Record
	var contentHash, uriDigest []byte
	var externalNode, previousRevision *string
	var externalMemory *int64
	err := db.QueryRow(ctx, `
		SELECT r.logical_memory_id,r.id,r.revision,
		       CASE WHEN h.current_record_revision_id=r.id THEN h.record_generation ELSE r.record_generation END,
		       r.learner_generation,r.candidate_id,
		       r.previous_revision_id::text,r.external_uri,r.external_uri_digest,r.content_hash,
		       CASE
		         WHEN h.current_record_revision_id=r.id THEN h.status
		         WHEN successor_head.status='applied' THEN 'superseded'
		         WHEN dh.status='applied' THEN 'applied'
		         ELSE 'permanently_rejected'
		       END,
		       CASE WHEN h.current_record_revision_id=r.id THEN h.current_delivery_id ELSE r.delivery_id END,
		       CASE WHEN h.current_record_revision_id=r.id THEN h.receipt_id ELSE dh.current_receipt_id END,
		       CASE WHEN h.current_record_revision_id=r.id THEN h.external_node_id::text END,
		       CASE WHEN h.current_record_revision_id=r.id THEN h.external_memory_id END,
		       r.created_at,
		       CASE
		         WHEN h.current_record_revision_id=r.id THEN h.applied_at
		         WHEN dh.status='applied' THEN delivery_receipt.created_at
		       END,
		       CASE
		         WHEN h.current_record_revision_id<>r.id AND successor_head.status='applied'
		         THEN successor_receipt.created_at
		       END,
		       CASE WHEN h.current_record_revision_id=r.id THEN h.deleted_at END
		FROM memory_record_revisions r
		JOIN memory_record_heads h ON h.logical_memory_id=r.logical_memory_id
		JOIN memory_delivery_heads dh ON dh.delivery_id=r.delivery_id
		JOIN memory_delivery_receipts delivery_receipt ON delivery_receipt.id=dh.current_receipt_id
		LEFT JOIN memory_record_revisions successor ON successor.previous_revision_id=r.id
		LEFT JOIN memory_delivery_heads successor_head ON successor_head.delivery_id=successor.delivery_id
		LEFT JOIN memory_delivery_receipts successor_receipt ON successor_receipt.id=successor_head.current_receipt_id
		WHERE r.id=$1`, id).Scan(
		&value.LogicalMemoryID, &value.ID, &value.Revision, &value.RecordGeneration, &value.LearnerGeneration,
		&value.CandidateID, &previousRevision, &value.ExternalURI, &uriDigest, &contentHash, &value.Status,
		&value.DeliveryID, &value.ReceiptID, &externalNode, &externalMemory, &value.CreatedAt,
		&value.AppliedAt, &value.SupersededAt, &value.DeletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("load memory record: %w", err)
	}
	value.ContentHash = hex.EncodeToString(contentHash)
	value.PreviousRevisionID = deref(previousRevision)
	value.ExternalURIDigest = hex.EncodeToString(uriDigest)
	value.ExternalNodeID = deref(externalNode)
	if externalMemory != nil {
		value.ExternalMemoryID = *externalMemory
	}
	value.CreatedAt = value.CreatedAt.UTC()
	value.AppliedAt = utcPointer(value.AppliedAt)
	value.SupersededAt = utcPointer(value.SupersededAt)
	value.DeletedAt = utcPointer(value.DeletedAt)
	return value, nil
}

func loadDelivery(ctx context.Context, db DBTX, id string) (memory.Delivery, error) {
	var value memory.Delivery
	var payloadHash []byte
	var disposition, category *string
	err := db.QueryRow(ctx, `
		SELECT d.id,d.kind,d.logical_memory_id,d.record_revision_id,d.record_revision,d.learner_generation,
		       d.record_generation,d.payload_id,d.payload_hash,d.external_uri,h.attempt_state,h.status,
		       h.public_status,h.terminal_disposition,d.valid_until,h.attempt_count,h.last_error_category,
		       h.current_receipt_id,d.created_at,h.updated_at
		FROM memory_deliveries d JOIN memory_delivery_heads h ON h.delivery_id=d.id WHERE d.id=$1`, id).Scan(
		&value.ID, &value.Kind, &value.LogicalMemoryID, &value.RecordRevisionID, &value.RecordRevision,
		&value.LearnerGeneration, &value.RecordGeneration, &value.PayloadID, &payloadHash, &value.ExternalURI,
		&value.AttemptState, &value.Status, &value.PublicStatus, &disposition, &value.ValidUntil,
		&value.AttemptCount, &category, &value.ReceiptID, &value.CreatedAt, &value.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("load memory delivery: %w", err)
	}
	value.PayloadHash = hex.EncodeToString(payloadHash)
	value.Disposition = deref(disposition)
	value.LastCategory = deref(category)
	value.ValidUntil = value.ValidUntil.UTC()
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	return value, nil
}

func loadReceipt(ctx context.Context, db DBTX, id string) (memory.Receipt, error) {
	var value memory.Receipt
	var evidence []byte
	err := db.QueryRow(ctx, `
		SELECT id,delivery_id,version,status,reason,verification_method,evidence_digest,created_at
		FROM memory_delivery_receipts WHERE id=$1`, id).Scan(
		&value.ID, &value.DeliveryID, &value.Version, &value.Status, &value.Reason,
		&value.VerificationMethod, &evidence, &value.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("load memory delivery receipt: %w", err)
	}
	if evidence != nil {
		value.EvidenceDigest = hex.EncodeToString(evidence)
	}
	value.CreatedAt = value.CreatedAt.UTC()
	return value, nil
}

func (s *Store) Record(ctx context.Context, logicalMemoryID string) (memory.RecordView, error) {
	if !canonicalUUID(logicalMemoryID) {
		return memory.RecordView{}, invalid("invalid_logical_memory_id")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.RecordView{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := lockReadableGeneration(ctx, tx)
	if err != nil {
		return memory.RecordView{}, err
	}
	var recordRevisionID string
	if err := tx.QueryRow(ctx, `
		SELECT current_record_revision_id FROM memory_record_heads
		WHERE logical_memory_id=$1 FOR SHARE`, logicalMemoryID).Scan(&recordRevisionID); errors.Is(err, pgx.ErrNoRows) {
		return memory.RecordView{}, &memory.Error{Code: memory.CodeNotFound}
	} else if err != nil {
		return memory.RecordView{}, fmt.Errorf("load current memory record identity: %w", err)
	}
	record, err := loadRecord(ctx, tx, recordRevisionID)
	if err != nil {
		return memory.RecordView{}, err
	}
	delivery, err := loadDelivery(ctx, tx, record.DeliveryID)
	if err != nil {
		return memory.RecordView{}, err
	}
	receipt, err := loadReceipt(ctx, tx, record.ReceiptID)
	if err != nil {
		return memory.RecordView{}, err
	}
	value := memory.RecordView{
		Record: record, Delivery: delivery, Receipt: receipt,
		ReadGeneration: memory.GenerationStamp{
			LearnerGeneration: generation.LearnerGeneration,
			MemoryGeneration:  generation.MemoryGeneration,
		},
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.RecordView{}, err
	}
	return value, nil
}

func (s *Store) ListCandidates(ctx context.Context, request memory.PageRequest) (memory.CandidatePage, error) {
	limit := normalizeLimit(request.Limit)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.CandidatePage{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := lockReadableGeneration(ctx, tx)
	if err != nil {
		return memory.CandidatePage{}, err
	}
	cursor, err := decodePageCursor(request.Cursor, generation)
	if err != nil {
		return memory.CandidatePage{}, err
	}
	// Query reads must never expose an expired payload, even if the periodic sweep has not run.
	if _, err := expireCandidatesTx(ctx, tx, "", 0); err != nil {
		return memory.CandidatePage{}, err
	}
	var cursorTime, cursorID any
	if cursor != nil {
		cursorTime, cursorID = cursor.Time, cursor.ID
	}
	rows, err := tx.Query(ctx, `SELECT `+candidateColumns+`
		FROM memory_candidates c
		JOIN memory_candidate_heads h ON h.candidate_id=c.id
		LEFT JOIN memory_logical_memories lm ON lm.created_from_candidate_id=c.id
		LEFT JOIN memory_candidate_payloads p ON p.candidate_id=c.id
		WHERE ($1::timestamptz IS NULL OR (c.created_at,c.id)<($1,$2::uuid))
		ORDER BY c.created_at DESC,c.id DESC LIMIT $3`, cursorTime, cursorID, limit+1)
	if err != nil {
		return memory.CandidatePage{}, fmt.Errorf("list memory candidates: %w", err)
	}
	var result memory.CandidatePage
	result.ReadGeneration = memory.GenerationStamp{LearnerGeneration: generation.LearnerGeneration, MemoryGeneration: generation.MemoryGeneration}
	for rows.Next() {
		value, err := scanCandidate(rows)
		if err != nil {
			rows.Close()
			return result, fmt.Errorf("scan memory candidate page: %w", err)
		}
		value.ReadGeneration = result.ReadGeneration
		result.Items = append(result.Items, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("iterate memory candidate page: %w", err)
	}
	rows.Close()
	if len(result.Items) > limit {
		last := result.Items[limit-1].Candidate
		result.Items = result.Items[:limit]
		result.NextCursor = encodePageCursor(pageCursor{
			LearnerGeneration: result.ReadGeneration.LearnerGeneration,
			MemoryGeneration:  result.ReadGeneration.MemoryGeneration,
			Time:              last.CreatedAt,
			ID:                last.ID,
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.CandidatePage{}, err
	}
	return result, nil
}

func (s *Store) ListRecords(ctx context.Context, request memory.PageRequest) (memory.RecordPage, error) {
	limit := normalizeLimit(request.Limit)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.RecordPage{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := lockReadableGeneration(ctx, tx)
	if err != nil {
		return memory.RecordPage{}, err
	}
	cursor, err := decodePageCursor(request.Cursor, generation)
	if err != nil {
		return memory.RecordPage{}, err
	}
	var cursorTime, cursorID any
	if cursor != nil {
		cursorTime, cursorID = cursor.Time, cursor.ID
	}
	rows, err := tx.Query(ctx, `
		SELECT r.logical_memory_id,r.id,r.revision,h.record_generation,r.learner_generation,r.candidate_id,
		       r.previous_revision_id::text,r.external_uri,r.external_uri_digest,r.content_hash,h.status,
		       h.current_delivery_id,h.receipt_id,h.external_node_id::text,h.external_memory_id,r.created_at,
		       h.applied_at,h.superseded_at,h.deleted_at
		FROM memory_record_heads h JOIN memory_record_revisions r ON r.id=h.current_record_revision_id
		WHERE ($1::timestamptz IS NULL OR (r.created_at,r.id)<($1,$2::uuid))
		ORDER BY r.created_at DESC,r.id DESC LIMIT $3`, cursorTime, cursorID, limit+1)
	if err != nil {
		return memory.RecordPage{}, fmt.Errorf("list memory records: %w", err)
	}
	var result memory.RecordPage
	result.ReadGeneration = memory.GenerationStamp{LearnerGeneration: generation.LearnerGeneration, MemoryGeneration: generation.MemoryGeneration}
	for rows.Next() {
		var value memory.Record
		var contentHash, uriDigest []byte
		var externalNode, previousRevision *string
		var externalMemory *int64
		if err := rows.Scan(
			&value.LogicalMemoryID, &value.ID, &value.Revision, &value.RecordGeneration, &value.LearnerGeneration,
			&value.CandidateID, &previousRevision, &value.ExternalURI, &uriDigest, &contentHash, &value.Status,
			&value.DeliveryID, &value.ReceiptID, &externalNode, &externalMemory, &value.CreatedAt,
			&value.AppliedAt, &value.SupersededAt, &value.DeletedAt,
		); err != nil {
			rows.Close()
			return result, fmt.Errorf("scan memory record page: %w", err)
		}
		value.ContentHash = hex.EncodeToString(contentHash)
		value.PreviousRevisionID = deref(previousRevision)
		value.ExternalURIDigest = hex.EncodeToString(uriDigest)
		value.ExternalNodeID = deref(externalNode)
		if externalMemory != nil {
			value.ExternalMemoryID = *externalMemory
		}
		value.CreatedAt = value.CreatedAt.UTC()
		value.AppliedAt = utcPointer(value.AppliedAt)
		value.SupersededAt = utcPointer(value.SupersededAt)
		value.DeletedAt = utcPointer(value.DeletedAt)
		result.Items = append(result.Items, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("iterate memory record page: %w", err)
	}
	rows.Close()
	if len(result.Items) > limit {
		last := result.Items[limit-1]
		result.Items = result.Items[:limit]
		result.NextCursor = encodePageCursor(pageCursor{
			LearnerGeneration: result.ReadGeneration.LearnerGeneration,
			MemoryGeneration:  result.ReadGeneration.MemoryGeneration,
			Time:              last.CreatedAt,
			ID:                last.ID,
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.RecordPage{}, err
	}
	return result, nil
}

func normalizeLimit(value int) int {
	if value <= 0 {
		return 50
	}
	if value > 200 {
		return 200
	}
	return value
}

func encodePageCursor(value pageCursor) string {
	wire := pageCursorWire{
		LearnerGeneration: value.LearnerGeneration,
		MemoryGeneration:  value.MemoryGeneration,
		Time:              value.Time.UTC().Format(time.RFC3339Nano),
		ID:                value.ID,
	}
	encoded, _ := json.Marshal(wire)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodePageCursor(value string, generation memory.Generation) (*pageCursor, error) {
	stale := func() (*pageCursor, error) { return nil, &memory.Error{Code: memory.CodeStaleCursor} }
	if value == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return stale()
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var wire pageCursorWire
	if err := decoder.Decode(&wire); err != nil {
		return stale()
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return stale()
	}
	parsedID, err := uuid.Parse(wire.ID)
	if err != nil || parsedID.String() != wire.ID || wire.Time == "" || !strings.HasSuffix(wire.Time, "Z") ||
		wire.LearnerGeneration < 1 || wire.MemoryGeneration < 1 ||
		wire.LearnerGeneration != generation.LearnerGeneration || wire.MemoryGeneration != generation.MemoryGeneration {
		return stale()
	}
	parsedTime, err := time.Parse(time.RFC3339Nano, wire.Time)
	if err != nil || parsedTime.IsZero() || parsedTime.Location() != time.UTC || parsedTime.Format(time.RFC3339Nano) != wire.Time {
		return stale()
	}
	return &pageCursor{
		LearnerGeneration: wire.LearnerGeneration,
		MemoryGeneration:  wire.MemoryGeneration,
		Time:              parsedTime,
		ID:                wire.ID,
	}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
