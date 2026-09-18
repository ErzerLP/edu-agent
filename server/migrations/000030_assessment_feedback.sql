-- 只记录正式答案提交时已校验的正文版本；历史空值表示未知，不回填当前版本。
ALTER TABLE learning_attempts ADD COLUMN artifact_id UUID;
ALTER TABLE learning_attempts ADD COLUMN artifact_version BIGINT;
ALTER TABLE learning_attempts ADD CONSTRAINT learning_attempt_content_version CHECK (
 (artifact_id IS NULL AND artifact_version IS NULL) OR
 (artifact_id IS NOT NULL AND artifact_version IS NOT NULL AND artifact_version > 0)
);
