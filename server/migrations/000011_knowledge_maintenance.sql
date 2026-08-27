-- Pairing profiles are frozen into each one-time code so exchange cannot choose or
-- upgrade the scopes issued to the resulting token. Existing codes retain the
-- historical user behavior and gain explicit learning approval authority.
ALTER TABLE pairing_codes ADD COLUMN scopes TEXT[];
UPDATE pairing_codes SET scopes=ARRAY[
    'devices:read','devices:manage','model:probe','knowledge:read','knowledge:write',
    'learning:read','learning:write','learning:approve','memory:read','memory:write',
    'privacy:read','privacy:device'
];
ALTER TABLE pairing_codes ALTER COLUMN scopes SET NOT NULL;
ALTER TABLE pairing_codes ADD CONSTRAINT pairing_codes_scopes_nonempty CHECK (
    cardinality(scopes)>0
    AND array_position(scopes,NULL) IS NULL
    AND array_position(scopes,'') IS NULL
);

-- Knowledge maintenance proposals freeze server-computed analysis and a prepared canonical
-- revision. The maintenance path does not write evidence, events, mastery, or review projections;
-- an applied revision may atomically create inert learning-owned carryover proposals below.
CREATE TABLE knowledge_maintenance_proposals (
    proposal_id UUID PRIMARY KEY,
    request_id UUID NOT NULL UNIQUE,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash)=32),
    kind TEXT NOT NULL CHECK (kind IN ('candidate','rollback')),
    status TEXT NOT NULL CHECK (status IN ('open','applied','rejected','stale','redacted')),
    base_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    rollback_target_revision_id UUID REFERENCES knowledge_revisions(id),
    planned_revision_id UUID NOT NULL,
    planned_revision_no BIGINT NOT NULL CHECK (planned_revision_no>=1),
    planned_manifest_hash BYTEA NOT NULL CHECK (octet_length(planned_manifest_hash)=32),
    current_revision_id UUID REFERENCES knowledge_revisions(id),
    applied_revision_id UUID REFERENCES knowledge_revisions(id),
    basis_hash BYTEA NOT NULL CHECK (octet_length(basis_hash)=32),
    knowledge_generation BIGINT NOT NULL CHECK (knowledge_generation>=1),
    evidence_generation BIGINT NOT NULL CHECK (evidence_generation>=1),
    canonicalizer_version TEXT NOT NULL,
    identity_policy_version TEXT NOT NULL,
    diff_version TEXT NOT NULL,
    risk_version TEXT NOT NULL,
    auto_policy_version TEXT NOT NULL,
    record JSONB NOT NULL,
    prepared_commit JSONB NOT NULL,
    created_by_device_id UUID NOT NULL REFERENCES devices(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    redacted_at TIMESTAMPTZ,
    CONSTRAINT knowledge_maintenance_proposal_kind_shape CHECK (
        (kind='candidate' AND rollback_target_revision_id IS NULL)
        OR (kind='rollback' AND rollback_target_revision_id IS NOT NULL)
    ),
    CONSTRAINT knowledge_maintenance_proposal_status_shape CHECK (
        (status='open' AND current_revision_id IS NULL AND applied_revision_id IS NULL AND redacted_at IS NULL)
        OR (status='applied' AND applied_revision_id=planned_revision_id AND redacted_at IS NULL)
        OR (status IN ('rejected','stale') AND applied_revision_id IS NULL AND redacted_at IS NULL)
        OR (status='redacted' AND applied_revision_id IS NULL AND redacted_at IS NOT NULL)
        OR (status IN ('applied','rejected','stale') AND redacted_at IS NOT NULL)
    ),
    CONSTRAINT knowledge_maintenance_proposal_time_order CHECK (updated_at>=created_at)
);
CREATE INDEX knowledge_maintenance_proposal_status_order
    ON knowledge_maintenance_proposals(status,created_at,proposal_id);
CREATE INDEX knowledge_maintenance_proposal_base
    ON knowledge_maintenance_proposals(base_revision_id,created_at,proposal_id);

