-- Offline sync server core. Historical migrations 000001-000006 remain immutable.

LOCK TABLE learning_evidence IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE learning_evidence_invalidations IN SHARE MODE;
LOCK TABLE learning_events IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE learning_event_payloads IN SHARE MODE;

ALTER TABLE learning_aggregate_heads
    DROP CONSTRAINT learning_aggregate_heads_aggregate_type_check,
    ADD CONSTRAINT learning_aggregate_heads_aggregate_type_check
        CHECK (aggregate_type IN ('goal','session','privacy','offline_attempt'));
ALTER TABLE learning_inbox
    DROP CONSTRAINT learning_inbox_aggregate_type_check,
    ADD CONSTRAINT learning_inbox_aggregate_type_check
        CHECK (aggregate_type IN ('goal','session','offline_attempt'));
ALTER TABLE learning_events
    DROP CONSTRAINT learning_events_aggregate_type_check,
    ADD CONSTRAINT learning_events_aggregate_type_check
        CHECK (aggregate_type IN ('goal','session','privacy','offline_attempt')),
    ADD COLUMN parent_session_id UUID REFERENCES tutoring_sessions(id),
    ADD COLUMN event_source TEXT NOT NULL DEFAULT 'online'
        CHECK (event_source IN ('online','offline')),
    ADD COLUMN archive_disposition TEXT
        CHECK (archive_disposition IS NULL OR archive_disposition IN ('succeeded','rejected')),
    ADD COLUMN evidence_disposition TEXT
        CHECK (evidence_disposition IS NULL OR evidence_disposition IN (
            'accepted','provisional','pending_evaluation','not_eligible','not_applicable','unchanged'
        )),
    ADD COLUMN goal_revision_id UUID REFERENCES learning_goal_revisions(id),
    ADD COLUMN route_revision_id UUID REFERENCES learning_route_revisions(id),
    ADD COLUMN knowledge_revision_id UUID REFERENCES knowledge_revisions(id),
    ADD COLUMN activity_id UUID,
    ADD COLUMN activity_revision BIGINT,
    ADD CONSTRAINT learning_event_activity_owner
        FOREIGN KEY(activity_id,activity_revision)
        REFERENCES learning_activities(id,revision) MATCH FULL,
    ADD CONSTRAINT learning_event_offline_shape CHECK (
        (event_source='online' AND parent_session_id IS NULL)
        OR
        (event_source='offline' AND aggregate_type='offline_attempt'
            AND parent_session_id IS NOT NULL
            AND goal_revision_id IS NOT NULL
            AND route_revision_id IS NOT NULL
            AND knowledge_revision_id IS NOT NULL
            AND activity_id IS NOT NULL
            AND activity_revision IS NOT NULL
            AND archive_disposition IS NOT NULL
            AND evidence_disposition IS NOT NULL)
    );
CREATE INDEX learning_events_parent_session_idx
    ON learning_events(parent_session_id,event_seq)
    WHERE parent_session_id IS NOT NULL;

ALTER TABLE learning_attempts
    ADD COLUMN evidence_eligibility BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN evidence_ineligible_reason TEXT,
    ADD COLUMN archive_disposition TEXT NOT NULL DEFAULT 'online'
        CHECK (archive_disposition IN ('online','offline_succeeded')),
    ADD COLUMN offline_submission_id UUID,
    ADD CONSTRAINT learning_attempt_evidence_eligibility_shape CHECK (
        (evidence_eligibility AND evidence_ineligible_reason IS NULL)
        OR
        (NOT evidence_eligibility AND evidence_ineligible_reason IN (
            'duplicate_activity_submission','stale_knowledge_head','expired_activity',
            'stale_context','stale_policy','answer_revealed'
        ))
    ),
    ADD CONSTRAINT learning_attempt_activity_identity
        UNIQUE(id,activity_id,activity_revision);

ALTER TABLE learning_assessments
    ADD COLUMN evidence_eligibility BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN evidence_ineligible_reason TEXT,
    ADD CONSTRAINT learning_assessment_evidence_eligibility_shape CHECK (
        (evidence_eligibility AND evidence_ineligible_reason IS NULL)
        OR
        (NOT evidence_eligibility AND evidence_ineligible_reason IN (
            'duplicate_activity_submission','stale_knowledge_head','expired_activity',
            'stale_context','stale_policy','answer_revealed'
        ))
    );

