-- Direction is learning-owned; the space and legacy request semantics stay intact.
CREATE TABLE learning_space_directions (
 space_id UUID PRIMARY KEY REFERENCES learning_spaces(id),
 privacy_generation BIGINT NOT NULL CHECK(privacy_generation>0),
 version BIGINT NOT NULL CHECK(version>0)
);
CREATE TABLE learning_direction_revisions (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_space_directions(space_id),
 privacy_generation BIGINT NOT NULL CHECK(privacy_generation>0),
 version BIGINT NOT NULL CHECK(version>0),
 ciphertext BYTEA NOT NULL CHECK(octet_length(ciphertext)>28),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(space_id,privacy_generation,version)
);
CREATE TABLE learning_direction_goals (
 space_id UUID PRIMARY KEY REFERENCES learning_space_directions(space_id),
 privacy_generation BIGINT NOT NULL CHECK(privacy_generation>0),
 goal_id UUID NOT NULL UNIQUE,
 direction_version BIGINT NOT NULL DEFAULT 0 CHECK(direction_version>=0),
 goal_revision_id UUID REFERENCES learning_goal_revisions(id)
);
CREATE TRIGGER learning_space_directions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_space_directions
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_direction_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_direction_revisions
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_direction_goals_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_direction_goals
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_direction_revisions_immutable BEFORE UPDATE OR DELETE ON learning_direction_revisions
 FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();

ALTER TABLE learning_tutor_conversations ADD COLUMN mode TEXT NOT NULL DEFAULT 'mentor' CHECK(mode IN ('mentor','study'));
ALTER TABLE learning_tutor_conversations ADD CONSTRAINT study_conversation_scope CHECK(mode<>'study' OR (saved AND goal_id IS NULL AND teaching_session_id IS NULL));
