-- 开学仍使用原运行租约和预算，按目标与设备独立恢复。
ALTER TABLE learning_mentor_sessions DROP CONSTRAINT learning_mentor_sessions_kind_check;
ALTER TABLE learning_mentor_sessions ADD CHECK(kind IN ('mentor','research','start_learning'));

-- 政策固定目标语义与授权；实际采用的知识可以追加版本。
CREATE TABLE knowledge_policies (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 goal_id UUID NOT NULL,
 goal_revision_id UUID NOT NULL REFERENCES learning_goal_revisions(id),
 request JSONB NOT NULL
);
CREATE TABLE knowledge_concepts (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 goal_id UUID NOT NULL,
 semantic_key TEXT NOT NULL,
 UNIQUE(space_id,goal_id,semantic_key)
);
CREATE TABLE knowledge_concept_revisions (
 id UUID PRIMARY KEY,
 concept_id UUID NOT NULL REFERENCES knowledge_concepts(id),
 name TEXT NOT NULL,
 support JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE knowledge_context_revisions (
 id UUID PRIMARY KEY,
 policy_id UUID NOT NULL REFERENCES knowledge_policies(id),
 scope_snapshot_id UUID NOT NULL REFERENCES knowledge_scope_snapshots(id),
 run_id UUID NOT NULL UNIQUE,
 previous_revision_id UUID REFERENCES knowledge_context_revisions(id),
 concept_revision_ids UUID[] NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX knowledge_context_policy ON knowledge_context_revisions(policy_id,created_at DESC,id);
CREATE TRIGGER knowledge_policies_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_policies
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_concepts_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_concepts
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_concept_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_concept_revisions
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_context_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_context_revisions
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');

-- 可空关联不重写历史事件，也不向旧 CLI 的严格响应 DTO 增添字段。
ALTER TABLE tutoring_sessions ADD COLUMN knowledge_context_revision_id UUID;
ALTER TABLE learning_activities ADD COLUMN knowledge_context_revision_id UUID;
ALTER TABLE learning_activities ADD COLUMN artifact_id UUID;
ALTER TABLE learning_activities ADD COLUMN artifact_version BIGINT CHECK(artifact_version>0);

-- 正式知识版本只能追加；身份清除由各 owner 分别移除派生关系。
CREATE FUNCTION knowledge_context_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT privacy_owner_scrub_permitted('knowledge') THEN
  RAISE EXCEPTION 'knowledge_context_revision_immutable';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER knowledge_policies_immutable BEFORE UPDATE OR DELETE ON knowledge_policies
FOR EACH ROW EXECUTE FUNCTION knowledge_context_immutable();
CREATE TRIGGER knowledge_concepts_immutable BEFORE UPDATE OR DELETE ON knowledge_concepts
FOR EACH ROW EXECUTE FUNCTION knowledge_context_immutable();
CREATE TRIGGER knowledge_concept_revisions_immutable BEFORE UPDATE OR DELETE ON knowledge_concept_revisions
FOR EACH ROW EXECUTE FUNCTION knowledge_context_immutable();
CREATE TRIGGER knowledge_context_revisions_immutable BEFORE UPDATE OR DELETE ON knowledge_context_revisions
FOR EACH ROW EXECUTE FUNCTION knowledge_context_immutable();

-- 切换目标后必须重新选择其知识上下文，不能把旧目标的授权带入新现场。
CREATE FUNCTION tutoring_clear_switched_context() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.goal_revision_id IS DISTINCT FROM OLD.goal_revision_id THEN
  NEW.knowledge_context_revision_id=NULL;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER tutoring_clear_switched_context BEFORE UPDATE ON tutoring_sessions
FOR EACH ROW EXECUTE FUNCTION tutoring_clear_switched_context();
