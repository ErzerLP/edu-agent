-- 冻结资料范围可作为教学关系的不可变外键锚点，不推进集合 head，不复制正文。
ALTER TABLE knowledge_revisions ALTER COLUMN collection_id DROP NOT NULL;
ALTER TABLE knowledge_revisions ADD COLUMN scope_snapshot_id UUID UNIQUE REFERENCES knowledge_scope_snapshots(id);
ALTER TABLE knowledge_revisions ADD CONSTRAINT knowledge_revision_scope_owner
 CHECK ((collection_id IS NOT NULL AND scope_snapshot_id IS NULL) OR (collection_id IS NULL AND scope_snapshot_id IS NOT NULL AND scope_snapshot_id=id));

-- 教学事务显式选择已核验的学习区；结算既有事实可跨过归档门，隐私门仍始终生效。
CREATE OR REPLACE FUNCTION privacy_enforce_owner_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE space_status TEXT; write_space UUID; settlement BOOLEAN;
BEGIN
 IF privacy_owner_scrub_permitted(TG_ARGV[0]) THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 PERFORM privacy_lock_owner_gate(TG_ARGV[0],'write',NULL);
 write_space := '00000000-0000-4000-8000-000000000001';
 IF TG_ARGV[0]='knowledge' THEN
  write_space := COALESCE(NULLIF(current_setting('edu_agent.knowledge_space',true),'')::uuid,write_space);
 ELSIF TG_ARGV[0] IN ('learning','tutoring') THEN
  write_space := COALESCE(NULLIF(current_setting('edu_agent.teaching_space',true),'')::uuid,
    NULLIF(current_setting('edu_agent.goal_space',true),'')::uuid,write_space);
 END IF;
 settlement := TG_ARGV[0] IN ('learning','tutoring') AND current_setting('edu_agent.teaching_settlement',true)='on';
 IF TG_ARGV[0] IN ('learning','knowledge','tutoring','memory')
    AND TG_TABLE_NAME NOT IN ('learning_spaces','learning_space_operations',
      'learning_event_clock','learning_aggregate_heads','learning_event_payloads','learning_events')
    AND TG_TABLE_NAME NOT LIKE 'learning_projection_%' AND NOT COALESCE(settlement,false) THEN
  SELECT status INTO space_status FROM learning_spaces WHERE id=write_space FOR SHARE;
  IF space_status IS DISTINCT FROM 'active' THEN RAISE EXCEPTION 'learning_space_archived'; END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
