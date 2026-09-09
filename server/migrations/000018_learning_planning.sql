-- 规划正文和回执受 learning 隐私门保护；清除保留无正文身份，阻止旧请求复活。
CREATE TABLE learning_plans (
 id UUID PRIMARY KEY,
 goal_id UUID NOT NULL,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 payload JSONB
);
CREATE INDEX learning_plans_goal ON learning_plans(space_id,goal_id,id);
CREATE TABLE learning_plan_operations (
 device_id UUID NOT NULL REFERENCES devices(id),
 operation_id UUID NOT NULL,
 request_hash TEXT NOT NULL,
 payload JSONB,
 PRIMARY KEY(device_id,operation_id)
);
CREATE TRIGGER learning_plans_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_plans
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
CREATE TRIGGER learning_plan_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_plan_operations
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
