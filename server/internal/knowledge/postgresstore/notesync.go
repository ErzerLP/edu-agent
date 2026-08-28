package postgresstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	notesyncintegration "github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgresstore "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	NotesyncPublicationBusinessType = notesyncintegration.PublicationBusinessType
	notesyncPublicationMaxAttempts  = 5
)

type Option func(*Store)

type NotesyncPublicationConfig struct {
	Vault      string
	PathPrefix string
}

// WithNotesyncPublication atomically emits NoteSync publication intents for changed documents.
func WithNotesyncPublication(config ...NotesyncPublicationConfig) Option {
	return func(store *Store) {
		store.notesyncPublication = true
		if len(config) == 1 {
			store.notesyncVault = config[0].Vault
			store.notesyncPathPrefix = config[0].PathPrefix
		}
	}
}

type NotesyncPublicationIntent = notesyncintegration.PublicationIntent

func lockCurrentNotesyncOutboxGeneration(ctx context.Context, tx pgx.Tx) (int64, error) {
	var outboxGeneration int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate($1,'write',NULL)`, privacy.OwnerOutbox).Scan(&outboxGeneration); err != nil {
		return 0, fmt.Errorf("lock notesync outbox generation: %w", err)
	}
	return outboxGeneration, nil
}

func lockNotesyncOutboxGeneration(ctx context.Context, tx pgx.Tx, generation int64) error {
	outboxGeneration, err := lockCurrentNotesyncOutboxGeneration(ctx, tx)
	if err != nil {
		return err
	}
	if outboxGeneration != generation {
		return fmt.Errorf("lock notesync outbox generation: expected %d, got %d", generation, outboxGeneration)
	}
	return nil
}

type notesyncParentDocument struct {
	RevisionID string
	Path       string
}

func loadParentDocumentRevisions(ctx context.Context, tx pgx.Tx, parentRevisionID *string) (map[string]notesyncParentDocument, error) {
	parent := make(map[string]notesyncParentDocument)
	if parentRevisionID == nil {
		return parent, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT document_id::text,document_revision_id::text,canonical_path
		FROM knowledge_snapshot_documents
		WHERE knowledge_revision_id=$1`, *parentRevisionID)
	if err != nil {
		return nil, fmt.Errorf("read parent document revisions for notesync publication: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var documentID string
		var document notesyncParentDocument
		if err := rows.Scan(&documentID, &document.RevisionID, &document.Path); err != nil {
			return nil, fmt.Errorf("scan parent document revision for notesync publication: %w", err)
		}
		parent[documentID] = document
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate parent document revisions for notesync publication: %w", err)
	}
	return parent, nil
}

func notesyncPublicationDocuments(revision knowledge.KnowledgeRevision, parent map[string]notesyncParentDocument) []knowledge.SnapshotDocument {
	changed := make([]knowledge.SnapshotDocument, 0, len(revision.Documents))
	for _, snapshot := range revision.Documents {
		if parent[snapshot.Revision.DocumentID].RevisionID == snapshot.Revision.ID {
			continue
		}
		changed = append(changed, snapshot)
	}
	return changed
}

func notesyncAffectedDocumentIDs(revision knowledge.KnowledgeRevision, parent map[string]notesyncParentDocument) []string {
	current := make(map[string]knowledge.SnapshotDocument, len(revision.Documents))
	affected := make([]string, 0, len(revision.Documents)+len(parent))
	for _, snapshot := range revision.Documents {
		documentID := snapshot.Revision.DocumentID
		current[documentID] = snapshot
		previous, exists := parent[documentID]
		if !exists || previous.RevisionID != snapshot.Revision.ID || previous.Path != snapshot.Path {
			affected = append(affected, documentID)
		}
	}
	for documentID := range parent {
		if _, exists := current[documentID]; !exists {
			affected = append(affected, documentID)
		}
	}
	sort.Strings(affected)
	return affected
}

