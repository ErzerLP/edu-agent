-- 正文由 learningcontent 拥有；复用 learning 隐私屏障，不把正文复制进教学事件。
CREATE TABLE learning_content_artifacts (
    id UUID PRIMARY KEY,
    space_id UUID NOT NULL REFERENCES learning_spaces(id),
    goal_id UUID NOT NULL,
    goal_revision_id UUID NOT NULL REFERENCES learning_goal_revisions(id),
    session_id UUID NOT NULL REFERENCES tutoring_sessions(id),
    activity_id UUID NOT NULL REFERENCES learning_activities(id),
    activity_revision BIGINT NOT NULL CHECK(activity_revision>0),
    privacy_generation BIGINT NOT NULL,
    latest_version BIGINT NOT NULL CHECK(latest_version>0),
    committed_version BIGINT NOT NULL CHECK(committed_version>0),
    UNIQUE(activity_id,activity_revision)
);
CREATE TABLE learning_content_revisions (
    artifact_id UUID NOT NULL REFERENCES learning_content_artifacts(id),
    version BIGINT NOT NULL CHECK(version>0),
    status TEXT NOT NULL CHECK(status IN ('draft','failed','committed')),
    actor_device_id UUID NOT NULL REFERENCES devices(id),
    ciphertext BYTEA NOT NULL CHECK(octet_length(ciphertext)>28),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(artifact_id,version)
);
CREATE TABLE learning_content_operations (
    device_id UUID NOT NULL REFERENCES devices(id),
    operation_id UUID NOT NULL,
    artifact_id UUID NOT NULL,
    version BIGINT NOT NULL,
    request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
    PRIMARY KEY(device_id,operation_id),
    FOREIGN KEY(artifact_id,version) REFERENCES learning_content_revisions(artifact_id,version)
);
CREATE TRIGGER learning_content_artifacts_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_content_artifacts
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_content_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_content_revisions
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_content_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_content_operations
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();

-- 正式历史只允许追加；全局清除凭已有 owner scrub 授权删除。
CREATE FUNCTION learning_content_immutable_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT privacy_owner_scrub_permitted('learning') THEN
  RAISE EXCEPTION 'learning_content_revision_immutable';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER learning_content_revisions_immutable BEFORE UPDATE OR DELETE ON learning_content_revisions
FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