ALTER TABLE learning_evidence
    ADD COLUMN accepted_event_seq BIGINT;
WITH accepted AS (
    SELECT e.id AS evidence_id,min(event.event_seq) AS accepted_event_seq,count(*) AS matches
    FROM learning_evidence e
    JOIN learning_events event ON event.event_type='EvidenceAccepted'
    JOIN learning_event_payloads payload
      ON payload.id=event.payload_id AND payload.payload_hash=event.payload_hash
     AND payload.redacted_at IS NULL
     AND payload.payload->>'evidence_id'=e.id::text
    GROUP BY e.id
)
UPDATE learning_evidence evidence
SET accepted_event_seq=accepted.accepted_event_seq
FROM accepted
WHERE accepted.evidence_id=evidence.id AND accepted.matches=1;
WITH metadata_candidates AS (
    SELECT evidence.id AS evidence_id,event.event_seq,
           count(*) OVER (PARTITION BY evidence.id) AS matches
    FROM learning_evidence evidence
    JOIN learning_assessment_decisions decision ON decision.id=evidence.decision_id
    JOIN learning_events event
      ON event.event_type='EvidenceAccepted'
     AND event.aggregate_type='session'
     AND event.aggregate_id=evidence.session_id
     AND event.device_id=decision.actor_device_id
     AND event.received_at=evidence.received_at
    WHERE evidence.accepted_event_seq IS NULL
)
UPDATE learning_evidence evidence
SET accepted_event_seq=candidate.event_seq
FROM metadata_candidates candidate
WHERE candidate.evidence_id=evidence.id AND candidate.matches=1;
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM learning_evidence
        WHERE accepted_event_seq IS NULL AND rubric_revision<>'[redacted]'
    ) THEN
        RAISE EXCEPTION 'cannot recover unique accepted event sequence for historical evidence';
    END IF;
END
$$;
ALTER TABLE learning_evidence
    ADD CONSTRAINT learning_evidence_accepted_event_sequence_shape
        CHECK (accepted_event_seq IS NOT NULL OR rubric_revision='[redacted]');
CREATE UNIQUE INDEX learning_evidence_accepted_event_seq_unique
    ON learning_evidence(accepted_event_seq)
    WHERE accepted_event_seq IS NOT NULL;

ALTER TABLE learning_projection_evidence
    ADD COLUMN accepted_event_seq BIGINT;
UPDATE learning_projection_evidence projection
SET accepted_event_seq=evidence.accepted_event_seq,
    item=CASE WHEN evidence.accepted_event_seq IS NULL THEN projection.item
              ELSE jsonb_set(projection.item,'{accepted_event_seq}',to_jsonb(evidence.accepted_event_seq),TRUE) END
FROM learning_evidence evidence
WHERE evidence.id=projection.evidence_id;
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM learning_projection_evidence projection
        JOIN learning_evidence evidence ON evidence.id=projection.evidence_id
        WHERE projection.accepted_event_seq IS NULL AND evidence.rubric_revision<>'[redacted]'
    ) THEN
        RAISE EXCEPTION 'cannot recover accepted event sequence for projected evidence';
    END IF;
END
$$;
CREATE INDEX learning_projection_evidence_order
    ON learning_projection_evidence(generation_id,accepted_event_seq,evidence_id);

UPDATE learning_projection_timeline timeline
SET item=timeline.item || jsonb_build_object(
        'parent_session_id',COALESCE(event.parent_session_id::text,''),
        'source',event.event_source,
        'archive_disposition',COALESCE(event.archive_disposition,''),
        'evidence_disposition',COALESCE(event.evidence_disposition,''),
        'actor_device_id',event.device_id::text)
FROM learning_events event
WHERE event.event_seq=timeline.event_seq AND event.id=timeline.event_id;
UPDATE learning_projection_generations
SET incomplete=TRUE,
    reason_codes=CASE
        WHEN 'projection_version_upgrade_required'=ANY(reason_codes) THEN reason_codes
        ELSE array_append(reason_codes,'projection_version_upgrade_required')
    END
WHERE status='active' AND projection_version<>'learning-projection-v2';

