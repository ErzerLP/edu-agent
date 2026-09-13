-- 浏览器会话归 identity 所有；只保存随机会话摘要，不保存明文凭据。
CREATE TABLE identity_web_sessions (
    session_hash BYTEA PRIMARY KEY CHECK (octet_length(session_hash)=32),
    token_id UUID NOT NULL REFERENCES device_tokens(id),
    learner_generation BIGINT NOT NULL CHECK (learner_generation>0),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX identity_web_sessions_token ON identity_web_sessions(token_id);
CREATE TRIGGER identity_web_sessions_privacy_write_gate
BEFORE INSERT OR UPDATE OR DELETE ON identity_web_sessions
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('identity');
