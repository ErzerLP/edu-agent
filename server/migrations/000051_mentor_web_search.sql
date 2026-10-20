ALTER TABLE learning_tutor_conversations
 ADD COLUMN web_search_enabled BOOLEAN NOT NULL DEFAULT TRUE,
 ADD COLUMN web_search_version BIGINT NOT NULL DEFAULT 1 CHECK(web_search_version>0),
 ADD COLUMN web_access_epoch BIGINT NOT NULL DEFAULT 1 CHECK(web_access_epoch>0);

CREATE TABLE learning_web_resources (
 id UUID PRIMARY KEY,
 owner_kind TEXT NOT NULL CHECK(owner_kind='mentor_run'),
 owner_id UUID NOT NULL REFERENCES learning_mentor_runs(id) ON DELETE CASCADE,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 device_id UUID NOT NULL REFERENCES devices(id),
 privacy_generation BIGINT NOT NULL,
 url_digest BYTEA NOT NULL CHECK(octet_length(url_digest)=32),
 origin TEXT NOT NULL CHECK(origin IN ('search','user','page')),
 ciphertext BYTEA NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 UNIQUE(owner_kind,owner_id,url_digest)
);
CREATE TABLE learning_web_documents (
 id UUID PRIMARY KEY,
 revision_id UUID NOT NULL UNIQUE,
 resource_id UUID NOT NULL REFERENCES learning_web_resources(id) ON DELETE CASCADE,
 owner_kind TEXT NOT NULL CHECK(owner_kind='mentor_run'),
 owner_id UUID NOT NULL REFERENCES learning_mentor_runs(id) ON DELETE CASCADE,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 device_id UUID NOT NULL REFERENCES devices(id),
 privacy_generation BIGINT NOT NULL,
 document_hash BYTEA NOT NULL CHECK(octet_length(document_hash)=32),
 expires_at TIMESTAMPTZ NOT NULL,
 UNIQUE(resource_id)
);
CREATE INDEX learning_web_documents_owner ON learning_web_documents(owner_kind,owner_id,id);
CREATE TABLE learning_web_document_payloads (
 id UUID PRIMARY KEY REFERENCES learning_web_documents(id) ON DELETE CASCADE,
 privacy_generation BIGINT NOT NULL,
 ciphertext BYTEA NOT NULL,
 attachment BYTEA
);
CREATE TABLE learning_web_import_previews (
 id UUID PRIMARY KEY,
 owner_id UUID NOT NULL REFERENCES learning_mentor_runs(id) ON DELETE CASCADE,
 document_id UUID NOT NULL REFERENCES learning_web_documents(id) ON DELETE CASCADE,
 device_id UUID NOT NULL REFERENCES devices(id),
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 privacy_generation BIGINT NOT NULL,
 version BIGINT NOT NULL DEFAULT 1,
 ciphertext BYTEA NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE learning_web_import_receipts (
 id UUID PRIMARY KEY,
 owner_id UUID NOT NULL REFERENCES learning_mentor_runs(id) ON DELETE CASCADE,
 document_id UUID NOT NULL REFERENCES learning_web_documents(id) ON DELETE CASCADE,
 revision_id UUID NOT NULL,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 privacy_generation BIGINT NOT NULL,
 ciphertext BYTEA NOT NULL,
 UNIQUE(owner_id,revision_id,space_id)
);
CREATE TABLE learning_web_operations (
 id UUID PRIMARY KEY,
 operation_id UUID NOT NULL,
 device_id UUID NOT NULL REFERENCES devices(id),
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 conversation_id UUID NOT NULL REFERENCES learning_tutor_conversations(id) ON DELETE CASCADE,
 run_id UUID REFERENCES learning_mentor_runs(id) ON DELETE CASCADE,
 privacy_generation BIGINT NOT NULL,
 request_hash BYTEA NOT NULL,
 ciphertext BYTEA NOT NULL,
 UNIQUE(device_id,operation_id)
);

CREATE TABLE learning_search_calls (
 id UUID PRIMARY KEY,
 task_id UUID NOT NULL,
 privacy_generation BIGINT NOT NULL,
 attempt_key UUID NOT NULL,
 attempt_no BIGINT NOT NULL CHECK(attempt_no>0),
 lease_id UUID NOT NULL,
 lease_until TIMESTAMPTZ NOT NULL,
 provider TEXT NOT NULL,
 configuration TEXT NOT NULL,
 stage TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('prepared','not_dispatched','dispatching','received','failed','cancelled','outcome_unknown')),
 started_at TIMESTAMPTZ,
 finished_at TIMESTAMPTZ,
 UNIQUE(task_id,attempt_key),
 UNIQUE(task_id,attempt_no),
 FOREIGN KEY(task_id,privacy_generation) REFERENCES learning_model_tasks(id,privacy_generation) ON DELETE CASCADE,
 CHECK((state IN ('prepared','not_dispatched'))=(started_at IS NULL)),
 CHECK((state IN ('prepared','dispatching'))=(finished_at IS NULL))
);
CREATE INDEX learning_search_calls_time ON learning_search_calls(privacy_generation,started_at,id);
CREATE INDEX learning_search_calls_task ON learning_search_calls(task_id,started_at,id);
CREATE INDEX learning_search_calls_open ON learning_search_calls(privacy_generation,lease_until) WHERE state IN ('prepared','dispatching');

DO $$ DECLARE t TEXT; BEGIN
 FOREACH t IN ARRAY ARRAY['learning_web_resources','learning_web_documents','learning_web_document_payloads','learning_web_import_previews','learning_web_import_receipts','learning_web_operations','learning_search_calls'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate()',t||'_privacy',t);
  EXECUTE format('COMMENT ON TABLE %I IS %L',t,'privacy owner learning; learning_typed_payload; encrypted web data or numeric search accounting; scrub and rotation registered');
 END LOOP;
END $$;

-- Notebook imports have no goal. Existing goal research retains its contract.
ALTER TABLE knowledge_source_revisions ALTER COLUMN goal_id DROP NOT NULL;