CREATE TABLE offline_device_credentials (
    device_id UUID PRIMARY KEY REFERENCES devices(id),
    credential_epoch BIGINT NOT NULL CHECK (credential_epoch >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
INSERT INTO offline_device_credentials(device_id,credential_epoch,created_at,updated_at)
SELECT id,1,created_at,COALESCE(revoked_at,created_at) FROM devices;

CREATE TABLE offline_prepare_claims (
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash)=32),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    status TEXT NOT NULL CHECK (status IN ('pending','processing','published','rejected')),
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    request_body JSONB NOT NULL,
    model_artifact JSONB,
    result_body JSONB,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(device_id,operation_id),
    CONSTRAINT offline_prepare_claim_lease_shape CHECK (
        (status='processing' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status<>'processing' AND lease_token IS NULL AND lease_expires_at IS NULL)
    ),
    CONSTRAINT offline_prepare_claim_result_shape CHECK (
        (status IN ('pending','processing') AND result_body IS NULL)
        OR (status IN ('published','rejected') AND result_body IS NOT NULL)
    )
);

CREATE TABLE offline_activities (
    id UUID NOT NULL,
    revision BIGINT NOT NULL CHECK (revision=1),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    parent_session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    goal_revision_id UUID NOT NULL REFERENCES learning_goal_revisions(id),
    route_revision_id UUID NOT NULL REFERENCES learning_route_revisions(id),
    route_step_id UUID NOT NULL REFERENCES learning_route_steps(id),
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    target_node_id UUID NOT NULL,
    target_node_revision_id UUID NOT NULL,
    prompt TEXT NOT NULL CHECK (char_length(prompt) BETWEEN 1 AND 8000),
    activity_type TEXT NOT NULL CHECK (activity_type IN ('objective','open')),
    rubric_revision TEXT NOT NULL,
    rubric JSONB NOT NULL,
    difficulty INTEGER NOT NULL CHECK (difficulty BETWEEN 1 AND 5),
    allowed_help TEXT[] NOT NULL,
    activity_policy_version TEXT NOT NULL,
    assessment_policy_version TEXT NOT NULL,
    review_policy_version TEXT NOT NULL,
    practice_kind TEXT NOT NULL CHECK (practice_kind IN ('practice','review')),
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash)=32),
    issued_at TIMESTAMPTZ NOT NULL,
    eligible_until TIMESTAMPTZ NOT NULL,
    archive_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(id,revision),
    CONSTRAINT offline_activity_identity UNIQUE(
        id,revision,parent_session_id,goal_revision_id,route_revision_id,
        knowledge_revision_id,target_node_id,target_node_revision_id
    ),
    CONSTRAINT offline_activity_route_owner
        FOREIGN KEY(route_revision_id,goal_revision_id,knowledge_revision_id)
        REFERENCES learning_route_revisions(id,goal_revision_id,knowledge_revision_id),
    CONSTRAINT offline_activity_route_step_owner
        FOREIGN KEY(route_step_id,route_revision_id,knowledge_revision_id,target_node_id,target_node_revision_id)
        REFERENCES learning_route_steps(id,route_revision_id,knowledge_revision_id,node_id,node_revision_id),
    CONSTRAINT offline_activity_deadlines CHECK (
        issued_at <= eligible_until AND eligible_until <= archive_until
        AND archive_until <= issued_at + interval '37 days'
    )
);
CREATE INDEX offline_activities_session_idx
    ON offline_activities(parent_session_id,issued_at,id);

CREATE TABLE offline_activity_references (
    activity_id UUID NOT NULL,
    activity_revision BIGINT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    knowledge_revision_id UUID NOT NULL,
    node_id UUID NOT NULL,
    node_revision_id UUID NOT NULL,
    document_revision_id UUID NOT NULL,
    source_start INTEGER NOT NULL CHECK (source_start >= 0),
    source_end INTEGER NOT NULL CHECK (source_end > source_start),
    slice_text TEXT NOT NULL,
    slice_hash BYTEA NOT NULL CHECK (octet_length(slice_hash)=32),
    PRIMARY KEY(activity_id,activity_revision,ordinal),
    CONSTRAINT offline_activity_reference_activity_owner
        FOREIGN KEY(activity_id,activity_revision)
        REFERENCES offline_activities(id,revision),
    CONSTRAINT offline_activity_reference_node_owner
        FOREIGN KEY(node_revision_id,node_id,document_revision_id)
        REFERENCES knowledge_node_revisions(id,node_id,document_revision_id),
    CONSTRAINT offline_activity_reference_snapshot_owner
        FOREIGN KEY(knowledge_revision_id,document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_revision_id)
);

