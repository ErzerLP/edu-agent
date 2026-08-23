package postgresstore

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

var _ privacy.LocalOwnerPort = (*Store)(nil)

func (s *Store) Owner() privacy.OwnerKind {
	return privacy.OwnerKnowledge
}

func (s *Store) CloseGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(false); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$2,read_open=FALSE,write_open=FALSE,active_erasure_id=$3,updated_at=$4
		WHERE owner_kind=$1 AND learner_generation=$5
		  AND read_open=TRUE AND write_open=TRUE AND active_erasure_id IS NULL`,
		privacy.OwnerKnowledge, transition.TargetGeneration, transition.ErasureID, transition.At.UTC(), transition.FromGeneration)
	if err != nil {
		return fmt.Errorf("close knowledge generation gate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "knowledge_generation_close_cas_failed"}
	}
	return nil
}

func (s *Store) OpenGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(true); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=$4
		WHERE owner_kind=$1 AND learner_generation=$2
		  AND read_open=FALSE AND write_open=FALSE AND active_erasure_id=$3`,
		privacy.OwnerKnowledge, transition.TargetGeneration, transition.ErasureID, transition.At.UTC())
	if err != nil {
		return fmt.Errorf("open knowledge generation gate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "knowledge_generation_open_cas_failed"}
	}
	return nil
}

func (s *Store) RedactTx(ctx context.Context, request privacy.LocalRedactionRequest) error {
	if err := request.Validate(privacy.OwnerKnowledge); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin knowledge privacy scrub: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var permit string
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,$3,$4)::text`,
		request.ErasureID, request.LearnerGeneration, privacy.OwnerKnowledge, request.ReceiptID).Scan(&permit); err != nil {
		return fmt.Errorf("authorize knowledge privacy scrub: %w", err)
	}

	switch request.Store {
	case privacy.StoreKnowledgeContent:
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_revisions
			SET source='privacy_erasure',redacted_at=clock_timestamp(),redacted_by_erasure_id=$1
			WHERE redacted_at IS NULL AND privacy_owner_scrub_permitted('knowledge')`, request.ErasureID); err != nil {
			return fmt.Errorf("mark knowledge revisions redacted: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_document_payloads
			SET canonical_markdown=''
			WHERE privacy_owner_scrub_permitted('knowledge')`); err != nil {
			return fmt.Errorf("redact knowledge canonical markdown: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_snapshot_documents
			SET canonical_path='erased/'||document_revision_id::text,
				folded_path='erased/'||document_revision_id::text
			WHERE privacy_owner_scrub_permitted('knowledge')`); err != nil {
			return fmt.Errorf("redact knowledge snapshot paths: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_lineages
			SET reason='privacy_erasure'
			WHERE privacy_owner_scrub_permitted('knowledge')`); err != nil {
			return fmt.Errorf("redact knowledge lineage reasons: %w", err)
		}
	case privacy.StoreKnowledgeIndex:
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_node_revisions
			SET title='',ancestor_titles='[]'::jsonb
			WHERE privacy_owner_scrub_permitted('knowledge')`); err != nil {
			return fmt.Errorf("redact knowledge node index: %w", err)
		}
	case privacy.StoreKnowledgeArtifacts:
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_node_artifacts
			SET content=''
			WHERE privacy_owner_scrub_permitted('knowledge')`); err != nil {
			return fmt.Errorf("redact knowledge artifacts: %w", err)
		}
	default:
		return &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "unsupported_knowledge_local_store"}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit knowledge privacy scrub: %w", err)
	}
	return nil
}

func (s *Store) VerifyRedacted(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	if err := request.Validate(privacy.OwnerKnowledge); err != nil {
		return 0, err
	}
	var residual int64
	var query string
	var arguments []any
	switch request.Store {
	case privacy.StoreKnowledgeContent:
		query = `
			SELECT
				(SELECT count(*) FROM knowledge_document_payloads WHERE canonical_markdown <> '')+
				(SELECT count(*) FROM knowledge_snapshot_documents WHERE canonical_path NOT LIKE 'erased/%')+
				(SELECT count(*) FROM knowledge_snapshot_documents WHERE folded_path NOT LIKE 'erased/%')+
				(SELECT count(*) FROM knowledge_revisions
				 WHERE source <> 'privacy_erasure' OR redacted_at IS NULL OR redacted_by_erasure_id <> $1)+
				(SELECT count(*) FROM knowledge_lineages WHERE reason <> 'privacy_erasure')`
		arguments = append(arguments, request.ErasureID)
	case privacy.StoreKnowledgeIndex:
		query = `
			SELECT
				(SELECT count(*) FROM knowledge_node_revisions WHERE title <> '')+
				(SELECT count(*) FROM knowledge_node_revisions WHERE ancestor_titles <> '[]'::jsonb)`
	case privacy.StoreKnowledgeArtifacts:
		query = `SELECT count(*) FROM knowledge_node_artifacts WHERE content <> ''`
	default:
		return 0, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "unsupported_knowledge_local_store"}
	}
	if err := s.pool.QueryRow(ctx, query, arguments...).Scan(&residual); err != nil {
		return 0, fmt.Errorf("verify knowledge privacy scrub: %w", err)
	}
	return residual, nil
}
