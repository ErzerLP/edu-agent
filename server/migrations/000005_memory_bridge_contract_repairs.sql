ALTER TABLE memory_record_revisions
    ADD CONSTRAINT memory_record_revision_delivery_ref_identity UNIQUE(id,delivery_id);

CREATE TABLE memory_record_external_refs (
    record_revision_id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL,
    external_node_id UUID,
    external_memory_id BIGINT CHECK (external_memory_id IS NULL OR external_memory_id > 0),
    delivery_attempt_id UUID,
    delivery_receipt_id UUID,
    observed_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_record_external_ref_revision
        FOREIGN KEY(record_revision_id,delivery_id)
        REFERENCES memory_record_revisions(id,delivery_id),
    CONSTRAINT memory_record_external_ref_attempt
        FOREIGN KEY(delivery_attempt_id,delivery_id)
        REFERENCES memory_delivery_attempts(id,delivery_id),
    CONSTRAINT memory_record_external_ref_receipt
        FOREIGN KEY(delivery_receipt_id,delivery_id)
        REFERENCES memory_delivery_receipts(id,delivery_id),
    CONSTRAINT memory_record_external_ref_identity_shape CHECK (
        (external_node_id IS NULL AND external_memory_id IS NULL
         AND delivery_attempt_id IS NULL AND delivery_receipt_id IS NULL)
        OR
        (external_node_id IS NOT NULL AND external_memory_id IS NOT NULL
         AND delivery_attempt_id IS NOT NULL AND delivery_receipt_id IS NOT NULL)
    )
);

INSERT INTO memory_record_external_refs(
    record_revision_id,delivery_id,external_node_id,external_memory_id,
    delivery_attempt_id,delivery_receipt_id,observed_at)
SELECT r.id,r.delivery_id,h.external_node_id,h.external_memory_id,
       dh.current_attempt_id,dh.current_receipt_id,COALESCE(h.applied_at,dh.updated_at)
FROM memory_record_heads h
JOIN memory_record_revisions r ON r.id=h.current_record_revision_id
JOIN memory_delivery_heads dh ON dh.delivery_id=r.delivery_id
WHERE h.status='applied' AND dh.status='applied'
  AND h.external_node_id IS NOT NULL
  AND h.external_memory_id IS NOT NULL
  AND dh.current_attempt_id IS NOT NULL
ON CONFLICT(record_revision_id) DO NOTHING;

-- A queued correction in 000004 kept the previous applied identity on the
-- mutable head. Preserve that still-known historical identity before creating
-- explicit unknown rows for identities that are no longer recoverable.
INSERT INTO memory_record_external_refs(
    record_revision_id,delivery_id,external_node_id,external_memory_id,
    delivery_attempt_id,delivery_receipt_id,observed_at)
SELECT previous.id,previous.delivery_id,h.external_node_id,h.external_memory_id,
       previous_head.current_attempt_id,previous_head.current_receipt_id,
       COALESCE(h.applied_at,previous_head.updated_at)
FROM memory_record_heads h
JOIN memory_record_revisions current ON current.id=h.current_record_revision_id
JOIN memory_record_revisions previous ON previous.id=current.previous_revision_id
JOIN memory_delivery_heads previous_head ON previous_head.delivery_id=previous.delivery_id
WHERE h.status='queued'
  AND h.external_node_id IS NOT NULL
  AND h.external_memory_id IS NOT NULL
  AND previous_head.current_attempt_id IS NOT NULL
  AND previous_head.current_receipt_id IS NOT NULL
ON CONFLICT(record_revision_id) DO NOTHING;

-- 000004 stored only the current external identity. Preserve every already
-- historical revision explicitly without inventing an ID that is no longer
-- recoverable; future revisions receive a complete identity atomically on apply.
INSERT INTO memory_record_external_refs(record_revision_id,delivery_id,observed_at)
SELECT r.id,r.delivery_id,r.created_at
FROM memory_record_revisions r
LEFT JOIN memory_record_heads h ON h.current_record_revision_id=r.id
WHERE h.current_record_revision_id IS NULL
   OR h.status IN ('permanently_rejected','delete_pending','deleted')
