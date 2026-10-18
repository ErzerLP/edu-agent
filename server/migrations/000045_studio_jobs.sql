-- S-A persists planning only. A queued job is not a published artifact.
CREATE TABLE learning_studio_jobs (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 device_id UUID NOT NULL REFERENCES devices(id),
 privacy_generation BIGINT NOT NULL CHECK(privacy_generation>0),
 version BIGINT NOT NULL CHECK(version>0),
 kind TEXT NOT NULL CHECK(kind IN ('guide','brief','note')),
 status TEXT NOT NULL CHECK(status IN ('planning','awaiting_confirmation','queued','failed','invalidated')),
 active_plan_revision BIGINT NOT NULL DEFAULT 0,
 confirmed_plan_revision BIGINT NOT NULL DEFAULT 0,
 run_id UUID,
 ciphertext BYTEA NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX learning_studio_jobs_space ON learning_studio_jobs(space_id,privacy_generation,id);
CREATE TABLE learning_studio_plans (
 job_id UUID NOT NULL REFERENCES learning_studio_jobs(id) ON DELETE CASCADE,
 revision BIGINT NOT NULL CHECK(revision>0),
 schema_version INT NOT NULL DEFAULT 1 CHECK(schema_version=1),
 ciphertext BYTEA NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(job_id,revision)
);
CREATE TABLE learning_studio_plan_messages (
 job_id UUID NOT NULL REFERENCES learning_studio_jobs(id) ON DELETE CASCADE,
 operation_id UUID NOT NULL,
 ordinal BIGINT NOT NULL,
 ciphertext BYTEA NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(job_id,operation_id),
 UNIQUE(job_id,ordinal)
);
CREATE TABLE learning_studio_job_operations (
 device_id UUID NOT NULL REFERENCES devices(id),
 operation_id UUID NOT NULL,
 job_id UUID NOT NULL REFERENCES learning_studio_jobs(id) ON DELETE CASCADE,
 hash_version INT NOT NULL DEFAULT 1 CHECK(hash_version=1),
 request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
 ciphertext BYTEA NOT NULL,
 PRIMARY KEY(device_id,operation_id)
);
-- New jobs never share the legacy device/space/goal session head.
ALTER TABLE learning_mentor_sessions ADD COLUMN studio_job_id UUID;
ALTER TABLE learning_mentor_runs ADD COLUMN studio_job_id UUID;
ALTER TABLE learning_mentor_runs ADD COLUMN studio_stage_operation UUID;
DROP INDEX learning_mentor_legacy_binding;
CREATE UNIQUE INDEX learning_mentor_legacy_binding ON learning_mentor_sessions(device_id,space_id,goal_id,privacy_generation,kind) WHERE NOT history AND studio_job_id IS NULL;
CREATE UNIQUE INDEX learning_mentor_studio_job_session ON learning_mentor_sessions(studio_job_id) WHERE studio_job_id IS NOT NULL;
CREATE UNIQUE INDEX learning_mentor_studio_stage ON learning_mentor_runs(studio_job_id,studio_stage_operation) WHERE studio_job_id IS NOT NULL;
-- These derived rows are mutable only through the learning privacy write gate.
DO $$ DECLARE relation TEXT; BEGIN
 FOREACH relation IN ARRAY ARRAY['learning_studio_jobs','learning_studio_plans','learning_studio_plan_messages','learning_studio_job_operations'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate()',relation||'_privacy',relation);
 END LOOP;
END $$;
