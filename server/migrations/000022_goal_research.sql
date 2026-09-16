-- 研究复用导师运行的预算、租约、加密和隐私 owner，分别保留当前会话。
ALTER TABLE learning_mentor_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'mentor' CHECK(kind IN ('mentor','research'));
DO $$ DECLARE constraint_name TEXT; BEGIN
 SELECT conname INTO STRICT constraint_name FROM pg_constraint WHERE conrelid='learning_mentor_sessions'::regclass AND contype='u';
 EXECUTE format('ALTER TABLE learning_mentor_sessions DROP CONSTRAINT %I',constraint_name);
END $$;
ALTER TABLE learning_mentor_sessions ADD UNIQUE(device_id,space_id,goal_id,privacy_generation,kind);

-- 正式来源仅保存元数据与正文定位，正文沿用 knowledge 的权威文档载荷。
CREATE TABLE knowledge_source_revisions (
 source_id UUID PRIMARY KEY REFERENCES knowledge_collections(id),
 source_revision_id UUID NOT NULL UNIQUE,
 run_id UUID NOT NULL,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 goal_id UUID NOT NULL,
 knowledge_revision_id UUID NOT NULL REFERENCES knowledge_revisions(id),
 document_revision_id UUID NOT NULL REFERENCES knowledge_document_revisions(id),
 text_start INTEGER NOT NULL CHECK(text_start>=0),
 text_end INTEGER NOT NULL CHECK(text_end>=text_start),
 metadata JSONB NOT NULL
);
CREATE INDEX knowledge_source_revisions_scope ON knowledge_source_revisions(space_id,goal_id,run_id);
CREATE TRIGGER knowledge_source_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_source_revisions
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
