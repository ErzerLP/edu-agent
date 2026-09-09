-- 保留旧目标及修订身份；旧行按默认区解释，不重写历史事件或题目引用。
ALTER TABLE learning_goal_revisions ADD COLUMN space_id UUID NOT NULL
 DEFAULT '00000000-0000-4000-8000-000000000001' REFERENCES learning_spaces(id);
ALTER TABLE learning_goal_revisions ADD COLUMN management JSONB;
ALTER TABLE learning_goal_revisions ADD UNIQUE(id,space_id);
ALTER TABLE learning_goal_revisions ADD FOREIGN KEY(previous_revision_id,space_id)
 REFERENCES learning_goal_revisions(id,space_id);
CREATE INDEX learning_goal_space_versions ON learning_goal_revisions(space_id,goal_id,revision DESC);

-- 只有目标事务设置此上下文；其余教学写入仍遵循既有默认区契约。
CREATE OR REPLACE FUNCTION privacy_enforce_owner_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE space_status TEXT; write_space UUID;
BEGIN
 IF privacy_owner_scrub_permitted(TG_ARGV[0]) THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 PERFORM privacy_lock_owner_gate(TG_ARGV[0],'write',NULL);
 write_space := '00000000-0000-4000-8000-000000000001';
 IF TG_ARGV[0]='knowledge' THEN
  write_space := COALESCE(NULLIF(current_setting('edu_agent.knowledge_space',true),'')::uuid,write_space);
 ELSIF TG_ARGV[0]='learning' THEN
  write_space := COALESCE(NULLIF(current_setting('edu_agent.goal_space',true),'')::uuid,write_space);
 END IF;
 IF TG_ARGV[0] IN ('learning','knowledge','tutoring','memory')
    AND TG_TABLE_NAME NOT IN ('learning_spaces','learning_space_operations',
      'learning_event_clock','learning_aggregate_heads','learning_event_payloads','learning_events')
    AND TG_TABLE_NAME NOT LIKE 'learning_projection_%' THEN
  SELECT status INTO space_status FROM learning_spaces WHERE id=write_space FOR SHARE;
  IF space_status IS DISTINCT FROM 'active' THEN RAISE EXCEPTION 'learning_space_archived'; END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
