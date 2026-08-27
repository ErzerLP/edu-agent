-- S5 extends offline-device privacy child receipts with stable, generation-bound challenge metadata
-- and authenticated acknowledgment timestamps while preserving the append-only revision/head model.
ALTER TABLE privacy_offline_device_child_revisions
    ADD COLUMN challenge_revision BIGINT,
    ADD COLUMN issued_at TIMESTAMPTZ,
    ADD COLUMN acknowledged_at TIMESTAMPTZ;

UPDATE privacy_offline_device_child_revisions
SET challenge_revision=revision,
    issued_at=updated_at;

ALTER TABLE privacy_offline_device_child_revisions
    ALTER COLUMN challenge_revision SET NOT NULL,
    ALTER COLUMN issued_at SET NOT NULL,
    ADD CONSTRAINT privacy_offline_device_child_challenge_revision_positive CHECK (challenge_revision >= 1),
    ADD CONSTRAINT privacy_offline_device_child_ack_shape CHECK (
        (status IN ('succeeded','failed') AND acknowledged_at IS NOT NULL)
        OR (status IN ('pending','unknown') AND acknowledged_at IS NULL)
    );

CREATE INDEX privacy_offline_device_children_device_erasure_idx
    ON privacy_offline_device_children(device_id,erasure_id);