func lockNotesyncPublicationDocuments(
	ctx context.Context,
	tx pgx.Tx,
	revision knowledge.KnowledgeRevision,
	parent map[string]notesyncParentDocument,
) error {
	for _, documentID := range notesyncAffectedDocumentIDs(revision, parent) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('notesync-publication:'||$1,0))`, documentID); err != nil {
			return fmt.Errorf("lock notesync publication document %s: %w", documentID, err)
		}
	}
	return nil
}

func enqueueNotesyncPublicationIntents(
	ctx context.Context,
	tx pgx.Tx,
	revision knowledge.KnowledgeRevision,
	generation int64,
	parent map[string]notesyncParentDocument,
	resolution *knowledge.NotesyncImportResolution,
	review *notesyncintegration.Review,
) error {
	if (resolution == nil) != (review == nil) {
		return errors.New("notesync reviewed publication authority is incomplete")
	}
	resolvedDocumentFound := false
	for _, snapshot := range notesyncPublicationDocuments(revision, parent) {
		if resolution != nil && snapshot.Path == resolution.CanonicalPath &&
			snapshot.Revision.DocumentID == resolution.ExpectedDocumentID {
			if review.ReviewID != resolution.ReviewID || review.BasisHash != resolution.BasisHash ||
				review.Generation != generation || review.RemoteVault == "" || review.RemotePath == "" {
				return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
			}
			if err := enqueueNotesyncReviewImportIntent(ctx, tx, revision.ID, revision.RevisionNo, revision.CreatedAt,
				snapshot.Revision.DocumentID, snapshot.Revision.ID, generation, review.ReviewID); err != nil {
				return err
			}
			resolvedDocumentFound = true
			continue
		}
		if err := enqueueNotesyncPublicationIntent(ctx, tx, revision.ID, revision.RevisionNo, revision.CreatedAt,
			snapshot.Revision.DocumentID, snapshot.Revision.ID, generation); err != nil {
			return err
		}
	}
	if resolution != nil && !resolvedDocumentFound {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewStale}
	}
	return nil
}

func enqueueNotesyncPublicationIntent(
	ctx context.Context,
	tx pgx.Tx,
	knowledgeRevisionID string,
	revisionNo int64,
	createdAt time.Time,
	documentID string,
	documentRevisionID string,
	generation int64,
) error {
	payload, err := json.Marshal(NotesyncPublicationIntent{
		SchemaVersion: 1, DocumentID: documentID, KnowledgeRevisionID: knowledgeRevisionID,
		DocumentRevisionID: documentRevisionID, PublicationReason: notesyncintegration.PublicationReasonCanonicalRevision,
	})
	if err != nil {
		return fmt.Errorf("encode notesync publication intent: %w", err)
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: NotesyncPublicationBusinessType, AggregateID: documentID,
		IdempotencyKey: notesyncintegration.CanonicalPublicationIdempotencyKey(documentID, documentRevisionID, revisionNo, generation),
		Revision:       revisionNo, Generation: generation, Payload: payload, AuditMetadata: json.RawMessage(`{}`),
		MaxAttempts: notesyncPublicationMaxAttempts,
	}, createdAt)
	if err != nil {
		return fmt.Errorf("build notesync publication intent: %w", err)
	}
	if _, err := outboxpostgresstore.EnqueueWith(ctx, tx, message); err != nil {
		return fmt.Errorf("enqueue notesync publication intent for document %s: %w", documentID, err)
	}
	return nil
}

func enqueueNotesyncReviewImportIntent(
	ctx context.Context,
	tx pgx.Tx,
	knowledgeRevisionID string,
	revisionNo int64,
	createdAt time.Time,
	documentID string,
	documentRevisionID string,
	generation int64,
	reviewID string,
) error {
	payload, err := json.Marshal(NotesyncPublicationIntent{
		SchemaVersion: 1, DocumentID: documentID, KnowledgeRevisionID: knowledgeRevisionID,
		DocumentRevisionID: documentRevisionID, PublicationReason: notesyncintegration.PublicationReasonReviewImport,
		ReviewID: reviewID,
	})
	if err != nil {
		return fmt.Errorf("encode notesync reviewed import publication intent: %w", err)
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: NotesyncPublicationBusinessType, AggregateID: documentID,
		IdempotencyKey: notesyncintegration.ReviewPublicationIdempotencyKey(reviewID, documentRevisionID, generation),
		Revision:       revisionNo, Generation: generation, Payload: payload, AuditMetadata: json.RawMessage(`{}`),
		MaxAttempts: notesyncPublicationMaxAttempts,
	}, createdAt)
	if err != nil {
		return fmt.Errorf("build notesync reviewed import publication intent: %w", err)
	}
	if _, err := outboxpostgresstore.EnqueueWith(ctx, tx, message); err != nil {
		return fmt.Errorf("enqueue notesync reviewed import publication intent for document %s: %w", documentID, err)
	}
	return nil
}

func (s *Store) BootstrapNotesyncPublications(ctx context.Context) (int, error) {
	if !s.notesyncPublication {
		return 0, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin notesync publication bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	knowledgeGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return 0, err
	}
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate($1,'write',NULL)`, privacy.OwnerOutbox).Scan(&generation); err != nil {
		return 0, fmt.Errorf("lock notesync bootstrap outbox generation: %w", err)
	}
	if knowledgeGeneration != generation {
		return 0, fmt.Errorf("bootstrap notesync publications: knowledge generation %d differs from outbox generation %d", knowledgeGeneration, generation)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('notesync-publication-bootstrap',0))`); err != nil {
		return 0, fmt.Errorf("lock notesync publication bootstrap: %w", err)
	}
	var revisionID *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1 FOR SHARE`).Scan(&revisionID); err != nil {
		return 0, fmt.Errorf("lock knowledge head for notesync bootstrap: %w", err)
	}
	if revisionID == nil {
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit empty notesync publication bootstrap: %w", err)
		}
		return 0, nil
	}
	var revisionNo int64
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT revision_no,created_at FROM knowledge_revisions WHERE id=$1 AND redacted_at IS NULL`, *revisionID).Scan(&revisionNo, &createdAt); err != nil {
		return 0, fmt.Errorf("read notesync bootstrap revision: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT sd.document_id::text,sd.document_revision_id::text
		FROM knowledge_snapshot_documents sd
		WHERE sd.knowledge_revision_id=$1
		  AND NOT EXISTS (
		    SELECT 1 FROM knowledge_notesync_publications p
		    WHERE p.document_id=sd.document_id AND p.status='active'
		      AND p.published_knowledge_revision_id=$1
		      AND p.published_document_revision_id=sd.document_revision_id
		      AND p.published_revision_no=$3 AND p.generation=$2
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM outbox_messages o
		    WHERE o.idempotency_key='notesync.publish:'||sd.document_id::text||':'||
		          sd.document_revision_id::text||':'||$3::text||':'||$2::text
		  )
		ORDER BY sd.canonical_path`, *revisionID, generation, revisionNo)
	if err != nil {
		return 0, fmt.Errorf("read notesync bootstrap documents: %w", err)
	}
	var documents [][2]string
	for rows.Next() {
		var document [2]string
		if err := rows.Scan(&document[0], &document[1]); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan notesync bootstrap document: %w", err)
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate notesync bootstrap documents: %w", err)
	}
	rows.Close()
	for _, document := range documents {
		if err := enqueueNotesyncPublicationIntent(ctx, tx, *revisionID, revisionNo, createdAt, document[0], document[1], generation); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit notesync publication bootstrap: %w", err)
	}
	return len(documents), nil
}

