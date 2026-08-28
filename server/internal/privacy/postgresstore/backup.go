package postgresstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

type backupKeyStore struct{}

func newBackupKeyStore() privacy.BackupKeyDestroyer {
	return backupKeyStore{}
}

func (backupKeyStore) DestroyGenerationKeysTx(ctx context.Context, db privacy.DBTX, request privacy.BackupKeyDestroyRequest) (privacy.BackupKeyDestroyResult, error) {
	if db == nil {
		return privacy.BackupKeyDestroyResult{}, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "backup_key_database_required"}
	}
	if err := request.Validate(); err != nil {
		return privacy.BackupKeyDestroyResult{}, err
	}
	if _, err := db.Exec(ctx, `
		SELECT id
		FROM memory_generation_keys
		WHERE learner_generation<$1 AND destroyed_at IS NULL
		ORDER BY learner_generation,id
		FOR UPDATE`, request.LearnerGeneration); err != nil {
		return privacy.BackupKeyDestroyResult{}, fmt.Errorf("lock old backup generation keys: %w", err)
	}
	var keyMaterial string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(string_agg(
			id::text||':'||learner_generation::text||':'||encode(key_digest,'hex'),
			E'\n' ORDER BY learner_generation,id),'')
		FROM memory_generation_keys
		WHERE learner_generation<$1 AND destroyed_at IS NULL`, request.LearnerGeneration).Scan(&keyMaterial); err != nil {
		return privacy.BackupKeyDestroyResult{}, fmt.Errorf("summarize old backup generation keys: %w", err)
	}
	evidence := sha256.Sum256([]byte("managed-backup-key-destruction-v1\n" + request.ErasureID + "\n" + keyMaterial))
	tag, err := db.Exec(ctx, `
		UPDATE memory_generation_keys
		SET wrapped_key=NULL,destroyed_at=$2,destruction_evidence_digest=$3
		WHERE learner_generation<$1 AND destroyed_at IS NULL AND wrapped_key IS NOT NULL`,
		request.LearnerGeneration, request.At.UTC(), evidence[:])
	if err != nil {
		return privacy.BackupKeyDestroyResult{}, fmt.Errorf("destroy old backup generation keys: %w", err)
	}
	destroyedKeys := tag.RowsAffected()
	var residual int64
	if err := db.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_generation_keys WHERE learner_generation<$1 AND (destroyed_at IS NULL OR wrapped_key IS NOT NULL OR destruction_evidence_digest IS NULL))+
		  (SELECT count(*)
		   FROM memory_managed_backup_inventory inventory
		   JOIN memory_generation_keys keys ON keys.id=inventory.wrapped_key_id
		   WHERE inventory.learner_generation<$1 AND (keys.destroyed_at IS NULL OR keys.wrapped_key IS NOT NULL))`,
		request.LearnerGeneration).Scan(&residual); err != nil {
		return privacy.BackupKeyDestroyResult{}, fmt.Errorf("verify backup generation key destruction: %w", err)
	}
	if residual != 0 {
		return privacy.BackupKeyDestroyResult{}, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "backup_key_residual_present"}
	}
	var timestampMatches bool
	if err := db.QueryRow(ctx, `
		SELECT managed_backup_verified_unrecoverable_at=$2
		       AND managed_backup_scheduled_unrecoverable_after>=$2
		FROM privacy_erasures
		WHERE id=$1 AND target_learner_generation=$3`,
		request.ErasureID, request.At.UTC(), request.LearnerGeneration).Scan(&timestampMatches); err != nil {
		return privacy.BackupKeyDestroyResult{}, fmt.Errorf("verify backup unrecoverable time: %w", err)
	}
	if !timestampMatches {
		return privacy.BackupKeyDestroyResult{}, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "backup_unrecoverable_time_mismatch"}
	}
	return privacy.BackupKeyDestroyResult{
		DestroyedKeys: destroyedKeys, EvidenceDigest: hex.EncodeToString(evidence[:]), DestroyedAt: request.At.UTC(),
	}, nil
}

var _ privacy.BackupKeyDestroyer = backupKeyStore{}
