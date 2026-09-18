-- 概念修订保留既有身份和研究出处；结构不是按名称去重的课程树。
ALTER TABLE knowledge_concepts ALTER COLUMN goal_id DROP NOT NULL;
ALTER TABLE knowledge_concept_revisions ADD COLUMN structure JSONB NOT NULL DEFAULT '{}';
CREATE INDEX knowledge_concept_current ON knowledge_concept_revisions(concept_id,created_at DESC,id DESC);
CREATE INDEX knowledge_concept_space ON knowledge_concepts(space_id,id);
ALTER TABLE knowledge_context_revisions ADD COLUMN structure_base_id UUID REFERENCES knowledge_context_revisions(id);
ALTER TABLE knowledge_context_revisions ADD COLUMN structure_version BIGINT;

-- 旧版本按既有时间和身份顺序回填，不能把物理行顺序当作最新修订。
CREATE TABLE knowledge_concept_heads (
 concept_id UUID PRIMARY KEY REFERENCES knowledge_concepts(id),
 revision_id UUID NOT NULL REFERENCES knowledge_concept_revisions(id)
);
INSERT INTO knowledge_concept_heads(concept_id,revision_id)
SELECT DISTINCT ON(concept_id) concept_id,id FROM knowledge_concept_revisions ORDER BY concept_id,created_at DESC,id DESC;

CREATE TABLE knowledge_structure_heads (
 space_id UUID PRIMARY KEY REFERENCES learning_spaces(id),
 version BIGINT NOT NULL DEFAULT 0 CHECK(version>=0)
);
INSERT INTO knowledge_structure_heads(space_id,version)
SELECT c.space_id,count(*) FROM knowledge_concept_revisions r JOIN knowledge_concepts c ON c.id=r.concept_id GROUP BY c.space_id;
CREATE FUNCTION knowledge_structure_advance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO knowledge_structure_heads(space_id,version)
 SELECT space_id,1 FROM knowledge_concepts WHERE id=NEW.concept_id
 ON CONFLICT(space_id) DO UPDATE SET version=knowledge_structure_heads.version+1;
 INSERT INTO knowledge_concept_heads(concept_id,revision_id) VALUES(NEW.concept_id,NEW.id)
 ON CONFLICT(concept_id) DO UPDATE SET revision_id=EXCLUDED.revision_id;
 RETURN NEW;
END $$;
CREATE TRIGGER knowledge_structure_advance AFTER INSERT ON knowledge_concept_revisions
FOR EACH ROW EXECUTE FUNCTION knowledge_structure_advance();

CREATE TABLE knowledge_structure_proposals (
 id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 actor_device_id UUID NOT NULL REFERENCES devices(id),
 record JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX knowledge_structure_proposals_space ON knowledge_structure_proposals(space_id,id);
CREATE TABLE knowledge_structure_operations (
 operation_id UUID PRIMARY KEY,
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 actor_device_id UUID NOT NULL REFERENCES devices(id),
 request_hash TEXT NOT NULL,
 proposal_id UUID NOT NULL REFERENCES knowledge_structure_proposals(id)
);
CREATE TRIGGER knowledge_structure_heads_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_structure_heads
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_concept_heads_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_concept_heads
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_structure_proposals_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_structure_proposals
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
CREATE TRIGGER knowledge_structure_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON knowledge_structure_operations
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('knowledge');