func (s *Store) CanApplyNotesyncPublication(
	ctx context.Context,
	message outbox.Message,
	intent notesyncintegration.PublicationIntent,
) (outbox.ApplyDecision, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return outbox.ApplyDecision{}, fmt.Errorf("begin notesync publication authority check: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	fenced, err := notesyncGenerationFenced(ctx, tx, message.Generation)
	if err != nil {
		return outbox.ApplyDecision{}, err
	}
	if fenced {
		return outbox.ApplyDecision{TerminalDisposition: outbox.DispositionPrivacyErasure}, nil
	}
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return outbox.ApplyDecision{}, err
	}
	if generation != message.Generation {
		return outbox.ApplyDecision{TerminalDisposition: outbox.DispositionPrivacyErasure}, nil
	}
	outboxGeneration, err := lockCurrentNotesyncOutboxGeneration(ctx, tx)
	if err != nil {
		return outbox.ApplyDecision{}, err
	}
	if outboxGeneration != message.Generation {
		return outbox.ApplyDecision{TerminalDisposition: outbox.DispositionPrivacyErasure}, nil
	}
	current, err := currentNotesyncAuthority(ctx, tx, message, intent)
	if err != nil {
		return outbox.ApplyDecision{}, err
	}
	if !current {
		return outbox.ApplyDecision{TerminalDisposition: outbox.DispositionSuperseded}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return outbox.ApplyDecision{}, fmt.Errorf("commit notesync publication authority check: %w", err)
	}
	return outbox.ApplyDecision{Apply: true}, nil
}

