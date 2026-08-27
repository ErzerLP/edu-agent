-- S2a adds the knowledge-owned NoteSync publication/review state and extends the generic
-- Outbox terminal vocabulary required by a later consumer. No remote credential is stored.
ALTER TABLE outbox_messages
    DROP CONSTRAINT outbox_terminal_disposition_check;
ALTER TABLE outbox_messages
    ADD CONSTRAINT outbox_terminal_disposition_check CHECK (
        (status = 'canceled' AND terminal_disposition IS NOT NULL AND terminal_disposition IN (
            'fenced','superseded','privacy_erasure','expired','permanently_rejected','deleted',
            'review_required'
        ))
        OR (status <> 'canceled' AND terminal_disposition IS NULL)
    );

ALTER TABLE knowledge_revisions
    ADD CONSTRAINT knowledge_revision_id_number_unique UNIQUE(id,revision_no);
ALTER TABLE knowledge_snapshot_documents
    ADD CONSTRAINT knowledge_snapshot_revision_document_revision_unique
        UNIQUE(knowledge_revision_id,document_id,document_revision_id);

CREATE TABLE knowledge_notesync_publications (
    document_id UUID PRIMARY KEY REFERENCES knowledge_documents(id),
    remote_vault TEXT NOT NULL CHECK (
        char_length(remote_vault) BETWEEN 1 AND 255 AND octet_length(remote_vault) <= 1024
    ),
    remote_path TEXT NOT NULL CHECK (
        char_length(remote_path) BETWEEN 1 AND 512 AND octet_length(remote_path) <= 1024
    ),
    published_knowledge_revision_id UUID NOT NULL,
    published_document_revision_id UUID NOT NULL,
    published_revision_no BIGINT NOT NULL CHECK (published_revision_no >= 1),
    base_markdown TEXT NOT NULL CHECK (octet_length(base_markdown) <= 4194304),
    base_sha256 BYTEA NOT NULL CHECK (octet_length(base_sha256) = 32),
    remote_version BIGINT CHECK (remote_version IS NULL OR remote_version >= 0),
    remote_last_time BIGINT CHECK (remote_last_time IS NULL OR remote_last_time >= 0),
    generation BIGINT NOT NULL CHECK (generation >= 1),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','redacted')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    redacted_at TIMESTAMPTZ,
    CONSTRAINT knowledge_notesync_publication_revision_number
        FOREIGN KEY(published_knowledge_revision_id,published_revision_no)
        REFERENCES knowledge_revisions(id,revision_no),
    CONSTRAINT knowledge_notesync_publication_snapshot
        FOREIGN KEY(published_knowledge_revision_id,document_id,published_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_id,document_revision_id),
    CONSTRAINT knowledge_notesync_publication_time_order CHECK (updated_at >= created_at),
    CONSTRAINT knowledge_notesync_publication_redaction_shape CHECK (
        (status='active' AND redacted_at IS NULL)
        OR (status='redacted' AND redacted_at IS NOT NULL)
    )
);
CREATE UNIQUE INDEX knowledge_notesync_publication_remote_current
    ON knowledge_notesync_publications(remote_vault,remote_path)
    WHERE status='active';

CREATE FUNCTION protect_knowledge_notesync_publication_progress() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('knowledge') THEN
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'knowledge notesync publications cannot be deleted';
    END IF;
    IF NEW.generation < OLD.generation
       OR (NEW.generation = OLD.generation AND NEW.published_revision_no < OLD.published_revision_no) THEN
        RAISE EXCEPTION 'knowledge notesync publication cannot move backward';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER knowledge_notesync_publication_progress_monotonic
    BEFORE UPDATE OR DELETE ON knowledge_notesync_publications
    FOR EACH ROW EXECUTE FUNCTION protect_knowledge_notesync_publication_progress();

