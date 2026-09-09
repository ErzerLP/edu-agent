-- 目标聚合只保存可重建投影，不生成学习事实或 Evidence。
CREATE TABLE learning_projection_progress (
 generation_id UUID NOT NULL REFERENCES learning_projection_generations(id),
 goal_id UUID NOT NULL,
 space_id UUID NOT NULL,
 item JSONB NOT NULL,
 PRIMARY KEY(generation_id,goal_id)
);
CREATE INDEX learning_progress_space ON learning_projection_progress(generation_id,space_id,goal_id);
ALTER TABLE learning_projection_generations ADD COLUMN progress_version INTEGER NOT NULL DEFAULT 0;
CREATE TRIGGER learning_progress_privacy_write_gate BEFORE INSERT OR UPDATE OR DELETE ON learning_projection_progress
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