func (s *Store) ApplyNotesyncPublication(
	ctx context.Context,
	message outbox.Message,
	intent notesyncintegration.PublicationIntent,
	operation notesyncintegration.PublicationOperation,
) error {
	if operation == nil {
		return errors.New("notesync publication operation is required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin notesync publication transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	fenced, err := notesyncGenerationFenced(ctx, tx, message.Generation)
	if err != nil {
		return err
	}
	if fenced {
		return cancelFencedNotesyncPublication(ctx, tx, message)
	}
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return err
	}
	if generation != message.Generation {
		return cancelFencedNotesyncPublication(ctx, tx, message)
	}
	outboxGeneration, err := lockCurrentNotesyncOutboxGeneration(ctx, tx)
	if err != nil {
		return err
	}
	if outboxGeneration != message.Generation {
		return cancelFencedNotesyncPublication(ctx, tx, message)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('notesync-publication:'||$1,0))`, intent.DocumentID); err != nil {
		return fmt.Errorf("lock notesync publication document: %w", err)
	}
	if err := lockNotesyncMessage(ctx, tx, message); err != nil {
		return err
	}
	current, err := currentNotesyncAuthority(ctx, tx, message, intent)
	if err != nil {
		return err
	}
	if !current {
		if err := recordSupersededNotesyncAttempt(ctx, tx, message, intent); err != nil {
			return err
		}
		if err := outboxpostgresstore.CancelWith(ctx, tx, outbox.CancelRequest{
			IdempotencyKey: message.IdempotencyKey, LeaseToken: message.LeaseToken,
			Disposition: outbox.DispositionSuperseded, CanceledAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit superseded notesync publication: %w", err)
		}
		return nil
	}
	work, err := s.prepareNotesyncPublication(ctx, tx, message, intent)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended(
		  'notesync-remote:'||length($1)::text||':'||$1||':'||$2,0
		))`, work.RemoteVault, work.RemotePath); err != nil {
		return fmt.Errorf("lock notesync remote path: %w", err)
	}
	if work.Mapping == nil && (intent.PublicationReason == notesyncintegration.PublicationReasonCanonicalRevision ||
		intent.PublicationReason == notesyncintegration.PublicationReasonReviewImport) {
		var occupied bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM knowledge_notesync_publications
			  WHERE remote_vault=$1 AND remote_path=$2 AND status='active' AND document_id<>$3
			)`, work.RemoteVault, work.RemotePath, work.DocumentID).Scan(&occupied); err != nil {
			return fmt.Errorf("check notesync managed path ownership: %w", err)
		}
		work.PathOccupied = occupied
	}
	outcome, err := operation(ctx, work)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	var publicationFailure error
	switch outcome.Kind {
	case notesyncintegration.OutcomeApplied:
		if err := finalizeNotesyncApplied(ctx, tx, message, work, outcome, now); err != nil {
			return err
		}
	case notesyncintegration.OutcomeDeferred:
		if err := finalizeNotesyncDeferred(ctx, tx, message, outcome, now); err != nil {
			return err
		}
	case notesyncintegration.OutcomeFailed:
		if err := finalizeNotesyncFailed(ctx, tx, message, outcome, now); err != nil {
			return err
		}
		publicationFailure = notesyncintegration.FailedPublication(outcome.Category, outcome.Permanent)
	case notesyncintegration.OutcomeReview:
		if err := finalizeNotesyncReview(ctx, tx, message, work, outcome, now); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported notesync publication outcome %q", outcome.Kind)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit notesync publication transaction: %w", err)
	}
	return publicationFailure
}

func cancelFencedNotesyncPublication(ctx context.Context, tx pgx.Tx, message outbox.Message) error {
	if err := outboxpostgresstore.CancelWith(ctx, tx, outbox.CancelRequest{
		IdempotencyKey: message.IdempotencyKey, LeaseToken: message.LeaseToken,
		Disposition: outbox.DispositionPrivacyErasure, CanceledAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fenced notesync publication: %w", err)
	}
	return nil
}

func notesyncGenerationFenced(ctx context.Context, tx pgx.Tx, expected int64) (bool, error) {
	var knowledgeGeneration, outboxGeneration int64
	var knowledgeOpen, outboxOpen bool
	if err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='knowledge'),
		  (SELECT read_open FROM privacy_owner_generation_gates WHERE owner_kind='knowledge'),
		  (SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='outbox'),
		  (SELECT write_open FROM privacy_owner_generation_gates WHERE owner_kind='outbox')`).Scan(
		&knowledgeGeneration, &knowledgeOpen, &outboxGeneration, &outboxOpen,
	); err != nil {
		return false, fmt.Errorf("read notesync privacy generations: %w", err)
	}
	return knowledgeGeneration != expected || outboxGeneration != expected || !knowledgeOpen || !outboxOpen, nil
}

func currentNotesyncAuthority(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	intent notesyncintegration.PublicationIntent,
) (bool, error) {
	var revisionNo int64
	var redacted bool
	err := tx.QueryRow(ctx, `
		SELECT kr.revision_no,kr.redacted_at IS NOT NULL
		FROM knowledge_revisions kr
		JOIN knowledge_snapshot_documents sd
		  ON sd.knowledge_revision_id=kr.id AND sd.document_id=$2 AND sd.document_revision_id=$3
		WHERE kr.id=$1`, intent.KnowledgeRevisionID, intent.DocumentID, intent.DocumentRevisionID).Scan(&revisionNo, &redacted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read notesync intent revision authority: %w", err)
	}
	if redacted || revisionNo != message.Revision {
		return false, nil
	}
	var currentDocumentRevisionID string
	err = tx.QueryRow(ctx, `
		SELECT sd.document_revision_id::text
		FROM knowledge_catalog catalog
		JOIN knowledge_snapshot_documents sd
		  ON sd.knowledge_revision_id=catalog.head_revision_id AND sd.document_id=$1
		WHERE catalog.singleton_id=1`, intent.DocumentID).Scan(&currentDocumentRevisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read current notesync document authority: %w", err)
	}
	if currentDocumentRevisionID != intent.DocumentRevisionID {
		return false, nil
	}
	var publishedRevisionNo, mappingGeneration int64
	err = tx.QueryRow(ctx, `
		SELECT published_revision_no,generation
		FROM knowledge_notesync_publications
		WHERE document_id=$1 AND status='active'`, intent.DocumentID).Scan(&publishedRevisionNo, &mappingGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read notesync publication progress: %w", err)
	}
	return mappingGeneration == message.Generation && publishedRevisionNo <= message.Revision, nil
}

func lockNotesyncMessage(ctx context.Context, tx pgx.Tx, message outbox.Message) error {
	var matches bool
	err := tx.QueryRow(ctx, `
		SELECT business_type=$2 AND aggregate_id=$3 AND idempotency_key=$4
		   AND revision=$5 AND generation=$6 AND payload=$7::jsonb
		   AND status='processing' AND lease_token=$8::uuid
		FROM outbox_messages WHERE id=$1 FOR UPDATE`,
		message.ID, message.BusinessType, message.AggregateID, message.IdempotencyKey,
		message.Revision, message.Generation, message.Payload, message.LeaseToken).Scan(&matches)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !matches {
		return outbox.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock notesync outbox lease: %w", err)
	}
	return nil
}