CREATE TABLE knowledge_maintenance_decisions (
    decision_id UUID PRIMARY KEY,
    proposal_id UUID NOT NULL UNIQUE REFERENCES knowledge_maintenance_proposals(proposal_id),
    operation_id UUID UNIQUE,
    requested_decision TEXT NOT NULL CHECK (requested_decision IN ('auto','approve','reject')),
    outcome TEXT NOT NULL CHECK (outcome IN ('applied','rejected','stale')),
    reason TEXT NOT NULL CHECK (char_length(reason)>=1),
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT knowledge_maintenance_decision_shape CHECK (
        (requested_decision='auto' AND outcome='applied' AND operation_id IS NULL)
        OR (requested_decision='approve' AND outcome IN ('applied','stale') AND operation_id IS NOT NULL)
        OR (requested_decision='reject' AND outcome IN ('rejected','stale') AND operation_id IS NOT NULL)
    )
);

CREATE TABLE knowledge_maintenance_operations (
    operation_id UUID PRIMARY KEY,
    operation_type TEXT NOT NULL CHECK (operation_type IN ('create','decide')),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash)=32),
    proposal_id UUID NOT NULL REFERENCES knowledge_maintenance_proposals(proposal_id),
    completed_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE knowledge_revision_origins (
    revision_id UUID PRIMARY KEY REFERENCES knowledge_revisions(id),
    proposal_id UUID NOT NULL UNIQUE REFERENCES knowledge_maintenance_proposals(proposal_id),
    origin_version TEXT NOT NULL,
    origin_kind TEXT NOT NULL CHECK (origin_kind IN ('candidate','rollback')),
    base_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    rollback_target_revision_id UUID REFERENCES knowledge_revisions(id),
    basis_hash BYTEA NOT NULL CHECK (octet_length(basis_hash)=32),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT knowledge_revision_origin_kind_shape CHECK (
        (origin_kind='candidate' AND rollback_target_revision_id IS NULL)
        OR (origin_kind='rollback' AND rollback_target_revision_id IS NOT NULL)
    )
);

CREATE FUNCTION protect_knowledge_maintenance_proposal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('knowledge') THEN
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'knowledge maintenance proposals cannot be deleted';
    END IF;
    IF OLD.status<>'open' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'knowledge maintenance proposal terminal state is immutable';
    END IF;
    IF OLD.status='open' AND NEW.status NOT IN ('open','applied','rejected','stale','redacted') THEN
        RAISE EXCEPTION 'knowledge maintenance proposal has an invalid transition';
    END IF;
    IF ROW(
        NEW.proposal_id,NEW.request_id,NEW.request_hash,NEW.kind,NEW.base_revision_id,
        NEW.rollback_target_revision_id,NEW.planned_revision_id,NEW.planned_revision_no,
        NEW.planned_manifest_hash,NEW.basis_hash,NEW.knowledge_generation,NEW.evidence_generation,
        NEW.canonicalizer_version,NEW.identity_policy_version,NEW.diff_version,NEW.risk_version,
        NEW.auto_policy_version,NEW.record,NEW.prepared_commit,NEW.created_by_device_id,NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.proposal_id,OLD.request_id,OLD.request_hash,OLD.kind,OLD.base_revision_id,
        OLD.rollback_target_revision_id,OLD.planned_revision_id,OLD.planned_revision_no,
        OLD.planned_manifest_hash,OLD.basis_hash,OLD.knowledge_generation,OLD.evidence_generation,
        OLD.canonicalizer_version,OLD.identity_policy_version,OLD.diff_version,OLD.risk_version,
        OLD.auto_policy_version,OLD.record,OLD.prepared_commit,OLD.created_by_device_id,OLD.created_at
    ) THEN
        RAISE EXCEPTION 'knowledge maintenance proposal basis is immutable';
    END IF;
    IF OLD.status='open' AND NEW.status='open' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'knowledge maintenance open proposal is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER knowledge_maintenance_proposals_state_guard
    BEFORE UPDATE OR DELETE ON knowledge_maintenance_proposals
    FOR EACH ROW EXECUTE FUNCTION protect_knowledge_maintenance_proposal();