CREATE TABLE offline_packs (
    id UUID PRIMARY KEY,
    revision BIGINT NOT NULL CHECK (revision=1),
    prepare_device_id UUID NOT NULL REFERENCES devices(id),
    prepare_operation_id UUID NOT NULL,
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    parent_session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    response_body JSONB NOT NULL,
    response_hash BYTEA NOT NULL CHECK (octet_length(response_hash)=32),
    signer_key_id TEXT NOT NULL,
    signature BYTEA NOT NULL CHECK (octet_length(signature)=64),
    issued_at TIMESTAMPTZ NOT NULL,
    eligible_until TIMESTAMPTZ NOT NULL,
    archive_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_pack_prepare_unique UNIQUE(prepare_device_id,prepare_operation_id),
    CONSTRAINT offline_pack_deadlines CHECK (issued_at <= eligible_until AND eligible_until <= archive_until)
);

CREATE TABLE offline_submission_authorizations (
    submission_id UUID PRIMARY KEY,
    pack_id UUID NOT NULL REFERENCES offline_packs(id),
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    device_seq BIGINT NOT NULL CHECK (device_seq >= 1),
    offline_activity_id UUID NOT NULL,
    activity_revision BIGINT NOT NULL CHECK (activity_revision=1),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    credential_epoch BIGINT NOT NULL CHECK (credential_epoch >= 1),
    expected_version BIGINT NOT NULL CHECK (expected_version=0),
    authorization_payload JSONB NOT NULL,
    authorization_hash BYTEA NOT NULL CHECK (octet_length(authorization_hash)=32),
    signer_key_id TEXT NOT NULL,
    signature BYTEA NOT NULL CHECK (octet_length(signature)=64),
    eligible_until TIMESTAMPTZ NOT NULL,
    archive_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_submission_operation_unique UNIQUE(device_id,operation_id),
    CONSTRAINT offline_submission_sequence_unique UNIQUE(device_id,device_seq),
    CONSTRAINT offline_submission_identity UNIQUE(device_id,device_seq,operation_id,submission_id),
    CONSTRAINT offline_submission_activity_owner
        FOREIGN KEY(offline_activity_id,activity_revision)
        REFERENCES offline_activities(id,revision),
    CONSTRAINT offline_submission_deadlines CHECK (eligible_until <= archive_until)
);

CREATE TABLE offline_device_sequence_heads (
    device_id UUID PRIMARY KEY REFERENCES devices(id),
    high_water BIGINT NOT NULL CHECK (high_water >= 0),
    updated_at TIMESTAMPTZ NOT NULL
);
INSERT INTO offline_device_sequence_heads(device_id,high_water,updated_at)
SELECT id,0,clock_timestamp() FROM devices;

CREATE TABLE offline_device_sequence_reservations (
    device_id UUID NOT NULL,
    device_seq BIGINT NOT NULL CHECK (device_seq >= 1),
    operation_id UUID NOT NULL,
    submission_id UUID NOT NULL,
    authorization_hash BYTEA NOT NULL CHECK (octet_length(authorization_hash)=32),
    reserved_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(device_id,device_seq),
    CONSTRAINT offline_sequence_reservation_operation UNIQUE(device_id,operation_id),
    CONSTRAINT offline_sequence_reservation_submission UNIQUE(submission_id),
    CONSTRAINT offline_sequence_reservation_authorization
        FOREIGN KEY(device_id,device_seq,operation_id,submission_id)
        REFERENCES offline_submission_authorizations(device_id,device_seq,operation_id,submission_id)
);

