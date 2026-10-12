-- Associations only; all attempts, assessments and progress remain in learning.
CREATE TABLE learning_studio_quiz_questions (
 artifact_id UUID NOT NULL,
 version BIGINT NOT NULL,
 question INTEGER NOT NULL CHECK(question BETWEEN 1 AND 8),
 session_id UUID NOT NULL UNIQUE REFERENCES tutoring_sessions(id),
 activity_id UUID NOT NULL UNIQUE REFERENCES learning_activities(id),
 content_artifact_id UUID NOT NULL UNIQUE REFERENCES learning_content_artifacts(id),
 PRIMARY KEY(artifact_id,version,question),
 FOREIGN KEY(artifact_id,version) REFERENCES learning_studio_revisions(artifact_id,version)
);
CREATE TABLE learning_studio_quiz_operations (
 device_id UUID NOT NULL REFERENCES devices(id),
 operation_id UUID NOT NULL,
 artifact_id UUID NOT NULL,
 version BIGINT NOT NULL,
 question INTEGER NOT NULL,
 request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
 PRIMARY KEY(device_id,operation_id),
 FOREIGN KEY(artifact_id,version,question) REFERENCES learning_studio_quiz_questions(artifact_id,version,question)
);
CREATE TRIGGER learning_studio_quiz_questions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_studio_quiz_questions
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_studio_quiz_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_studio_quiz_operations
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_studio_quiz_questions_immutable BEFORE UPDATE OR DELETE ON learning_studio_quiz_questions
 FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
CREATE TRIGGER learning_studio_quiz_operations_immutable BEFORE UPDATE OR DELETE ON learning_studio_quiz_operations
 FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