CREATE FUNCTION reject_knowledge_maintenance_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('knowledge') THEN
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' OR NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'knowledge maintenance history is append-only';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER knowledge_maintenance_decisions_immutable
    BEFORE UPDATE OR DELETE ON knowledge_maintenance_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_knowledge_maintenance_history_mutation();
CREATE TRIGGER knowledge_maintenance_operations_immutable
    BEFORE UPDATE OR DELETE ON knowledge_maintenance_operations
    FOR EACH ROW EXECUTE FUNCTION reject_knowledge_maintenance_history_mutation();
CREATE TRIGGER knowledge_revision_origins_immutable
    BEFORE UPDATE OR DELETE ON knowledge_revision_origins
    FOR EACH ROW EXECUTE FUNCTION reject_knowledge_maintenance_history_mutation();

DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'knowledge_maintenance_proposals','knowledge_maintenance_decisions',
        'knowledge_maintenance_operations','knowledge_revision_origins'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write(%L)',
            table_name||'_privacy_write_gate',table_name,'knowledge'
        );
    END LOOP;
END $$;

-- Carryover is learning-owned. Knowledge application may only insert inert proposals in
-- the same transaction as the canonical revision; decisions remain separate learning events.
ALTER TABLE learning_aggregate_heads
    DROP CONSTRAINT learning_aggregate_heads_aggregate_type_check,
    ADD CONSTRAINT learning_aggregate_heads_aggregate_type_check
        CHECK (aggregate_type IN ('goal','session','privacy','offline_attempt','evidence_carryover'));
ALTER TABLE learning_events
    DROP CONSTRAINT learning_events_aggregate_type_check,
    ADD CONSTRAINT learning_events_aggregate_type_check
        CHECK (aggregate_type IN ('goal','session','privacy','offline_attempt','evidence_carryover'));

CREATE TABLE learning_evidence_carryover_proposals (
    proposal_id UUID PRIMARY KEY,
    carryover_key BYTEA NOT NULL UNIQUE CHECK (octet_length(carryover_key)=32),
    knowledge_proposal_id UUID NOT NULL REFERENCES knowledge_maintenance_proposals(proposal_id),
    status TEXT NOT NULL CHECK (status IN ('open','approved','rejected','stale','redacted')),
    source_evidence_id UUID REFERENCES learning_evidence(id),
    source_knowledge_revision_id UUID REFERENCES knowledge_revisions(id),
    source_node_revision_id UUID REFERENCES knowledge_node_revisions(id),
    target_knowledge_revision_id UUID REFERENCES knowledge_revisions(id),
    knowledge_basis_hash BYTEA NOT NULL CHECK (octet_length(knowledge_basis_hash)=32),
    evidence_fingerprint BYTEA NOT NULL CHECK (octet_length(evidence_fingerprint)=32),
    candidate_fingerprint BYTEA NOT NULL CHECK (octet_length(candidate_fingerprint)=32),
    basis_fingerprint BYTEA NOT NULL CHECK (octet_length(basis_fingerprint)=32),
    knowledge_generation BIGINT NOT NULL CHECK (knowledge_generation>=1),
    learning_generation BIGINT NOT NULL CHECK (learning_generation>=1),
    policy_version TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    redacted_at TIMESTAMPTZ,
    CONSTRAINT learning_evidence_carryover_proposal_status_shape CHECK (
        (status='open' AND redacted_at IS NULL)
        OR (status IN ('approved','rejected','stale') AND redacted_at IS NULL)
        OR (status='redacted' AND redacted_at IS NOT NULL)
        OR (status IN ('approved','rejected','stale') AND redacted_at IS NOT NULL)
    ),
    CONSTRAINT learning_evidence_carryover_proposal_reference_shape CHECK (
        redacted_at IS NOT NULL
        OR (source_evidence_id IS NOT NULL AND source_knowledge_revision_id IS NOT NULL
            AND source_node_revision_id IS NOT NULL AND target_knowledge_revision_id IS NOT NULL)
    ),
    CONSTRAINT learning_evidence_carryover_proposal_time_order CHECK (updated_at>=created_at)
);
CREATE UNIQUE INDEX learning_evidence_carryover_proposal_source
    ON learning_evidence_carryover_proposals(knowledge_proposal_id,source_evidence_id)
    WHERE source_evidence_id IS NOT NULL;
