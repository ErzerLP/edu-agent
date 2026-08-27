package postgresstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	notesyncintegration "github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgresstore "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const notesyncReviewColumns = `
	review_id::text,document_id::text,remote_document_id::text,remote_vault,remote_path,
	kind,reason_code,status,head_knowledge_revision_id::text,head_knowledge_revision_no,
	canonical_path,base_missing,base_knowledge_revision_id::text,base_knowledge_revision_no,
	base_document_revision_id::text,base_remote_path,base_remote_version,base_remote_last_time,
	base_markdown,base_sha256,
	local_missing,local_knowledge_revision_id::text,local_knowledge_revision_no,
	local_document_revision_id::text,local_markdown,local_sha256,
	remote_missing,remote_markdown,remote_sha256,remote_version,remote_last_time,
	remote_source_revision_id::text,base_to_local_diff,base_to_remote_diff,
	local_diff_truncated,remote_diff_truncated,basis_hash,generation,resolution_kind,
	resolution_operation_id::text,resolved_by_device_id::text,resolved_knowledge_revision_id::text,
	resolved_document_id::text,resolved_document_revision_id::text,created_at,updated_at,resolved_at`

const notesyncReviewSummaryColumns = `
	review_id::text,document_id::text,remote_document_id::text,remote_vault,remote_path,
	kind,reason_code,status,head_knowledge_revision_id::text,head_knowledge_revision_no,canonical_path,
	base_missing,base_knowledge_revision_id::text,base_knowledge_revision_no,base_document_revision_id::text,
	base_remote_path,base_remote_version,base_remote_last_time,base_sha256,
	local_missing,local_knowledge_revision_id::text,local_knowledge_revision_no,local_document_revision_id::text,local_sha256,
	remote_missing,remote_sha256,remote_version,remote_last_time,remote_source_revision_id::text,
	basis_hash,generation,resolution_kind,resolution_operation_id::text,resolved_by_device_id::text,
	resolved_knowledge_revision_id::text,resolved_document_id::text,resolved_document_revision_id::text,
	created_at,updated_at,resolved_at`