func (s *Store) prepareNotesyncPublication(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	intent notesyncintegration.PublicationIntent,
) (notesyncintegration.PublicationWork, error) {
	var canonicalPath, canonicalMarkdown string
	var revisionCreatedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT current_snapshot.canonical_path,payload.canonical_markdown,target_revision.created_at
		FROM knowledge_catalog catalog
		JOIN knowledge_snapshot_documents current_snapshot
		  ON current_snapshot.knowledge_revision_id=catalog.head_revision_id
		 AND current_snapshot.document_id=$1 AND current_snapshot.document_revision_id=$2
		JOIN knowledge_document_payloads payload ON payload.document_revision_id=current_snapshot.document_revision_id
		JOIN knowledge_revisions target_revision ON target_revision.id=$3
		WHERE catalog.singleton_id=1`, intent.DocumentID, intent.DocumentRevisionID, intent.KnowledgeRevisionID).Scan(
		&canonicalPath, &canonicalMarkdown, &revisionCreatedAt,
	); err != nil {
		return notesyncintegration.PublicationWork{}, fmt.Errorf("load notesync publication target: %w", err)
	}
	targetMarkdown, err := knowledge.ExportMarkdown(canonicalMarkdown, intent.KnowledgeRevisionID)
	if err != nil {
		return notesyncintegration.PublicationWork{}, fmt.Errorf("render notesync publication markdown: %w", err)
	}
	mapping, err := loadNotesyncMapping(ctx, tx, intent.DocumentID)
	if err != nil {
		return notesyncintegration.PublicationWork{}, err
	}
	remoteVault, remotePath := s.notesyncVault, ""
	if mapping != nil {
		remoteVault, remotePath = mapping.RemoteVault, mapping.RemotePath
	} else {
		remotePath, err = notesyncintegration.ManagedPath(s.notesyncPathPrefix, canonicalPath)
		if err != nil || remoteVault == "" {
			return notesyncintegration.PublicationWork{}, fmt.Errorf("derive notesync managed target: %w", err)
		}
	}
	var reviewRemote *notesyncintegration.RemoteObservation
	if intent.PublicationReason == notesyncintegration.PublicationReasonReviewKeepCanonical ||
		intent.PublicationReason == notesyncintegration.PublicationReasonReviewImport {
		review, reviewErr := readNotesyncReview(ctx, tx, intent.ReviewID, false)
		if reviewErr != nil {
			return notesyncintegration.PublicationWork{}, reviewErr
		}
		validResolutionKind := review.ResolutionKind == notesyncintegration.ResolutionKeepCanonical
		validDocument := review.DocumentID == intent.DocumentID
		if intent.PublicationReason == notesyncintegration.PublicationReasonReviewImport {
			validResolutionKind = review.ResolutionKind == notesyncintegration.ResolutionAcceptRemote ||
				review.ResolutionKind == notesyncintegration.ResolutionMerged
			validDocument = review.ResolvedDocumentID == intent.DocumentID
		}
		if review.Status != notesyncintegration.ReviewStatusResolved || !validResolutionKind || !validDocument ||
			review.ResolvedKnowledgeRevisionID != intent.KnowledgeRevisionID ||
			review.ResolvedDocumentRevisionID != intent.DocumentRevisionID || review.Generation != message.Generation {
			return notesyncintegration.PublicationWork{}, errors.New("notesync review publication authority is stale")
		}
		remoteVault, remotePath = review.RemoteVault, review.RemotePath
		reviewRemote = &notesyncintegration.RemoteObservation{
			Missing: review.Remote.Missing, Markdown: review.Remote.Markdown,
			Version: review.Remote.RemoteVersion, LastTime: review.Remote.RemoteLastTime,
		}
	}
	attempt, err := prepareNotesyncAttempt(ctx, tx, message, intent, mapping)
	if err != nil {
		return notesyncintegration.PublicationWork{}, err
	}
	if attempt.baseMissing != (mapping == nil) || mapping != nil && attempt.baseMarkdown != mapping.BaseMarkdown {
		return notesyncintegration.PublicationWork{}, errors.New("notesync publication attempt base no longer matches mapping")
	}
	return notesyncintegration.PublicationWork{
		DocumentID: intent.DocumentID, KnowledgeRevisionID: intent.KnowledgeRevisionID,
		DocumentRevisionID: intent.DocumentRevisionID, RevisionNo: message.Revision,
		Generation: message.Generation, CanonicalPath: canonicalPath, TargetMarkdown: targetMarkdown,
		TargetModifiedAt: revisionCreatedAt.UTC(), RemoteVault: remoteVault, RemotePath: remotePath,
		PublicationReason: intent.PublicationReason, ReviewID: intent.ReviewID, ReviewRemote: reviewRemote, Mapping: mapping,
	}, nil
}

func loadNotesyncMapping(ctx context.Context, tx pgx.Tx, documentID string) (*notesyncintegration.PublicationMapping, error) {
	var mapping notesyncintegration.PublicationMapping
	var remoteVersion, remoteLastTime *int64
	err := tx.QueryRow(ctx, `
		SELECT document_id::text,remote_vault,remote_path,published_knowledge_revision_id::text,
		       published_document_revision_id::text,published_revision_no,base_markdown,
		       remote_version,remote_last_time,generation
		FROM knowledge_notesync_publications
		WHERE document_id=$1 AND status='active' FOR UPDATE`, documentID).Scan(
		&mapping.DocumentID, &mapping.RemoteVault, &mapping.RemotePath, &mapping.KnowledgeRevisionID,
		&mapping.DocumentRevisionID, &mapping.RevisionNo, &mapping.BaseMarkdown,
		&remoteVersion, &remoteLastTime, &mapping.Generation,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock notesync publication mapping: %w", err)
	}
	if remoteVersion != nil {
		mapping.RemoteVersion = *remoteVersion
	}
	if remoteLastTime != nil {
		mapping.RemoteLastTime = *remoteLastTime
	}
	return &mapping, nil
}

type notesyncAttemptBase struct {
	baseMissing  bool
	baseMarkdown string
}

func prepareNotesyncAttempt(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	intent notesyncintegration.PublicationIntent,
	mapping *notesyncintegration.PublicationMapping,
) (notesyncAttemptBase, error) {
	baseMissing := mapping == nil
	var baseMarkdown any
	var baseSHA any
	if mapping != nil {
		baseMarkdown = mapping.BaseMarkdown
		hash := sha256.Sum256([]byte(mapping.BaseMarkdown))
		baseSHA = hash[:]
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_notesync_publication_attempts(
		  id,outbox_id,idempotency_key,document_id,knowledge_revision_id,document_revision_id,
		  knowledge_revision_no,generation,publication_reason,status,base_missing,base_markdown,
		  base_sha256,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'prepared',$10,$11,$12,$13,$13)
		ON CONFLICT(outbox_id) DO UPDATE
		SET status='prepared',error_category=NULL,error_at=NULL,updated_at=EXCLUDED.updated_at`,
		uuid.NewString(), message.ID, message.IdempotencyKey, intent.DocumentID,
		intent.KnowledgeRevisionID, intent.DocumentRevisionID, message.Revision, message.Generation,
		intent.PublicationReason, baseMissing, baseMarkdown, baseSHA, now); err != nil {
		return notesyncAttemptBase{}, fmt.Errorf("prepare notesync publication attempt: %w", err)
	}
	var attempt notesyncAttemptBase
	if err := tx.QueryRow(ctx, `
		SELECT base_missing,COALESCE(base_markdown,'')
		FROM knowledge_notesync_publication_attempts WHERE outbox_id=$1`, message.ID).Scan(
		&attempt.baseMissing, &attempt.baseMarkdown,
	); err != nil {
		return notesyncAttemptBase{}, fmt.Errorf("read notesync publication attempt base: %w", err)
	}
	return attempt, nil
}