CREATE TABLE knowledge_notesync_publication_attempts (
    id UUID PRIMARY KEY,
    outbox_id UUID NOT NULL UNIQUE REFERENCES outbox_messages(id) DEFERRABLE INITIALLY DEFERRED,
    idempotency_key TEXT NOT NULL UNIQUE,
    document_id UUID NOT NULL REFERENCES knowledge_documents(id),
    knowledge_revision_id UUID NOT NULL,
    document_revision_id UUID NOT NULL,
    knowledge_revision_no BIGINT NOT NULL CHECK (knowledge_revision_no >= 1),
    generation BIGINT NOT NULL CHECK (generation >= 1),
    publication_reason TEXT NOT NULL CHECK (publication_reason IN (
        'canonical_revision','review_keep_canonical','review_import'
    )),
    status TEXT NOT NULL CHECK (status IN (
        'prepared','unknown','retryable','applied','review_required','superseded','redacted'
    )),
    base_missing BOOLEAN NOT NULL,
    base_markdown TEXT CHECK (base_markdown IS NULL OR octet_length(base_markdown) <= 4194304),
    base_sha256 BYTEA CHECK (base_sha256 IS NULL OR octet_length(base_sha256) = 32),
    error_category TEXT CHECK (
        error_category IS NULL OR char_length(error_category) BETWEEN 1 AND 200
    ),
    error_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT knowledge_notesync_attempt_idempotency_shape CHECK (
        (publication_reason='canonical_revision' AND
         idempotency_key = 'notesync.publish:' || document_id::text || ':' ||
                           document_revision_id::text || ':' || knowledge_revision_no::text || ':' ||
                           generation::text)
        OR (publication_reason IN ('review_keep_canonical','review_import') AND
            idempotency_key LIKE 'notesync.review.publish:%:' ||
                                 document_revision_id::text || ':' || generation::text)
    ),
    CONSTRAINT knowledge_notesync_attempt_revision_number
        FOREIGN KEY(knowledge_revision_id,knowledge_revision_no)
        REFERENCES knowledge_revisions(id,revision_no),
    CONSTRAINT knowledge_notesync_attempt_snapshot
        FOREIGN KEY(knowledge_revision_id,document_id,document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_id,document_revision_id),
    CONSTRAINT knowledge_notesync_attempt_base_shape CHECK (
        (base_missing AND base_markdown IS NULL AND base_sha256 IS NULL)
        OR (NOT base_missing AND base_markdown IS NOT NULL AND base_sha256 IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_attempt_error_shape CHECK (
        (error_category IS NULL AND error_at IS NULL)
        OR (error_category IS NOT NULL AND error_at IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_attempt_redacted_shape CHECK (
        status <> 'redacted' OR (base_missing AND base_markdown IS NULL AND base_sha256 IS NULL)
    ),
    CONSTRAINT knowledge_notesync_attempt_time_order CHECK (updated_at >= created_at)
);
CREATE INDEX knowledge_notesync_attempt_document_order
    ON knowledge_notesync_publication_attempts(document_id,knowledge_revision_no,generation,created_at);

CREATE FUNCTION protect_knowledge_notesync_attempt_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('knowledge') THEN
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'knowledge notesync publication attempts cannot be deleted';
    END IF;
    IF ROW(
        NEW.id,NEW.outbox_id,NEW.idempotency_key,NEW.document_id,NEW.knowledge_revision_id,
        NEW.document_revision_id,NEW.knowledge_revision_no,NEW.generation,NEW.publication_reason,
        NEW.base_missing,NEW.base_markdown,NEW.base_sha256,NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.id,OLD.outbox_id,OLD.idempotency_key,OLD.document_id,OLD.knowledge_revision_id,
        OLD.document_revision_id,OLD.knowledge_revision_no,OLD.generation,OLD.publication_reason,
        OLD.base_missing,OLD.base_markdown,OLD.base_sha256,OLD.created_at
    ) THEN
        RAISE EXCEPTION 'knowledge notesync publication attempt identity is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER knowledge_notesync_publication_attempts_identity_immutable
    BEFORE UPDATE OR DELETE ON knowledge_notesync_publication_attempts
    FOR EACH ROW EXECUTE FUNCTION protect_knowledge_notesync_attempt_identity();

CREATE TABLE knowledge_notesync_reviews (
    review_id UUID PRIMARY KEY,
    document_id UUID REFERENCES knowledge_documents(id),
    remote_vault TEXT NOT NULL CHECK (
        char_length(remote_vault) BETWEEN 1 AND 255 AND octet_length(remote_vault) <= 1024
    ),
    remote_path TEXT NOT NULL CHECK (
        char_length(remote_path) BETWEEN 1 AND 512 AND octet_length(remote_path) <= 1024
    ),
    kind TEXT NOT NULL CHECK (kind IN (
        'remote_changed','both_changed','remote_missing','remote_moved',
        'unbased_remote','path_occupied','invalid_remote_markdown'
    )),
    reason_code TEXT NOT NULL CHECK (reason_code IN (
        'remote_content_changed','both_sides_changed','remote_note_missing',
        'remote_identity_moved','unmanaged_remote_note','remote_path_occupied',
        'remote_markdown_invalid','publication_preflight_changed','publication_readback_changed'
    )),
    status TEXT NOT NULL CHECK (status IN ('open','resolved','closed')),
    head_knowledge_revision_id UUID,
    head_knowledge_revision_no BIGINT CHECK (
        head_knowledge_revision_no IS NULL OR head_knowledge_revision_no >= 1
    ),
    canonical_path TEXT NOT NULL CHECK (
        char_length(canonical_path) BETWEEN 1 AND 512 AND octet_length(canonical_path) <= 1024
    ),
    remote_document_id UUID,
    base_missing BOOLEAN NOT NULL,
    base_knowledge_revision_id UUID,
    base_knowledge_revision_no BIGINT CHECK (
        base_knowledge_revision_no IS NULL OR base_knowledge_revision_no >= 1
    ),
    base_document_revision_id UUID,
    base_remote_path TEXT CHECK (
        base_remote_path IS NULL OR (char_length(base_remote_path) BETWEEN 1 AND 512 AND octet_length(base_remote_path) <= 1024)
    ),
    base_remote_version BIGINT CHECK (base_remote_version IS NULL OR base_remote_version >= 0),
    base_remote_last_time BIGINT CHECK (base_remote_last_time IS NULL OR base_remote_last_time >= 0),
    base_markdown TEXT CHECK (base_markdown IS NULL OR octet_length(base_markdown) <= 4194304),
    base_sha256 BYTEA CHECK (base_sha256 IS NULL OR octet_length(base_sha256) = 32),
    local_missing BOOLEAN NOT NULL,
    local_knowledge_revision_id UUID,
    local_knowledge_revision_no BIGINT CHECK (
        local_knowledge_revision_no IS NULL OR local_knowledge_revision_no >= 1
    ),
    local_document_revision_id UUID,
    local_markdown TEXT CHECK (local_markdown IS NULL OR octet_length(local_markdown) <= 4194304),
    local_sha256 BYTEA CHECK (local_sha256 IS NULL OR octet_length(local_sha256) = 32),
    remote_missing BOOLEAN NOT NULL,
    remote_markdown TEXT CHECK (remote_markdown IS NULL OR octet_length(remote_markdown) <= 4194304),
    remote_sha256 BYTEA CHECK (remote_sha256 IS NULL OR octet_length(remote_sha256) = 32),
    remote_version BIGINT CHECK (remote_version IS NULL OR remote_version >= 0),
    remote_last_time BIGINT CHECK (remote_last_time IS NULL OR remote_last_time >= 0),
    remote_source_revision_id UUID,
    base_to_local_diff TEXT NOT NULL CHECK (octet_length(base_to_local_diff) <= 262144),
    base_to_remote_diff TEXT NOT NULL CHECK (octet_length(base_to_remote_diff) <= 262144),
    local_diff_truncated BOOLEAN NOT NULL,
    remote_diff_truncated BOOLEAN NOT NULL,
    basis_hash BYTEA NOT NULL CHECK (octet_length(basis_hash) = 32),
    generation BIGINT NOT NULL CHECK (generation >= 1),
    resolution_kind TEXT CHECK (resolution_kind IS NULL OR resolution_kind IN (
        'accept_remote','keep_canonical','merged','superseded','privacy_redaction'
    )),
    resolution_operation_id UUID,
    resolved_by_device_id UUID,
    resolved_knowledge_revision_id UUID,
    resolved_document_id UUID,
    resolved_document_revision_id UUID,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    CONSTRAINT knowledge_notesync_review_head_revision_number
        FOREIGN KEY(head_knowledge_revision_id,head_knowledge_revision_no)
        REFERENCES knowledge_revisions(id,revision_no) MATCH FULL,
    CONSTRAINT knowledge_notesync_review_base_revision_number
        FOREIGN KEY(base_knowledge_revision_id,base_knowledge_revision_no)
        REFERENCES knowledge_revisions(id,revision_no) MATCH FULL,
    CONSTRAINT knowledge_notesync_review_base_snapshot
        FOREIGN KEY(base_knowledge_revision_id,document_id,base_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_id,document_revision_id),
    CONSTRAINT knowledge_notesync_review_local_revision_number
        FOREIGN KEY(local_knowledge_revision_id,local_knowledge_revision_no)
        REFERENCES knowledge_revisions(id,revision_no) MATCH FULL,
    CONSTRAINT knowledge_notesync_review_local_snapshot
        FOREIGN KEY(local_knowledge_revision_id,document_id,local_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_id,document_revision_id),
    CONSTRAINT knowledge_notesync_review_resolved_snapshot
        FOREIGN KEY(resolved_knowledge_revision_id,resolved_document_id,resolved_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_id,document_revision_id),
    CONSTRAINT knowledge_notesync_review_base_shape CHECK (
        (base_missing AND base_knowledge_revision_id IS NULL AND base_knowledge_revision_no IS NULL
                      AND base_document_revision_id IS NULL AND base_remote_path IS NULL
                      AND base_remote_version IS NULL AND base_remote_last_time IS NULL
                      AND base_markdown IS NULL AND base_sha256 IS NULL)
        OR (NOT base_missing AND document_id IS NOT NULL AND base_knowledge_revision_id IS NOT NULL
                         AND base_knowledge_revision_no IS NOT NULL AND base_document_revision_id IS NOT NULL
                         AND base_remote_path IS NOT NULL AND base_remote_version IS NOT NULL
                         AND base_remote_last_time IS NOT NULL AND base_markdown IS NOT NULL AND base_sha256 IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_review_local_shape CHECK (
        (local_missing AND local_knowledge_revision_id IS NULL AND local_knowledge_revision_no IS NULL
                       AND local_document_revision_id IS NULL AND local_markdown IS NULL AND local_sha256 IS NULL)
        OR (NOT local_missing AND document_id IS NOT NULL AND local_knowledge_revision_id IS NOT NULL
                          AND local_knowledge_revision_no IS NOT NULL AND local_document_revision_id IS NOT NULL
                          AND local_markdown IS NOT NULL AND local_sha256 IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_review_remote_shape CHECK (
        (remote_missing AND remote_markdown IS NULL AND remote_sha256 IS NULL
                        AND remote_version IS NULL AND remote_last_time IS NULL)
        OR (NOT remote_missing AND remote_markdown IS NOT NULL AND remote_sha256 IS NOT NULL
                               AND remote_version IS NOT NULL AND remote_last_time IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_review_resolution_result_shape CHECK (
        (resolved_knowledge_revision_id IS NULL AND resolved_document_id IS NULL
                                                AND resolved_document_revision_id IS NULL)
        OR (resolved_knowledge_revision_id IS NOT NULL AND resolved_document_id IS NOT NULL
                                                     AND resolved_document_revision_id IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_review_status_shape CHECK (
        (status='open' AND resolution_kind IS NULL AND resolution_operation_id IS NULL
                       AND resolved_by_device_id IS NULL AND resolved_knowledge_revision_id IS NULL
                       AND resolved_document_id IS NULL AND resolved_document_revision_id IS NULL AND resolved_at IS NULL)
        OR (status='resolved' AND resolution_kind IN ('accept_remote','keep_canonical','merged')
                             AND resolution_operation_id IS NOT NULL AND resolved_by_device_id IS NOT NULL
                             AND resolved_knowledge_revision_id IS NOT NULL AND resolved_document_id IS NOT NULL
                             AND resolved_document_revision_id IS NOT NULL AND resolved_at IS NOT NULL)
        OR (status='closed' AND resolution_kind IN ('superseded','privacy_redaction')
                           AND resolution_operation_id IS NULL AND resolved_by_device_id IS NULL
                           AND resolved_knowledge_revision_id IS NULL AND resolved_document_id IS NULL
                           AND resolved_document_revision_id IS NULL AND resolved_at IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_review_time_order CHECK (
        updated_at >= created_at AND (resolved_at IS NULL OR resolved_at >= created_at)
    )
);
CREATE UNIQUE INDEX knowledge_notesync_review_single_open_basis
    ON knowledge_notesync_reviews(
        COALESCE(document_id,'00000000-0000-0000-0000-000000000000'::uuid),
        remote_vault,remote_path,basis_hash
    ) WHERE status='open';
CREATE INDEX knowledge_notesync_review_status_created
    ON knowledge_notesync_reviews(status,created_at,review_id);

CREATE FUNCTION protect_knowledge_notesync_review_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('knowledge') THEN
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'knowledge notesync reviews cannot be deleted';
    END IF;
    IF OLD.status <> 'open' THEN
        IF NEW IS DISTINCT FROM OLD THEN
            RAISE EXCEPTION 'knowledge notesync review terminal resolution is immutable';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.status NOT IN ('resolved','closed') THEN
        RAISE EXCEPTION 'knowledge notesync review must leave open exactly once';
    END IF;
    IF ROW(
        NEW.review_id,NEW.document_id,NEW.remote_vault,NEW.remote_path,NEW.kind,NEW.reason_code,
        NEW.head_knowledge_revision_id,NEW.head_knowledge_revision_no,NEW.canonical_path,
        NEW.remote_document_id,
        NEW.base_missing,NEW.base_knowledge_revision_id,NEW.base_knowledge_revision_no,
        NEW.base_document_revision_id,NEW.base_remote_path,NEW.base_remote_version,NEW.base_remote_last_time,
        NEW.base_markdown,NEW.base_sha256,
        NEW.local_missing,NEW.local_knowledge_revision_id,NEW.local_knowledge_revision_no,
        NEW.local_document_revision_id,NEW.local_markdown,NEW.local_sha256,
        NEW.remote_missing,NEW.remote_markdown,NEW.remote_sha256,NEW.remote_version,
        NEW.remote_last_time,NEW.remote_source_revision_id,
        NEW.base_to_local_diff,NEW.base_to_remote_diff,NEW.local_diff_truncated,NEW.remote_diff_truncated,
        NEW.basis_hash,NEW.generation,NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.review_id,OLD.document_id,OLD.remote_vault,OLD.remote_path,OLD.kind,OLD.reason_code,
        OLD.head_knowledge_revision_id,OLD.head_knowledge_revision_no,OLD.canonical_path,
        OLD.remote_document_id,
        OLD.base_missing,OLD.base_knowledge_revision_id,OLD.base_knowledge_revision_no,
        OLD.base_document_revision_id,OLD.base_remote_path,OLD.base_remote_version,OLD.base_remote_last_time,
        OLD.base_markdown,OLD.base_sha256,
        OLD.local_missing,OLD.local_knowledge_revision_id,OLD.local_knowledge_revision_no,
        OLD.local_document_revision_id,OLD.local_markdown,OLD.local_sha256,
        OLD.remote_missing,OLD.remote_markdown,OLD.remote_sha256,OLD.remote_version,
        OLD.remote_last_time,OLD.remote_source_revision_id,
        OLD.base_to_local_diff,OLD.base_to_remote_diff,OLD.local_diff_truncated,OLD.remote_diff_truncated,
        OLD.basis_hash,OLD.generation,OLD.created_at
    ) THEN
        RAISE EXCEPTION 'knowledge notesync review snapshots are immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER knowledge_notesync_reviews_snapshot_immutable
    BEFORE UPDATE OR DELETE ON knowledge_notesync_reviews
    FOR EACH ROW EXECUTE FUNCTION protect_knowledge_notesync_review_snapshot();

CREATE TABLE knowledge_notesync_resolution_operations (
    device_id UUID NOT NULL,
    operation_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    review_id UUID NOT NULL REFERENCES knowledge_notesync_reviews(review_id),
    generation BIGINT NOT NULL CHECK (generation >= 1),
    resolution_kind TEXT NOT NULL CHECK (
        resolution_kind IN ('accept_remote','keep_canonical','merged','privacy_redaction')
    ),
    result_knowledge_revision_id UUID,
    result_document_id UUID,
    result_document_revision_id UUID,
    unchanged BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL DEFAULT 'completed' CHECK (status IN ('completed','redacted')),
    completed_at TIMESTAMPTZ NOT NULL,
    redacted_at TIMESTAMPTZ,
    PRIMARY KEY(device_id,operation_id),
    CONSTRAINT knowledge_notesync_resolution_review_operation_unique
        UNIQUE(review_id,device_id,operation_id),
    CONSTRAINT knowledge_notesync_resolution_result_shape CHECK (
        (result_knowledge_revision_id IS NULL AND result_document_id IS NULL
                                                AND result_document_revision_id IS NULL)
        OR (result_knowledge_revision_id IS NOT NULL AND result_document_id IS NOT NULL
                                                     AND result_document_revision_id IS NOT NULL)
    ),
    CONSTRAINT knowledge_notesync_resolution_result_snapshot
        FOREIGN KEY(result_knowledge_revision_id,result_document_id,result_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_id,document_revision_id),
    CONSTRAINT knowledge_notesync_resolution_redaction_shape CHECK (
        (status='completed' AND resolution_kind<>'privacy_redaction' AND redacted_at IS NULL
                            AND result_knowledge_revision_id IS NOT NULL AND result_document_id IS NOT NULL
                            AND result_document_revision_id IS NOT NULL)
        OR (status='redacted' AND resolution_kind='privacy_redaction' AND redacted_at IS NOT NULL
                              AND result_knowledge_revision_id IS NULL AND result_document_id IS NULL
                              AND result_document_revision_id IS NULL
                              AND request_hash=decode(repeat('00',32),'hex'))
    )
);
CREATE INDEX knowledge_notesync_resolution_review_order
    ON knowledge_notesync_resolution_operations(review_id,completed_at,device_id,operation_id);

ALTER TABLE knowledge_notesync_reviews
    ADD CONSTRAINT knowledge_notesync_review_resolution_operation
    FOREIGN KEY(review_id,resolved_by_device_id,resolution_operation_id)
    REFERENCES knowledge_notesync_resolution_operations(review_id,device_id,operation_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION protect_knowledge_notesync_resolution_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('knowledge') THEN
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' OR NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'knowledge notesync resolution operation is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER knowledge_notesync_resolution_operation_immutable
    BEFORE UPDATE OR DELETE ON knowledge_notesync_resolution_operations
    FOR EACH ROW EXECUTE FUNCTION protect_knowledge_notesync_resolution_operation();

DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'knowledge_notesync_publications',
        'knowledge_notesync_publication_attempts',
        'knowledge_notesync_reviews',
        'knowledge_notesync_resolution_operations'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write(%L)',
            table_name || '_privacy_write_gate',table_name,'knowledge'
        );
    END LOOP;
END $$;