ON CONFLICT(record_revision_id) DO NOTHING;

CREATE TABLE memory_erasure_deliveries (
    id UUID PRIMARY KEY,
    erasure_id UUID NOT NULL,
    logical_memory_id UUID NOT NULL REFERENCES memory_logical_memories(id),
    target_learner_generation BIGINT NOT NULL CHECK (target_learner_generation >= 2),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_erasure_delivery_once UNIQUE(erasure_id,logical_memory_id),
    CONSTRAINT memory_erasure_delivery_erasure
        FOREIGN KEY(erasure_id,target_learner_generation)
        REFERENCES privacy_erasures(id,target_learner_generation),
    CONSTRAINT memory_erasure_delivery_identity UNIQUE(id,erasure_id,logical_memory_id)
);

CREATE TABLE memory_erasure_delivery_sources (
    erasure_delivery_id UUID NOT NULL REFERENCES memory_erasure_deliveries(id),
    reconciliation_id UUID NOT NULL REFERENCES memory_expiry_reconciliations(id),
    bound_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(erasure_delivery_id,reconciliation_id)
);

CREATE TABLE memory_erasure_delivery_receipts (
    id UUID PRIMARY KEY,
    erasure_delivery_id UUID NOT NULL REFERENCES memory_erasure_deliveries(id),
    store_kind TEXT NOT NULL CHECK (store_kind IN ('nocturne_paths','nocturne_orphan_history')),
    version BIGINT NOT NULL CHECK (version >= 1),
    status TEXT NOT NULL CHECK (status IN ('pending','succeeded','partial','failed','unknown')),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    verification_method TEXT NOT NULL CHECK (char_length(verification_method) BETWEEN 1 AND 500),
    evidence_digest BYTEA CHECK (evidence_digest IS NULL OR octet_length(evidence_digest) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_erasure_delivery_receipt_version UNIQUE(erasure_delivery_id,store_kind,version),
    CONSTRAINT memory_erasure_delivery_receipt_identity UNIQUE(id,erasure_delivery_id,store_kind,version)
);

CREATE TABLE memory_erasure_delivery_attempts (
    id UUID PRIMARY KEY,
    erasure_delivery_id UUID NOT NULL REFERENCES memory_erasure_deliveries(id),
    store_kind TEXT NOT NULL CHECK (store_kind IN ('nocturne_paths','nocturne_orphan_history')),
    reconciliation_id UUID NOT NULL REFERENCES memory_expiry_reconciliations(id),
    attempt_token UUID NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_erasure_delivery_attempt_source
        FOREIGN KEY(erasure_delivery_id,reconciliation_id)
        REFERENCES memory_erasure_delivery_sources(erasure_delivery_id,reconciliation_id),
    CONSTRAINT memory_erasure_delivery_attempt_once
        UNIQUE(erasure_delivery_id,store_kind,reconciliation_id),
    CONSTRAINT memory_erasure_delivery_attempt_identity UNIQUE(id,erasure_delivery_id,store_kind)
);

CREATE TABLE memory_erasure_delivery_attempt_heads (
    attempt_id UUID PRIMARY KEY,
    erasure_delivery_id UUID NOT NULL,
    store_kind TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reconciling','delete_pending','succeeded','conflict')),
    authorization_receipt_id UUID NOT NULL REFERENCES privacy_erasure_step_receipts(id),
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT memory_erasure_delivery_attempt_head_owner
        FOREIGN KEY(attempt_id,erasure_delivery_id,store_kind)
        REFERENCES memory_erasure_delivery_attempts(id,erasure_delivery_id,store_kind),
    CONSTRAINT memory_erasure_delivery_attempt_lease_shape CHECK (
        (state IN ('reconciling','delete_pending') AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state IN ('succeeded','conflict') AND lease_token IS NULL AND lease_expires_at IS NULL)
    )
);
CREATE UNIQUE INDEX memory_erasure_delivery_single_active_attempt
    ON memory_erasure_delivery_attempt_heads(erasure_delivery_id)
    WHERE state IN ('reconciling','delete_pending');

CREATE TABLE memory_erasure_delivery_scopes (
    erasure_delivery_id UUID NOT NULL REFERENCES memory_erasure_deliveries(id),
    store_kind TEXT NOT NULL CHECK (store_kind IN ('nocturne_paths','nocturne_orphan_history')),
    status TEXT NOT NULL CHECK (status IN ('pending','reconciling','delete_pending','succeeded','conflict')),
    current_attempt_id UUID,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    current_receipt_id UUID NOT NULL,
    current_receipt_version BIGINT NOT NULL CHECK (current_receipt_version >= 1),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(erasure_delivery_id,store_kind),
    CONSTRAINT memory_erasure_delivery_scope_attempt
        FOREIGN KEY(current_attempt_id,erasure_delivery_id,store_kind)
        REFERENCES memory_erasure_delivery_attempts(id,erasure_delivery_id,store_kind)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT memory_erasure_delivery_scope_receipt
        FOREIGN KEY(current_receipt_id,erasure_delivery_id,store_kind,current_receipt_version)
        REFERENCES memory_erasure_delivery_receipts(id,erasure_delivery_id,store_kind,version)
        DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE memory_remote_delete_plans
    ADD COLUMN erasure_delivery_id UUID UNIQUE REFERENCES memory_erasure_deliveries(id);

WITH active AS (
    SELECT g.active_erasure_id AS erasure_id,g.learner_generation AS target_generation,
           r.logical_memory_id,min(r.created_at) AS created_at
    FROM privacy_owner_generation_gates g
    JOIN memory_expiry_reconciliations r ON r.learner_generation<g.learner_generation
    WHERE g.owner_kind='memory' AND g.active_erasure_id IS NOT NULL
    GROUP BY g.active_erasure_id,g.learner_generation,r.logical_memory_id
)
INSERT INTO memory_erasure_deliveries(id,erasure_id,logical_memory_id,target_learner_generation,created_at)
SELECT gen_random_uuid(),erasure_id,logical_memory_id,target_generation,created_at
FROM active
ON CONFLICT(erasure_id,logical_memory_id) DO NOTHING;

INSERT INTO memory_erasure_delivery_sources(erasure_delivery_id,reconciliation_id,bound_at)
SELECT ed.id,r.id,clock_timestamp()
FROM memory_erasure_deliveries ed
JOIN memory_expiry_reconciliations r
  ON r.logical_memory_id=ed.logical_memory_id
 AND r.learner_generation<ed.target_learner_generation
ON CONFLICT(erasure_delivery_id,reconciliation_id) DO NOTHING;

INSERT INTO memory_erasure_delivery_receipts(
    id,erasure_delivery_id,store_kind,version,status,reason,verification_method,created_at)
SELECT gen_random_uuid(),ed.id,scope.store_kind,1,
       CASE WHEN EXISTS (
         SELECT 1 FROM memory_erasure_delivery_sources source
         JOIN memory_expiry_reconciliations reconciliation ON reconciliation.id=source.reconciliation_id
         WHERE source.erasure_delivery_id=ed.id
           AND reconciliation.status NOT IN ('absence_verified','verified')
       ) THEN 'pending' ELSE 'succeeded' END,
       CASE WHEN EXISTS (
         SELECT 1 FROM memory_erasure_delivery_sources source
         JOIN memory_expiry_reconciliations reconciliation ON reconciliation.id=source.reconciliation_id
         WHERE source.erasure_delivery_id=ed.id
           AND reconciliation.status NOT IN ('absence_verified','verified')
       ) THEN 'remote_erasure_pending' ELSE 'remote_reconciliation_already_verified' END,
       CASE WHEN EXISTS (
         SELECT 1 FROM memory_erasure_delivery_sources source
         JOIN memory_expiry_reconciliations reconciliation ON reconciliation.id=source.reconciliation_id
         WHERE source.erasure_delivery_id=ed.id
           AND reconciliation.status NOT IN ('absence_verified','verified')
       ) THEN 'not_yet_verified' ELSE 'existing_reconciliation_terminal_state' END,
       ed.created_at
FROM memory_erasure_deliveries ed
CROSS JOIN (VALUES ('nocturne_paths'),('nocturne_orphan_history')) AS scope(store_kind)
WHERE NOT EXISTS (
    SELECT 1 FROM memory_erasure_delivery_scopes existing
    WHERE existing.erasure_delivery_id=ed.id AND existing.store_kind=scope.store_kind
);

INSERT INTO memory_erasure_delivery_scopes(
    erasure_delivery_id,store_kind,status,current_attempt_id,attempt_count,
    current_receipt_id,current_receipt_version,updated_at)
SELECT receipt.erasure_delivery_id,receipt.store_kind,receipt.status,NULL,0,receipt.id,receipt.version,receipt.created_at
FROM memory_erasure_delivery_receipts receipt
WHERE receipt.version=1
ON CONFLICT(erasure_delivery_id,store_kind) DO NOTHING;

CREATE FUNCTION reject_memory_contract_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'memory contract history is append-only';
END $$;

CREATE TRIGGER memory_record_external_refs_immutable
BEFORE UPDATE OR DELETE ON memory_record_external_refs
FOR EACH ROW EXECUTE FUNCTION reject_memory_contract_history_mutation();

DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'memory_erasure_deliveries','memory_erasure_delivery_sources',
        'memory_erasure_delivery_attempts','memory_erasure_delivery_receipts'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_memory_contract_history_mutation()',
            table_name || '_immutable', table_name
        );
    END LOOP;
    FOREACH table_name IN ARRAY ARRAY[
        'memory_record_external_refs','memory_erasure_deliveries','memory_erasure_delivery_sources',
        'memory_erasure_delivery_attempts','memory_erasure_delivery_attempt_heads',
        'memory_erasure_delivery_receipts','memory_erasure_delivery_scopes'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write(%L)',
            table_name || '_privacy_write_gate', table_name, 'memory'
        );
    END LOOP;
END $$;

ALTER TABLE knowledge_revisions
    ADD COLUMN redacted_at TIMESTAMPTZ,
    ADD COLUMN redacted_by_erasure_id UUID REFERENCES privacy_erasures(id),
    ADD CONSTRAINT knowledge_revision_redaction_shape CHECK (
        (redacted_at IS NULL AND redacted_by_erasure_id IS NULL)
        OR (redacted_at IS NOT NULL AND redacted_by_erasure_id IS NOT NULL)
    );
CREATE INDEX knowledge_revision_redacted_by_erasure_idx
    ON knowledge_revisions(redacted_by_erasure_id,revision_no)
    WHERE redacted_by_erasure_id IS NOT NULL;

CREATE FUNCTION protect_managed_backup_erasure_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.erasure_id IS NOT NULL AND NEW.erasure_id IS DISTINCT FROM OLD.erasure_id THEN
        RAISE EXCEPTION 'managed backup erasure binding is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER memory_managed_backup_erasure_binding_immutable
BEFORE UPDATE OF erasure_id ON memory_managed_backup_inventory
FOR EACH ROW EXECUTE FUNCTION protect_managed_backup_erasure_binding();

CREATE TABLE privacy_migration_lease (
    singleton_id SMALLINT PRIMARY KEY CHECK (singleton_id=1),
    operation_id UUID,
    backup_identity BYTEA CHECK (backup_identity IS NULL OR octet_length(backup_identity)=32),
    acquired_at TIMESTAMPTZ,
    released_at TIMESTAMPTZ,
    CONSTRAINT privacy_migration_lease_shape CHECK (
        (operation_id IS NULL AND backup_identity IS NULL AND acquired_at IS NULL AND released_at IS NULL)
        OR (operation_id IS NOT NULL AND backup_identity IS NOT NULL AND acquired_at IS NOT NULL)
    ),
    CONSTRAINT privacy_migration_lease_release_order CHECK (
        released_at IS NULL OR released_at>=acquired_at
    )
);
INSERT INTO privacy_migration_lease(singleton_id) VALUES(1);