CREATE TABLE offline_device_sequence_claims (
    device_id UUID NOT NULL,
    device_seq BIGINT NOT NULL,
    operation_id UUID NOT NULL,
    submission_id UUID NOT NULL,
    operation_hash BYTEA NOT NULL CHECK (octet_length(operation_hash)=32),
    terminal_status TEXT NOT NULL CHECK (terminal_status IN ('succeeded','rejected')),
    claimed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(device_id,device_seq),
    CONSTRAINT offline_sequence_claim_operation UNIQUE(device_id,operation_id),
    CONSTRAINT offline_sequence_claim_submission UNIQUE(submission_id),
    CONSTRAINT offline_sequence_claim_reservation
        FOREIGN KEY(device_id,device_seq)
        REFERENCES offline_device_sequence_reservations(device_id,device_seq),
    CONSTRAINT offline_sequence_claim_authorization
        FOREIGN KEY(device_id,device_seq,operation_id,submission_id)
        REFERENCES offline_submission_authorizations(device_id,device_seq,operation_id,submission_id)
);

CREATE TABLE offline_attempt_heads (
    submission_id UUID PRIMARY KEY REFERENCES offline_submission_authorizations(submission_id),
    device_id UUID NOT NULL,
    offline_activity_id UUID NOT NULL,
    activity_revision BIGINT NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'reserved','claimed_succeeded','claimed_rejected','expired','revoked'
    )),
    reserved_operation_id UUID NOT NULL,
    terminal_operation_id UUID,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version >= 0),
    first_event_seq BIGINT,
    last_event_seq BIGINT,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_attempt_head_authorization
        FOREIGN KEY(device_id,reserved_operation_id)
        REFERENCES offline_submission_authorizations(device_id,operation_id),
    CONSTRAINT offline_attempt_head_event_range CHECK (
        (state='reserved' AND terminal_operation_id IS NULL AND aggregate_version=0
            AND first_event_seq IS NULL AND last_event_seq IS NULL)
        OR
        (state<>'reserved' AND terminal_operation_id=reserved_operation_id AND aggregate_version >= 1
            AND first_event_seq >= 1 AND last_event_seq >= first_event_seq)
    )
);
CREATE UNIQUE INDEX offline_attempt_single_active_submission
    ON offline_attempt_heads(device_id,offline_activity_id,activity_revision)
    WHERE state='reserved';

ALTER TABLE learning_attempts
    ADD CONSTRAINT learning_attempt_offline_submission
        FOREIGN KEY(offline_submission_id) REFERENCES offline_attempt_heads(submission_id);

CREATE TABLE learning_activity_evidence_claims (
    activity_id UUID NOT NULL,
    activity_revision BIGINT NOT NULL,
    winning_attempt_id UUID NOT NULL,
    claim_source TEXT NOT NULL CHECK (claim_source IN ('online','offline')),
    claimed_event_seq BIGINT,
    claimed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(activity_id,activity_revision),
    CONSTRAINT learning_activity_evidence_claim_identity
        UNIQUE(activity_id,activity_revision,winning_attempt_id),
    CONSTRAINT learning_activity_evidence_claim_attempt
        FOREIGN KEY(winning_attempt_id,activity_id,activity_revision)
        REFERENCES learning_attempts(id,activity_id,activity_revision),
    CONSTRAINT learning_activity_evidence_claim_event_shape CHECK (
        claimed_event_seq IS NULL OR claimed_event_seq >= 1
    )
);

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM learning_evidence evidence
        LEFT JOIN learning_evidence_invalidations invalidation ON invalidation.evidence_id=evidence.id
        WHERE invalidation.evidence_id IS NULL
        GROUP BY evidence.activity_id,evidence.activity_revision
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'multiple active evidence rows exist for one activity revision';
    END IF;
END
$$;
INSERT INTO learning_activity_evidence_claims(
    activity_id,activity_revision,winning_attempt_id,claim_source,claimed_event_seq,claimed_at)
SELECT evidence.activity_id,evidence.activity_revision,evidence.attempt_id,'online',
       evidence.accepted_event_seq,evidence.received_at
FROM learning_evidence evidence
LEFT JOIN learning_evidence_invalidations invalidation ON invalidation.evidence_id=evidence.id
WHERE invalidation.evidence_id IS NULL;

ALTER TABLE learning_evidence
    ADD CONSTRAINT learning_evidence_winning_attempt
        FOREIGN KEY(activity_id,activity_revision,attempt_id)
        REFERENCES learning_activity_evidence_claims(activity_id,activity_revision,winning_attempt_id);

