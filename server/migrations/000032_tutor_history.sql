-- 旧的七日运行继续使用原合同，不自动转为永久历史。
ALTER TABLE learning_mentor_sessions ADD COLUMN history BOOLEAN NOT NULL DEFAULT FALSE;
DO $$ DECLARE constraint_name TEXT; BEGIN
 SELECT conname INTO STRICT constraint_name FROM pg_constraint WHERE conrelid='learning_mentor_sessions'::regclass AND contype='u';
 EXECUTE format('ALTER TABLE learning_mentor_sessions DROP CONSTRAINT %I',constraint_name);
END $$;
CREATE UNIQUE INDEX learning_mentor_legacy_binding ON learning_mentor_sessions(device_id,space_id,goal_id,privacy_generation,kind) WHERE NOT history;

CREATE TABLE learning_tutor_conversations (
 id UUID PRIMARY KEY REFERENCES learning_mentor_sessions(id),
 device_id UUID NOT NULL REFERENCES devices(id),
 space_id UUID NOT NULL REFERENCES learning_spaces(id),
 goal_id UUID,
 teaching_session_id UUID,
 privacy_generation BIGINT NOT NULL,
 saved BOOLEAN NOT NULL,
 version BIGINT NOT NULL DEFAULT 1,
 provider TEXT NOT NULL,
 endpoint TEXT NOT NULL,
 title BYTEA,
 request_hash BYTEA NOT NULL,
 deleted BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 CHECK(saved OR title IS NULL),
 CHECK(teaching_session_id IS NULL OR goal_id IS NOT NULL)
);
CREATE INDEX learning_tutor_history_context ON learning_tutor_conversations(device_id,privacy_generation,space_id,goal_id,teaching_session_id,updated_at DESC,id DESC) WHERE NOT deleted;
CREATE TABLE learning_tutor_turns (
 conversation_id UUID NOT NULL REFERENCES learning_tutor_conversations(id),
 run_id UUID PRIMARY KEY REFERENCES learning_mentor_runs(id),
 ordinal BIGINT NOT NULL,
 ciphertext BYTEA,
 UNIQUE(conversation_id,ordinal)
);
CREATE TRIGGER learning_tutor_conversations_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_tutor_conversations
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_tutor_turns_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_tutor_turns
FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();

-- 停机轮换只允许重封装 ciphertext；版本、身份和业务正文语义仍由原 owner 保持。
CREATE FUNCTION learning_ciphertext_rotation_permitted(target oid, before_row jsonb, after_row jsonb) RETURNS boolean LANGUAGE sql AS $$
 SELECT current_setting('app.mentor_key_rotation',true)='on'
 AND before_row-'ciphertext'=after_row-'ciphertext'
 AND EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND relation=target AND mode='AccessExclusiveLock' AND granted)
$$;
CREATE OR REPLACE FUNCTION learning_content_immutable_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND learning_ciphertext_rotation_permitted(TG_RELID,to_jsonb(OLD),to_jsonb(NEW)) THEN RETURN NEW; END IF;
 IF NOT privacy_owner_scrub_permitted('learning') THEN RAISE EXCEPTION 'learning_content_revision_immutable'; END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION learning_change_revision_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND learning_ciphertext_rotation_permitted(TG_RELID,to_jsonb(OLD),to_jsonb(NEW)) THEN RETURN NEW; END IF;
 IF NOT privacy_owner_scrub_permitted('learning') THEN RAISE EXCEPTION 'learning_change_revision_immutable'; END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
