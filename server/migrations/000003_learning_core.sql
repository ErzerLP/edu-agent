ALTER TABLE knowledge_snapshot_documents
    ADD CONSTRAINT knowledge_snapshot_revision_document_unique
    UNIQUE (knowledge_revision_id, document_revision_id);
ALTER TABLE knowledge_node_revisions
    ADD CONSTRAINT knowledge_node_revision_full_identity_unique
    UNIQUE (id, node_id, document_revision_id);

CREATE TABLE learning_event_clock (
    singleton_id SMALLINT PRIMARY KEY CHECK (singleton_id = 1),
    current_event_seq BIGINT NOT NULL DEFAULT 0 CHECK (current_event_seq >= 0),
    updated_at TIMESTAMPTZ NOT NULL
);
INSERT INTO learning_event_clock(singleton_id, current_event_seq, updated_at) VALUES (1, 0, now());

CREATE TABLE learning_aggregate_heads (
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('goal', 'session')),
    aggregate_id UUID NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version >= 0),
    last_event_seq BIGINT NOT NULL DEFAULT 0 CHECK (last_event_seq >= 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (aggregate_type, aggregate_id)
);

CREATE TABLE learning_inbox (
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('goal', 'session')),
    aggregate_id UUID NOT NULL,
    terminal_status TEXT NOT NULL CHECK (terminal_status IN ('succeeded', 'rejected')),
    result JSONB NOT NULL,
    first_event_seq BIGINT CHECK (first_event_seq >= 1),
    last_event_seq BIGINT CHECK (last_event_seq >= first_event_seq),
    completed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (device_id, operation_id)
);

CREATE TABLE learning_event_payloads (
    id UUID PRIMARY KEY,
    payload JSONB NOT NULL,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    redacted_at TIMESTAMPTZ,
    CONSTRAINT learning_event_payload_hash_identity UNIQUE(id, payload_hash)
);

CREATE TABLE learning_events (
    event_seq BIGINT PRIMARY KEY CHECK (event_seq >= 1),
    id UUID NOT NULL UNIQUE,
    event_type TEXT NOT NULL,
    event_schema_version INTEGER NOT NULL CHECK (event_schema_version >= 1),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('goal', 'session')),
    aggregate_id UUID NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version >= 1),
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    operation_ordinal INTEGER NOT NULL CHECK (operation_ordinal >= 0),
    received_at TIMESTAMPTZ NOT NULL,
    occurred_at TIMESTAMPTZ,
    payload_id UUID NOT NULL,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT learning_event_aggregate_version_unique
        UNIQUE (aggregate_type, aggregate_id, aggregate_version),
    CONSTRAINT learning_event_operation_ordinal_unique
        UNIQUE (device_id, operation_id, operation_ordinal),
    CONSTRAINT learning_event_payload_owner FOREIGN KEY(payload_id, payload_hash)
        REFERENCES learning_event_payloads(id, payload_hash)
);
CREATE INDEX learning_events_aggregate_idx
    ON learning_events(aggregate_type, aggregate_id, event_seq);
CREATE INDEX learning_events_operation_idx
    ON learning_events(device_id, operation_id, operation_ordinal);

CREATE TABLE learning_goal_revisions (
    id UUID PRIMARY KEY,
    goal_id UUID NOT NULL,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    goal_text TEXT NOT NULL CHECK (char_length(goal_text) BETWEEN 1 AND 4000),
    source TEXT NOT NULL CHECK (char_length(source) BETWEEN 1 AND 200),
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    created_at TIMESTAMPTZ NOT NULL,
    previous_revision_id UUID,
    previous_goal_id UUID,
    previous_revision BIGINT,
    CONSTRAINT learning_goal_revision_number_unique UNIQUE(goal_id, revision),
    CONSTRAINT learning_goal_revision_identity_unique UNIQUE(id, goal_id, revision),
    CONSTRAINT learning_goal_previous_unique UNIQUE(previous_revision_id),
    CONSTRAINT learning_goal_previous_shape CHECK (
        (revision = 1 AND previous_revision_id IS NULL AND previous_goal_id IS NULL AND previous_revision IS NULL)
        OR
        (revision > 1 AND previous_revision_id IS NOT NULL AND previous_goal_id IS NOT NULL AND previous_revision IS NOT NULL AND previous_goal_id = goal_id AND previous_revision = revision - 1)
    ),
    CONSTRAINT learning_goal_previous_lineage FOREIGN KEY(previous_revision_id, previous_goal_id, previous_revision)
        REFERENCES learning_goal_revisions(id, goal_id, revision)
);

