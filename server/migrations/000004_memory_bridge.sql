-- Converting the enum to text rewrites the status column. The explicit table lock makes the
-- migration's compatibility boundary visible and prevents a legacy writer from creating a
-- lease shape between cleanup and constraint validation.
LOCK TABLE outbox_messages IN ACCESS EXCLUSIVE MODE;

UPDATE outbox_messages
SET status='pending',available_at=LEAST(available_at,clock_timestamp()),
    lease_token=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE status='processing'
  AND (lease_token IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp());
UPDATE outbox_messages
SET lease_token=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE status <> 'processing' AND (lease_token IS NOT NULL OR lease_expires_at IS NOT NULL);

DROP INDEX outbox_claim_idx;
ALTER TABLE outbox_messages ALTER COLUMN status DROP DEFAULT;
ALTER TABLE outbox_messages ALTER COLUMN status TYPE TEXT USING status::text;
DROP TYPE outbox_status;
ALTER TABLE outbox_messages
    ALTER COLUMN status SET DEFAULT 'pending',
    ADD COLUMN terminal_disposition TEXT,
    ADD CONSTRAINT outbox_status_check
        CHECK (status IN ('pending','processing','applied','dead','canceled')),
    ADD CONSTRAINT outbox_terminal_disposition_check CHECK (
        (status = 'canceled' AND terminal_disposition IS NOT NULL AND terminal_disposition IN (
            'fenced','superseded','privacy_erasure','expired','permanently_rejected','deleted'
        ))
        OR (status <> 'canceled' AND terminal_disposition IS NULL)
    ),
    ADD CONSTRAINT outbox_lease_shape CHECK (
        (status = 'processing' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'processing' AND lease_token IS NULL AND lease_expires_at IS NULL)
    );
CREATE INDEX outbox_claim_idx ON outbox_messages(status,available_at,lease_expires_at,created_at)
    WHERE status IN ('pending','processing');

-- Existing first-party credentials gain ordinary memory and receipt-read scopes only while
-- both the token and its owning device remain active. Erasure authority is always separate.
UPDATE device_tokens AS token
SET scopes = ARRAY(
    SELECT DISTINCT scope
    FROM unnest(token.scopes || ARRAY['memory:read','memory:write','privacy:read']::TEXT[]) AS scope
    ORDER BY scope
)
WHERE token.revoked_at IS NULL
  AND EXISTS (
      SELECT 1 FROM devices AS device
      WHERE device.id=token.device_id AND device.revoked_at IS NULL
  );

CREATE TABLE memory_candidates (
    id UUID PRIMARY KEY,
    candidate_uri TEXT NOT NULL UNIQUE,
    logical_memory_id UUID,
    payload_id UUID NOT NULL UNIQUE,
    content_hash BYTEA NOT NULL CHECK (octet_length(content_hash) = 32),
    source_kind TEXT NOT NULL CHECK (source_kind IN (
        'user_statement','model_inference','long_term_background','generated_summary'
    )),
    source_event_id UUID,
    source_operation_id UUID,
    source_model_id TEXT CHECK (source_model_id IS NULL OR char_length(source_model_id) BETWEEN 1 AND 500),
    source_prompt_revision TEXT CHECK (source_prompt_revision IS NULL OR char_length(source_prompt_revision) BETWEEN 1 AND 500),
    source_hashes BYTEA[] NOT NULL DEFAULT '{}',
    proposer_id UUID NOT NULL,
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    category TEXT NOT NULL CHECK (category IN (
        'interaction_preference','time_constraint','personal_context','generated_summary',
        'raw_chat','complete_attempt','question_or_rubric','goal','route','mastery','evidence',
        'misconception','review_queue','sync_state','device_token','model_secret','nocturne_secret'
    )),
    sensitivity TEXT NOT NULL CHECK (sensitivity IN ('non_sensitive','sensitive')),
    stability TEXT NOT NULL CHECK (stability IN ('transient','stable')),
    valid_until TIMESTAMPTZ NOT NULL,
    admission_policy_version TEXT NOT NULL CHECK (admission_policy_version = 'memory-admission-v1'),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_candidate_uri_shape CHECK (candidate_uri = 'candidate://' || id::text),
    CONSTRAINT memory_candidate_expiry_shape CHECK (valid_until > created_at),
    CONSTRAINT memory_candidate_model_provenance CHECK (
        source_kind NOT IN ('model_inference','generated_summary')
        OR (source_model_id IS NOT NULL AND source_prompt_revision IS NOT NULL AND cardinality(source_hashes) > 0)
    )
);
CREATE INDEX memory_candidates_expiry_idx ON memory_candidates(valid_until,id);
CREATE INDEX memory_candidates_query_idx ON memory_candidates(created_at DESC,id DESC);

