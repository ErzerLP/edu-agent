-- 显式复习承载只引用原事实，正文及评分仍由原 owner 管理。
CREATE TABLE learning_review_sessions (
 session_id UUID PRIMARY KEY REFERENCES tutoring_sessions(id),
 task_id UUID NOT NULL,
 source_evidence_id UUID NOT NULL REFERENCES learning_evidence(id),
 source_attempt_id UUID NOT NULL REFERENCES learning_attempts(id),
 created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX learning_review_sessions_task ON learning_review_sessions(task_id,created_at DESC);
CREATE TRIGGER learning_review_sessions_privacy_write_gate BEFORE INSERT OR UPDATE OR DELETE ON learning_review_sessions
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