CREATE TABLE learning_route_revisions (
    id UUID PRIMARY KEY,
    route_id UUID NOT NULL,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    goal_revision_id UUID NOT NULL REFERENCES learning_goal_revisions(id),
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    route_policy_version TEXT NOT NULL,
    source_proposal_id UUID,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_route_revision_number_unique UNIQUE(route_id, revision),
    CONSTRAINT learning_route_revision_owner_unique UNIQUE(id, goal_revision_id, knowledge_revision_id)
);

CREATE TABLE learning_route_steps (
    id UUID PRIMARY KEY,
    route_revision_id UUID NOT NULL REFERENCES learning_route_revisions(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    knowledge_revision_id UUID NOT NULL,
    node_id UUID NOT NULL,
    node_revision_id UUID NOT NULL,
    document_revision_id UUID NOT NULL,
    teaching_intent TEXT NOT NULL CHECK (char_length(teaching_intent) BETWEEN 1 AND 1000),
    completion_condition TEXT NOT NULL CHECK (char_length(completion_condition) BETWEEN 1 AND 1000),
    CONSTRAINT learning_route_step_order_unique UNIQUE(route_revision_id, ordinal),
    CONSTRAINT learning_route_step_node_unique UNIQUE(route_revision_id, node_revision_id),
    CONSTRAINT learning_route_step_owner_unique UNIQUE(id, route_revision_id, knowledge_revision_id),
    CONSTRAINT learning_route_step_target_unique UNIQUE(id, route_revision_id, knowledge_revision_id, node_id, node_revision_id),
    CONSTRAINT learning_route_step_node_owner
        FOREIGN KEY (node_revision_id, node_id, document_revision_id)
        REFERENCES knowledge_node_revisions(id, node_id, document_revision_id),
    CONSTRAINT learning_route_step_snapshot_owner
        FOREIGN KEY (knowledge_revision_id, document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id, document_revision_id)
);

CREATE TABLE tutoring_sessions (
    id UUID PRIMARY KEY,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version >= 1),
    state TEXT NOT NULL CHECK (state IN (
        'Idle','GoalReady','Diagnostic','RouteActive','ActivityIssued','AwaitingResponse',
        'Evaluating','Feedback','AdvanceOrReview','Completed','FocusSuspended',
        'FreeQuestion','FreeAnswer','FocusResumed'
    )),
    goal_revision_id UUID REFERENCES learning_goal_revisions(id),
    route_revision_id UUID REFERENCES learning_route_revisions(id),
    route_step_id UUID REFERENCES learning_route_steps(id),
    knowledge_revision_id UUID REFERENCES knowledge_revisions(id),
    focus_node_revision_id UUID REFERENCES knowledge_node_revisions(id),
    activity_id UUID,
    attempt_id UUID,
    attached_quiz BOOLEAN NOT NULL DEFAULT FALSE,
    started_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);

CREATE TABLE tutoring_focus_frames (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    saved_state TEXT NOT NULL CHECK (saved_state IN ('RouteActive','ActivityIssued','AwaitingResponse')),
    goal_revision_id UUID REFERENCES learning_goal_revisions(id),
    route_revision_id UUID REFERENCES learning_route_revisions(id),
    route_step_id UUID REFERENCES learning_route_steps(id),
    knowledge_revision_id UUID REFERENCES knowledge_revisions(id),
    focus_node_revision_id UUID REFERENCES knowledge_node_revisions(id),
    activity_id UUID,
    attempt_id UUID,
    saved_aggregate_version BIGINT NOT NULL CHECK (saved_aggregate_version >= 1),
    created_event_seq BIGINT NOT NULL CHECK (created_event_seq >= 1),
    invalidated_at TIMESTAMPTZ,
    invalidation_reason TEXT,
    resumed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX tutoring_focus_frame_single_active
    ON tutoring_focus_frames(session_id)
    WHERE invalidated_at IS NULL AND resumed_at IS NULL;

CREATE TABLE tutoring_free_questions (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    focus_frame_id UUID NOT NULL REFERENCES tutoring_focus_frames(id),
    question_text TEXT NOT NULL CHECK (char_length(question_text) BETWEEN 1 AND 8000),
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    references_snapshot JSONB NOT NULL,
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    occurred_at TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE tutoring_proposal_requests (
    device_id UUID NOT NULL REFERENCES devices(id),
    request_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    proposal_type TEXT NOT NULL CHECK (proposal_type IN ('route','activity','assessment','free_answer','explanation')),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('goal','session')),
    aggregate_id UUID NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version >= 0),
    input JSONB NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','processing','ready','failed')),
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    attempt_categories TEXT[] NOT NULL DEFAULT '{}',
    result_proposal_id UUID,
    error_category TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(device_id, request_id),
    CHECK ((status = 'processing') = (lease_token IS NOT NULL AND lease_expires_at IS NOT NULL))
);

