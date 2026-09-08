-- Stable compatibility identity: no historical resources, events or signatures
-- are rewritten. Their owners will use this ID for subsequent migrations.
CREATE TABLE learning_spaces (
 id UUID PRIMARY KEY,
 name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
 description TEXT NOT NULL DEFAULT '' CHECK (char_length(description)<=2000),
 status TEXT NOT NULL CHECK (status IN ('active','archived')),
 version BIGINT NOT NULL CHECK (version>0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO learning_spaces(id,name,status,version)
VALUES ('00000000-0000-4000-8000-000000000001','默认学习区','active',1);
CREATE TABLE learning_space_operations (
 device_id UUID NOT NULL,
 operation_id UUID NOT NULL,
 request_hash BYTEA NOT NULL,
 result JSONB NOT NULL,
 PRIMARY KEY(device_id,operation_id)
);
CREATE TRIGGER learning_spaces_privacy_write_gate BEFORE INSERT OR UPDATE OR DELETE ON learning_spaces
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
CREATE TRIGGER learning_space_operations_privacy_write_gate BEFORE INSERT OR UPDATE OR DELETE ON learning_space_operations
 FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');

-- Keep the archive check in the same transaction as existing business writes,
-- including MCP and background callers. The share lock orders writes against
-- archive/restore. Privacy redaction remains possible for archived spaces.
CREATE OR REPLACE FUNCTION privacy_enforce_owner_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE space_status TEXT;
BEGIN
 IF privacy_owner_scrub_permitted(TG_ARGV[0]) THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 PERFORM privacy_lock_owner_gate(TG_ARGV[0],'write',NULL);
 IF TG_ARGV[0] IN ('learning','knowledge','tutoring','memory')
    AND TG_TABLE_NAME NOT IN ('learning_spaces','learning_space_operations',
      'learning_event_clock','learning_aggregate_heads','learning_event_payloads','learning_events')
    AND TG_TABLE_NAME NOT LIKE 'learning_projection_%' THEN
  SELECT status INTO space_status FROM learning_spaces
   WHERE id='00000000-0000-4000-8000-000000000001' FOR SHARE;
  IF space_status IS DISTINCT FROM 'active' THEN RAISE EXCEPTION 'learning_space_archived'; END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