func recordSupersededNotesyncAttempt(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	intent notesyncintegration.PublicationIntent,
) error {
	mapping, err := loadNotesyncMapping(ctx, tx, intent.DocumentID)
	if err != nil {
		return err
	}
	if _, err := prepareNotesyncAttempt(ctx, tx, message, intent, mapping); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE knowledge_notesync_publication_attempts
		SET status='superseded',error_category='superseded',error_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE outbox_id=$1`, message.ID); err != nil {
		return fmt.Errorf("mark notesync publication attempt superseded: %w", err)
	}
	return nil
}

func finalizeNotesyncApplied(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	work notesyncintegration.PublicationWork,
	outcome notesyncintegration.PublicationOutcome,
	now time.Time,
) error {
	if outcome.Remote.Missing || outcome.Remote.Markdown != work.TargetMarkdown || outcome.Remote.Version < 0 || outcome.Remote.LastTime < 0 {
		return errors.New("notesync applied outcome lacks exact verified remote target")
	}
	hash := sha256.Sum256([]byte(work.TargetMarkdown))
	command, err := tx.Exec(ctx, `
		INSERT INTO knowledge_notesync_publications(
		  document_id,remote_vault,remote_path,published_knowledge_revision_id,
		  published_document_revision_id,published_revision_no,base_markdown,base_sha256,
		  remote_version,remote_last_time,generation,status,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'active',$12,$12)
		ON CONFLICT(document_id) DO UPDATE SET
		  remote_vault=EXCLUDED.remote_vault,remote_path=EXCLUDED.remote_path,
		  published_knowledge_revision_id=EXCLUDED.published_knowledge_revision_id,
		  published_document_revision_id=EXCLUDED.published_document_revision_id,
		  published_revision_no=EXCLUDED.published_revision_no,base_markdown=EXCLUDED.base_markdown,
		  base_sha256=EXCLUDED.base_sha256,remote_version=EXCLUDED.remote_version,
		  remote_last_time=EXCLUDED.remote_last_time,generation=EXCLUDED.generation,
		  status='active',updated_at=EXCLUDED.updated_at,redacted_at=NULL
		WHERE knowledge_notesync_publications.generation < EXCLUDED.generation
		   OR (knowledge_notesync_publications.generation=EXCLUDED.generation
		       AND knowledge_notesync_publications.published_revision_no<=EXCLUDED.published_revision_no)`,
		work.DocumentID, work.RemoteVault, work.RemotePath, work.KnowledgeRevisionID,
		work.DocumentRevisionID, work.RevisionNo, work.TargetMarkdown, hash[:],
		outcome.Remote.Version, outcome.Remote.LastTime, work.Generation, now)
	if err != nil {
		return fmt.Errorf("advance notesync publication mapping: %w", err)
	}
	if command.RowsAffected() != 1 {
		return errors.New("notesync publication mapping refused to move backward")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE knowledge_notesync_publication_attempts
		SET status='applied',error_category=NULL,error_at=NULL,updated_at=$2
		WHERE outbox_id=$1`, message.ID, now); err != nil {
		return fmt.Errorf("mark notesync publication attempt applied: %w", err)
	}
	return outboxpostgresstore.MarkAppliedWith(ctx, tx, message.ID, message.LeaseToken, now)
}