CREATE TABLE offline_operation_statuses (
    ticket_id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    submission_id UUID NOT NULL REFERENCES offline_attempt_heads(submission_id),
    ingest_receipt_id UUID NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_operation_status_operation UNIQUE(device_id,operation_id)
);
CREATE TABLE offline_operation_status_revisions (
    id UUID PRIMARY KEY,
    ticket_id UUID NOT NULL REFERENCES offline_operation_statuses(ticket_id),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    archive_status TEXT NOT NULL CHECK (archive_status IN ('archived_succeeded','archived_rejected')),
    assessment_status TEXT NOT NULL CHECK (assessment_status IN (
        'not_requested','queued','processing','pending_retry','completed','failed'
    )),
    evidence_status TEXT NOT NULL CHECK (evidence_status IN (
        'accepted','provisional','pending_evaluation','not_eligible','not_applicable','unchanged'
    )),
    reason_codes TEXT[] NOT NULL DEFAULT '{}',
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version >= 1),
    first_event_seq BIGINT NOT NULL CHECK (first_event_seq >= 1),
    last_event_seq BIGINT NOT NULL CHECK (last_event_seq >= first_event_seq),
    projection_as_of_event_seq BIGINT NOT NULL CHECK (projection_as_of_event_seq >= last_event_seq),
    assessment_id UUID REFERENCES learning_assessments(id),
    evidence_id UUID REFERENCES learning_evidence(id),
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_operation_status_revision_unique UNIQUE(ticket_id,revision),
    CONSTRAINT offline_operation_status_revision_identity UNIQUE(id,ticket_id,revision),
    CONSTRAINT offline_operation_status_combination CHECK (
        (archive_status='archived_rejected' AND assessment_status='not_requested' AND evidence_status='unchanged')
        OR
        (archive_status='archived_succeeded' AND assessment_status='not_requested'
            AND evidence_status IN ('provisional','not_eligible','not_applicable'))
        OR
        (archive_status='archived_succeeded' AND assessment_status='queued'
            AND evidence_status='pending_evaluation')
        OR
        (archive_status='archived_succeeded' AND assessment_status='completed'
            AND evidence_status IN ('accepted','provisional','not_eligible'))
    )
);
CREATE TABLE offline_operation_status_heads (
    ticket_id UUID PRIMARY KEY REFERENCES offline_operation_statuses(ticket_id),
    current_revision_id UUID NOT NULL UNIQUE,
    current_revision BIGINT NOT NULL CHECK (current_revision >= 1),
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_operation_status_head_revision
        FOREIGN KEY(current_revision_id,ticket_id,current_revision)
        REFERENCES offline_operation_status_revisions(id,ticket_id,revision)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE offline_evaluation_jobs (
    id UUID PRIMARY KEY,
    submission_id UUID NOT NULL UNIQUE REFERENCES offline_attempt_heads(submission_id),
    attempt_id UUID NOT NULL UNIQUE REFERENCES learning_attempts(id),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    status TEXT NOT NULL CHECK (status IN ('queued','processing','pending_retry','completed','failed')),
    frozen_request JSONB NOT NULL,
    outbox_id UUID NOT NULL UNIQUE REFERENCES outbox_messages(id) DEFERRABLE INITIALLY DEFERRED,
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    available_at TIMESTAMPTZ NOT NULL,
    retry_deadline TIMESTAMPTZ NOT NULL,
    last_error_category TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_evaluation_job_lease_shape CHECK (
        (status='processing' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status<>'processing' AND lease_token IS NULL AND lease_expires_at IS NULL)
    ),
    CONSTRAINT offline_evaluation_job_deadline CHECK (retry_deadline >= created_at)
);

CREATE TABLE offline_device_possessions (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id),
    learner_generation BIGINT NOT NULL CHECK (learner_generation >= 1),
    first_pack_id UUID NOT NULL REFERENCES offline_packs(id),
    first_seen_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT offline_device_possession_once UNIQUE(device_id,learner_generation)
);

