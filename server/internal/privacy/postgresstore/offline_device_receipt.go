package postgresstore

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OfflineDevicePurgeReceipt 只查询原设备原代次的正式回执，用于清除响应丢失后的核对。
func (s *Store) OfflineDevicePurgeReceipt(ctx context.Context, deviceID string, generation int64) (privacy.OfflineDeviceChildReceipt, bool, error) {
	var receipt privacy.OfflineDeviceChildReceipt
	if uuid.Validate(deviceID) != nil || generation < 1 {
		return receipt, false, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_offline_device_receipt_identity"}
	}
	err := s.pool.QueryRow(ctx, `SELECT c.erasure_id::text,c.device_id::text,c.source_generation,e.target_learner_generation,
	 r.challenge_revision,r.status,r.updated_at,r.stable_reason
	 FROM privacy_offline_device_children c JOIN privacy_erasures e ON e.id=c.erasure_id
	 JOIN privacy_offline_device_child_heads h ON h.child_id=c.id
	 JOIN privacy_offline_device_child_revisions r ON r.id=h.current_revision_id
	 WHERE c.device_id=$1 AND c.source_generation=$2 AND r.status='succeeded'
	 ORDER BY e.requested_at DESC,c.erasure_id DESC LIMIT 1`, deviceID, generation).Scan(
		&receipt.ErasureID, &receipt.DeviceID, &receipt.SourceGeneration, &receipt.CurrentGeneration, &receipt.ChallengeRevision, &receipt.Status, &receipt.UpdatedAt, &receipt.StableReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return receipt, false, nil
	}
	return receipt, err == nil, err
}
