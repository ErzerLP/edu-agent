-- 集合只增加归属，不重写任何历史身份或正文。
CREATE TABLE knowledge_collections (
 id UUID PRIMARY KEY,
 owner_space_id UUID NOT NULL DEFAULT '00000000-0000-4000-8000-000000000001' REFERENCES learning_spaces(id),
 name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
 source TEXT NOT NULL CHECK (char_length(source) BETWEEN 1 AND 500),
 shared BOOLEAN NOT NULL DEFAULT false,
 head_revision_id UUID REFERENCES knowledge_revisions(id),
 version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
INSERT INTO knowledge_collections(id,name,source,head_revision_id)
 SELECT '00000000-0000-4000-8000-000000000002','默认资料库','legacy',head_revision_id FROM knowledge_catalog;
ALTER TABLE knowledge_revisions ADD COLUMN collection_id UUID NOT NULL
 DEFAULT '00000000-0000-4000-8000-000000000002' REFERENCES knowledge_collections(id);
ALTER TABLE knowledge_revisions DROP CONSTRAINT knowledge_revisions_revision_no_key;
ALTER TABLE knowledge_revisions ADD UNIQUE(collection_id,revision_no);
CREATE TABLE knowledge_collection_links (
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 collection_id UUID NOT NULL REFERENCES knowledge_collections(id),
 PRIMARY KEY(space_id,collection_id)
);
INSERT INTO knowledge_collection_links VALUES
 ('00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002');
CREATE TABLE knowledge_scope_snapshots (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 entries JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 redacted BOOLEAN NOT NULL DEFAULT false
);
CREATE TRIGGER knowledge_collections_privacy_write_gate BEFORE INSERT OR UPDATE OR DELETE ON knowledge_collections
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_collection_links_privacy_write_gate BEFORE INSERT OR UPDATE OR DELETE ON knowledge_collection_links
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_scope_snapshots_privacy_write_gate BEFORE INSERT OR UPDATE OR DELETE ON knowledge_scope_snapshots
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');

-- 旧维护入口仍推进默认 catalog；同步默认集合的 head，保留原写入故障边界。
CREATE FUNCTION knowledge_sync_default_collection() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE knowledge_collections SET head_revision_id=NEW.head_revision_id
 WHERE id='00000000-0000-4000-8000-000000000002';
 RETURN NEW;
END $$;
CREATE TRIGGER knowledge_sync_default_collection AFTER UPDATE ON knowledge_catalog
 FOR EACH ROW EXECUTE FUNCTION knowledge_sync_default_collection();

-- 已接入的知识写入在事务内设置已校验的学习区，其余 owner 保持默认区兼容规则。
CREATE OR REPLACE FUNCTION privacy_enforce_owner_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE space_status TEXT; write_space UUID;
BEGIN
 IF privacy_owner_scrub_permitted(TG_ARGV[0]) THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 PERFORM privacy_lock_owner_gate(TG_ARGV[0],'write',NULL);
 write_space := '00000000-0000-4000-8000-000000000001';
 IF TG_ARGV[0]='knowledge' THEN
  write_space := COALESCE(NULLIF(current_setting('edu_agent.knowledge_space',true),'')::uuid,write_space);
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