func finalizeNotesyncFailed(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	outcome notesyncintegration.PublicationOutcome,
	now time.Time,
) error {
	if strings.TrimSpace(outcome.Category) == "" {
		return errors.New("notesync failed outcome requires a category")
	}
	status := "retryable"
	if outcome.Category == "notesync_publication_outcome_unknown" {
		status = "unknown"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE knowledge_notesync_publication_attempts
		SET status=$2,error_category=$3,error_at=$4,updated_at=$4
		WHERE outbox_id=$1`, message.ID, status, outcome.Category, now); err != nil {
		return fmt.Errorf("mark notesync publication attempt failed: %w", err)
	}
	return nil
}

func finalizeNotesyncDeferred(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	outcome notesyncintegration.PublicationOutcome,
	now time.Time,
) error {
	if strings.TrimSpace(outcome.Category) == "" || outcome.AvailableAt.IsZero() || !outcome.AvailableAt.After(now) {
		return errors.New("notesync deferred outcome requires a category and future availability")
	}
	status := "retryable"
	if outcome.Category == "notesync_publication_outcome_unknown" {
		status = "unknown"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE knowledge_notesync_publication_attempts
		SET status=$2,error_category=$3,error_at=$4,updated_at=$4
		WHERE outbox_id=$1`, message.ID, status, outcome.Category, now); err != nil {
		return fmt.Errorf("mark notesync publication attempt deferred: %w", err)
	}
	return outboxpostgresstore.MarkDeferredWith(ctx, tx, message.ID, message.LeaseToken, outcome.Category, now, outcome.AvailableAt)
}