CREATE TABLE tutoring_proposal_artifacts (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL,
    request_id UUID NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version >= 1),
    input_hash BYTEA NOT NULL CHECK (octet_length(input_hash) = 32),
    proposal_type TEXT NOT NULL CHECK (proposal_type IN ('route','activity','assessment','free_answer','explanation')),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('goal','session')),
    aggregate_id UUID NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version >= 0),
    goal_revision_id UUID REFERENCES learning_goal_revisions(id),
    route_revision_id UUID REFERENCES learning_route_revisions(id),
    activity_id UUID,
    attempt_id UUID,
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    artifact JSONB NOT NULL,
    trusted_model_id TEXT NOT NULL,
    model_parameters JSONB NOT NULL,
    prompt_revision TEXT NOT NULL,
    attempt_categories TEXT[] NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT tutoring_proposal_request_unique UNIQUE(device_id, request_id),
    CONSTRAINT tutoring_proposal_request_owner FOREIGN KEY(device_id, request_id)
        REFERENCES tutoring_proposal_requests(device_id, request_id)
);
ALTER TABLE tutoring_proposal_requests
    ADD CONSTRAINT tutoring_proposal_result_fk FOREIGN KEY(result_proposal_id)
    REFERENCES tutoring_proposal_artifacts(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE learning_route_revisions
    ADD CONSTRAINT learning_route_source_proposal_fk FOREIGN KEY(source_proposal_id)
    REFERENCES tutoring_proposal_artifacts(id);

CREATE TABLE tutoring_free_answers (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    focus_frame_id UUID NOT NULL REFERENCES tutoring_focus_frames(id),
    free_question_id UUID NOT NULL REFERENCES tutoring_free_questions(id),
    answer_text TEXT NOT NULL CHECK (char_length(answer_text) BETWEEN 1 AND 32000),
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    references_snapshot JSONB NOT NULL,
    source_proposal_id UUID REFERENCES tutoring_proposal_artifacts(id),
    received_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT tutoring_free_answer_question_unique UNIQUE(free_question_id)
);

CREATE TABLE learning_activities (
    id UUID PRIMARY KEY,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
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
    source_proposal_id UUID REFERENCES tutoring_proposal_artifacts(id),
    attached_free_question_id UUID REFERENCES tutoring_free_questions(id),
    attached_free_answer_id UUID REFERENCES tutoring_free_answers(id),
    is_review BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_activity_revision_unique UNIQUE(id, revision),
    CONSTRAINT learning_activity_attempt_owner_unique UNIQUE(id, revision, session_id),
    CONSTRAINT learning_activity_evidence_owner_unique
        UNIQUE(id, revision, session_id, goal_revision_id, route_revision_id, knowledge_revision_id),
    CONSTRAINT learning_activity_route_owner FOREIGN KEY(route_revision_id, goal_revision_id, knowledge_revision_id)
        REFERENCES learning_route_revisions(id, goal_revision_id, knowledge_revision_id),
    CONSTRAINT learning_activity_route_step_owner FOREIGN KEY(route_step_id, route_revision_id, knowledge_revision_id)
        REFERENCES learning_route_steps(id, route_revision_id, knowledge_revision_id),
    CONSTRAINT learning_activity_target_owner FOREIGN KEY(route_step_id, route_revision_id, knowledge_revision_id, target_node_id, target_node_revision_id)
        REFERENCES learning_route_steps(id, route_revision_id, knowledge_revision_id, node_id, node_revision_id)
);

CREATE TABLE learning_activity_references (
    activity_id UUID NOT NULL REFERENCES learning_activities(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    knowledge_revision_id UUID NOT NULL,
    node_id UUID NOT NULL,
    node_revision_id UUID NOT NULL,
    document_revision_id UUID NOT NULL,
    source_start INTEGER NOT NULL CHECK (source_start >= 0),
    source_end INTEGER NOT NULL CHECK (source_end > source_start),
    slice_text TEXT NOT NULL,
    slice_hash BYTEA NOT NULL CHECK (octet_length(slice_hash) = 32),
    PRIMARY KEY(activity_id, ordinal),
    CONSTRAINT learning_activity_reference_node_unique UNIQUE(activity_id, node_revision_id),
    CONSTRAINT learning_activity_reference_node_owner
        FOREIGN KEY(node_revision_id, node_id, document_revision_id)
        REFERENCES knowledge_node_revisions(id, node_id, document_revision_id),
    CONSTRAINT learning_activity_reference_snapshot_owner
        FOREIGN KEY(knowledge_revision_id, document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id, document_revision_id)
);

CREATE TABLE learning_attempt_payloads (
    id UUID PRIMARY KEY,
    answer_text TEXT NOT NULL,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE learning_attempts (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    activity_id UUID NOT NULL,
    activity_revision BIGINT NOT NULL,
    answer_payload_id UUID NOT NULL REFERENCES learning_attempt_payloads(id),
    help_level TEXT NOT NULL CHECK (help_level IN ('none','hint','scaffold','answer_revealed')),
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    occurred_at TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT learning_attempt_owner_unique UNIQUE(id, session_id, activity_id, activity_revision),
    CONSTRAINT learning_attempt_activity_owner FOREIGN KEY(activity_id, activity_revision)
        REFERENCES learning_activities(id, revision),
    CONSTRAINT learning_attempt_session_activity_owner FOREIGN KEY(activity_id, activity_revision, session_id)
        REFERENCES learning_activities(id, revision, session_id)
);
ALTER TABLE tutoring_sessions
    ADD CONSTRAINT tutoring_session_activity_fk FOREIGN KEY(activity_id)
        REFERENCES learning_activities(id) DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT tutoring_session_attempt_fk FOREIGN KEY(attempt_id)
        REFERENCES learning_attempts(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE tutoring_focus_frames
    ADD CONSTRAINT tutoring_focus_activity_fk FOREIGN KEY(activity_id)
        REFERENCES learning_activities(id) DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT tutoring_focus_attempt_fk FOREIGN KEY(attempt_id)
        REFERENCES learning_attempts(id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE learning_assessments (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    attempt_id UUID NOT NULL REFERENCES learning_attempts(id),
    activity_id UUID NOT NULL,
    activity_revision BIGINT NOT NULL,
    rubric_complete BOOLEAN NOT NULL,
    confidence INTEGER NOT NULL CHECK (confidence BETWEEN 0 AND 1000),
    risk_flags TEXT[] NOT NULL,
    trusted_model_id TEXT NOT NULL,
    model_parameters JSONB NOT NULL,
    prompt_revision TEXT NOT NULL,
    proposal_input_hash BYTEA NOT NULL CHECK (octet_length(proposal_input_hash) = 32),
    model_attempts INTEGER NOT NULL CHECK (model_attempts BETWEEN 1 AND 2),
    attempt_categories TEXT[] NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_assessment_attempt_unique UNIQUE(attempt_id),
    CONSTRAINT learning_assessment_owner_unique UNIQUE(id, session_id, attempt_id, activity_id, activity_revision),
    CONSTRAINT learning_assessment_activity_owner FOREIGN KEY(activity_id, activity_revision)
        REFERENCES learning_activities(id, revision),
    CONSTRAINT learning_assessment_attempt_owner FOREIGN KEY(attempt_id, session_id, activity_id, activity_revision)
        REFERENCES learning_attempts(id, session_id, activity_id, activity_revision)
);
CREATE TABLE learning_assessment_items (
    assessment_id UUID NOT NULL REFERENCES learning_assessments(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    rubric_item_id TEXT NOT NULL,
    conclusion TEXT NOT NULL CHECK (conclusion IN ('pass','partial','fail','unassessed')),
    answer_start INTEGER NOT NULL CHECK (answer_start >= 0),
    answer_end INTEGER NOT NULL CHECK (answer_end >= answer_start),
    answer_quote TEXT NOT NULL,
    answer_quote_hash BYTEA NOT NULL CHECK (octet_length(answer_quote_hash) = 32),
    knowledge_revision_id UUID,
    knowledge_node_revision_id UUID,
    knowledge_node_id UUID,
    knowledge_document_revision_id UUID,
    knowledge_start INTEGER,
    knowledge_end INTEGER,
    knowledge_quote TEXT,
    knowledge_quote_hash BYTEA,
    misconception_candidate TEXT,
    PRIMARY KEY(assessment_id, ordinal),
    CONSTRAINT learning_assessment_item_unique UNIQUE(assessment_id, rubric_item_id),
    CONSTRAINT learning_assessment_item_node_owner
        FOREIGN KEY(knowledge_node_revision_id, knowledge_node_id, knowledge_document_revision_id)
        REFERENCES knowledge_node_revisions(id, node_id, document_revision_id) MATCH FULL,
    CONSTRAINT learning_assessment_item_snapshot_owner
        FOREIGN KEY(knowledge_revision_id, knowledge_document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id, document_revision_id) MATCH FULL,
    CONSTRAINT learning_assessment_item_provenance_shape CHECK (
        (knowledge_revision_id IS NULL
            AND knowledge_node_revision_id IS NULL
            AND knowledge_node_id IS NULL
            AND knowledge_document_revision_id IS NULL
            AND knowledge_start IS NULL
            AND knowledge_end IS NULL
            AND knowledge_quote IS NULL
            AND knowledge_quote_hash IS NULL)
        OR
        (knowledge_revision_id IS NOT NULL
            AND knowledge_node_revision_id IS NOT NULL
            AND knowledge_node_id IS NOT NULL
            AND knowledge_document_revision_id IS NOT NULL
            AND knowledge_start IS NOT NULL
            AND knowledge_end IS NOT NULL
            AND knowledge_quote IS NOT NULL
            AND knowledge_quote_hash IS NOT NULL)
    ),
    CONSTRAINT learning_assessment_item_knowledge_hash_shape
        CHECK (knowledge_quote_hash IS NULL OR octet_length(knowledge_quote_hash) = 32)
);
CREATE TABLE learning_assessment_decisions (
    id UUID PRIMARY KEY,
    assessment_id UUID NOT NULL REFERENCES learning_assessments(id),
    version BIGINT NOT NULL CHECK (version >= 1),
    disposition TEXT NOT NULL CHECK (disposition IN ('provisional','accepted','overridden','voided')),
    conclusions JSONB NOT NULL,
    reason TEXT,
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    replaces_decision_id UUID REFERENCES learning_assessment_decisions(id),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_assessment_decision_owner_unique UNIQUE(id, assessment_id),
    CONSTRAINT learning_assessment_decision_version_unique UNIQUE(assessment_id, version),
    CONSTRAINT learning_assessment_replaced_once UNIQUE(replaces_decision_id)
);

CREATE TABLE learning_evidence (
    id UUID PRIMARY KEY,
    decision_id UUID NOT NULL,
    assessment_id UUID NOT NULL,
    session_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    activity_id UUID NOT NULL,
    activity_revision BIGINT NOT NULL,
    goal_revision_id UUID NOT NULL REFERENCES learning_goal_revisions(id),
    route_revision_id UUID NOT NULL REFERENCES learning_route_revisions(id),
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    node_revision_id UUID NOT NULL,
    node_id UUID NOT NULL,
    document_revision_id UUID NOT NULL,
    rubric_revision TEXT NOT NULL,
    evidence_kind TEXT NOT NULL CHECK (evidence_kind IN ('practice_recall','review_recall')),
    activity_type TEXT NOT NULL CHECK (activity_type IN ('objective','open')),
    outcome TEXT NOT NULL CHECK (outcome IN ('pass','partial','fail')),
    help_level TEXT NOT NULL CHECK (help_level IN ('none','hint','scaffold')),
    received_at TIMESTAMPTZ NOT NULL,
    acceptance_policy_version TEXT NOT NULL,
    reducer_policy_version TEXT NOT NULL,
    review_policy_version TEXT NOT NULL,
    misconception_candidates JSONB NOT NULL DEFAULT '[]'::jsonb,
    rubric_outcomes JSONB NOT NULL DEFAULT '[]'::jsonb,
    CONSTRAINT learning_evidence_decision_unique UNIQUE(decision_id),
    CONSTRAINT learning_evidence_decision_owner FOREIGN KEY(decision_id, assessment_id)
        REFERENCES learning_assessment_decisions(id, assessment_id),
    CONSTRAINT learning_evidence_assessment_owner FOREIGN KEY(assessment_id, session_id, attempt_id, activity_id, activity_revision)
        REFERENCES learning_assessments(id, session_id, attempt_id, activity_id, activity_revision),
    CONSTRAINT learning_evidence_attempt_owner FOREIGN KEY(attempt_id, session_id, activity_id, activity_revision)
        REFERENCES learning_attempts(id, session_id, activity_id, activity_revision),
    CONSTRAINT learning_evidence_activity_owner FOREIGN KEY(activity_id, activity_revision, session_id, goal_revision_id, route_revision_id, knowledge_revision_id)
        REFERENCES learning_activities(id, revision, session_id, goal_revision_id, route_revision_id, knowledge_revision_id),
    CONSTRAINT learning_evidence_node_owner FOREIGN KEY(node_revision_id, node_id, document_revision_id)
        REFERENCES knowledge_node_revisions(id, node_id, document_revision_id),
    CONSTRAINT learning_evidence_snapshot_owner FOREIGN KEY(knowledge_revision_id, document_revision_id)
        REFERENCES knowledge_snapshot_documents(knowledge_revision_id, document_revision_id)
);
CREATE TABLE learning_evidence_invalidations (
    id UUID PRIMARY KEY,
    evidence_id UUID NOT NULL REFERENCES learning_evidence(id),
    decision_id UUID REFERENCES learning_assessment_decisions(id),
    reason TEXT NOT NULL CHECK (char_length(reason) >= 1),
    event_seq BIGINT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_evidence_invalidated_once UNIQUE(evidence_id)
);

CREATE TABLE learning_exposures (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    exposure_kind TEXT NOT NULL CHECK (exposure_kind IN ('reading','explanation','free_answer','answer_revealed')),
    content TEXT NOT NULL,
    references_snapshot JSONB NOT NULL,
    source_proposal_id UUID REFERENCES tutoring_proposal_artifacts(id),
    received_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE learning_misconception_revisions (
    id UUID PRIMARY KEY,
    misconception_id UUID NOT NULL,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    node_revision_id UUID NOT NULL REFERENCES knowledge_node_revisions(id),
    rubric_item_id TEXT NOT NULL,
    candidate_hash BYTEA NOT NULL CHECK (octet_length(candidate_hash) = 32),
    candidate_text TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('proposed','supported','challenged','resolved')),
    source_evidence_ids UUID[] NOT NULL,
    counter_evidence_ids UUID[] NOT NULL,
    caused_by_evidence_id UUID NOT NULL REFERENCES learning_evidence(id),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_misconception_revision_unique UNIQUE(misconception_id, revision)
);

CREATE TABLE learning_projection_generations (
    id UUID PRIMARY KEY,
    projection_version TEXT NOT NULL,
    reducer_version TEXT NOT NULL,
    assessment_policy_version TEXT NOT NULL,
    review_policy_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('building','active','failed','retired')),
    target_high_water BIGINT NOT NULL CHECK (target_high_water >= 0),
    checkpoint_event_seq BIGINT NOT NULL CHECK (checkpoint_event_seq >= 0),
    fingerprint BYTEA CHECK (fingerprint IS NULL OR octet_length(fingerprint) = 32),
    incomplete BOOLEAN NOT NULL DEFAULT FALSE,
    reason_codes TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX learning_projection_single_active
    ON learning_projection_generations(status) WHERE status = 'active';
CREATE TABLE learning_projection_head (
    singleton_id SMALLINT PRIMARY KEY CHECK (singleton_id = 1),
    active_generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    rebuilding_generation_id UUID REFERENCES learning_projection_generations(id),
    rebuild_lease_token UUID,
    rebuild_lease_expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT learning_projection_rebuild_lease_shape CHECK (
        (rebuilding_generation_id IS NULL AND rebuild_lease_token IS NULL AND rebuild_lease_expires_at IS NULL)
        OR
        (rebuilding_generation_id IS NOT NULL AND rebuild_lease_token IS NOT NULL AND rebuild_lease_expires_at IS NOT NULL)
    ),
    CONSTRAINT learning_projection_rebuild_not_active CHECK (
        rebuilding_generation_id IS NULL OR rebuilding_generation_id <> active_generation_id
    )
);
CREATE TABLE learning_projection_checkpoints (
    generation_id UUID PRIMARY KEY REFERENCES learning_projection_generations(id),
    event_seq BIGINT NOT NULL CHECK (event_seq >= 0),
    fingerprint BYTEA CHECK (fingerprint IS NULL OR octet_length(fingerprint) = 32),
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE learning_projection_timeline (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    event_seq BIGINT NOT NULL,
    event_id UUID NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, event_seq)
);
CREATE TABLE learning_projection_routes (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    route_revision_id UUID NOT NULL,
    route_id UUID NOT NULL,
    revision BIGINT NOT NULL,
    event_seq BIGINT NOT NULL,
    is_current BOOLEAN NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, route_revision_id)
);
CREATE TABLE learning_projection_sessions (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    session_id UUID NOT NULL,
    updated_event_seq BIGINT NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, session_id)
);
CREATE TABLE learning_projection_nodes (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    node_revision_id UUID NOT NULL,
    updated_event_seq BIGINT NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, node_revision_id)
);
CREATE TABLE learning_projection_evidence (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    evidence_id UUID NOT NULL,
    node_revision_id UUID NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, evidence_id)
);
CREATE TABLE learning_projection_reviews (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    node_revision_id UUID NOT NULL,
    due_at TIMESTAMPTZ NOT NULL,
    stable_id UUID NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, node_revision_id)
);
CREATE INDEX learning_projection_reviews_order
    ON learning_projection_reviews(generation_id, due_at, node_revision_id, stable_id);
CREATE TABLE learning_projection_misconceptions (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    misconception_id UUID NOT NULL,
    node_revision_id UUID NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, misconception_id)
);
CREATE TABLE learning_projection_stats (
    generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
    session_id UUID NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY(generation_id, session_id)
);

