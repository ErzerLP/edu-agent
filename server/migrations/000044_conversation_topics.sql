-- Derived metadata only: original messages remain in learning_tutor_turns.
ALTER TABLE learning_tutor_conversations ADD UNIQUE(id,privacy_generation);
ALTER TABLE learning_tutor_conversations ADD COLUMN topic_index_scope TEXT;
ALTER TABLE learning_tutor_turns ADD UNIQUE(conversation_id,run_id);

CREATE TABLE learning_conversation_messages (
 message_id UUID PRIMARY KEY,
 conversation_id UUID NOT NULL,
 run_id UUID NOT NULL,
 message_slot INTEGER NOT NULL CHECK(message_slot>=-1 AND message_slot<262143),
 ordinal BIGINT NOT NULL CHECK(ordinal>0),
 privacy_generation BIGINT NOT NULL,
 scope_fingerprint TEXT NOT NULL,
 estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens>=0),
 covered BOOLEAN NOT NULL DEFAULT FALSE,
 UNIQUE(run_id,message_slot),
 UNIQUE(conversation_id,ordinal),
 UNIQUE(conversation_id,privacy_generation,message_id),
 FOREIGN KEY(conversation_id,privacy_generation) REFERENCES learning_tutor_conversations(id,privacy_generation) ON DELETE CASCADE,
 FOREIGN KEY(conversation_id,run_id) REFERENCES learning_tutor_turns(conversation_id,run_id) ON DELETE CASCADE
);
CREATE INDEX learning_conversation_messages_pending ON learning_conversation_messages(conversation_id,scope_fingerprint,ordinal) WHERE NOT covered;

CREATE TABLE learning_conversation_topics (
 id UUID PRIMARY KEY,
 conversation_id UUID NOT NULL,
 privacy_generation BIGINT NOT NULL,
 scope_fingerprint TEXT NOT NULL,
 revision BIGINT NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 ciphertext BYTEA NOT NULL,
 UNIQUE(conversation_id,privacy_generation,id),
 FOREIGN KEY(conversation_id,privacy_generation) REFERENCES learning_tutor_conversations(id,privacy_generation) ON DELETE CASCADE
);
CREATE INDEX learning_conversation_topics_page ON learning_conversation_topics(conversation_id,id);

CREATE TABLE learning_topic_anchors (
 conversation_id UUID NOT NULL,
 privacy_generation BIGINT NOT NULL,
 topic_id UUID NOT NULL,
 message_id UUID NOT NULL,
 PRIMARY KEY(topic_id,message_id),
 FOREIGN KEY(conversation_id,privacy_generation,topic_id) REFERENCES learning_conversation_topics(conversation_id,privacy_generation,id) ON DELETE CASCADE,
 FOREIGN KEY(conversation_id,privacy_generation,message_id) REFERENCES learning_conversation_messages(conversation_id,privacy_generation,message_id) ON DELETE CASCADE
);

CREATE TABLE learning_topic_index_heads (
 conversation_id UUID PRIMARY KEY,
 privacy_generation BIGINT NOT NULL,
 revision BIGINT NOT NULL DEFAULT 0,
 covered_through BIGINT NOT NULL DEFAULT 0,
 status TEXT NOT NULL DEFAULT 'not_started' CHECK(status IN ('not_started','queued','running','ready','failed')),
 run_id UUID REFERENCES learning_mentor_runs(id),
 lease_id UUID,
 lease_until TIMESTAMPTZ,
 last_failure TEXT NOT NULL DEFAULT '' CHECK(last_failure IN ('','model_failed','context_changed','lease_expired','input_too_large','invalid_output')),
 FOREIGN KEY(conversation_id,privacy_generation) REFERENCES learning_tutor_conversations(id,privacy_generation) ON DELETE CASCADE
);
CREATE TABLE learning_topic_index_scopes (
 conversation_id UUID NOT NULL,
 privacy_generation BIGINT NOT NULL,
 scope_fingerprint TEXT NOT NULL,
 scanned_through BIGINT NOT NULL DEFAULT 0,
 covered_through BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY(conversation_id,privacy_generation,scope_fingerprint),
 FOREIGN KEY(conversation_id,privacy_generation) REFERENCES learning_tutor_conversations(id,privacy_generation) ON DELETE CASCADE
);
-- Missing coverage is represented by messages.covered=false, including scopes
-- outside the currently authorized selection; skipped messages are never covered.
DO $$ DECLARE t TEXT; BEGIN
 FOREACH t IN ARRAY ARRAY['learning_conversation_messages','learning_conversation_topics','learning_topic_anchors','learning_topic_index_heads','learning_topic_index_scopes'] LOOP
  EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate()',t||'_privacy',t);
 END LOOP;
END $$;