ALTER TABLE privacy_erasure_step_receipts
    DROP CONSTRAINT privacy_erasure_step_receipts_store_kind_check,
    ADD CONSTRAINT privacy_erasure_step_receipts_store_kind_check CHECK (store_kind IN (
        'identity_metadata','knowledge_content','knowledge_index','knowledge_artifacts',
        'learning_event_payload','learning_typed_payload','tutoring_payload','inbox_outbox',
        'projection_generations','memory_candidate_delivery','process_cache','nocturne_paths',
        'nocturne_orphan_history','nocturne_snapshot_changeset','managed_backup','external_provider',
        'offline_device_cache'
    ));
DO $$
DECLARE
    erasure RECORD;
    receipt_id UUID;
BEGIN
    FOR erasure IN
        SELECT value.id,value.requested_at,head.updated_at
        FROM privacy_erasures value
        JOIN privacy_erasure_heads head ON head.erasure_id=value.id
        WHERE NOT EXISTS (
            SELECT 1 FROM privacy_erasure_receipt_heads receipt_head
            WHERE receipt_head.erasure_id=value.id
              AND receipt_head.store_kind='offline_device_cache'
        )
    LOOP
        receipt_id := gen_random_uuid();
        INSERT INTO privacy_erasure_step_receipts(
            id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,
            status,stable_reason,verification_method)
        VALUES(
            receipt_id,erasure.id,'offline_device_cache',1,
            sha256(convert_to(erasure.id::text || E'\noffline_device_cache\nlegacy-no-possession','UTF8')),
            erasure.requested_at,erasure.updated_at,'succeeded',
            'no_offline_device_possession','pre_offline_sync_migration');
        INSERT INTO privacy_erasure_receipt_heads(
            erasure_id,store_kind,current_receipt_id,current_version,updated_at)
        VALUES(erasure.id,'offline_device_cache',receipt_id,1,erasure.updated_at);
    END LOOP;
END
$$;

CREATE TABLE privacy_offline_device_children (
    id UUID PRIMARY KEY,
    erasure_id UUID NOT NULL REFERENCES privacy_erasures(id),
    device_id UUID NOT NULL REFERENCES devices(id),
    source_generation BIGINT NOT NULL CHECK (source_generation >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT privacy_offline_device_child_once UNIQUE(erasure_id,device_id),
    CONSTRAINT privacy_offline_device_child_identity UNIQUE(id,erasure_id,device_id,source_generation)
);
CREATE TABLE privacy_offline_device_child_revisions (
    id UUID PRIMARY KEY,
    child_id UUID NOT NULL REFERENCES privacy_offline_device_children(id),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    status TEXT NOT NULL CHECK (status IN ('pending','succeeded','unknown','failed')),
    challenge_key_version BIGINT NOT NULL CHECK (challenge_key_version >= 1),
    challenge_hash BYTEA NOT NULL CHECK (octet_length(challenge_hash)=32),
    stable_reason TEXT NOT NULL CHECK (char_length(stable_reason) BETWEEN 1 AND 1000),
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT privacy_offline_device_child_revision_unique UNIQUE(child_id,revision),
    CONSTRAINT privacy_offline_device_child_revision_identity UNIQUE(id,child_id,revision)
);
CREATE TABLE privacy_offline_device_child_heads (
    child_id UUID PRIMARY KEY REFERENCES privacy_offline_device_children(id),
    current_revision_id UUID NOT NULL UNIQUE,
    current_revision BIGINT NOT NULL CHECK (current_revision >= 1),
    status TEXT NOT NULL CHECK (status IN ('pending','succeeded','unknown','failed')),
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT privacy_offline_device_child_head_revision
        FOREIGN KEY(current_revision_id,child_id,current_revision)
        REFERENCES privacy_offline_device_child_revisions(id,child_id,revision)
        DEFERRABLE INITIALLY DEFERRED
);

UPDATE device_tokens AS token
SET scopes = ARRAY(
    SELECT DISTINCT scope
    FROM unnest(token.scopes || ARRAY['privacy:device']::TEXT[]) AS scope
    ORDER BY scope
)
WHERE token.revoked_at IS NULL
  AND EXISTS (
      SELECT 1 FROM devices device
      WHERE device.id=token.device_id AND device.revoked_at IS NULL
  );

CREATE FUNCTION initialize_offline_device_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO offline_device_credentials(device_id,credential_epoch,created_at,updated_at)
    VALUES(NEW.id,1,NEW.created_at,NEW.created_at);
    INSERT INTO offline_device_sequence_heads(device_id,high_water,updated_at)
    VALUES(NEW.id,0,NEW.created_at);
    RETURN NEW;
END $$;
CREATE TRIGGER devices_initialize_offline_state
AFTER INSERT ON devices
FOR EACH ROW EXECUTE FUNCTION initialize_offline_device_state();

CREATE FUNCTION protect_offline_prepare_claim_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('learning') THEN
        RETURN NEW;
    END IF;
    IF ROW(NEW.device_id,NEW.operation_id,NEW.request_hash,NEW.learner_generation,NEW.request_body,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.device_id,OLD.operation_id,OLD.request_hash,OLD.learner_generation,OLD.request_body,OLD.created_at) THEN
        RAISE EXCEPTION 'offline prepare claim identity is immutable';
    END IF;
    IF OLD.status IN ('published','rejected') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'offline prepare claim terminal state is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER offline_prepare_claim_state_guard
BEFORE UPDATE ON offline_prepare_claims
FOR EACH ROW EXECUTE FUNCTION protect_offline_prepare_claim_state();

CREATE FUNCTION protect_offline_attempt_head_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.submission_id,NEW.device_id,NEW.offline_activity_id,NEW.activity_revision,NEW.reserved_operation_id)
       IS DISTINCT FROM ROW(OLD.submission_id,OLD.device_id,OLD.offline_activity_id,OLD.activity_revision,OLD.reserved_operation_id) THEN
        RAISE EXCEPTION 'offline attempt head identity is immutable';
    END IF;
    IF OLD.state<>'reserved' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'offline attempt terminal state is immutable';
    END IF;
    IF OLD.state='reserved' AND NEW.state='reserved' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'offline attempt reservation is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER offline_attempt_head_state_guard