func (s *Store) LoadNotesyncPreviewState(
	ctx context.Context,
	vault string,
	remotePath string,
	canonicalPath string,
	remoteDocumentID string,
) (notesyncintegration.PreviewState, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return notesyncintegration.PreviewState{}, fmt.Errorf("begin notesync preview state read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return notesyncintegration.PreviewState{}, err
	}
	state := notesyncintegration.PreviewState{Generation: generation, CanonicalPath: canonicalPath}
	var headRevisionID *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1`).Scan(&headRevisionID); err != nil {
		return notesyncintegration.PreviewState{}, fmt.Errorf("read notesync preview head: %w", err)
	}
	if headRevisionID != nil {
		state.HeadRevisionID = *headRevisionID
		if err := tx.QueryRow(ctx, `SELECT revision_no FROM knowledge_revisions WHERE id=$1 AND redacted_at IS NULL`, *headRevisionID).Scan(&state.HeadRevisionNo); err != nil {
			return notesyncintegration.PreviewState{}, fmt.Errorf("read notesync preview head revision: %w", err)
		}
	}
	pathMapping, err := loadNotesyncMappingByPath(ctx, tx, vault, remotePath)
	if err != nil {
		return notesyncintegration.PreviewState{}, err
	}
	var identityMapping *notesyncintegration.PublicationMapping
	if remoteDocumentID != "" {
		identityMapping, err = loadNotesyncMappingForRead(ctx, tx, remoteDocumentID)
		if err != nil {
			return notesyncintegration.PreviewState{}, err
		}
	}
	var localByPath, localByIdentity *notesyncPreviewLocal
	if state.HeadRevisionID != "" {
		localByPath, err = loadNotesyncPreviewLocal(ctx, tx, state.HeadRevisionID, state.HeadRevisionNo, "canonical_path", canonicalPath)
		if err != nil {
			return notesyncintegration.PreviewState{}, err
		}
		if remoteDocumentID != "" {
			localByIdentity, err = loadNotesyncPreviewLocal(ctx, tx, state.HeadRevisionID, state.HeadRevisionNo, "document_id", remoteDocumentID)
			if err != nil {
				return notesyncintegration.PreviewState{}, err
			}
		}
	}

	var selected *notesyncPreviewLocal
	switch {
	case identityMapping != nil && (identityMapping.RemoteVault != vault || identityMapping.RemotePath != remotePath):
		state.IdentityMoved = true
		state.Mapping = identityMapping
		state.DocumentID = identityMapping.DocumentID
		selected = localByIdentity
	case pathMapping != nil && remoteDocumentID != "" && pathMapping.DocumentID != remoteDocumentID:
		state.PathOccupied = true
		state.Mapping = pathMapping
		state.DocumentID = pathMapping.DocumentID
		selected = localByPath
	case identityMapping != nil:
		state.Mapping = identityMapping
		state.DocumentID = identityMapping.DocumentID
		selected = localByIdentity
	case pathMapping != nil:
		state.Mapping = pathMapping
		state.DocumentID = pathMapping.DocumentID
		selected = localByPath
	case localByIdentity != nil && localByIdentity.CanonicalPath != canonicalPath:
		state.IdentityMoved = true
		state.DocumentID = localByIdentity.DocumentID
		selected = localByIdentity
	case localByPath != nil && remoteDocumentID != "" && localByPath.DocumentID != remoteDocumentID:
		state.PathOccupied = true
		state.DocumentID = localByPath.DocumentID
		selected = localByPath
	case localByIdentity != nil:
		state.DocumentID = localByIdentity.DocumentID
		selected = localByIdentity
	case localByPath != nil:
		state.DocumentID = localByPath.DocumentID
		selected = localByPath
	}
	if selected == nil && state.HeadRevisionID != "" && state.DocumentID != "" {
		selected, err = loadNotesyncPreviewLocal(ctx, tx, state.HeadRevisionID, state.HeadRevisionNo, "document_id", state.DocumentID)
		if err != nil {
			return notesyncintegration.PreviewState{}, err
		}
	}
	if selected == nil {
		state.Local = notesyncintegration.ReviewSnapshot{Missing: true}
	} else {
		state.CanonicalPath = selected.CanonicalPath
		state.Local = selected.Snapshot
	}
	if err := tx.Commit(ctx); err != nil {
		return notesyncintegration.PreviewState{}, fmt.Errorf("commit notesync preview state read: %w", err)
	}
	return state, nil
}

type notesyncPreviewLocal struct {
	DocumentID    string
	CanonicalPath string
	Snapshot      notesyncintegration.ReviewSnapshot
}

func loadNotesyncPreviewLocal(
	ctx context.Context,
	tx pgx.Tx,
	headRevisionID string,
	headRevisionNo int64,
	lookupColumn string,
	lookupValue string,
) (*notesyncPreviewLocal, error) {
	if lookupColumn != "canonical_path" && lookupColumn != "document_id" {
		return nil, errors.New("invalid notesync preview local lookup")
	}
	var local notesyncPreviewLocal
	var canonicalMarkdown string
	query := `
		SELECT snapshot.document_id::text,snapshot.canonical_path,snapshot.document_revision_id::text,payload.canonical_markdown
		FROM knowledge_snapshot_documents snapshot
		JOIN knowledge_document_payloads payload ON payload.document_revision_id=snapshot.document_revision_id
		WHERE snapshot.knowledge_revision_id=$1 AND snapshot.` + lookupColumn + `=$2`
	err := tx.QueryRow(ctx, query, headRevisionID, lookupValue).Scan(
		&local.DocumentID, &local.CanonicalPath, &local.Snapshot.DocumentRevisionID, &canonicalMarkdown,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read notesync preview local document: %w", err)
	}
	exported, err := knowledge.ExportMarkdown(canonicalMarkdown, headRevisionID)
	if err != nil {
		return nil, fmt.Errorf("render notesync preview local Markdown: %w", err)
	}
	local.Snapshot = notesyncintegration.ReviewSnapshot{
		KnowledgeRevisionID: headRevisionID, KnowledgeRevisionNo: headRevisionNo,
		DocumentRevisionID: local.Snapshot.DocumentRevisionID, SourceRevisionID: headRevisionID,
		Path: local.CanonicalPath, Markdown: exported, SHA256: notesyncMarkdownHash(exported),
	}
	return &local, nil
}

func loadNotesyncMappingForRead(ctx context.Context, tx pgx.Tx, documentID string) (*notesyncintegration.PublicationMapping, error) {
	return scanNotesyncMapping(tx.QueryRow(ctx, `
		SELECT document_id::text,remote_vault,remote_path,published_knowledge_revision_id::text,
		       published_document_revision_id::text,published_revision_no,base_markdown,
		       remote_version,remote_last_time,generation
		FROM knowledge_notesync_publications
		WHERE document_id=$1 AND status='active'`, documentID), "read notesync publication mapping by document")
}

func loadNotesyncMappingByPath(ctx context.Context, tx pgx.Tx, vault, remotePath string) (*notesyncintegration.PublicationMapping, error) {
	return scanNotesyncMapping(tx.QueryRow(ctx, `
		SELECT document_id::text,remote_vault,remote_path,published_knowledge_revision_id::text,
		       published_document_revision_id::text,published_revision_no,base_markdown,
		       remote_version,remote_last_time,generation
		FROM knowledge_notesync_publications
		WHERE remote_vault=$1 AND remote_path=$2 AND status='active'`, vault, remotePath), "read notesync publication mapping by path")
}

func scanNotesyncMapping(row pgx.Row, operation string) (*notesyncintegration.PublicationMapping, error) {
	var mapping notesyncintegration.PublicationMapping
	var remoteVersion, remoteLastTime *int64
	err := row.Scan(
		&mapping.DocumentID, &mapping.RemoteVault, &mapping.RemotePath, &mapping.KnowledgeRevisionID,
		&mapping.DocumentRevisionID, &mapping.RevisionNo, &mapping.BaseMarkdown,
		&remoteVersion, &remoteLastTime, &mapping.Generation,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	if remoteVersion != nil {
		mapping.RemoteVersion = *remoteVersion
	}
	if remoteLastTime != nil {
		mapping.RemoteLastTime = *remoteLastTime
	}
	return &mapping, nil
}

func (s *Store) SaveNotesyncReview(ctx context.Context, review notesyncintegration.Review) (notesyncintegration.Review, error) {
	if err := validateNotesyncReviewRecord(review); err != nil {
		return notesyncintegration.Review{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return notesyncintegration.Review{}, fmt.Errorf("begin notesync review save: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return notesyncintegration.Review{}, err
	}
	if generation != review.Generation {
		return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	var headID *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1 FOR SHARE`).Scan(&headID); err != nil {
		return notesyncintegration.Review{}, fmt.Errorf("lock notesync review head: %w", err)
	}
	if optionalString(headID) != review.HeadRevisionID {
		return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	basis, _ := hex.DecodeString(review.BasisHash)
	baseSHA := nullableHash(review.Base)
	localSHA := nullableHash(review.Local)
	remoteSHA := nullableHash(review.Remote)
	command, err := tx.Exec(ctx, `
		INSERT INTO knowledge_notesync_reviews(
		  review_id,document_id,remote_document_id,remote_vault,remote_path,kind,reason_code,status,
		  head_knowledge_revision_id,head_knowledge_revision_no,canonical_path,
		  base_missing,base_knowledge_revision_id,base_knowledge_revision_no,base_document_revision_id,
		  base_remote_path,base_remote_version,base_remote_last_time,base_markdown,base_sha256,
		  local_missing,local_knowledge_revision_id,local_knowledge_revision_no,local_document_revision_id,local_markdown,local_sha256,
		  remote_missing,remote_markdown,remote_sha256,remote_version,remote_last_time,remote_source_revision_id,
		  base_to_local_diff,base_to_remote_diff,local_diff_truncated,remote_diff_truncated,
		  basis_hash,generation,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'open',$8,$9,$10,
		       $11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,
		       $26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39)
		ON CONFLICT DO NOTHING`,
		review.ReviewID, nullableUUID(review.DocumentID), nullableUUID(review.RemoteDocumentID), review.RemoteVault,
		review.RemotePath, review.Category, review.ReasonCode, nullableUUID(review.HeadRevisionID), nullablePositive(review.HeadRevisionNo),
		review.CanonicalPath, review.Base.Missing, nullableUUID(review.Base.KnowledgeRevisionID), nullablePositive(review.Base.KnowledgeRevisionNo),
		nullableUUID(review.Base.DocumentRevisionID), nullableBaseValue(review.Base, review.Base.Path),
		nullableRemoteValue(review.Base, review.Base.RemoteVersion), nullableRemoteValue(review.Base, review.Base.RemoteLastTime),
		nullableMarkdown(review.Base), baseSHA,
		review.Local.Missing, nullableUUID(review.Local.KnowledgeRevisionID), nullablePositive(review.Local.KnowledgeRevisionNo),
		nullableUUID(review.Local.DocumentRevisionID), nullableMarkdown(review.Local), localSHA,
		review.Remote.Missing, nullableMarkdown(review.Remote), remoteSHA,
		nullableRemoteValue(review.Remote, review.Remote.RemoteVersion), nullableRemoteValue(review.Remote, review.Remote.RemoteLastTime),
		nullableUUID(review.Remote.SourceRevisionID), review.Diff.BaseToLocal, review.Diff.BaseToRemote,
		review.Diff.LocalTruncated, review.Diff.RemoteTruncated, basis, review.Generation,
		review.CreatedAt.UTC(), review.UpdatedAt.UTC(),
	)
	if err != nil {
		return notesyncintegration.Review{}, fmt.Errorf("insert notesync review: %w", err)
	}
	var stored notesyncintegration.Review
	if command.RowsAffected() == 1 {
		stored, err = readNotesyncReview(ctx, tx, review.ReviewID, false)
	} else {
		stored, err = readNotesyncReviewByBasis(ctx, tx, review)
	}
	if err != nil {
		return notesyncintegration.Review{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return notesyncintegration.Review{}, fmt.Errorf("commit notesync review save: %w", err)
	}
	return stored, nil
}

func (s *Store) ListNotesyncReviews(ctx context.Context, command notesyncintegration.ReviewListCommand) (notesyncintegration.ReviewPage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return notesyncintegration.ReviewPage{}, fmt.Errorf("begin notesync review list: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return notesyncintegration.ReviewPage{}, err
	}
	afterAt, afterID, err := notesyncintegration.DecodeReviewCursor(command.Cursor, generation)
	if err != nil {
		return notesyncintegration.ReviewPage{}, err
	}
	var cursorAt any
	var cursorID any
	if !afterAt.IsZero() {
		cursorAt = afterAt
		cursorID = afterID
	}
	rows, err := tx.Query(ctx, `SELECT `+notesyncReviewSummaryColumns+`
		FROM knowledge_notesync_reviews
		WHERE ($1='all' OR status=$1)
		  AND ($2::timestamptz IS NULL OR (created_at,review_id)>($2,$3::uuid))
		ORDER BY created_at,review_id LIMIT $4`, command.Status, cursorAt, cursorID, command.Limit+1)
	if err != nil {
		return notesyncintegration.ReviewPage{}, fmt.Errorf("query notesync reviews: %w", err)
	}
	defer rows.Close()
	result := notesyncintegration.ReviewPage{Items: make([]notesyncintegration.ReviewSummary, 0, command.Limit)}
	for rows.Next() {
		review, scanErr := scanNotesyncReviewSummary(rows)
		if scanErr != nil {
			return notesyncintegration.ReviewPage{}, scanErr
		}
		result.Items = append(result.Items, review)
	}
	if err := rows.Err(); err != nil {
		return notesyncintegration.ReviewPage{}, fmt.Errorf("iterate notesync reviews: %w", err)
	}
	if len(result.Items) > command.Limit {
		result.Items = result.Items[:command.Limit]
		last := result.Items[len(result.Items)-1]
		result.NextCursor = notesyncintegration.EncodeReviewCursor(generation, last.CreatedAt, last.ReviewID)
	}
	if err := tx.Commit(ctx); err != nil {
		return notesyncintegration.ReviewPage{}, fmt.Errorf("commit notesync review list: %w", err)
	}
	return result, nil
}

func (s *Store) NotesyncReview(ctx context.Context, reviewID string) (notesyncintegration.Review, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return notesyncintegration.Review{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	review, err := readNotesyncReview(ctx, tx, reviewID, false)
	if err != nil {
		return notesyncintegration.Review{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return notesyncintegration.Review{}, fmt.Errorf("commit notesync review read: %w", err)
	}
	return review, nil
}

func (s *Store) LookupNotesyncResolution(ctx context.Context, deviceID, operationID string) (notesyncintegration.ResolutionOperationRecord, bool, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return notesyncintegration.ResolutionOperationRecord{}, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	record, exists, err := lookupNotesyncResolutionWith(ctx, tx, deviceID, operationID, false)
	if err != nil {
		return notesyncintegration.ResolutionOperationRecord{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return notesyncintegration.ResolutionOperationRecord{}, false, fmt.Errorf("commit notesync resolution replay read: %w", err)
	}
	return record, exists, nil
}

func (s *Store) ResolveNotesyncKeep(ctx context.Context, request notesyncintegration.KeepResolutionRequest) (notesyncintegration.ResolutionResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return notesyncintegration.ResolutionResult{}, fmt.Errorf("begin notesync keep resolution: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return notesyncintegration.ResolutionResult{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1||':'||$2,0))`, request.DeviceID, request.OperationID); err != nil {
		return notesyncintegration.ResolutionResult{}, fmt.Errorf("lock notesync keep operation: %w", err)
	}
	if stored, exists, err := lookupNotesyncResolutionWith(ctx, tx, request.DeviceID, request.OperationID, true); err != nil {
		return notesyncintegration.ResolutionResult{}, err
	} else if exists {
		if stored.RequestHash != request.RequestHash {
			return notesyncintegration.ResolutionResult{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewIdempotencyConflict}
		}
		stored.Result.Replayed = true
		return stored.Result, nil
	}
	var currentHead *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1 FOR UPDATE`).Scan(&currentHead); err != nil {
		return notesyncintegration.ResolutionResult{}, fmt.Errorf("lock notesync keep head: %w", err)
	}
	review, err := readNotesyncReview(ctx, tx, request.ReviewID, true)
	if err != nil {
		return notesyncintegration.ResolutionResult{}, err
	}
	if err := validateLockedNotesyncReview(ctx, tx, review, request.BasisHash, generation, optionalString(currentHead), request.ObservedRemote); err != nil {
		return notesyncintegration.ResolutionResult{}, err
	}
	outboxGeneration, err := lockCurrentNotesyncOutboxGeneration(ctx, tx)
	if err != nil {
		return notesyncintegration.ResolutionResult{}, err
	}
	if generation != outboxGeneration {
		return notesyncintegration.ResolutionResult{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewContentRedacted}
	}
	if review.Local.Missing || review.DocumentID == "" || currentHead == nil {
		return notesyncintegration.ResolutionResult{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('notesync-publication:'||$1,0))`, review.DocumentID); err != nil {
		return notesyncintegration.ResolutionResult{}, fmt.Errorf("lock notesync keep document: %w", err)
	}
	var currentRevisionNo int64
	var currentDocumentRevisionID string
	if err := tx.QueryRow(ctx, `
		SELECT revision.revision_no,snapshot.document_revision_id::text
		FROM knowledge_revisions revision
		JOIN knowledge_snapshot_documents snapshot ON snapshot.knowledge_revision_id=revision.id
		WHERE revision.id=$1 AND snapshot.document_id=$2 AND snapshot.canonical_path=$3`,
		*currentHead, review.DocumentID, review.CanonicalPath).Scan(&currentRevisionNo, &currentDocumentRevisionID); err != nil {
		return notesyncintegration.ResolutionResult{}, fmt.Errorf("read notesync keep canonical document: %w", err)
	}
	if currentDocumentRevisionID != review.Local.DocumentRevisionID || currentRevisionNo != review.HeadRevisionNo {
		return notesyncintegration.ResolutionResult{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	if err := enqueueNotesyncReviewPublicationIntent(ctx, tx, review, *currentHead, currentRevisionNo, currentDocumentRevisionID, generation, request.ResolvedAt); err != nil {
		return notesyncintegration.ResolutionResult{}, err
	}
	result := notesyncintegration.ResolutionResult{
		ReviewID: review.ReviewID, ResolutionKind: notesyncintegration.ResolutionKeepCanonical,
		KnowledgeRevisionID: *currentHead, DocumentID: review.DocumentID, DocumentRevisionID: currentDocumentRevisionID,
		Unchanged: true,
	}
	if err := completeNotesyncResolution(ctx, tx, review, request.DeviceID, request.OperationID, request.RequestHash,
		notesyncintegration.ResolutionKeepCanonical, generation, result, request.ResolvedAt); err != nil {
		return notesyncintegration.ResolutionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return notesyncintegration.ResolutionResult{}, fmt.Errorf("commit notesync keep resolution: %w", err)
	}
	return result, nil
}

func enqueueNotesyncReviewPublicationIntent(
	ctx context.Context,
	tx pgx.Tx,
	review notesyncintegration.Review,
	knowledgeRevisionID string,
	revisionNo int64,
	documentRevisionID string,
	generation int64,
	createdAt time.Time,
) error {
	payload, err := json.Marshal(notesyncintegration.PublicationIntent{
		SchemaVersion: 1, DocumentID: review.DocumentID, KnowledgeRevisionID: knowledgeRevisionID,
		DocumentRevisionID: documentRevisionID, PublicationReason: notesyncintegration.PublicationReasonReviewKeepCanonical,
		ReviewID: review.ReviewID,
	})
	if err != nil {
		return fmt.Errorf("encode notesync review publication intent: %w", err)
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: NotesyncPublicationBusinessType, AggregateID: review.DocumentID,
		IdempotencyKey: fmt.Sprintf("notesync.review.publish:%s:%s:%d", review.ReviewID, documentRevisionID, generation),
		Revision:       revisionNo, Generation: generation, Payload: payload, AuditMetadata: json.RawMessage(`{}`),
		MaxAttempts: notesyncPublicationMaxAttempts,
	}, createdAt.UTC())
	if err != nil {
		return fmt.Errorf("build notesync review publication intent: %w", err)
	}
	if _, err := outboxpostgresstore.EnqueueWith(ctx, tx, message); err != nil {
		return fmt.Errorf("enqueue notesync review publication intent: %w", err)
	}
	return nil
}

func validateLockedNotesyncReview(
	ctx context.Context,
	tx pgx.Tx,
	review notesyncintegration.Review,
	basisHash string,
	generation int64,
	currentHead string,
	observed notesyncintegration.ReviewSnapshot,
) error {
	if review.Status != notesyncintegration.ReviewStatusOpen || review.BasisHash != basisHash ||
		review.Generation != generation || review.HeadRevisionID != currentHead {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	if review.Remote.Missing != observed.Missing || (!review.Remote.Missing &&
		(review.Remote.SHA256 != observed.SHA256 || review.Remote.Markdown != observed.Markdown ||
			review.Remote.RemoteVersion != observed.RemoteVersion || review.Remote.RemoteLastTime != observed.RemoteLastTime)) {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	if review.Local.Missing {
		if review.DocumentID != "" && currentHead != "" {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_snapshot_documents WHERE knowledge_revision_id=$1 AND document_id=$2)`, currentHead, review.DocumentID).Scan(&exists); err != nil {
				return fmt.Errorf("check notesync missing local snapshot: %w", err)
			}
			if exists {
				return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
			}
		}
	} else {
		var documentRevisionID string
		if err := tx.QueryRow(ctx, `
			SELECT document_revision_id::text FROM knowledge_snapshot_documents
			WHERE knowledge_revision_id=$1 AND document_id=$2 AND canonical_path=$3`,
			currentHead, review.DocumentID, review.CanonicalPath).Scan(&documentRevisionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
			}
			return fmt.Errorf("read notesync current local snapshot: %w", err)
		}
		if documentRevisionID != review.Local.DocumentRevisionID {
			return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
		}
	}
	if review.Base.Missing {
		if review.DocumentID != "" {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_notesync_publications WHERE document_id=$1 AND status='active')`, review.DocumentID).Scan(&exists); err != nil {
				return fmt.Errorf("check notesync missing publication base: %w", err)
			}
			if exists {
				return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
			}
		}
	} else {
		mapping, err := loadNotesyncMapping(ctx, tx, review.DocumentID)
		if err != nil {
			return err
		}
		if mapping == nil || mapping.RemoteVault != review.RemoteVault || mapping.RemotePath != review.Base.Path ||
			mapping.KnowledgeRevisionID != review.Base.KnowledgeRevisionID || mapping.DocumentRevisionID != review.Base.DocumentRevisionID ||
			notesyncMarkdownHash(mapping.BaseMarkdown) != review.Base.SHA256 || mapping.RemoteVersion != review.Base.RemoteVersion ||
			mapping.RemoteLastTime != review.Base.RemoteLastTime || mapping.Generation != generation {
			return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
		}
	}
	return nil
}

func completeNotesyncResolution(
	ctx context.Context,
	tx pgx.Tx,
	review notesyncintegration.Review,
	deviceID string,
	operationID string,
	requestHash string,
	kind string,
	generation int64,
	result notesyncintegration.ResolutionResult,
	resolvedAt time.Time,
) error {
	hash, err := hex.DecodeString(requestHash)
	if err != nil || len(hash) != sha256.Size {
		return errors.New("invalid notesync resolution request hash")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_notesync_resolution_operations(
		  device_id,operation_id,request_hash,review_id,generation,resolution_kind,
		  result_knowledge_revision_id,result_document_id,result_document_revision_id,
		  unchanged,status,completed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'completed',$11)`,
		deviceID, operationID, hash, review.ReviewID, generation, kind,
		nullableUUID(result.KnowledgeRevisionID), nullableUUID(result.DocumentID), nullableUUID(result.DocumentRevisionID),
		result.Unchanged, resolvedAt.UTC()); err != nil {
		return fmt.Errorf("record notesync resolution operation: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE knowledge_notesync_reviews
		SET status='resolved',resolution_kind=$2,resolution_operation_id=$3,resolved_by_device_id=$4,
		    resolved_knowledge_revision_id=$5,resolved_document_id=$6,resolved_document_revision_id=$7,
		    updated_at=$8,resolved_at=$8
		WHERE review_id=$1 AND status='open'`, review.ReviewID, kind, operationID, deviceID,
		nullableUUID(result.KnowledgeRevisionID), nullableUUID(result.DocumentID), nullableUUID(result.DocumentRevisionID), resolvedAt.UTC())
	if err != nil {
		return fmt.Errorf("resolve notesync review: %w", err)
	}
	if command.RowsAffected() != 1 {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	return nil
}

func lookupNotesyncResolutionWith(ctx context.Context, tx pgx.Tx, deviceID, operationID string, forUpdate bool) (notesyncintegration.ResolutionOperationRecord, bool, error) {
	query := `
		SELECT request_hash,review_id::text,resolution_kind,result_knowledge_revision_id::text,
		       result_document_id::text,result_document_revision_id::text,unchanged
		FROM knowledge_notesync_resolution_operations
		WHERE device_id=$1 AND operation_id=$2 AND status='completed'`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var hash []byte
	var result notesyncintegration.ResolutionResult
	err := tx.QueryRow(ctx, query, deviceID, operationID).Scan(
		&hash, &result.ReviewID, &result.ResolutionKind, &result.KnowledgeRevisionID,
		&result.DocumentID, &result.DocumentRevisionID, &result.Unchanged,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return notesyncintegration.ResolutionOperationRecord{}, false, nil
	}
	if err != nil {
		return notesyncintegration.ResolutionOperationRecord{}, false, fmt.Errorf("read notesync resolution operation: %w", err)
	}
	return notesyncintegration.ResolutionOperationRecord{RequestHash: hex.EncodeToString(hash), Result: result}, true, nil
}

func readNotesyncReview(ctx context.Context, db queryer, reviewID string, forUpdate bool) (notesyncintegration.Review, error) {
	query := `SELECT ` + notesyncReviewColumns + ` FROM knowledge_notesync_reviews WHERE review_id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	review, err := scanNotesyncReview(db.QueryRow(ctx, query, reviewID))
	if errors.Is(err, pgx.ErrNoRows) {
		return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewNotFound}
	}
	return review, err
}

func readNotesyncReviewByBasis(ctx context.Context, db queryer, review notesyncintegration.Review) (notesyncintegration.Review, error) {
	basis, _ := hex.DecodeString(review.BasisHash)
	query := `SELECT ` + notesyncReviewColumns + ` FROM knowledge_notesync_reviews
		WHERE COALESCE(document_id,'00000000-0000-0000-0000-000000000000'::uuid)=COALESCE($1::uuid,'00000000-0000-0000-0000-000000000000'::uuid)
		  AND remote_vault=$2 AND remote_path=$3 AND basis_hash=$4 AND status='open'`
	stored, err := scanNotesyncReview(db.QueryRow(ctx, query, nullableUUID(review.DocumentID), review.RemoteVault, review.RemotePath, basis))
	if err != nil {
		return notesyncintegration.Review{}, fmt.Errorf("read idempotent notesync review: %w", err)
	}
	return stored, nil
}

func scanNotesyncReviewSummary(row pgx.Row) (notesyncintegration.ReviewSummary, error) {
	var review notesyncintegration.Review
	var documentID, remoteDocumentID, headID *string
	var baseKnowledgeID, baseDocumentID, basePath, localKnowledgeID, localDocumentID *string
	var remoteSourceID, resolutionKind, resolutionOperationID, resolvedByDeviceID *string
	var resolvedKnowledgeID, resolvedDocumentID, resolvedDocumentRevisionID *string
	var baseRevisionNo, baseRemoteVersion, baseRemoteLastTime, localRevisionNo, headRevisionNo, remoteVersion, remoteLastTime *int64
	var baseHash, localHash, remoteHash, basisHash []byte
	err := row.Scan(
		&review.ReviewID, &documentID, &remoteDocumentID, &review.RemoteVault, &review.RemotePath,
		&review.Category, &review.ReasonCode, &review.Status, &headID, &headRevisionNo, &review.CanonicalPath,
		&review.Base.Missing, &baseKnowledgeID, &baseRevisionNo, &baseDocumentID,
		&basePath, &baseRemoteVersion, &baseRemoteLastTime, &baseHash,
		&review.Local.Missing, &localKnowledgeID, &localRevisionNo, &localDocumentID, &localHash,
		&review.Remote.Missing, &remoteHash, &remoteVersion, &remoteLastTime, &remoteSourceID,
		&basisHash, &review.Generation, &resolutionKind, &resolutionOperationID, &resolvedByDeviceID,
		&resolvedKnowledgeID, &resolvedDocumentID, &resolvedDocumentRevisionID,
		&review.CreatedAt, &review.UpdatedAt, &review.ResolvedAt,
	)
	if err != nil {
		return notesyncintegration.ReviewSummary{}, err
	}
	review.DocumentID = optionalString(documentID)
	review.RemoteDocumentID = optionalString(remoteDocumentID)
	review.HeadRevisionID = optionalString(headID)
	review.HeadRevisionNo = optionalInt64(headRevisionNo)
	review.ResolutionKind = optionalString(resolutionKind)
	review.ResolutionOperationID = optionalString(resolutionOperationID)
	review.ResolvedByDeviceID = optionalString(resolvedByDeviceID)
	review.ResolvedKnowledgeRevisionID = optionalString(resolvedKnowledgeID)
	review.ResolvedDocumentID = optionalString(resolvedDocumentID)
	review.ResolvedDocumentRevisionID = optionalString(resolvedDocumentRevisionID)
	review.BasisHash = hex.EncodeToString(basisHash)
	review.Base = notesyncintegration.ReviewSnapshot{
		Missing: review.Base.Missing, KnowledgeRevisionID: optionalString(baseKnowledgeID),
		KnowledgeRevisionNo: optionalInt64(baseRevisionNo), DocumentRevisionID: optionalString(baseDocumentID),
		SourceRevisionID: optionalString(baseKnowledgeID), Path: optionalString(basePath), SHA256: hex.EncodeToString(baseHash),
		RemoteVersion: optionalInt64(baseRemoteVersion), RemoteLastTime: optionalInt64(baseRemoteLastTime),
	}
	review.Local = notesyncintegration.ReviewSnapshot{
		Missing: review.Local.Missing, KnowledgeRevisionID: optionalString(localKnowledgeID),
		KnowledgeRevisionNo: optionalInt64(localRevisionNo), DocumentRevisionID: optionalString(localDocumentID),
		SourceRevisionID: optionalString(localKnowledgeID), Path: review.CanonicalPath, SHA256: hex.EncodeToString(localHash),
	}
	review.Remote = notesyncintegration.ReviewSnapshot{
		Missing: review.Remote.Missing, SourceRevisionID: optionalString(remoteSourceID), Path: review.RemotePath,
		SHA256: hex.EncodeToString(remoteHash), RemoteVersion: optionalInt64(remoteVersion), RemoteLastTime: optionalInt64(remoteLastTime),
	}
	review.CreatedAt = review.CreatedAt.UTC()
	review.UpdatedAt = review.UpdatedAt.UTC()
	if review.ResolvedAt != nil {
		value := review.ResolvedAt.UTC()
		review.ResolvedAt = &value
	}
	return notesyncintegration.SummarizeReview(review), nil
}

func scanNotesyncReview(row pgx.Row) (notesyncintegration.Review, error) {
	var review notesyncintegration.Review
	var documentID, remoteDocumentID, headID *string
	var baseKnowledgeID, baseDocumentID, basePath, localKnowledgeID, localDocumentID *string
	var remoteSourceID, resolutionKind, resolutionOperationID, resolvedByDeviceID *string
	var resolvedKnowledgeID, resolvedDocumentID, resolvedDocumentRevisionID *string
	var baseRevisionNo, baseRemoteVersion, baseRemoteLastTime, localRevisionNo, headRevisionNo, remoteVersion, remoteLastTime *int64
	var baseMarkdown, localMarkdown, remoteMarkdown *string
	var baseHash, localHash, remoteHash, basisHash []byte
	err := row.Scan(
		&review.ReviewID, &documentID, &remoteDocumentID, &review.RemoteVault, &review.RemotePath,
		&review.Category, &review.ReasonCode, &review.Status, &headID, &headRevisionNo,
		&review.CanonicalPath, &review.Base.Missing, &baseKnowledgeID, &baseRevisionNo,
		&baseDocumentID, &basePath, &baseRemoteVersion, &baseRemoteLastTime, &baseMarkdown, &baseHash, &review.Local.Missing, &localKnowledgeID,
		&localRevisionNo, &localDocumentID, &localMarkdown, &localHash, &review.Remote.Missing,
		&remoteMarkdown, &remoteHash, &remoteVersion, &remoteLastTime, &remoteSourceID,
		&review.Diff.BaseToLocal, &review.Diff.BaseToRemote, &review.Diff.LocalTruncated, &review.Diff.RemoteTruncated,
		&basisHash,
		&review.Generation, &resolutionKind, &resolutionOperationID, &resolvedByDeviceID,
		&resolvedKnowledgeID, &resolvedDocumentID, &resolvedDocumentRevisionID,
		&review.CreatedAt, &review.UpdatedAt, &review.ResolvedAt,
	)
	if err != nil {
		return notesyncintegration.Review{}, err
	}
	review.DocumentID = optionalString(documentID)
	review.RemoteDocumentID = optionalString(remoteDocumentID)
	review.HeadRevisionID = optionalString(headID)
	review.HeadRevisionNo = optionalInt64(headRevisionNo)
	review.ResolutionKind = optionalString(resolutionKind)
	review.ResolutionOperationID = optionalString(resolutionOperationID)
	review.ResolvedByDeviceID = optionalString(resolvedByDeviceID)
	review.ResolvedKnowledgeRevisionID = optionalString(resolvedKnowledgeID)
	review.ResolvedDocumentID = optionalString(resolvedDocumentID)
	review.ResolvedDocumentRevisionID = optionalString(resolvedDocumentRevisionID)
	review.BasisHash = hex.EncodeToString(basisHash)
	review.Base = notesyncintegration.ReviewSnapshot{
		Missing: review.Base.Missing, KnowledgeRevisionID: optionalString(baseKnowledgeID), KnowledgeRevisionNo: optionalInt64(baseRevisionNo),
		DocumentRevisionID: optionalString(baseDocumentID), SourceRevisionID: optionalString(baseKnowledgeID),
		Path: optionalString(basePath), Markdown: optionalString(baseMarkdown), SHA256: hex.EncodeToString(baseHash),
		RemoteVersion: optionalInt64(baseRemoteVersion), RemoteLastTime: optionalInt64(baseRemoteLastTime),
	}
	review.Local = notesyncintegration.ReviewSnapshot{
		Missing: review.Local.Missing, KnowledgeRevisionID: optionalString(localKnowledgeID), KnowledgeRevisionNo: optionalInt64(localRevisionNo),
		DocumentRevisionID: optionalString(localDocumentID), SourceRevisionID: optionalString(localKnowledgeID),
		Path: review.CanonicalPath, Markdown: optionalString(localMarkdown), SHA256: hex.EncodeToString(localHash),
	}
	review.Remote = notesyncintegration.ReviewSnapshot{
		Missing: review.Remote.Missing, SourceRevisionID: optionalString(remoteSourceID), Path: review.RemotePath,
		Markdown: optionalString(remoteMarkdown), SHA256: hex.EncodeToString(remoteHash),
		RemoteVersion: optionalInt64(remoteVersion), RemoteLastTime: optionalInt64(remoteLastTime),
	}
	review.CreatedAt = review.CreatedAt.UTC()
	review.UpdatedAt = review.UpdatedAt.UTC()
	if review.ResolvedAt != nil {
		value := review.ResolvedAt.UTC()
		review.ResolvedAt = &value
	}
	return review, nil
}

func (s *Store) lockNotesyncImportResolution(
	ctx context.Context,
	tx pgx.Tx,
	metadata knowledge.NotesyncImportResolution,
	generation int64,
	currentHead string,
) (notesyncintegration.Review, error) {
	if uuid.Validate(metadata.ReviewID) != nil || uuid.Validate(metadata.DeviceID) != nil ||
		uuid.Validate(metadata.OperationID) != nil || len(metadata.BasisHash) != sha256.Size*2 ||
		len(metadata.RequestHash) != sha256.Size*2 ||
		(metadata.Kind != notesyncintegration.ResolutionAcceptRemote && metadata.Kind != notesyncintegration.ResolutionMerged) ||
		metadata.CanonicalPath == "" || metadata.ResolvedAt.IsZero() {
		return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
	}
	if stored, exists, err := lookupNotesyncResolutionWith(ctx, tx, metadata.DeviceID, metadata.OperationID, true); err != nil {
		return notesyncintegration.Review{}, err
	} else if exists {
		if stored.RequestHash != metadata.RequestHash {
			return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewIdempotencyConflict}
		}
		return notesyncintegration.Review{}, errors.New("notesync resolution operation completed before knowledge import")
	}
	review, err := readNotesyncReview(ctx, tx, metadata.ReviewID, true)
	if err != nil {
		return notesyncintegration.Review{}, err
	}
	observed := notesyncintegration.ReviewSnapshot{
		Missing: metadata.ObservedRemoteMissing, Path: review.RemotePath,
		Markdown: metadata.ObservedRemoteMarkdown, SHA256: metadata.ObservedRemoteSHA256,
		RemoteVersion: metadata.ObservedRemoteVersion, RemoteLastTime: metadata.ObservedRemoteLastTime,
	}
	if err := validateLockedNotesyncReview(ctx, tx, review, metadata.BasisHash, generation, currentHead, observed); err != nil {
		return notesyncintegration.Review{}, err
	}
	if review.CanonicalPath != metadata.CanonicalPath {
		return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	if metadata.Kind == notesyncintegration.ResolutionAcceptRemote &&
		(review.Remote.Missing || review.Category == notesyncintegration.PreviewCategoryInvalidRemoteMarkdown) {
		return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
	}
	expectedDocumentID := review.RemoteDocumentID
	if metadata.Kind == notesyncintegration.ResolutionMerged && !review.Local.Missing && review.DocumentID != "" {
		expectedDocumentID = review.DocumentID
	}
	if expectedDocumentID == "" {
		expectedDocumentID = review.DocumentID
	}
	if expectedDocumentID == "" || uuid.Validate(metadata.ExpectedDocumentID) != nil || metadata.ExpectedDocumentID != expectedDocumentID {
		return notesyncintegration.Review{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
	}
	return review, nil
}

func (s *Store) completeNotesyncImportResolution(
	ctx context.Context,
	tx pgx.Tx,
	prepared knowledge.PreparedCommit,
	review notesyncintegration.Review,
) (notesyncintegration.ResolutionResult, error) {
	metadata := prepared.NotesyncResolution
	if metadata == nil || prepared.Unchanged {
		return notesyncintegration.ResolutionResult{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
	}
	var resolved *knowledge.SnapshotDocument
	for index := range prepared.Revision.Documents {
		document := &prepared.Revision.Documents[index]
		if document.Path != metadata.CanonicalPath {
			continue
		}
		if metadata.ExpectedDocumentID != "" && document.Revision.DocumentID != metadata.ExpectedDocumentID {
			return notesyncintegration.ResolutionResult{}, &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
		}
		resolved = document
		break
	}
	if resolved == nil {
		return notesyncintegration.ResolutionResult{}, errors.New("notesync import resolution lacks its canonical document")
	}
	result := notesyncintegration.ResolutionResult{
		ReviewID: review.ReviewID, ResolutionKind: metadata.Kind,
		KnowledgeRevisionID: prepared.Revision.ID, DocumentID: resolved.Revision.DocumentID,
		DocumentRevisionID: resolved.Revision.ID,
	}
	if err := completeNotesyncResolution(ctx, tx, review, metadata.DeviceID, metadata.OperationID,
		metadata.RequestHash, metadata.Kind, review.Generation, result, metadata.ResolvedAt); err != nil {
		return notesyncintegration.ResolutionResult{}, err
	}
	return result, nil
}

func validateCompletedNotesyncImportResolution(
	ctx context.Context,
	tx pgx.Tx,
	metadata *knowledge.NotesyncImportResolution,
) error {
	if metadata == nil {
		return nil
	}
	record, exists, err := lookupNotesyncResolutionWith(ctx, tx, metadata.DeviceID, metadata.OperationID, true)
	if err != nil {
		return err
	}
	if !exists || record.RequestHash != metadata.RequestHash || record.Result.ReviewID != metadata.ReviewID ||
		record.Result.ResolutionKind != metadata.Kind {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewIdempotencyConflict}
	}
	return nil
}

func validateNotesyncReviewRecord(review notesyncintegration.Review) error {
	basis, basisErr := hex.DecodeString(review.BasisHash)
	if uuid.Validate(review.ReviewID) != nil || review.Status != notesyncintegration.ReviewStatusOpen ||
		review.Generation < 1 || review.RemoteVault == "" || review.RemotePath == "" || review.CanonicalPath == "" ||
		!validNotesyncOpenReviewCategory(review.Category) || review.ReasonCode == "" || basisErr != nil || len(basis) != sha256.Size ||
		review.BasisHash != notesyncintegration.ReviewBasisHash(review) ||
		len(review.Diff.BaseToLocal) > 256<<10 || len(review.Diff.BaseToRemote) > 256<<10 ||
		!utf8.ValidString(review.Diff.BaseToLocal) || !utf8.ValidString(review.Diff.BaseToRemote) {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
	}
	for index, snapshot := range []notesyncintegration.ReviewSnapshot{review.Base, review.Local, review.Remote} {
		if snapshot.Missing {
			if snapshot.Markdown != "" || snapshot.SHA256 != "" || snapshot.RemoteVersion != 0 || snapshot.RemoteLastTime != 0 {
				return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
			}
			continue
		}
		if len(snapshot.Markdown) > knowledge.MaxDocumentBytes || snapshot.SHA256 != notesyncMarkdownHash(snapshot.Markdown) ||
			snapshot.RemoteVersion < 0 || snapshot.RemoteLastTime < 0 || index == 0 && snapshot.Path == "" {
			return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewInvalidRequest}
		}
	}
	return nil
}

func validNotesyncOpenReviewCategory(value string) bool {
	switch value {
	case notesyncintegration.PreviewCategoryInvalidRemoteMarkdown, notesyncintegration.PreviewCategoryRemoteMoved,
		notesyncintegration.PreviewCategoryPathOccupied, notesyncintegration.PreviewCategoryUnbasedRemote,
		notesyncintegration.PreviewCategoryRemoteMissing, notesyncintegration.PreviewCategoryRemoteChanged,
		notesyncintegration.PreviewCategoryBothChanged:
		return true
	default:
		return false
	}
}

func notesyncMarkdownHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func nullableHash(snapshot notesyncintegration.ReviewSnapshot) any {
	if snapshot.Missing {
		return nil
	}
	value, _ := hex.DecodeString(snapshot.SHA256)
	return value
}

func nullableMarkdown(snapshot notesyncintegration.ReviewSnapshot) any {
	if snapshot.Missing {
		return nil
	}
	return snapshot.Markdown
}

func nullableUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBaseValue(snapshot notesyncintegration.ReviewSnapshot, value string) any {
	if snapshot.Missing {
		return nil
	}
	return value
}

func nullableRemoteValue(snapshot notesyncintegration.ReviewSnapshot, value int64) any {
	if snapshot.Missing {
		return nil
	}
	return value
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func optionalInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
