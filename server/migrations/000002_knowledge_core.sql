CREATE TABLE knowledge_catalog (
    singleton_id SMALLINT PRIMARY KEY CHECK (singleton_id = 1),
    head_revision_id UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO knowledge_catalog(singleton_id) VALUES (1);

CREATE TABLE knowledge_revisions (
    id UUID PRIMARY KEY,
    revision_no BIGINT NOT NULL UNIQUE CHECK (revision_no >= 1),
    parent_revision_id UUID REFERENCES knowledge_revisions(id),
    manifest_hash BYTEA NOT NULL CHECK (octet_length(manifest_hash) = 32),
    source TEXT NOT NULL CHECK (char_length(source) BETWEEN 1 AND 500),
    created_by_device_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    canonicalizer_version TEXT NOT NULL,
    parser_version TEXT NOT NULL,
    indexer_version TEXT NOT NULL,
    identity_policy_version TEXT NOT NULL,
    CONSTRAINT knowledge_revision_linear_parent UNIQUE(parent_revision_id)
);
ALTER TABLE knowledge_catalog
    ADD CONSTRAINT knowledge_catalog_head_fk FOREIGN KEY (head_revision_id) REFERENCES knowledge_revisions(id);
CREATE INDEX knowledge_revision_manifest_idx ON knowledge_revisions(manifest_hash);

CREATE TABLE knowledge_import_operations (
    operation_id UUID PRIMARY KEY,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    result_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    unchanged BOOLEAN NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE knowledge_documents (
    id UUID PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE knowledge_document_revisions (
    id UUID PRIMARY KEY,
    document_id UUID NOT NULL REFERENCES knowledge_documents(id),
    canonical_hash BYTEA NOT NULL CHECK (octet_length(canonical_hash) = 32),
    semantic_hash BYTEA NOT NULL CHECK (octet_length(semantic_hash) = 32),
    root_node_id UUID NOT NULL,
    root_marker BOOLEAN NOT NULL DEFAULT TRUE CHECK (root_marker),
    parser_version TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT knowledge_document_revision_identity UNIQUE(id, document_id),
    CONSTRAINT knowledge_document_revision_content UNIQUE(document_id, canonical_hash, parser_version)
);

CREATE TABLE knowledge_document_payloads (
    document_revision_id UUID PRIMARY KEY REFERENCES knowledge_document_revisions(id),
    canonical_markdown TEXT NOT NULL
);

CREATE TABLE knowledge_snapshot_documents (
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    canonical_path TEXT NOT NULL CHECK (canonical_path <> ''),
    folded_path TEXT NOT NULL CHECK (folded_path <> ''),
    document_id UUID NOT NULL REFERENCES knowledge_documents(id),
    document_revision_id UUID NOT NULL,
    PRIMARY KEY (knowledge_revision_id, canonical_path),
    CONSTRAINT knowledge_snapshot_folded_path UNIQUE(knowledge_revision_id, folded_path),
    CONSTRAINT knowledge_snapshot_document_identity UNIQUE(knowledge_revision_id, document_id),
    CONSTRAINT knowledge_snapshot_document_revision_owner
        FOREIGN KEY (document_revision_id, document_id)
        REFERENCES knowledge_document_revisions(id, document_id)
);
CREATE INDEX knowledge_snapshot_document_revision_idx
    ON knowledge_snapshot_documents(document_revision_id);

CREATE TABLE knowledge_nodes (
    id UUID PRIMARY KEY,
    document_id UUID NOT NULL REFERENCES knowledge_documents(id),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT knowledge_node_document_identity UNIQUE(id, document_id)
);
ALTER TABLE knowledge_document_revisions
    ADD CONSTRAINT knowledge_document_revision_root_owner
    FOREIGN KEY (root_node_id, document_id)
    REFERENCES knowledge_nodes(id, document_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE knowledge_node_revisions (
    id UUID PRIMARY KEY,
    node_id UUID NOT NULL,
    document_id UUID NOT NULL,
    document_revision_id UUID NOT NULL,
    parent_node_revision_id UUID,
    is_root BOOLEAN GENERATED ALWAYS AS (parent_node_revision_id IS NULL) STORED,
    sibling_index INTEGER NOT NULL CHECK (sibling_index >= 0),
    heading_level SMALLINT NOT NULL CHECK (heading_level BETWEEN 0 AND 6),
    title TEXT NOT NULL,
    ancestor_titles JSONB NOT NULL,
    heading_start INTEGER NOT NULL CHECK (heading_start >= 0),
    heading_end INTEGER NOT NULL CHECK (heading_end >= heading_start),
    heading_start_line INTEGER NOT NULL CHECK (heading_start_line >= 1),
    heading_end_line INTEGER NOT NULL CHECK (heading_end_line >= heading_start_line),
    local_body_start INTEGER NOT NULL CHECK (local_body_start >= 0),
    local_body_end INTEGER NOT NULL CHECK (local_body_end >= local_body_start),
    local_body_start_line INTEGER NOT NULL CHECK (local_body_start_line >= 1),
    local_body_end_line INTEGER NOT NULL CHECK (local_body_end_line >= local_body_start_line),
    section_start INTEGER NOT NULL CHECK (section_start >= 0),
    section_end INTEGER NOT NULL CHECK (section_end >= section_start),
    section_start_line INTEGER NOT NULL CHECK (section_start_line >= 1),
    section_end_line INTEGER NOT NULL CHECK (section_end_line >= section_start_line),
    semantic_local_body_hash BYTEA NOT NULL CHECK (octet_length(semantic_local_body_hash) = 32),
    indexer_version TEXT NOT NULL,
    CONSTRAINT knowledge_node_revision_identity UNIQUE(id, document_revision_id),
    CONSTRAINT knowledge_node_revision_root_identity UNIQUE(document_revision_id, node_id, is_root),
    CONSTRAINT knowledge_node_revision_owner
        FOREIGN KEY (node_id, document_id)
        REFERENCES knowledge_nodes(id, document_id),
    CONSTRAINT knowledge_node_revision_document_owner
        FOREIGN KEY (document_revision_id, document_id)
        REFERENCES knowledge_document_revisions(id, document_id),
    CONSTRAINT knowledge_node_revision_parent_owner
        FOREIGN KEY (parent_node_revision_id, document_revision_id)
        REFERENCES knowledge_node_revisions(id, document_revision_id),
    CONSTRAINT knowledge_node_revision_version UNIQUE(document_revision_id, node_id, indexer_version),
    CONSTRAINT knowledge_node_sibling_order UNIQUE(document_revision_id, parent_node_revision_id, sibling_index)
);
CREATE UNIQUE INDEX knowledge_node_revision_single_root
    ON knowledge_node_revisions(document_revision_id)
    WHERE parent_node_revision_id IS NULL;
ALTER TABLE knowledge_document_revisions
    ADD CONSTRAINT knowledge_document_revision_exact_root
    FOREIGN KEY (id, root_node_id, root_marker)
    REFERENCES knowledge_node_revisions(document_revision_id, node_id, is_root)
    DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX knowledge_node_revision_document_idx
    ON knowledge_node_revisions(document_revision_id, parent_node_revision_id, sibling_index);

CREATE TABLE knowledge_lineages (
    id UUID PRIMARY KEY,
    knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
    action TEXT NOT NULL CHECK (action IN ('rewrite', 'split', 'merge')),
    actor_device_id UUID NOT NULL,
    reason TEXT NOT NULL CHECK (char_length(reason) >= 1),
    policy_version TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE knowledge_lineage_members (
    lineage_id UUID NOT NULL REFERENCES knowledge_lineages(id),
    role TEXT NOT NULL CHECK (role IN ('source', 'target')),
    node_revision_id UUID NOT NULL REFERENCES knowledge_node_revisions(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY(lineage_id, role, ordinal),
    CONSTRAINT knowledge_lineage_member_unique UNIQUE(lineage_id, role, node_revision_id)
);

CREATE TABLE knowledge_node_artifacts (
    id UUID PRIMARY KEY,
    node_revision_id UUID NOT NULL REFERENCES knowledge_node_revisions(id),
    kind TEXT NOT NULL,
    producer_version TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    model_version TEXT NOT NULL,
    input_hash BYTEA NOT NULL CHECK (octet_length(input_hash) = 32),
    content TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ready', 'failed', 'stale')),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT knowledge_node_artifact_version UNIQUE(
        node_revision_id, kind, producer_version, prompt_version, model_version, input_hash
    )
);

UPDATE device_tokens
SET scopes = ARRAY(
    SELECT DISTINCT scope
    FROM unnest(scopes || ARRAY['knowledge:read', 'knowledge:write']) AS scope
    ORDER BY scope
)
WHERE revoked_at IS NULL;