BEFORE UPDATE ON offline_attempt_heads
FOR EACH ROW EXECUTE FUNCTION protect_offline_attempt_head_state();

CREATE FUNCTION protect_offline_operation_status_head() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.ticket_id<>OLD.ticket_id OR NEW.current_revision<=OLD.current_revision THEN
        RAISE EXCEPTION 'offline operation status head cannot regress';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER offline_operation_status_head_guard
BEFORE UPDATE ON offline_operation_status_heads
FOR EACH ROW EXECUTE FUNCTION protect_offline_operation_status_head();

CREATE FUNCTION protect_privacy_offline_child_head() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.child_id<>OLD.child_id OR NEW.current_revision<=OLD.current_revision THEN
        RAISE EXCEPTION 'privacy offline child head cannot regress';
    END IF;
    IF OLD.status='succeeded' THEN
        RAISE EXCEPTION 'privacy offline child success is terminal';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER privacy_offline_device_child_head_guard
BEFORE UPDATE ON privacy_offline_device_child_heads
FOR EACH ROW EXECUTE FUNCTION protect_privacy_offline_child_head();

DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'offline_device_credentials','offline_activities','offline_activity_references','offline_packs',
        'offline_submission_authorizations','offline_device_sequence_reservations',
        'offline_device_sequence_claims','learning_activity_evidence_claims',
        'offline_operation_statuses','offline_operation_status_revisions',
        'offline_device_possessions'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_learning_history_mutation()',
            table_name||'_immutable',table_name
        );
    END LOOP;
    FOREACH table_name IN ARRAY ARRAY[
        'privacy_offline_device_children','privacy_offline_device_child_revisions'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_privacy_history_mutation()',
            table_name||'_immutable',table_name
        );
    END LOOP;
END $$;

DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'offline_device_credentials','offline_prepare_claims','offline_activities',
        'offline_activity_references','offline_packs','offline_submission_authorizations',
        'offline_device_sequence_heads','offline_device_sequence_reservations',
        'offline_device_sequence_claims','offline_attempt_heads','learning_activity_evidence_claims',
        'offline_operation_statuses','offline_operation_status_revisions',
        'offline_operation_status_heads','offline_evaluation_jobs','offline_device_possessions'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write(%L)',
            table_name||'_privacy_write_gate',table_name,'learning'
        );
    END LOOP;
END $$;
