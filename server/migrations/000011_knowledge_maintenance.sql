-- Knowledge maintenance proposals freeze server-computed analysis and a prepared canonical
-- revision. No learning-owned row is written by this migration or its application path.
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