WITH initial AS (
    INSERT INTO learning_projection_generations(
        id, projection_version, reducer_version, assessment_policy_version, review_policy_version,
        status, target_high_water, checkpoint_event_seq, fingerprint, created_at, completed_at)
    VALUES(
        gen_random_uuid(), 'learning-projection-v1', 'mastery-reducer-v1',
        'assessment-acceptance-v1', 'fixed-interval-v1', 'active', 0, 0,
        decode('714bcedf27d4844f7d1c027582fb935d2b68b826bd0ffb3741ae782d3a5f4f56', 'hex'), now(), now())
    RETURNING id
)
INSERT INTO learning_projection_head(singleton_id, active_generation_id, updated_at)
SELECT 1, id, now() FROM initial;
INSERT INTO learning_projection_checkpoints(generation_id, event_seq, fingerprint, updated_at)
SELECT active_generation_id, 0, decode('714bcedf27d4844f7d1c027582fb935d2b68b826bd0ffb3741ae782d3a5f4f56', 'hex'), now()
FROM learning_projection_head WHERE singleton_id = 1;

CREATE FUNCTION reject_learning_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'learning history is append-only';
END $$;
DO $$
DECLARE table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'learning_inbox','learning_events','learning_goal_revisions','learning_route_revisions',
        'learning_route_steps','tutoring_free_questions','tutoring_free_answers',
        'tutoring_proposal_artifacts','learning_activities','learning_activity_references',
        'learning_attempt_payloads','learning_attempts','learning_assessments',
        'learning_assessment_items','learning_assessment_decisions','learning_evidence',
        'learning_evidence_invalidations','learning_exposures','learning_misconception_revisions'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_learning_history_mutation()',
            table_name || '_immutable', table_name
        );
    END LOOP;
END $$;

UPDATE device_tokens
SET scopes = ARRAY(
    SELECT DISTINCT scope
    FROM unnest(scopes || ARRAY['learning:read', 'learning:write']) AS scope
    ORDER BY scope
)
WHERE revoked_at IS NULL;