CREATE TABLE memory_candidate_decisions (
    id UUID PRIMARY KEY,
    candidate_id UUID NOT NULL REFERENCES memory_candidates(id),
    revision BIGINT NOT NULL CHECK (revision >= 2),
    decision TEXT NOT NULL CHECK (decision IN ('admit','reject','expire')),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    actor_kind TEXT NOT NULL CHECK (actor_kind IN ('device','model','system')),
    actor_device_id UUID REFERENCES devices(id),
    operation_id UUID,
    request_hash BYTEA CHECK (request_hash IS NULL OR octet_length(request_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_candidate_decision_revision UNIQUE(candidate_id,revision),
    CONSTRAINT memory_candidate_decision_identity UNIQUE(id,candidate_id,revision),
    CONSTRAINT memory_candidate_decision_actor CHECK (
        (actor_kind = 'system' AND actor_device_id IS NULL)
        OR (actor_kind <> 'system' AND actor_device_id IS NOT NULL)
    ),
    CONSTRAINT memory_candidate_decision_operation CHECK (
        (operation_id IS NULL AND request_hash IS NULL)
        OR (operation_id IS NOT NULL AND request_hash IS NOT NULL)
    )
);
CREATE TABLE memory_candidate_heads (
    candidate_id UUID PRIMARY KEY REFERENCES memory_candidates(id),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    status TEXT NOT NULL CHECK (status IN ('pending_review','admitted','rejected','expired')),
    current_decision_id UUID,
    payload_available BOOLEAN NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_candidate_head_decision
        FOREIGN KEY(current_decision_id,candidate_id,revision)
        REFERENCES memory_candidate_decisions(id,candidate_id,revision)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_candidate_head_shape CHECK (
        (status = 'pending_review' AND revision = 1 AND current_decision_id IS NULL AND payload_available)
        OR (status <> 'pending_review' AND revision >= 2 AND current_decision_id IS NOT NULL AND NOT payload_available)
    )
);
CREATE INDEX memory_candidate_heads_pending_idx ON memory_candidate_heads(candidate_id)
    WHERE status = 'pending_review';
CREATE TABLE memory_candidate_payloads (
    id UUID PRIMARY KEY,
    candidate_id UUID NOT NULL UNIQUE REFERENCES memory_candidates(id),
    content TEXT NOT NULL,
    content_hash BYTEA NOT NULL CHECK (octet_length(content_hash) = 32),
    valid_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_candidate_payload_identity UNIQUE(id,content_hash),
    CONSTRAINT memory_candidate_payload_expiry CHECK (valid_until > created_at)
);

CREATE TABLE memory_logical_memories (
    id UUID PRIMARY KEY,
    created_from_candidate_id UUID NOT NULL UNIQUE REFERENCES memory_candidates(id),
    created_at TIMESTAMPTZ NOT NULL
);
ALTER TABLE memory_candidates
    ADD CONSTRAINT memory_candidate_logical_memory_fk
    FOREIGN KEY(logical_memory_id) REFERENCES memory_logical_memories(id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE memory_record_revisions (
    id UUID PRIMARY KEY,
    logical_memory_id UUID NOT NULL REFERENCES memory_logical_memories(id),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    record_generation BIGINT NOT NULL CHECK (record_generation >= 1),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    candidate_id UUID NOT NULL UNIQUE REFERENCES memory_candidates(id),
    previous_revision_id UUID,
    external_uri TEXT NOT NULL,
    external_uri_digest BYTEA NOT NULL CHECK (octet_length(external_uri_digest) = 32),
    content_hash BYTEA NOT NULL CHECK (octet_length(content_hash) = 32),
    delivery_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_record_revision_number UNIQUE(logical_memory_id,revision),
    CONSTRAINT memory_record_revision_generation UNIQUE(logical_memory_id,record_generation),
    CONSTRAINT memory_record_revision_head_identity
        UNIQUE(id,logical_memory_id,revision,record_generation),
    CONSTRAINT memory_record_revision_current_identity
        UNIQUE(id,logical_memory_id,revision),
    CONSTRAINT memory_record_revision_delivery_identity
        UNIQUE(id,logical_memory_id,revision,learner_generation),
    CONSTRAINT memory_record_revision_identity
        UNIQUE(id,logical_memory_id,revision,record_generation,learner_generation),
    CONSTRAINT memory_record_revision_owner_identity UNIQUE(id,logical_memory_id),
    CONSTRAINT memory_record_previous_once UNIQUE(previous_revision_id),
    CONSTRAINT memory_record_previous_owner
        FOREIGN KEY(previous_revision_id,logical_memory_id)
        REFERENCES memory_record_revisions(id,logical_memory_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_record_lineage_shape CHECK (
        (revision = 1 AND previous_revision_id IS NULL)
        OR (revision > 1 AND previous_revision_id IS NOT NULL)
    )
);
CREATE INDEX memory_record_revisions_query_idx
    ON memory_record_revisions(created_at DESC,id DESC);

CREATE TABLE memory_record_heads (
    logical_memory_id UUID PRIMARY KEY REFERENCES memory_logical_memories(id),
    current_record_revision_id UUID NOT NULL UNIQUE,
    current_revision BIGINT NOT NULL CHECK (current_revision >= 1),
    record_generation BIGINT NOT NULL CHECK (record_generation >= 1),
    status TEXT NOT NULL CHECK (status IN (
        'queued','applied','permanently_rejected','superseded','delete_pending','deleted'
    )),
    current_delivery_id UUID NOT NULL,
    receipt_id UUID NOT NULL,
    external_node_id UUID,
    external_memory_id BIGINT CHECK (external_memory_id IS NULL OR external_memory_id > 0),
    applied_at TIMESTAMPTZ,
    superseded_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_record_head_revision_owner
        FOREIGN KEY(current_record_revision_id,logical_memory_id,current_revision)
        REFERENCES memory_record_revisions(id,logical_memory_id,revision),
    CONSTRAINT memory_record_head_terminal_time CHECK (
        (status <> 'applied' OR applied_at IS NOT NULL)
        AND (status <> 'superseded' OR superseded_at IS NOT NULL)
        AND (status <> 'deleted' OR deleted_at IS NOT NULL)
    )
);

CREATE TABLE memory_deliveries (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('admit','correction','delete','erasure')),
    logical_memory_id UUID NOT NULL REFERENCES memory_logical_memories(id),
    record_revision_id UUID NOT NULL REFERENCES memory_record_revisions(id),
    record_revision BIGINT NOT NULL CHECK (record_revision >= 1),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    record_generation BIGINT NOT NULL CHECK (record_generation >= 1),
    payload_id UUID NOT NULL UNIQUE,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    external_uri TEXT NOT NULL,
    outbox_id UUID NOT NULL UNIQUE REFERENCES outbox_messages(id) DEFERRABLE INITIALLY DEFERRED,
    outbox_idempotency_key TEXT NOT NULL UNIQUE,
    valid_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_delivery_record_unique UNIQUE(record_revision_id,kind),
    CONSTRAINT memory_delivery_record_owner
        FOREIGN KEY(record_revision_id,logical_memory_id,record_revision,learner_generation)
        REFERENCES memory_record_revisions(id,logical_memory_id,revision,learner_generation)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_delivery_owner_identity UNIQUE(id,logical_memory_id),
    CONSTRAINT memory_delivery_record_identity
        UNIQUE(id,logical_memory_id,record_revision_id,record_revision,record_generation),
    CONSTRAINT memory_delivery_expiry_identity
        UNIQUE(id,logical_memory_id,external_uri,payload_hash,learner_generation,record_generation),
    CONSTRAINT memory_delivery_expiry CHECK (valid_until > created_at)
);
ALTER TABLE memory_record_revisions
    ADD CONSTRAINT memory_record_delivery_owner
        FOREIGN KEY(delivery_id,logical_memory_id,id,revision,record_generation)
        REFERENCES memory_deliveries(id,logical_memory_id,record_revision_id,record_revision,record_generation)
        DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE memory_delivery_payloads (
    id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL UNIQUE REFERENCES memory_deliveries(id),
    content TEXT NOT NULL,
    content_hash BYTEA NOT NULL CHECK (octet_length(content_hash) = 32),
    valid_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_delivery_payload_identity UNIQUE(id,content_hash),
    CONSTRAINT memory_delivery_payload_expiry CHECK (valid_until > created_at)
);

CREATE TABLE memory_delivery_receipts (
    id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL REFERENCES memory_deliveries(id),
    version BIGINT NOT NULL CHECK (version >= 1),
    status TEXT NOT NULL CHECK (status IN (
        'pending','succeeded','partial','failed','unknown','not_applicable','unsupported'
    )),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    verification_method TEXT NOT NULL CHECK (char_length(verification_method) BETWEEN 1 AND 500),
    evidence_digest BYTEA CHECK (evidence_digest IS NULL OR octet_length(evidence_digest) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_delivery_receipt_version UNIQUE(delivery_id,version),
    CONSTRAINT memory_delivery_receipt_owner UNIQUE(id,delivery_id),
    CONSTRAINT memory_delivery_receipt_identity UNIQUE(id,delivery_id,version)
);
CREATE TABLE memory_delivery_heads (
    delivery_id UUID PRIMARY KEY,
    logical_memory_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'queued','applied','permanently_rejected','fenced','expiry_reconciling','expired','delete_pending','deleted'
    )),
    public_status TEXT NOT NULL CHECK (public_status IN ('queued','applied','rejected')),
    terminal_disposition TEXT CHECK (terminal_disposition IS NULL OR terminal_disposition IN (
        'fenced','superseded','privacy_erasure','expired','permanently_rejected','deleted'
    )),
    attempt_state TEXT NOT NULL CHECK (attempt_state IN (
        'prepared','sent','unknown','reconciling','confirmed','fenced','failed'
    )),
    current_attempt_id UUID,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error_category TEXT,
    current_receipt_id UUID NOT NULL,
    current_receipt_version BIGINT NOT NULL CHECK (current_receipt_version >= 1),
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_delivery_head_receipt_owner
        FOREIGN KEY(current_receipt_id,delivery_id,current_receipt_version)
        REFERENCES memory_delivery_receipts(id,delivery_id,version)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_delivery_head_owner FOREIGN KEY(delivery_id,logical_memory_id)
        REFERENCES memory_deliveries(id,logical_memory_id),
    CONSTRAINT memory_delivery_public_status_shape CHECK (
        (status = 'applied' AND public_status = 'applied' AND terminal_disposition IS NULL)
        OR (status IN ('permanently_rejected','fenced','expired','deleted')
            AND public_status = 'rejected' AND terminal_disposition IS NOT NULL)
        OR (status IN ('queued','expiry_reconciling','delete_pending')
            AND public_status = 'queued' AND terminal_disposition IS NULL)
    )
);
CREATE UNIQUE INDEX memory_delivery_single_active
    ON memory_delivery_heads(logical_memory_id)
    WHERE status IN ('queued','expiry_reconciling','delete_pending');

ALTER TABLE memory_record_heads
    ADD CONSTRAINT memory_record_head_delivery_fk
        FOREIGN KEY(current_delivery_id,logical_memory_id,current_record_revision_id,current_revision,record_generation)
        REFERENCES memory_deliveries(id,logical_memory_id,record_revision_id,record_revision,record_generation)
        DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT memory_record_head_receipt_fk FOREIGN KEY(receipt_id,current_delivery_id)
        REFERENCES memory_delivery_receipts(id,delivery_id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE memory_delivery_attempts (
    id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL REFERENCES memory_deliveries(id),
    attempt_token UUID NOT NULL UNIQUE,
    authorized_by_attempt_id UUID,
    authorization_boot_epoch TEXT,
    authorization_evidence_digest BYTEA CHECK (
        authorization_evidence_digest IS NULL OR octet_length(authorization_evidence_digest) = 32
    ),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_delivery_attempt_identity UNIQUE(id,delivery_id),
    CONSTRAINT memory_delivery_attempt_token_identity UNIQUE(delivery_id,attempt_token),
    CONSTRAINT memory_delivery_attempt_authorization_owner
        FOREIGN KEY(authorized_by_attempt_id,delivery_id)
        REFERENCES memory_delivery_attempts(id,delivery_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_delivery_attempt_authorization_shape CHECK (
        (authorized_by_attempt_id IS NULL AND authorization_boot_epoch IS NULL AND authorization_evidence_digest IS NULL)
        OR (authorized_by_attempt_id IS NOT NULL AND authorization_boot_epoch IS NOT NULL AND authorization_evidence_digest IS NOT NULL)
    )
);
CREATE TABLE memory_delivery_attempt_heads (
    attempt_id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'prepared','sent','unknown','reconciling','confirmed','fenced','failed'
    )),
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    boot_epoch TEXT,
    sent_at TIMESTAMPTZ,
    unknown_at TIMESTAMPTZ,
    restart_boot_epoch TEXT,
    absence_observations INTEGER NOT NULL DEFAULT 0 CHECK (absence_observations >= 0),
    restart_verified_at TIMESTAMPTZ,
    restart_evidence_digest BYTEA CHECK (restart_evidence_digest IS NULL OR octet_length(restart_evidence_digest) = 32),
    result_digest BYTEA CHECK (result_digest IS NULL OR octet_length(result_digest) = 32),
    error_category TEXT,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_delivery_attempt_head_owner
        FOREIGN KEY(attempt_id,delivery_id) REFERENCES memory_delivery_attempts(id,delivery_id),
    CONSTRAINT memory_delivery_attempt_lease_shape CHECK (
        (state IN ('prepared','sent','unknown','reconciling')
            AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state IN ('confirmed','fenced','failed')
            AND lease_token IS NULL AND lease_expires_at IS NULL)
    ),
    CONSTRAINT memory_delivery_attempt_send_shape CHECK (
        (state = 'prepared' AND sent_at IS NULL AND boot_epoch IS NULL)
        OR (state IN ('sent','unknown','reconciling','confirmed') AND sent_at IS NOT NULL AND boot_epoch IS NOT NULL)
        OR (state = 'fenced' AND (
            (sent_at IS NULL AND boot_epoch IS NULL) OR (sent_at IS NOT NULL AND boot_epoch IS NOT NULL)
        ))
        OR (state = 'failed' AND sent_at IS NULL AND boot_epoch IS NULL)
    ),
    CONSTRAINT memory_delivery_attempt_sent_not_failed CHECK (sent_at IS NULL OR state <> 'failed'),
    CONSTRAINT memory_delivery_attempt_restart_authorization CHECK (
        (restart_verified_at IS NULL AND restart_boot_epoch IS NULL
            AND absence_observations = 0 AND restart_evidence_digest IS NULL)
        OR (state = 'fenced' AND sent_at IS NOT NULL AND restart_verified_at IS NOT NULL
            AND restart_boot_epoch IS NOT NULL AND absence_observations >= 2
            AND restart_evidence_digest IS NOT NULL AND restart_boot_epoch <> boot_epoch)
    )
);
CREATE UNIQUE INDEX memory_delivery_attempt_single_active
    ON memory_delivery_attempt_heads(delivery_id)
    WHERE state IN ('prepared','sent','unknown','reconciling');
ALTER TABLE memory_delivery_heads
    ADD CONSTRAINT memory_delivery_head_attempt_fk
    FOREIGN KEY(current_attempt_id,delivery_id)
    REFERENCES memory_delivery_attempts(id,delivery_id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE memory_expiry_reconciliations (
    id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL,
    logical_memory_id UUID NOT NULL REFERENCES memory_logical_memories(id),
    external_uri TEXT NOT NULL,
    content_hash BYTEA NOT NULL CHECK (octet_length(content_hash) = 32),
    attempt_token UUID NOT NULL,
    sent_boot_epoch TEXT NOT NULL,
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    record_generation BIGINT NOT NULL CHECK (record_generation >= 1),
    status TEXT NOT NULL CHECK (status IN (
        'pending','reconciling','delete_pending','absence_verified','verified','conflict'
    )),
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    reason TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_expiry_reconciliation_attempt UNIQUE(delivery_id,attempt_token),
    CONSTRAINT memory_expiry_reconciliation_delivery_fk
        FOREIGN KEY(delivery_id,logical_memory_id,external_uri,content_hash,learner_generation,record_generation)
        REFERENCES memory_deliveries(id,logical_memory_id,external_uri,payload_hash,learner_generation,record_generation)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_expiry_reconciliation_attempt_fk
        FOREIGN KEY(delivery_id,attempt_token)
        REFERENCES memory_delivery_attempts(delivery_id,attempt_token)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_expiry_reconciliation_lease CHECK (
        (status IN ('reconciling','delete_pending') AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status NOT IN ('reconciling','delete_pending') AND lease_token IS NULL AND lease_expires_at IS NULL)
    )
);
CREATE UNIQUE INDEX memory_expiry_reconciliation_single_claim
    ON memory_expiry_reconciliations(delivery_id)
    WHERE status IN ('reconciling','delete_pending');

CREATE TABLE memory_remote_delete_plans (
    id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL UNIQUE REFERENCES memory_deliveries(id),
    node_uuid UUID NOT NULL,
    external_uri TEXT NOT NULL,
    active_memory_id BIGINT NOT NULL CHECK (active_memory_id > 0),
    review_cleanup_needed BOOLEAN NOT NULL,
    snapshot_digest BYTEA NOT NULL CHECK (octet_length(snapshot_digest) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_remote_delete_plan_identity UNIQUE(id,delivery_id)
);
CREATE TABLE memory_remote_delete_versions (
    plan_id UUID NOT NULL REFERENCES memory_remote_delete_plans(id),
    memory_id BIGINT NOT NULL CHECK (memory_id > 0),
    was_active BOOLEAN NOT NULL,
    PRIMARY KEY(plan_id,memory_id)
);
CREATE UNIQUE INDEX memory_remote_delete_single_active_version
    ON memory_remote_delete_versions(plan_id) WHERE was_active;
CREATE TABLE memory_remote_delete_paths (
    plan_id UUID NOT NULL REFERENCES memory_remote_delete_plans(id),
    namespace TEXT NOT NULL CHECK (char_length(namespace) BETWEEN 1 AND 200),
    domain TEXT NOT NULL CHECK (char_length(domain) BETWEEN 1 AND 200),
    path TEXT NOT NULL CHECK (char_length(path) BETWEEN 1 AND 2000),
    uri TEXT NOT NULL CHECK (char_length(uri) BETWEEN 1 AND 2200),
    is_alias BOOLEAN NOT NULL,
    PRIMARY KEY(plan_id,namespace,domain,path)
);

CREATE TABLE memory_record_tombstones (
    id UUID PRIMARY KEY,
    logical_memory_id UUID NOT NULL REFERENCES memory_logical_memories(id),
    record_revision_id UUID NOT NULL,
    record_revision BIGINT NOT NULL CHECK (record_revision >= 1),
    previous_record_generation BIGINT NOT NULL CHECK (previous_record_generation >= 1),
    tombstone_record_generation BIGINT NOT NULL CHECK (tombstone_record_generation > previous_record_generation),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    delivery_id UUID NOT NULL UNIQUE REFERENCES memory_deliveries(id) DEFERRABLE INITIALLY DEFERRED,
    operation_device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    content_hash BYTEA NOT NULL CHECK (octet_length(content_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_record_tombstone_once UNIQUE(logical_memory_id),
    CONSTRAINT memory_record_tombstone_revision_owner
        FOREIGN KEY(record_revision_id,logical_memory_id,record_revision)
        REFERENCES memory_record_revisions(id,logical_memory_id,revision),
    CONSTRAINT memory_record_tombstone_operation UNIQUE(operation_device_id,operation_id)
);

CREATE TABLE memory_operation_inbox (
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    operation_kind TEXT NOT NULL CHECK (operation_kind IN (
        'create_candidate','candidate_decision','record_delete','delivery_replay','privacy_erasure'
    )),
    terminal_status TEXT NOT NULL CHECK (terminal_status IN ('succeeded','rejected')),
    candidate_id UUID REFERENCES memory_candidates(id),
    logical_memory_id UUID REFERENCES memory_logical_memories(id),
    record_revision_id UUID REFERENCES memory_record_revisions(id),
    delivery_id UUID REFERENCES memory_deliveries(id),
    completed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(device_id,operation_id),
    CONSTRAINT memory_operation_result_shape CHECK (candidate_id IS NOT NULL OR logical_memory_id IS NOT NULL)
);

CREATE TABLE privacy_erasures (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    reason_code TEXT NOT NULL CHECK (reason_code IN ('learner_request','account_closure','operator_request')),
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    requested_at TIMESTAMPTZ NOT NULL,
    target_learner_generation BIGINT NOT NULL CHECK (target_learner_generation >= 2),
    managed_backup_scheduled_unrecoverable_after TIMESTAMPTZ NOT NULL,
    managed_backup_verified_unrecoverable_at TIMESTAMPTZ,
    CONSTRAINT privacy_erasure_operation UNIQUE(device_id,operation_id),
    CONSTRAINT privacy_erasure_target_identity UNIQUE(id,target_learner_generation),
    CONSTRAINT privacy_erasure_backup_deadline CHECK (
        managed_backup_scheduled_unrecoverable_after <= requested_at + interval '30 days'
        AND (managed_backup_verified_unrecoverable_at IS NULL
            OR managed_backup_verified_unrecoverable_at <= managed_backup_scheduled_unrecoverable_after)
    )
);
CREATE TABLE privacy_erasure_heads (
    erasure_id UUID PRIMARY KEY REFERENCES privacy_erasures(id),
    status TEXT NOT NULL CHECK (status IN (
        'barrier_committed','local_scrubbed','remote_draining','remote_purged','verified','partial','blocked'
    )),
    summary_version BIGINT NOT NULL CHECK (summary_version >= 1),
    stable_reason TEXT NOT NULL CHECK (char_length(stable_reason) BETWEEN 1 AND 1000),
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX privacy_erasure_single_active
    ON privacy_erasure_heads((TRUE)) WHERE status <> 'verified';

CREATE TABLE privacy_erasure_step_receipts (
    id UUID PRIMARY KEY,
    erasure_id UUID NOT NULL REFERENCES privacy_erasures(id),
    store_kind TEXT NOT NULL CHECK (store_kind IN (
        'identity_metadata','knowledge_content','knowledge_index','knowledge_artifacts',
        'learning_event_payload','learning_typed_payload','tutoring_payload','inbox_outbox',
        'projection_generations','memory_candidate_delivery','process_cache','nocturne_paths',
        'nocturne_orphan_history','nocturne_snapshot_changeset','managed_backup','external_provider'
    )),
    version BIGINT NOT NULL CHECK (version >= 1),
    scope_digest BYTEA NOT NULL CHECK (octet_length(scope_digest) = 32),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN (
        'pending','succeeded','partial','failed','unknown','not_applicable','unsupported'
    )),
    stable_reason TEXT NOT NULL CHECK (char_length(stable_reason) BETWEEN 1 AND 1000),
    verification_method TEXT NOT NULL CHECK (char_length(verification_method) BETWEEN 1 AND 500),
    evidence_digest BYTEA CHECK (evidence_digest IS NULL OR octet_length(evidence_digest) = 32),
    CONSTRAINT privacy_erasure_step_version UNIQUE(erasure_id,store_kind,version),
    CONSTRAINT privacy_erasure_step_owner_identity UNIQUE(id,erasure_id,store_kind,version),
    CONSTRAINT privacy_erasure_step_erasure_identity UNIQUE(id,erasure_id),
    CONSTRAINT privacy_erasure_step_completion CHECK (
        (status = 'pending' AND completed_at IS NULL)
        OR (status <> 'pending' AND completed_at IS NOT NULL)
    )
);
CREATE TABLE privacy_erasure_receipt_heads (
    erasure_id UUID NOT NULL REFERENCES privacy_erasures(id),
    store_kind TEXT NOT NULL,
    current_receipt_id UUID NOT NULL UNIQUE,
    current_version BIGINT NOT NULL CHECK (current_version >= 1),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(erasure_id,store_kind),
    CONSTRAINT privacy_erasure_receipt_head_owner
        FOREIGN KEY(current_receipt_id,erasure_id,store_kind,current_version)
        REFERENCES privacy_erasure_step_receipts(id,erasure_id,store_kind,version)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE memory_reconciliation_maintenance_claims (
    reconciliation_id UUID PRIMARY KEY REFERENCES memory_expiry_reconciliations(id),
    erasure_id UUID NOT NULL,
    target_learner_generation BIGINT NOT NULL CHECK (target_learner_generation >= 2),
    receipt_id UUID NOT NULL,
    claimed_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_reconciliation_maintenance_erasure
        FOREIGN KEY(erasure_id,target_learner_generation)
        REFERENCES privacy_erasures(id,target_learner_generation),
    CONSTRAINT memory_reconciliation_maintenance_receipt
        FOREIGN KEY(receipt_id,erasure_id)
        REFERENCES privacy_erasure_step_receipts(id,erasure_id)
);

CREATE TABLE privacy_erasure_grants (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id),
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts > 0),
    created_by TEXT NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 200),
    CONSTRAINT privacy_erasure_grant_expiry CHECK (expires_at > created_at),
    CONSTRAINT privacy_erasure_grant_attempt_budget CHECK (attempts <= max_attempts),
    CONSTRAINT privacy_erasure_grant_consumed CHECK (
        consumed_at IS NULL OR (consumed_at >= created_at AND consumed_at <= expires_at)
    )
);
CREATE INDEX privacy_erasure_grants_active_idx ON privacy_erasure_grants(device_id,expires_at)
    WHERE consumed_at IS NULL;
CREATE FUNCTION protect_privacy_erasure_grant_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.device_id,NEW.token_hash,NEW.created_at,NEW.expires_at,NEW.max_attempts,NEW.created_by)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.device_id,OLD.token_hash,OLD.created_at,OLD.expires_at,OLD.max_attempts,OLD.created_by) THEN
        RAISE EXCEPTION 'privacy erasure grant identity is immutable';
    END IF;
    IF OLD.consumed_at IS NOT NULL AND NEW.consumed_at IS DISTINCT FROM OLD.consumed_at THEN
        RAISE EXCEPTION 'privacy erasure grant consumption is irreversible';
    END IF;
    IF NEW.attempts < OLD.attempts THEN
        RAISE EXCEPTION 'privacy erasure grant attempts cannot decrease';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER privacy_erasure_grant_state_guard
BEFORE UPDATE ON privacy_erasure_grants
FOR EACH ROW EXECUTE FUNCTION protect_privacy_erasure_grant_state();

CREATE TABLE privacy_redaction_barriers (
    erasure_id UUID PRIMARY KEY REFERENCES privacy_erasures(id),
    learner_generation BIGINT NOT NULL UNIQUE CHECK (learner_generation >= 2),
    redacted_through_event_seq BIGINT NOT NULL CHECK (redacted_through_event_seq >= 0),
    policy_version TEXT NOT NULL CHECK (char_length(policy_version) BETWEEN 1 AND 200),
    reason_code TEXT NOT NULL CHECK (reason_code IN ('learner_request','account_closure','operator_request')),
    event_id UUID NOT NULL UNIQUE,
    committed_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE privacy_owner_generation_gates (
    owner_kind TEXT PRIMARY KEY CHECK (owner_kind IN (
        'identity','knowledge','learning','tutoring','memory','outbox'
    )),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    read_open BOOLEAN NOT NULL,
    write_open BOOLEAN NOT NULL,
    active_erasure_id UUID REFERENCES privacy_erasures(id) DEFERRABLE INITIALLY DEFERRED,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT privacy_owner_gate_erasure_shape CHECK (
        (read_open AND write_open AND active_erasure_id IS NULL)
        OR (NOT read_open AND active_erasure_id IS NOT NULL)
        OR (read_open AND NOT write_open AND active_erasure_id IS NOT NULL)
    )
);
INSERT INTO privacy_owner_generation_gates(owner_kind,learner_generation,read_open,write_open,updated_at)
SELECT owner_kind,1,TRUE,TRUE,now()
FROM unnest(ARRAY['identity','knowledge','learning','tutoring','memory','outbox']) AS owner_kind;

CREATE TABLE privacy_owner_redaction_audit (
    id UUID PRIMARY KEY,
    erasure_id UUID NOT NULL REFERENCES privacy_erasures(id),
    owner_kind TEXT NOT NULL,
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 2),
    receipt_id UUID REFERENCES privacy_erasure_step_receipts(id),
    action TEXT NOT NULL CHECK (action IN ('gate_closed','scrubbed','verified','gate_opened')),
    evidence_digest BYTEA CHECK (evidence_digest IS NULL OR octet_length(evidence_digest) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT privacy_owner_redaction_action UNIQUE(erasure_id,owner_kind,action)
);

CREATE TABLE privacy_owner_scrub_permits (
    permit_token UUID PRIMARY KEY,
    erasure_id UUID NOT NULL,
    owner_kind TEXT NOT NULL,
    target_learner_generation BIGINT NOT NULL CHECK (target_learner_generation >= 2),
    receipt_id UUID NOT NULL,
    backend_pid INTEGER NOT NULL CHECK (backend_pid > 0),
    transaction_id BIGINT NOT NULL CHECK (transaction_id > 0),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT privacy_owner_scrub_permit_erasure
        FOREIGN KEY(erasure_id,target_learner_generation)
        REFERENCES privacy_erasures(id,target_learner_generation),
    CONSTRAINT privacy_owner_scrub_permit_owner
        FOREIGN KEY(owner_kind) REFERENCES privacy_owner_generation_gates(owner_kind),
    CONSTRAINT privacy_owner_scrub_permit_receipt
        FOREIGN KEY(receipt_id,erasure_id)
        REFERENCES privacy_erasure_step_receipts(id,erasure_id),
    CONSTRAINT privacy_owner_scrub_permit_transaction
        UNIQUE(erasure_id,owner_kind,receipt_id,backend_pid,transaction_id)
);

CREATE FUNCTION privacy_scrub_receipt_matches_owner(owner_kind TEXT,store_kind TEXT)
RETURNS BOOLEAN LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE owner_kind
        WHEN 'memory' THEN store_kind IN ('memory_candidate_delivery','nocturne_paths','nocturne_orphan_history')
        WHEN 'learning' THEN store_kind IN ('learning_event_payload','learning_typed_payload','tutoring_payload')
        WHEN 'outbox' THEN store_kind = 'inbox_outbox'
        WHEN 'knowledge' THEN store_kind IN ('knowledge_content','knowledge_index','knowledge_artifacts')
        WHEN 'identity' THEN store_kind = 'identity_metadata'
        WHEN 'tutoring' THEN store_kind = 'tutoring_payload'
        ELSE FALSE
    END
$$;

CREATE FUNCTION privacy_begin_owner_scrub(
    requested_erasure_id UUID,
    requested_target_generation BIGINT,
    requested_owner_kind TEXT,
    requested_receipt_id UUID
) RETURNS UUID LANGUAGE plpgsql AS $$
DECLARE
    generated_permit UUID := gen_random_uuid();
    inserted_count INTEGER;
BEGIN
    INSERT INTO privacy_owner_scrub_permits(
        permit_token,erasure_id,owner_kind,target_learner_generation,receipt_id,
        backend_pid,transaction_id,created_at)
    SELECT generated_permit,e.id,g.owner_kind,e.target_learner_generation,r.id,
           pg_backend_pid(),txid_current(),clock_timestamp()
    FROM privacy_erasures e
    JOIN privacy_erasure_heads eh ON eh.erasure_id=e.id AND eh.status <> 'verified'
    JOIN privacy_redaction_barriers b
      ON b.erasure_id=e.id AND b.learner_generation=e.target_learner_generation
    JOIN privacy_owner_generation_gates g
      ON g.owner_kind=requested_owner_kind
     AND g.active_erasure_id=e.id
     AND g.learner_generation=e.target_learner_generation
     AND NOT g.read_open AND NOT g.write_open
    JOIN privacy_erasure_step_receipts r
      ON r.id=requested_receipt_id AND r.erasure_id=e.id
     AND (
       (r.status='pending' AND r.completed_at IS NULL)
       OR (r.store_kind IN ('nocturne_paths','nocturne_orphan_history')
           AND r.status IN ('partial','unknown') AND r.completed_at IS NOT NULL)
     )
    JOIN privacy_erasure_receipt_heads rh
      ON rh.erasure_id=r.erasure_id AND rh.store_kind=r.store_kind
     AND rh.current_receipt_id=r.id AND rh.current_version=r.version
    WHERE e.id=requested_erasure_id
      AND e.target_learner_generation=requested_target_generation
      AND privacy_scrub_receipt_matches_owner(g.owner_kind,r.store_kind);
    GET DIAGNOSTICS inserted_count = ROW_COUNT;
    IF inserted_count <> 1 THEN
        RAISE EXCEPTION 'privacy owner scrub permit does not match an active barrier and receipt';
    END IF;
    PERFORM set_config('edu_agent.privacy_scrub_permit',generated_permit::text,TRUE);
    RETURN generated_permit;
END $$;

CREATE FUNCTION privacy_owner_scrub_permitted(requested_owner_kind TEXT)
RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1
        FROM privacy_owner_scrub_permits p
        JOIN privacy_erasures e
          ON e.id=p.erasure_id AND e.target_learner_generation=p.target_learner_generation
        JOIN privacy_erasure_heads eh ON eh.erasure_id=e.id AND eh.status <> 'verified'
        JOIN privacy_redaction_barriers b
          ON b.erasure_id=e.id AND b.learner_generation=e.target_learner_generation
        JOIN privacy_owner_generation_gates g
          ON g.owner_kind=p.owner_kind AND g.active_erasure_id=e.id
         AND g.learner_generation=e.target_learner_generation
         AND NOT g.read_open AND NOT g.write_open
        JOIN privacy_erasure_step_receipts r
          ON r.id=p.receipt_id AND r.erasure_id=e.id
         AND (
           (r.status='pending' AND r.completed_at IS NULL)
           OR (r.store_kind IN ('nocturne_paths','nocturne_orphan_history')
               AND r.status IN ('partial','unknown') AND r.completed_at IS NOT NULL)
         )
        JOIN privacy_erasure_receipt_heads rh
          ON rh.erasure_id=r.erasure_id AND rh.store_kind=r.store_kind
         AND rh.current_receipt_id=r.id AND rh.current_version=r.version
        WHERE p.owner_kind=requested_owner_kind
          AND p.backend_pid=pg_backend_pid()
          AND p.transaction_id=txid_current()
          AND p.permit_token::text=current_setting('edu_agent.privacy_scrub_permit',TRUE)
          AND privacy_scrub_receipt_matches_owner(p.owner_kind,r.store_kind)
    )
$$;

CREATE OR REPLACE FUNCTION reject_learning_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('learning') THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'learning history is append-only';
END $$;

CREATE TABLE memory_generation_keys (
    id UUID PRIMARY KEY,
    learner_generation BIGINT NOT NULL UNIQUE CHECK (learner_generation >= 1),
    wrapped_key BYTEA,
    key_digest BYTEA NOT NULL CHECK (octet_length(key_digest) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    destroyed_at TIMESTAMPTZ,
    destruction_evidence_digest BYTEA CHECK (
        destruction_evidence_digest IS NULL OR octet_length(destruction_evidence_digest) = 32
    ),
    CONSTRAINT memory_generation_key_id_generation UNIQUE (id,learner_generation),
    CONSTRAINT memory_generation_key_destroyed CHECK (
        (destroyed_at IS NULL AND wrapped_key IS NOT NULL AND destruction_evidence_digest IS NULL)
        OR (destroyed_at IS NOT NULL AND wrapped_key IS NULL AND destruction_evidence_digest IS NOT NULL)
    )
);
CREATE FUNCTION protect_memory_generation_key_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'memory generation keys cannot be deleted';
    END IF;
    IF ROW(NEW.id,NEW.learner_generation,NEW.wrapped_key,NEW.key_digest,NEW.created_at,NEW.destroyed_at,NEW.destruction_evidence_digest)
       IS NOT DISTINCT FROM
       ROW(OLD.id,OLD.learner_generation,OLD.wrapped_key,OLD.key_digest,OLD.created_at,OLD.destroyed_at,OLD.destruction_evidence_digest) THEN
        RETURN NEW;
    END IF;
    IF ROW(NEW.id,NEW.learner_generation,NEW.key_digest,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id,OLD.learner_generation,OLD.key_digest,OLD.created_at) THEN
        RAISE EXCEPTION 'memory generation key identity is immutable';
    END IF;
    IF OLD.destroyed_at IS NULL AND NEW.destroyed_at IS NOT NULL
       AND NEW.wrapped_key IS NULL AND NEW.destruction_evidence_digest IS NOT NULL THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'memory generation key lifecycle is immutable';
END $$;
CREATE TRIGGER memory_generation_key_lifecycle_immutable
BEFORE UPDATE OR DELETE ON memory_generation_keys
FOR EACH ROW EXECUTE FUNCTION protect_memory_generation_key_lifecycle();
CREATE TABLE memory_managed_backup_inventory (
    id UUID PRIMARY KEY,
    relative_path TEXT NOT NULL UNIQUE CHECK (relative_path <> '' AND relative_path !~ '(^/|(^|/)\.\.(/|$))'),
    created_at TIMESTAMPTZ NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    artifact_hash BYTEA NOT NULL CHECK (octet_length(artifact_hash) = 32),
    learner_generation BIGINT NOT NULL REFERENCES memory_generation_keys(learner_generation),
    wrapped_key_id UUID NOT NULL REFERENCES memory_generation_keys(id),
    erasure_id UUID REFERENCES privacy_erasures(id),
    pruned_at TIMESTAMPTZ,
    CONSTRAINT memory_managed_backup_inventory_key_generation
        FOREIGN KEY (wrapped_key_id,learner_generation)
        REFERENCES memory_generation_keys(id,learner_generation)
);

CREATE FUNCTION reject_memory_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('memory') THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'memory history is append-only';
END $$;
CREATE FUNCTION protect_memory_expiry_reconciliation_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('memory') THEN
        RETURN NEW;
    END IF;
    IF ROW(
        NEW.id,NEW.delivery_id,NEW.logical_memory_id,NEW.external_uri,NEW.content_hash,
        NEW.attempt_token,NEW.sent_boot_epoch,NEW.learner_generation,NEW.record_generation,NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.id,OLD.delivery_id,OLD.logical_memory_id,OLD.external_uri,OLD.content_hash,
        OLD.attempt_token,OLD.sent_boot_epoch,OLD.learner_generation,OLD.record_generation,OLD.created_at
    ) THEN
        RAISE EXCEPTION 'memory expiry reconciliation identity is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER memory_expiry_reconciliation_identity_immutable
BEFORE UPDATE ON memory_expiry_reconciliations
FOR EACH ROW EXECUTE FUNCTION protect_memory_expiry_reconciliation_identity();
CREATE FUNCTION reject_privacy_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'privacy history is append-only';
END $$;
DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'memory_candidates','memory_candidate_decisions','memory_logical_memories',
        'memory_record_revisions','memory_record_tombstones','memory_deliveries','memory_delivery_attempts',
        'memory_delivery_receipts','memory_operation_inbox','memory_reconciliation_maintenance_claims'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_memory_history_mutation()',
            table_name || '_immutable', table_name
        );
    END LOOP;
    FOREACH table_name IN ARRAY ARRAY[
        'privacy_erasures','privacy_erasure_step_receipts','privacy_redaction_barriers',
        'privacy_owner_redaction_audit','privacy_owner_scrub_permits'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_privacy_history_mutation()',
            table_name || '_immutable', table_name
        );
    END LOOP;
END $$;

ALTER TABLE learning_events DROP CONSTRAINT learning_events_aggregate_type_check;
ALTER TABLE learning_events ADD CONSTRAINT learning_events_aggregate_type_check CHECK (aggregate_type IN ('goal','session','privacy'));
ALTER TABLE learning_aggregate_heads DROP CONSTRAINT learning_aggregate_heads_aggregate_type_check;
ALTER TABLE learning_aggregate_heads ADD CONSTRAINT learning_aggregate_heads_aggregate_type_check CHECK (aggregate_type IN ('goal','session','privacy'));

CREATE OR REPLACE FUNCTION privacy_scrub_receipt_matches_owner(owner_kind TEXT,store_kind TEXT)
RETURNS BOOLEAN LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE owner_kind
  WHEN 'memory' THEN store_kind IN ('memory_candidate_delivery','nocturne_paths','nocturne_orphan_history')
  WHEN 'learning' THEN store_kind IN ('learning_event_payload','learning_typed_payload','projection_generations')
  WHEN 'outbox' THEN store_kind='inbox_outbox'
  WHEN 'knowledge' THEN store_kind IN ('knowledge_content','knowledge_index','knowledge_artifacts')
  WHEN 'identity' THEN store_kind='identity_metadata'
  WHEN 'tutoring' THEN store_kind='tutoring_payload'
  ELSE FALSE END
$$;

CREATE FUNCTION privacy_lock_owner_gate(requested_owner TEXT,requested_access TEXT,expected_generation BIGINT DEFAULT NULL)
RETURNS BIGINT LANGUAGE plpgsql AS $$
DECLARE current_generation BIGINT; access_open BOOLEAN; gate_updated_at TIMESTAMPTZ;
BEGIN
 IF requested_access NOT IN ('read','write') THEN RAISE EXCEPTION 'invalid privacy gate access'; END IF;
 PERFORM pg_advisory_xact_lock_shared(hashtextextended('privacy-owner:'||requested_owner,0));
 SELECT learner_generation,CASE requested_access WHEN 'read' THEN read_open ELSE write_open END,updated_at
 INTO current_generation,access_open,gate_updated_at FROM privacy_owner_generation_gates WHERE owner_kind=requested_owner;
 IF NOT FOUND THEN RAISE EXCEPTION 'unknown privacy owner gate'; END IF;
 IF expected_generation IS NOT NULL AND current_generation<>expected_generation THEN RAISE EXCEPTION 'privacy owner generation changed'; END IF;
 IF requested_access='write' AND transaction_timestamp()<gate_updated_at THEN RAISE EXCEPTION 'privacy owner generation changed'; END IF;
 IF NOT access_open THEN
  IF requested_access='read' THEN RAISE EXCEPTION 'content_redacted'; END IF;
  RAISE EXCEPTION 'privacy_clear_in_progress';
 END IF;
 RETURN current_generation;
END $$;
CREATE FUNCTION privacy_enforce_owner_write() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF privacy_owner_scrub_permitted(TG_ARGV[0]) THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 PERFORM privacy_lock_owner_gate(TG_ARGV[0],'write',NULL);
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION reject_learning_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF privacy_owner_scrub_permitted('learning') THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 RAISE EXCEPTION 'learning history is append-only';
END $$;
CREATE FUNCTION reject_tutoring_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF privacy_owner_scrub_permitted('tutoring') THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 RAISE EXCEPTION 'tutoring history is append-only';
END $$;
CREATE FUNCTION reject_knowledge_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF privacy_owner_scrub_permitted('knowledge') THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 RAISE EXCEPTION 'knowledge history is append-only';
END $$;
DROP TRIGGER tutoring_free_questions_immutable ON tutoring_free_questions;
DROP TRIGGER tutoring_free_answers_immutable ON tutoring_free_answers;
DROP TRIGGER tutoring_proposal_artifacts_immutable ON tutoring_proposal_artifacts;
CREATE TRIGGER tutoring_free_questions_immutable BEFORE UPDATE OR DELETE ON tutoring_free_questions FOR EACH ROW EXECUTE FUNCTION reject_tutoring_history_mutation();
CREATE TRIGGER tutoring_free_answers_immutable BEFORE UPDATE OR DELETE ON tutoring_free_answers FOR EACH ROW EXECUTE FUNCTION reject_tutoring_history_mutation();
CREATE TRIGGER tutoring_proposal_artifacts_immutable BEFORE UPDATE OR DELETE ON tutoring_proposal_artifacts FOR EACH ROW EXECUTE FUNCTION reject_tutoring_history_mutation();
CREATE TRIGGER learning_event_payloads_immutable BEFORE UPDATE OR DELETE ON learning_event_payloads FOR EACH ROW EXECUTE FUNCTION reject_learning_history_mutation();
DO $$
DECLARE table_name TEXT; owner_name TEXT;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['knowledge_revisions','knowledge_import_operations','knowledge_document_revisions','knowledge_document_payloads','knowledge_snapshot_documents','knowledge_node_revisions','knowledge_lineages','knowledge_lineage_members','knowledge_node_artifacts'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_knowledge_history_mutation()',table_name||'_privacy_immutable',table_name);
 END LOOP;
 FOR table_name,owner_name IN SELECT * FROM (VALUES
  ('devices','identity'),('pairing_codes','identity'),('device_tokens','identity'),
  ('knowledge_catalog','knowledge'),('knowledge_revisions','knowledge'),('knowledge_import_operations','knowledge'),('knowledge_documents','knowledge'),('knowledge_document_revisions','knowledge'),('knowledge_document_payloads','knowledge'),('knowledge_snapshot_documents','knowledge'),('knowledge_nodes','knowledge'),('knowledge_node_revisions','knowledge'),('knowledge_lineages','knowledge'),('knowledge_lineage_members','knowledge'),('knowledge_node_artifacts','knowledge'),
  ('learning_event_clock','learning'),('learning_aggregate_heads','learning'),('learning_inbox','learning'),('learning_event_payloads','learning'),('learning_events','learning'),('learning_goal_revisions','learning'),('learning_route_revisions','learning'),('learning_route_steps','learning'),('learning_activities','learning'),('learning_activity_references','learning'),('learning_attempt_payloads','learning'),('learning_attempts','learning'),('learning_assessments','learning'),('learning_assessment_items','learning'),('learning_assessment_decisions','learning'),('learning_evidence','learning'),('learning_evidence_invalidations','learning'),('learning_exposures','learning'),('learning_misconception_revisions','learning'),('learning_projection_generations','learning'),('learning_projection_head','learning'),('learning_projection_checkpoints','learning'),('learning_projection_timeline','learning'),('learning_projection_routes','learning'),('learning_projection_sessions','learning'),('learning_projection_nodes','learning'),('learning_projection_evidence','learning'),('learning_projection_reviews','learning'),('learning_projection_misconceptions','learning'),('learning_projection_stats','learning'),
  ('tutoring_sessions','tutoring'),('tutoring_focus_frames','tutoring'),('tutoring_free_questions','tutoring'),('tutoring_proposal_requests','tutoring'),('tutoring_proposal_artifacts','tutoring'),('tutoring_free_answers','tutoring'),('outbox_messages','outbox'),
  ('memory_candidates','memory'),('memory_candidate_payloads','memory'),('memory_deliveries','memory'),('memory_delivery_payloads','memory'),('memory_expiry_reconciliations','memory')
 ) AS guarded(table_name,owner_name) LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write(%L)',table_name||'_privacy_write_gate',table_name,owner_name);
 END LOOP;
END $$;