CREATE INDEX learning_evidence_carryover_proposal_status_order
    ON learning_evidence_carryover_proposals(status,created_at,proposal_id);

CREATE TABLE learning_evidence_carryover_candidates (
    proposal_id UUID NOT NULL REFERENCES learning_evidence_carryover_proposals(proposal_id),
    ordinal INTEGER NOT NULL CHECK (ordinal>=0),
    target_knowledge_revision_id UUID REFERENCES knowledge_revisions(id),
    target_node_id UUID REFERENCES knowledge_nodes(id),
    target_node_revision_id UUID REFERENCES knowledge_node_revisions(id),
    target_document_revision_id UUID REFERENCES knowledge_document_revisions(id),
    PRIMARY KEY(proposal_id,ordinal),
    CONSTRAINT learning_evidence_carryover_candidate_reference_shape CHECK (
        (target_knowledge_revision_id IS NULL AND target_node_id IS NULL
            AND target_node_revision_id IS NULL AND target_document_revision_id IS NULL)
        OR (target_knowledge_revision_id IS NOT NULL AND target_node_id IS NOT NULL
            AND target_node_revision_id IS NOT NULL AND target_document_revision_id IS NOT NULL)
    ),
    CONSTRAINT learning_evidence_carryover_candidate_node_owner
        FOREIGN KEY(target_node_revision_id,target_node_id,target_document_revision_id)
        REFERENCES knowledge_node_revisions(id,node_id,document_revision_id) MATCH FULL,
    CONSTRAINT learning_evidence_carryover_candidate_snapshot_owner
        FOREIGN KEY(target_knowledge_revision_id,target_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_revision_id) MATCH FULL
);
CREATE UNIQUE INDEX learning_evidence_carryover_candidate_target
    ON learning_evidence_carryover_candidates(proposal_id,target_node_revision_id)
    WHERE target_node_revision_id IS NOT NULL;

CREATE TABLE learning_evidence_carryover_decisions (
    decision_id UUID PRIMARY KEY,
    proposal_id UUID NOT NULL UNIQUE REFERENCES learning_evidence_carryover_proposals(proposal_id),
    operation_id UUID NOT NULL UNIQUE,
    requested_decision TEXT NOT NULL CHECK (requested_decision IN ('approve','reject')),
    outcome TEXT NOT NULL CHECK (outcome IN ('approved','rejected','stale')),
    reason TEXT NOT NULL CHECK (char_length(reason)>=1),
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    event_id UUID NOT NULL UNIQUE REFERENCES learning_events(id),
    event_seq BIGINT NOT NULL UNIQUE REFERENCES learning_events(event_seq),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_evidence_carryover_decision_shape CHECK (
        (requested_decision='approve' AND outcome IN ('approved','stale'))
        OR (requested_decision='reject' AND outcome='rejected')
    )
);

CREATE TABLE learning_evidence_carryover_operations (
    operation_id UUID PRIMARY KEY,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash)=32),
    proposal_id UUID NOT NULL REFERENCES learning_evidence_carryover_proposals(proposal_id),
    requested_decision TEXT NOT NULL CHECK (requested_decision IN ('approve','reject')),
    completed_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE learning_evidence_carryover_links (
    link_id UUID PRIMARY KEY,
    proposal_id UUID NOT NULL REFERENCES learning_evidence_carryover_proposals(proposal_id),
    decision_id UUID NOT NULL REFERENCES learning_evidence_carryover_decisions(decision_id),
    source_evidence_id UUID REFERENCES learning_evidence(id),
    target_knowledge_revision_id UUID REFERENCES knowledge_revisions(id),
    target_node_id UUID REFERENCES knowledge_nodes(id),
    target_node_revision_id UUID REFERENCES knowledge_node_revisions(id),
    target_document_revision_id UUID REFERENCES knowledge_document_revisions(id),
    event_id UUID NOT NULL REFERENCES learning_events(id),
    event_seq BIGINT NOT NULL REFERENCES learning_events(event_seq),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_evidence_carryover_link_reference_shape CHECK (
        (source_evidence_id IS NULL AND target_knowledge_revision_id IS NULL
            AND target_node_id IS NULL AND target_node_revision_id IS NULL
            AND target_document_revision_id IS NULL)
        OR (source_evidence_id IS NOT NULL AND target_knowledge_revision_id IS NOT NULL
            AND target_node_id IS NOT NULL AND target_node_revision_id IS NOT NULL
            AND target_document_revision_id IS NOT NULL)
    ),
    CONSTRAINT learning_evidence_carryover_link_node_owner
        FOREIGN KEY(target_node_revision_id,target_node_id,target_document_revision_id)
        REFERENCES knowledge_node_revisions(id,node_id,document_revision_id) MATCH FULL,
    CONSTRAINT learning_evidence_carryover_link_snapshot_owner
        FOREIGN KEY(target_knowledge_revision_id,target_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id,document_revision_id) MATCH FULL
);
CREATE UNIQUE INDEX learning_evidence_carryover_link_target
    ON learning_evidence_carryover_links(proposal_id,target_node_revision_id)
    WHERE target_node_revision_id IS NOT NULL;

