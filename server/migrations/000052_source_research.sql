CREATE TABLE learning_source_research_jobs (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 device_id UUID NOT NULL REFERENCES devices(id),
 privacy_generation BIGINT NOT NULL,
 version BIGINT NOT NULL DEFAULT 1,
 selection_version BIGINT NOT NULL DEFAULT 1,
 event_seq BIGINT NOT NULL DEFAULT 0,
 current_round_id UUID NOT NULL,
 selected_report_revision_id UUID,
 ciphertext BYTEA,
 deleted BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 expires_at TIMESTAMPTZ NOT NULL,
 UNIQUE(id,privacy_generation)
);
ALTER TABLE learning_mentor_runs ADD UNIQUE(id,privacy_generation);
CREATE TABLE learning_source_research_rounds (
 id UUID PRIMARY KEY,
 job_id UUID NOT NULL,
 privacy_generation BIGINT NOT NULL,
 number INTEGER NOT NULL CHECK(number>0),
 run_id UUID NOT NULL UNIQUE,
 FOREIGN KEY(run_id,privacy_generation) REFERENCES learning_mentor_runs(id,privacy_generation),
 execution_epoch BIGINT NOT NULL DEFAULT 1,
 fetch_http_requests INTEGER NOT NULL DEFAULT 0 CHECK(fetch_http_requests>=0),
 status TEXT NOT NULL CHECK(status IN ('queued','running','stopping','stopped','paused_tool_steps','completed','failed','outcome_unknown')),
 stage TEXT NOT NULL,
 limits JSONB NOT NULL,
 UNIQUE(job_id,number), UNIQUE(id,privacy_generation),
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE
);
CREATE UNIQUE INDEX learning_source_research_active ON learning_source_research_rounds(job_id) WHERE status IN ('queued','running','stopping');
ALTER TABLE learning_source_research_jobs ADD FOREIGN KEY(current_round_id,privacy_generation) REFERENCES learning_source_research_rounds(id,privacy_generation) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE learning_mentor_runs ADD COLUMN source_research_job_id UUID, ADD FOREIGN KEY(source_research_job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation);
ALTER TABLE learning_mentor_sessions DROP CONSTRAINT learning_mentor_sessions_kind_check;
ALTER TABLE learning_mentor_sessions ADD CONSTRAINT learning_mentor_sessions_kind_check CHECK(kind IN ('mentor','research','start_learning','content_edit','studio','assessment','source_research'));

