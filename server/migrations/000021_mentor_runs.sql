-- 导师运行属于 learning owner；正文只允许密文，事件 outbox 只保存无正文的通知。
CREATE TABLE learning_mentor_sessions (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id),
    space_id UUID NOT NULL REFERENCES learning_spaces(id),
    goal_id UUID NOT NULL,
    privacy_generation BIGINT NOT NULL,
    current_run_id UUID,
    UNIQUE(device_id,space_id,goal_id,privacy_generation)
);
CREATE TABLE learning_mentor_runs (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES learning_mentor_sessions(id),
    device_id UUID NOT NULL REFERENCES devices(id),
    token_id UUID NOT NULL REFERENCES device_tokens(id),
    space_id UUID NOT NULL REFERENCES learning_spaces(id),
    goal_id UUID NOT NULL,
    goal_version BIGINT NOT NULL,
    privacy_generation BIGINT NOT NULL,
    state JSONB NOT NULL,
    checkpoint BYTEA,
    process_id UUID NOT NULL,
    lease_id UUID,
    lease_until TIMESTAMPTZ,
    call_started BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX learning_mentor_runs_queue ON learning_mentor_runs ((state->>'status'),lease_until,created_at);
CREATE TABLE learning_mentor_operations (
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
    run_id UUID NOT NULL REFERENCES learning_mentor_runs(id),
    receipt JSONB NOT NULL,
    PRIMARY KEY(device_id,operation_id)
);
CREATE TABLE learning_mentor_events (
    run_id UUID NOT NULL REFERENCES learning_mentor_runs(id),
    seq BIGINT NOT NULL,
    event JSONB NOT NULL,
    PRIMARY KEY(run_id,seq)
);
CREATE TABLE learning_mentor_processes (id UUID PRIMARY KEY, live_until TIMESTAMPTZ NOT NULL);

-- 运行的跨区生命周期由应用短事务核对，不能回落默认区的教学写门禁。
CREATE FUNCTION learning_mentor_write_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT privacy_owner_scrub_permitted('learning') THEN
  PERFORM privacy_lock_owner_gate('learning','write',NULL);
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER learning_mentor_sessions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_mentor_sessions
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_mentor_runs_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_mentor_runs
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_mentor_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_mentor_operations
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_mentor_events_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_mentor_events
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
