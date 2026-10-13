-- C3a: private owner ports only. Formal learning records retain their owners.
CREATE TABLE learning_exercises (
 id UUID PRIMARY KEY,
 device_id UUID NOT NULL REFERENCES devices(id),
 operation_id UUID NOT NULL,
 request_hash TEXT NOT NULL,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 privacy_generation BIGINT NOT NULL,
 session_id UUID NOT NULL UNIQUE REFERENCES tutoring_sessions(id),
 activity_id UUID NOT NULL UNIQUE REFERENCES learning_activities(id),
 content_artifact_id UUID NOT NULL REFERENCES learning_content_artifacts(id),
 content_version BIGINT NOT NULL,
 ciphertext BYTEA NOT NULL,
 UNIQUE(device_id,operation_id),
 FOREIGN KEY(content_artifact_id,content_version) REFERENCES learning_content_revisions(artifact_id,version)
);
CREATE TABLE learning_exercise_help (
 id UUID PRIMARY KEY,
 exercise_id UUID NOT NULL REFERENCES learning_exercises(id),
 device_id UUID NOT NULL REFERENCES devices(id),
 operation_id UUID NOT NULL,
 level TEXT NOT NULL CHECK(level IN ('hint','scaffold','answer_revealed')),
 ciphertext BYTEA NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(device_id,operation_id)
);
CREATE TABLE learning_assessment_tasks (
 attempt_id UUID PRIMARY KEY REFERENCES learning_attempts(id),
 exercise_id UUID NOT NULL REFERENCES learning_exercises(id),
 device_id UUID NOT NULL REFERENCES devices(id),
 token_id UUID NOT NULL REFERENCES device_tokens(id),
 session_version BIGINT NOT NULL,
 request_id UUID NOT NULL UNIQUE,
 operation_id UUID NOT NULL UNIQUE,
 run_id UUID UNIQUE REFERENCES learning_mentor_runs(id),
 failure_reason TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER learning_exercises_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_exercises
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_exercise_help_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_exercise_help
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_assessment_tasks_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_assessment_tasks
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_exercises_immutable BEFORE UPDATE OR DELETE ON learning_exercises
 FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
CREATE TRIGGER learning_exercise_help_immutable BEFORE UPDATE OR DELETE ON learning_exercise_help
 FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
ALTER TABLE learning_mentor_sessions DROP CONSTRAINT learning_mentor_sessions_kind_check;
ALTER TABLE learning_mentor_sessions ADD CHECK(kind IN ('mentor','research','start_learning','content_edit','studio','assessment'));
