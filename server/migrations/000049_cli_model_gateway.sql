-- Learning owns only gateway identifiers, state and leases; no model content.
CREATE TABLE learning_cli_model_turns (
 privacy_generation BIGINT NOT NULL,
 actor_device_id UUID NOT NULL REFERENCES devices(id),
 session_id UUID NOT NULL,
 turn_id UUID NOT NULL,
 space_id UUID REFERENCES learning_spaces(id),
 task_id UUID NOT NULL UNIQUE,
 PRIMARY KEY (privacy_generation,actor_device_id,session_id,turn_id),
 FOREIGN KEY (task_id,privacy_generation) REFERENCES learning_model_tasks(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_cli_model_requests (
 privacy_generation BIGINT NOT NULL,
 actor_device_id UUID NOT NULL REFERENCES devices(id),
 request_id UUID NOT NULL,
 task_id UUID,
 target TEXT CHECK (target IN ('teaching','mentor')),
 configuration_revision TEXT,
 state TEXT NOT NULL CHECK (state IN ('accepted','running','succeeded','failed','cancelled','outcome_unknown')),
 reason TEXT CHECK (reason IN ('request_cancelled','device_revoked','model_scope_required','privacy_generation_changed','configuration_changed','gateway_unavailable','gateway_busy','model_idle_timeout','model_request_timeout','upstream_error','upstream_incompatible','upstream_rate_limited','context_limit_exceeded','output_limit_exceeded')),
 dispatched BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 finished_at TIMESTAMPTZ,
 lease_id UUID,
 lease_until TIMESTAMPTZ,
 PRIMARY KEY (privacy_generation,actor_device_id,request_id),
 FOREIGN KEY (task_id,privacy_generation) REFERENCES learning_model_tasks(id,privacy_generation) ON DELETE CASCADE,
 CHECK (task_id IS NOT NULL OR state='cancelled')
);
CREATE TABLE learning_model_execution_leases (
 id UUID PRIMARY KEY,
 privacy_generation BIGINT NOT NULL,
 owner UUID NOT NULL,
 source TEXT NOT NULL CHECK (source IN ('web','cli','server')),
 actor_device_id UUID REFERENCES devices(id),
 request_id UUID,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 lease_until TIMESTAMPTZ NOT NULL
);
CREATE INDEX learning_model_execution_leases_active ON learning_model_execution_leases(lease_until,source,actor_device_id);
CREATE INDEX learning_cli_model_requests_active ON learning_cli_model_requests(lease_until) WHERE state IN ('accepted','running');
CREATE INDEX learning_cli_model_turns_session ON learning_cli_model_turns(privacy_generation,actor_device_id,session_id);
CREATE TRIGGER learning_cli_model_turns_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_cli_model_turns FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_cli_model_requests_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_cli_model_requests FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_model_execution_leases_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_model_execution_leases FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
COMMENT ON TABLE learning_cli_model_turns IS 'privacy owner learning; learning_typed_payload; identifiers only';
COMMENT ON TABLE learning_cli_model_requests IS 'privacy owner learning; learning_typed_payload; deduplication and cancellation, no replay content';
COMMENT ON TABLE learning_model_execution_leases IS 'privacy owner learning; learning_typed_payload; shared model admission';

-- Deployment secret, independent of learner activity; gives stable opaque
-- revisions across instances/restarts, including unauthenticated model endpoints.
CREATE TABLE model_gateway_revision_key (singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton), secret TEXT NOT NULL);
INSERT INTO model_gateway_revision_key(secret) VALUES(gen_random_uuid()::text || gen_random_uuid()::text);

-- NOTIFY is transactional. Row locks and privacy gates remain authoritative.
CREATE FUNCTION model_gateway_notify() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE payload jsonb;
BEGIN
 IF TG_TABLE_NAME='devices' THEN
  IF NEW.revoked_at IS NULL OR OLD.revoked_at IS NOT NULL THEN RETURN NEW; END IF;
  payload=jsonb_build_object('device',NEW.id,'reason','device_revoked');
 ELSIF TG_TABLE_NAME='device_tokens' THEN
  IF NEW.revoked_at IS NULL OR OLD.revoked_at IS NOT NULL THEN RETURN NEW; END IF;
  payload=jsonb_build_object('device',NEW.device_id,'reason','device_revoked');
 ELSIF TG_TABLE_NAME='privacy_owner_generation_gates' THEN
  IF NEW.owner_kind<>'learning' OR (NEW.read_open AND NEW.write_open AND NEW.learner_generation=OLD.learner_generation) THEN RETURN NEW; END IF;
  payload=jsonb_build_object('reason','privacy_generation_changed','before_generation',NEW.learner_generation);
 ELSE
  IF NEW.state<>'cancelled' OR OLD.state='cancelled' THEN RETURN NEW; END IF;
  payload=jsonb_build_object('device',NEW.actor_device_id,'request',NEW.request_id,'reason',NEW.reason,'generation',NEW.privacy_generation);
 END IF;
 PERFORM pg_notify('model_gateway_cancel',(payload || jsonb_build_object('schema',current_schema()))::text);
 RETURN NEW;
END $$;
CREATE TRIGGER model_gateway_device_revoked AFTER UPDATE OF revoked_at ON devices FOR EACH ROW EXECUTE FUNCTION model_gateway_notify();
CREATE TRIGGER model_gateway_token_revoked AFTER UPDATE OF revoked_at ON device_tokens FOR EACH ROW EXECUTE FUNCTION model_gateway_notify();
CREATE TRIGGER model_gateway_privacy_closed AFTER UPDATE ON privacy_owner_generation_gates FOR EACH ROW EXECUTE FUNCTION model_gateway_notify();
CREATE TRIGGER model_gateway_request_cancelled AFTER UPDATE OF state ON learning_cli_model_requests FOR EACH ROW EXECUTE FUNCTION model_gateway_notify();
