-- Provenance only: canonical text and PDF originals remain in knowledge_document_payloads.
-- No legacy backfill: absence is unknown, never inferred from Markdown.
CREATE TABLE knowledge_source_metadata (
    document_revision_id UUID PRIMARY KEY REFERENCES knowledge_document_revisions(id),
    metadata JSONB NOT NULL CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 32768)
);
CREATE TRIGGER knowledge_source_metadata_privacy_write_gate
BEFORE INSERT OR UPDATE OR DELETE ON knowledge_source_metadata
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_source_metadata_privacy_immutable
BEFORE UPDATE OR DELETE ON knowledge_source_metadata
FOR EACH ROW EXECUTE FUNCTION reject_knowledge_history_mutation();

-- Pagination reuses knowledge_snapshot_document_identity(revision, document).