func finalizeNotesyncReview(
	ctx context.Context,
	tx pgx.Tx,
	message outbox.Message,
	work notesyncintegration.PublicationWork,
	outcome notesyncintegration.PublicationOutcome,
	now time.Time,
) error {
	if !validNotesyncReview(outcome) {
		return errors.New("invalid notesync publication review outcome")
	}
	review := notesyncintegration.Review{
		ReviewID: uuid.NewString(), Category: outcome.ReviewKind, ReasonCode: outcome.ReasonCode,
		Status: notesyncintegration.ReviewStatusOpen, Generation: work.Generation,
		HeadRevisionID: work.KnowledgeRevisionID, HeadRevisionNo: work.RevisionNo,
		DocumentID: work.DocumentID, CanonicalPath: work.CanonicalPath,
		RemoteVault: work.RemoteVault, RemotePath: work.RemotePath,
		Local: notesyncintegration.ReviewSnapshot{
			KnowledgeRevisionID: work.KnowledgeRevisionID, KnowledgeRevisionNo: work.RevisionNo,
			DocumentRevisionID: work.DocumentRevisionID, SourceRevisionID: work.KnowledgeRevisionID,
			Path: work.CanonicalPath, Markdown: work.TargetMarkdown, SHA256: notesyncMarkdownHash(work.TargetMarkdown),
		},
		Remote:    notesyncintegration.ReviewSnapshot{Missing: outcome.Remote.Missing, Path: work.RemotePath},
		CreatedAt: now, UpdatedAt: now,
	}
	if work.Mapping == nil {
		review.Base = notesyncintegration.ReviewSnapshot{Missing: true}
	} else {
		review.Base = notesyncintegration.ReviewSnapshot{
			KnowledgeRevisionID: work.Mapping.KnowledgeRevisionID, KnowledgeRevisionNo: work.Mapping.RevisionNo,
			DocumentRevisionID: work.Mapping.DocumentRevisionID, SourceRevisionID: work.Mapping.KnowledgeRevisionID,
			Path: work.Mapping.RemotePath, Markdown: work.Mapping.BaseMarkdown, SHA256: notesyncMarkdownHash(work.Mapping.BaseMarkdown),
			RemoteVersion: work.Mapping.RemoteVersion, RemoteLastTime: work.Mapping.RemoteLastTime,
		}
	}
	if !review.Remote.Missing {
		review.Remote.Markdown = outcome.Remote.Markdown
		review.Remote.SHA256 = notesyncMarkdownHash(outcome.Remote.Markdown)
		review.Remote.RemoteVersion = outcome.Remote.Version
		review.Remote.RemoteLastTime = outcome.Remote.LastTime
		if inspected, err := knowledge.NewCanonicalizer().Inspect(review.Remote.Markdown); err == nil {
			review.RemoteDocumentID = inspected.ExplicitDocumentID
			review.Remote.SourceRevisionID = inspected.ExplicitSourceRevisionID
		}
	}
	review.Diff = notesyncintegration.BuildThreeWayDiff(review.Base, review.Local, review.Remote)
	review.BasisHash = notesyncintegration.ReviewBasisHash(review)
	basisHash, _ := hex.DecodeString(review.BasisHash)
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_notesync_reviews(
		  review_id,document_id,remote_document_id,remote_vault,remote_path,kind,reason_code,status,
		  head_knowledge_revision_id,head_knowledge_revision_no,canonical_path,
		  base_missing,base_knowledge_revision_id,base_knowledge_revision_no,base_document_revision_id,
		  base_remote_path,base_remote_version,base_remote_last_time,base_markdown,base_sha256,
		  local_missing,local_knowledge_revision_id,local_knowledge_revision_no,
		  local_document_revision_id,local_markdown,local_sha256,
		  remote_missing,remote_markdown,remote_sha256,remote_version,remote_last_time,remote_source_revision_id,
		  base_to_local_diff,base_to_remote_diff,local_diff_truncated,remote_diff_truncated,
		  basis_hash,generation,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'open',$8,$9,$10,
		       $11,$12,$13,$14,$15,$16,$17,$18,$19,FALSE,$20,$21,$22,$23,$24,
		       $25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$37)
		ON CONFLICT DO NOTHING`,
		review.ReviewID, review.DocumentID, nullableUUID(review.RemoteDocumentID), review.RemoteVault, review.RemotePath,
		review.Category, review.ReasonCode, review.HeadRevisionID, review.HeadRevisionNo, review.CanonicalPath,
		review.Base.Missing, nullableUUID(review.Base.KnowledgeRevisionID), nullablePositive(review.Base.KnowledgeRevisionNo),
		nullableUUID(review.Base.DocumentRevisionID), nullableBaseValue(review.Base, review.Base.Path),
		nullableRemoteValue(review.Base, review.Base.RemoteVersion), nullableRemoteValue(review.Base, review.Base.RemoteLastTime),
		nullableMarkdown(review.Base), nullableHash(review.Base), review.Local.KnowledgeRevisionID, review.Local.KnowledgeRevisionNo,
		review.Local.DocumentRevisionID, review.Local.Markdown, nullableHash(review.Local), review.Remote.Missing,
		nullableMarkdown(review.Remote), nullableHash(review.Remote), nullableRemoteValue(review.Remote, review.Remote.RemoteVersion),
		nullableRemoteValue(review.Remote, review.Remote.RemoteLastTime), nullableUUID(review.Remote.SourceRevisionID),
		review.Diff.BaseToLocal, review.Diff.BaseToRemote, review.Diff.LocalTruncated, review.Diff.RemoteTruncated,
		basisHash, review.Generation, now); err != nil {
		return fmt.Errorf("insert notesync publication review: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE knowledge_notesync_publication_attempts
		SET status='review_required',error_category=$2,error_at=$3,updated_at=$3
		WHERE outbox_id=$1`, message.ID, outcome.ReasonCode, now); err != nil {
		return fmt.Errorf("mark notesync publication attempt for review: %w", err)
	}
	return outboxpostgresstore.CancelWith(ctx, tx, outbox.CancelRequest{
		IdempotencyKey: message.IdempotencyKey, LeaseToken: message.LeaseToken,
		Disposition: outbox.DispositionReviewRequired, CanceledAt: now,
	})
}

func validNotesyncReview(outcome notesyncintegration.PublicationOutcome) bool {
	switch outcome.ReviewKind {
	case notesyncintegration.ReviewKindRemoteChanged:
		return !outcome.Remote.Missing && (outcome.ReasonCode == notesyncintegration.ReviewReasonRemoteContentChanged ||
			outcome.ReasonCode == notesyncintegration.ReviewReasonPreflightChanged || outcome.ReasonCode == notesyncintegration.ReviewReasonReadbackChanged)
	case notesyncintegration.ReviewKindRemoteMissing:
		return outcome.Remote.Missing && outcome.ReasonCode == notesyncintegration.ReviewReasonRemoteNoteMissing
	case notesyncintegration.ReviewKindPathOccupied:
		return outcome.ReasonCode == notesyncintegration.ReviewReasonRemotePathOccupied ||
			outcome.ReasonCode == notesyncintegration.ReviewReasonPreflightChanged || outcome.ReasonCode == notesyncintegration.ReviewReasonReadbackChanged
	default:
		return false
	}
}
