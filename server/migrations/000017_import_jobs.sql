-- 正文仅保存在加密暂存文件中；删除任务密钥使残留文件不可恢复。
CREATE TABLE knowledge_import_jobs (
 id uuid PRIMARY KEY,
 actor_id uuid NOT NULL,
 space_id uuid NOT NULL REFERENCES learning_spaces(id),
 collection_id uuid NOT NULL REFERENCES knowledge_collections(id),
 generation bigint NOT NULL,
 version bigint NOT NULL,
 state jsonb NOT NULL,
 staging_key bytea CHECK (staging_key IS NULL OR octet_length(staging_key)=32)
);
CREATE INDEX knowledge_import_jobs_actor_idx ON knowledge_import_jobs(actor_id,space_id,collection_id);