CREATE TABLE learning_projection_carryovers (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    proposal_id UUID NOT NULL,
    approved_event_seq BIGINT NOT NULL CHECK (approved_event_seq>=1),
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id,proposal_id)
);
CREATE INDEX learning_projection_carryovers_order
    ON learning_projection_carryovers(generation_id,approved_event_seq,proposal_id);

CREATE FUNCTION protect_learning_evidence_carryover_proposal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF privacy_owner_scrub_permitted('learning') THEN
        IF TG_OP='DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN RAISE EXCEPTION 'learning evidence carryover proposals cannot be deleted'; END IF;
    IF OLD.status<>'open' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'learning evidence carryover proposal terminal state is immutable';
    END IF;
    IF OLD.status='open' AND NEW.status NOT IN ('open','approved','rejected','stale','redacted') THEN
        RAISE EXCEPTION 'learning evidence carryover proposal has an invalid transition';
    END IF;
    IF ROW(NEW.proposal_id,NEW.carryover_key,NEW.knowledge_proposal_id,NEW.source_evidence_id,
        NEW.source_knowledge_revision_id,NEW.source_node_revision_id,NEW.target_knowledge_revision_id,
        NEW.knowledge_basis_hash,NEW.evidence_fingerprint,NEW.candidate_fingerprint,
        NEW.basis_fingerprint,NEW.knowledge_generation,NEW.learning_generation,
        NEW.policy_version,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.proposal_id,OLD.carryover_key,OLD.knowledge_proposal_id,OLD.source_evidence_id,
        OLD.source_knowledge_revision_id,OLD.source_node_revision_id,OLD.target_knowledge_revision_id,
        OLD.knowledge_basis_hash,OLD.evidence_fingerprint,OLD.candidate_fingerprint,
        OLD.basis_fingerprint,OLD.knowledge_generation,OLD.learning_generation,
        OLD.policy_version,OLD.created_at) THEN
        RAISE EXCEPTION 'learning evidence carryover proposal basis is immutable';
    END IF;
    IF OLD.status='open' AND NEW.status='open' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'learning evidence carryover open proposal is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER learning_evidence_carryover_proposals_state_guard
    BEFORE UPDATE OR DELETE ON learning_evidence_carryover_proposals
    FOR EACH ROW EXECUTE FUNCTION protect_learning_evidence_carryover_proposal();

DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'learning_evidence_carryover_candidates','learning_evidence_carryover_decisions',
        'learning_evidence_carryover_operations','learning_evidence_carryover_links'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_learning_history_mutation()',
            table_name||'_immutable',table_name
        );
    END LOOP;
    FOREACH table_name IN ARRAY ARRAY[
        'learning_evidence_carryover_proposals','learning_evidence_carryover_candidates',
        'learning_evidence_carryover_decisions','learning_evidence_carryover_operations',
        'learning_evidence_carryover_links','learning_projection_carryovers'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write(%L)',
            table_name||'_privacy_write_gate',table_name,'learning'
        );
    END LOOP;
END $$;