-- Public snapshots retain one owner. The run remains the dispatch/lease unit.
ALTER TABLE learning_web_resources DROP CONSTRAINT learning_web_resources_owner_kind_check, DROP CONSTRAINT learning_web_resources_owner_id_fkey;
ALTER TABLE learning_web_documents DROP CONSTRAINT learning_web_documents_owner_kind_check, DROP CONSTRAINT learning_web_documents_owner_id_fkey;
ALTER TABLE learning_web_resources ADD CHECK(owner_kind IN ('mentor_run','source_research')), ADD COLUMN source_research_job_id UUID, ADD FOREIGN KEY(source_research_job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE;
ALTER TABLE learning_web_documents ADD CHECK(owner_kind IN ('mentor_run','source_research')), ADD COLUMN source_research_job_id UUID, ADD FOREIGN KEY(source_research_job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE;
ALTER TABLE learning_web_resources ADD CHECK((owner_kind='source_research' AND source_research_job_id IS NOT NULL AND source_research_job_id=owner_id) OR (owner_kind='mentor_run' AND source_research_job_id IS NULL));
ALTER TABLE learning_web_documents ADD CHECK((owner_kind='source_research' AND source_research_job_id IS NOT NULL AND source_research_job_id=owner_id) OR (owner_kind='mentor_run' AND source_research_job_id IS NULL));
ALTER TABLE learning_web_resources ADD COLUMN mentor_run_owner_id UUID GENERATED ALWAYS AS (CASE WHEN owner_kind='mentor_run' THEN owner_id END) STORED REFERENCES learning_mentor_runs(id) ON DELETE CASCADE;
ALTER TABLE learning_web_documents ADD COLUMN mentor_run_owner_id UUID GENERATED ALWAYS AS (CASE WHEN owner_kind='mentor_run' THEN owner_id END) STORED REFERENCES learning_mentor_runs(id) ON DELETE CASCADE;
ALTER TABLE learning_web_documents ADD UNIQUE(id,privacy_generation);
ALTER TABLE learning_web_operations ALTER COLUMN conversation_id DROP NOT NULL;
ALTER TABLE learning_web_operations ADD COLUMN source_research_job_id UUID, ADD FOREIGN KEY(source_research_job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE;

CREATE TABLE learning_source_research_candidates (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, round_id UUID NOT NULL,
 privacy_generation BIGINT NOT NULL,
 document_id UUID NOT NULL,
 FOREIGN KEY(document_id,privacy_generation) REFERENCES learning_web_documents(id,privacy_generation) ON DELETE CASCADE,
 rank INTEGER NOT NULL, ciphertext BYTEA NOT NULL,
 UNIQUE(job_id,document_id), UNIQUE(id,privacy_generation),
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE,
 FOREIGN KEY(round_id,privacy_generation) REFERENCES learning_source_research_rounds(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_source_research_selections (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, privacy_generation BIGINT NOT NULL, ciphertext BYTEA NOT NULL,
 FOREIGN KEY(id,privacy_generation) REFERENCES learning_source_research_candidates(id,privacy_generation) ON DELETE CASCADE,
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_source_research_reports (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, round_id UUID NOT NULL, privacy_generation BIGINT NOT NULL,
 version BIGINT NOT NULL, status TEXT NOT NULL CHECK(status IN ('partial','complete')), ciphertext BYTEA NOT NULL,
 UNIQUE(id,privacy_generation), UNIQUE(job_id,version),
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE,
 FOREIGN KEY(round_id,privacy_generation) REFERENCES learning_source_research_rounds(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_source_research_sections (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, report_id UUID NOT NULL, privacy_generation BIGINT NOT NULL,
 ordinal INTEGER NOT NULL, ciphertext BYTEA NOT NULL, UNIQUE(report_id,ordinal),
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE,
 FOREIGN KEY(report_id,privacy_generation) REFERENCES learning_source_research_reports(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_source_research_results (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, round_id UUID NOT NULL, privacy_generation BIGINT NOT NULL,
 version BIGINT NOT NULL, ciphertext BYTEA NOT NULL, UNIQUE(job_id,version),
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE,
 FOREIGN KEY(round_id,privacy_generation) REFERENCES learning_source_research_rounds(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_source_research_events (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, privacy_generation BIGINT NOT NULL,
 seq BIGINT NOT NULL, ciphertext BYTEA NOT NULL, UNIQUE(job_id,seq),
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_source_research_previews (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, privacy_generation BIGINT NOT NULL,
 ciphertext BYTEA NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_source_research_imports (
 id UUID PRIMARY KEY, job_id UUID NOT NULL, privacy_generation BIGINT NOT NULL,
 report_id UUID NOT NULL, ciphertext BYTEA NOT NULL, UNIQUE(job_id,report_id),
 FOREIGN KEY(job_id,privacy_generation) REFERENCES learning_source_research_jobs(id,privacy_generation) ON DELETE CASCADE
);
DO $$ DECLARE t TEXT; BEGIN
 FOREACH t IN ARRAY ARRAY['jobs','rounds','candidates','selections','reports','sections','results','events','previews','imports'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate()','learning_source_research_'||t||'_privacy','learning_source_research_'||t);
  EXECUTE format('COMMENT ON TABLE %I IS %L','learning_source_research_'||t,'privacy owner learning; learning_typed_payload; encrypted source research; scrub and rotation registered');
 END LOOP;
END $$;
