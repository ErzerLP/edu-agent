-- 仅保存原设备恢复所需的凭据摘要；没有学习正文，跨代次仅可办理设备清除回执。
-- 顺延主线已有的复习承载、导师历史迁移，不修改已发布迁移。
CREATE TABLE identity_web_offline_sessions (
    session_hash bytea PRIMARY KEY CHECK (octet_length(session_hash)=32),
    token_id uuid NOT NULL REFERENCES device_tokens(id),
    learner_generation bigint NOT NULL CHECK (learner_generation>0),
    expires_at timestamptz NOT NULL
);
CREATE INDEX identity_web_offline_sessions_token ON identity_web_offline_sessions(token_id);
