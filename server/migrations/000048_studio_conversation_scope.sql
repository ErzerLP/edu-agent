-- The encrypted input owns the selection; this identity only supports deletion.
ALTER TABLE learning_studio_jobs ADD COLUMN conversation_id UUID REFERENCES learning_tutor_conversations(id);
CREATE INDEX learning_studio_jobs_conversation ON learning_studio_jobs(conversation_id) WHERE conversation_id IS NOT NULL;
