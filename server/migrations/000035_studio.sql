-- Independent Studio artifacts; legacy classroom identities and constraints stay intact.
CREATE TABLE learning_studio_artifacts (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 goal_id UUID,
 kind TEXT NOT NULL CHECK(kind IN ('note','guide','brief','quiz')),
 privacy_generation BIGINT NOT NULL CHECK(privacy_generation>0),
 latest_version BIGINT NOT NULL CHECK(latest_version>0)
);
CREATE INDEX learning_studio_space ON learning_studio_artifacts(space_id,privacy_generation,id);
CREATE TABLE learning_studio_revisions (
 artifact_id UUID NOT NULL REFERENCES learning_studio_artifacts(id),
 version BIGINT NOT NULL CHECK(version>0),
 actor_device_id UUID NOT NULL REFERENCES devices(id),
 ciphertext BYTEA NOT NULL CHECK(octet_length(ciphertext)>28),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(artifact_id,version)
);
CREATE TABLE learning_studio_operations (
 device_id UUID NOT NULL REFERENCES devices(id),
 operation_id UUID NOT NULL,
 artifact_id UUID NOT NULL,
 version BIGINT NOT NULL,
 request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
 PRIMARY KEY(device_id,operation_id),
 FOREIGN KEY(artifact_id,version) REFERENCES learning_studio_revisions(artifact_id,version)
);
CREATE TRIGGER learning_studio_artifacts_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_studio_artifacts
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_studio_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_studio_revisions
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_studio_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_studio_operations
 FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_studio_revisions_immutable BEFORE UPDATE OR DELETE ON learning_studio_revisions
 FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
CREATE TRIGGER learning_studio_operations_immutable BEFORE UPDATE OR DELETE ON learning_studio_operations
 FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
