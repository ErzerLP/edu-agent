-- 在内容协作迁移后接入教学变更；已接收答案恢复后仍按原 rubric 结算。
ALTER TABLE tutoring_focus_frames DROP CONSTRAINT tutoring_focus_frames_saved_state_check;
ALTER TABLE tutoring_focus_frames ADD CHECK(saved_state IN ('RouteActive','ActivityIssued','AwaitingResponse','Evaluating','Feedback'));

CREATE TABLE learning_changes (
    id uuid PRIMARY KEY,
    space_id uuid NOT NULL REFERENCES learning_spaces(id),
    goal_id uuid NOT NULL,
    session_id uuid NOT NULL REFERENCES tutoring_sessions(id),
    device_id uuid NOT NULL REFERENCES devices(id),
    token_id uuid NOT NULL REFERENCES device_tokens(id),
    privacy_generation bigint NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    status text NOT NULL CHECK (status IN ('proposed','waiting_approval','approved','queued_for_boundary','applied','stale','rejected','cancelled','needs_sources')),
    ciphertext bytea,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX learning_changes_queue ON learning_changes(status,created_at);
CREATE INDEX learning_changes_goal ON learning_changes(space_id,goal_id,created_at);
CREATE TABLE learning_change_revisions (
    change_id uuid NOT NULL REFERENCES learning_changes(id),
    revision bigint NOT NULL,
    ciphertext bytea,
    PRIMARY KEY(change_id,revision)
);
CREATE TABLE learning_change_operations (
    device_id uuid NOT NULL REFERENCES devices(id),
    operation_id uuid NOT NULL,
    change_id uuid NOT NULL REFERENCES learning_changes(id),
    request_hash text NOT NULL,
    PRIMARY KEY(device_id,operation_id)
);
CREATE TABLE learning_adaptive_modes (
    goal_id uuid PRIMARY KEY,
    space_id uuid NOT NULL REFERENCES learning_spaces(id),
    version bigint NOT NULL CHECK (version > 0),
    mode text NOT NULL CHECK (mode IN ('adaptive','cautious'))
);
CREATE TABLE learning_change_events (
    seq bigserial PRIMARY KEY,
    change_id uuid NOT NULL REFERENCES learning_changes(id),
    revision bigint NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER learning_changes_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_changes
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
CREATE TRIGGER learning_change_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_change_revisions
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
CREATE TRIGGER learning_change_operations_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_change_operations
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
CREATE TRIGGER learning_adaptive_modes_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_adaptive_modes
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
CREATE TRIGGER learning_change_events_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_change_events
FOR EACH ROW EXECUTE FUNCTION privacy_enforce_owner_write('learning');
CREATE FUNCTION learning_change_revision_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT privacy_owner_scrub_permitted('learning') THEN
  RAISE EXCEPTION 'learning_change_revision_immutable';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER learning_change_revision_immutable BEFORE UPDATE OR DELETE ON learning_change_revisions
FOR EACH ROW EXECUTE FUNCTION learning_change_revision_immutable();
