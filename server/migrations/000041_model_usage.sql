-- learning privacy owner / learning_typed_payload: identifiers and counters only.
CREATE TABLE learning_model_tasks (
 id UUID PRIMARY KEY,
 privacy_generation BIGINT NOT NULL,
 space_id UUID REFERENCES learning_spaces(id),
 source TEXT NOT NULL CHECK (source IN ('web','cli','server')),
 actor_device_id UUID REFERENCES devices(id),
 kind TEXT NOT NULL,
 origin_id UUID NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE (privacy_generation,kind,origin_id),
 UNIQUE (id,privacy_generation)
);
CREATE INDEX learning_model_tasks_space ON learning_model_tasks(privacy_generation,space_id,id);
CREATE TABLE learning_model_calls (
 id UUID PRIMARY KEY,
 task_id UUID NOT NULL,
 privacy_generation BIGINT NOT NULL,
 stage TEXT NOT NULL,
 attempt_key UUID NOT NULL,
 attempt_no BIGINT NOT NULL CHECK (attempt_no>0),
 lease_id UUID NOT NULL,
 dispatch_state TEXT NOT NULL CHECK (dispatch_state IN ('prepared','not_dispatched','dispatching','received','failed','cancelled','outcome_unknown')),
 prepared_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 started_at TIMESTAMPTZ,
 finished_at TIMESTAMPTZ,
 provider TEXT NOT NULL,
 destination TEXT NOT NULL,
 model TEXT NOT NULL,
 api_style TEXT NOT NULL,
 configuration TEXT NOT NULL,
 input_tokens BIGINT CHECK (input_tokens>=0),
 output_tokens BIGINT CHECK (output_tokens>=0),
 reasoning_tokens BIGINT CHECK (reasoning_tokens>=0),
 provider_total_tokens BIGINT CHECK (provider_total_tokens>=0),
 total_tokens BIGINT CHECK (total_tokens>=0),
 input_reported BOOLEAN GENERATED ALWAYS AS (input_tokens IS NOT NULL) STORED,
 output_reported BOOLEAN GENERATED ALWAYS AS (output_tokens IS NOT NULL) STORED,
 reasoning_reported BOOLEAN GENERATED ALWAYS AS (reasoning_tokens IS NOT NULL) STORED,
 total_reported BOOLEAN GENERATED ALWAYS AS (provider_total_tokens IS NOT NULL) STORED,
 usage_inconsistent BOOLEAN NOT NULL DEFAULT FALSE,
 UNIQUE (task_id,attempt_key),
 UNIQUE (task_id,attempt_no),
 FOREIGN KEY (task_id,privacy_generation) REFERENCES learning_model_tasks(id,privacy_generation) ON DELETE CASCADE,
 CHECK ((dispatch_state IN ('prepared','not_dispatched')) = (started_at IS NULL)),
 CHECK ((dispatch_state IN ('prepared','dispatching')) = (finished_at IS NULL)),
 CHECK (NOT usage_inconsistent OR total_tokens IS NULL)
);
CREATE INDEX learning_model_calls_time ON learning_model_calls(privacy_generation,started_at,id);
CREATE INDEX learning_model_calls_task ON learning_model_calls(task_id,started_at,id);
CREATE INDEX learning_model_calls_model ON learning_model_calls(privacy_generation,model,started_at,id);
-- Worker claim polls recover open attempts; keep that scan off the full ledger.
CREATE INDEX learning_model_calls_open ON learning_model_calls(privacy_generation,task_id) WHERE dispatch_state IN ('prepared','dispatching');
CREATE TRIGGER learning_model_tasks_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_model_tasks
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_model_calls_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_model_calls
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
COMMENT ON TABLE learning_model_tasks IS 'privacy owner learning; store kind learning_typed_payload; scrub deletes tasks and cascades calls';
COMMENT ON TABLE learning_model_calls IS 'privacy owner learning; store kind learning_typed_payload; no content, endpoints or credentials';
-- Deployment coverage, not learner activity: survives erasure, never backfills old runs.
CREATE TABLE model_usage_coverage (singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton), started_at TIMESTAMPTZ NOT NULL);
INSERT INTO model_usage_coverage(started_at) VALUES(clock_timestamp());
