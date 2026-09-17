-- 局部加工运行与普通导师、研究、开学各自恢复，不复用其他任务的批准。
ALTER TABLE learning_mentor_sessions DROP CONSTRAINT learning_mentor_sessions_kind_check;
ALTER TABLE learning_mentor_sessions ADD CHECK(kind IN ('mentor','research','start_learning','content_edit'));

-- 收藏与阅读版本是设备偏好，不决定内容是否首次保存。
CREATE TABLE learning_content_preferences (
    device_id UUID NOT NULL REFERENCES devices(id),
    artifact_id UUID NOT NULL REFERENCES learning_content_artifacts(id) ON DELETE CASCADE,
    favorite BOOLEAN NOT NULL DEFAULT FALSE,
    pinned_version BIGINT,
    PRIMARY KEY(device_id,artifact_id),
    FOREIGN KEY(artifact_id,pinned_version) REFERENCES learning_content_revisions(artifact_id,version)
);
CREATE TRIGGER learning_content_preferences_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_content_preferences
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
