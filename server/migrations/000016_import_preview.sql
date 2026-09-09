-- 回执仅保存计数与稳定归属，不保存客户端路径、正文或预览任务。
ALTER TABLE knowledge_import_operations ADD COLUMN summary jsonb;
