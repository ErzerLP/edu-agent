-- 用户参考只推进后续知识上下文，不改写已发行题目的引用。
ALTER TABLE knowledge_context_revisions ADD COLUMN reference_selection JSONB;
ALTER TABLE knowledge_context_revisions ADD COLUMN reference_base_entries JSONB;
CREATE TABLE knowledge_reference_heads (
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 goal_id UUID NOT NULL,
 target_id UUID NOT NULL,
 version BIGINT NOT NULL CHECK(version > 0),
 context_id UUID NOT NULL REFERENCES knowledge_context_revisions(id),
 PRIMARY KEY(space_id,goal_id,target_id)
);
CREATE TABLE knowledge_reference_operations (
 device_id UUID NOT NULL REFERENCES devices(id),
 operation_id UUID NOT NULL,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 goal_id UUID NOT NULL,
 request_hash TEXT NOT NULL,
 result JSONB NOT NULL,
 PRIMARY KEY(device_id,operation_id)
);
CREATE TRIGGER knowledge_reference_heads_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_reference_heads
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_reference_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_reference_operations
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
