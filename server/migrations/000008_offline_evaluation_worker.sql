-- S4 extends the immutable operation-status projection with observable worker lifecycle states.
ALTER TABLE offline_operation_status_revisions
    DROP CONSTRAINT offline_operation_status_combination;

ALTER TABLE offline_operation_status_revisions
    ADD CONSTRAINT offline_operation_status_combination CHECK (
        (archive_status='archived_rejected' AND assessment_status='not_requested' AND evidence_status='unchanged')
        OR
        (archive_status='archived_succeeded' AND assessment_status='not_requested'
            AND evidence_status IN ('provisional','not_eligible','not_applicable'))
        OR
        (archive_status='archived_succeeded' AND assessment_status IN ('queued','processing','pending_retry')
            AND evidence_status='pending_evaluation')
        OR
        (archive_status='archived_succeeded' AND assessment_status='completed'
            AND evidence_status IN ('accepted','provisional','not_eligible'))
        OR
        (archive_status='archived_succeeded' AND assessment_status='failed'
            AND evidence_status='unchanged')
    );

ALTER TABLE offline_evaluation_jobs
    ADD COLUMN frozen_request_hash BYTEA CHECK (frozen_request_hash IS NULL OR octet_length(frozen_request_hash)=32),
    ADD COLUMN model_artifact JSONB,
    ADD COLUMN model_artifact_hash BYTEA CHECK (model_artifact_hash IS NULL OR octet_length(model_artifact_hash)=32),
    ADD COLUMN last_error_at TIMESTAMPTZ,
    ADD COLUMN result_assessment_id UUID REFERENCES learning_assessments(id),
    ADD COLUMN result_decision_id UUID REFERENCES learning_assessment_decisions(id),
    ADD COLUMN result_evidence_id UUID REFERENCES learning_evidence(id),
    ADD CONSTRAINT offline_evaluation_job_artifact_shape CHECK (
        (model_artifact IS NULL AND model_artifact_hash IS NULL)
        OR (model_artifact IS NOT NULL AND model_artifact_hash IS NOT NULL)
    );

-- Historical S2/S3 rows predate the explicit frozen-input digest. PostgreSQL JSONB
-- text is stable enough to seed an upgrade hash; the S4 worker accepts this legacy
-- representation once and writes canonical JCS hashes for every new job.
UPDATE offline_evaluation_jobs
SET frozen_request_hash=sha256(convert_to(frozen_request::text, 'UTF8'))
WHERE frozen_request_hash IS NULL;

ALTER TABLE offline_evaluation_jobs
    ALTER COLUMN frozen_request_hash SET NOT NULL;
