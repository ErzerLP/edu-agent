ALTER TABLE tutoring_free_questions
    ADD COLUMN session_aggregate_version BIGINT;

LOCK TABLE tutoring_free_questions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE learning_events IN SHARE MODE;
LOCK TABLE learning_event_payloads IN SHARE MODE;
DROP TRIGGER tutoring_free_questions_immutable ON tutoring_free_questions;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM learning_events event
        JOIN learning_event_payloads payload
          ON payload.id=event.payload_id
         AND payload.payload_hash=event.payload_hash
        WHERE event.event_type='FreeQuestionAsked'
          AND event.aggregate_type='session'
          AND payload.redacted_at IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'cannot recover free question version from redacted event payload';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM learning_events event
        JOIN learning_event_payloads payload
          ON payload.id=event.payload_id
         AND payload.payload_hash=event.payload_hash
        WHERE event.event_type='FreeQuestionAsked'
          AND event.aggregate_type='session'
          AND payload.redacted_at IS NULL
          AND (
              jsonb_typeof(payload.payload)<>'object'
              OR COALESCE(payload.payload->>'free_question_id','')=''
              OR COALESCE(payload.payload->>'session_id','')=''
              OR COALESCE(payload.payload->>'focus_frame_id','')=''
          )
    ) THEN
        RAISE EXCEPTION 'cannot recover free question version from incomplete event payload';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM learning_events event
        JOIN learning_event_payloads payload
          ON payload.id=event.payload_id
         AND payload.payload_hash=event.payload_hash
        LEFT JOIN tutoring_free_questions question
          ON question.id::text=payload.payload->>'free_question_id'
         AND question.session_id::text=payload.payload->>'session_id'
         AND question.focus_frame_id::text=payload.payload->>'focus_frame_id'
        WHERE event.event_type='FreeQuestionAsked'
          AND event.aggregate_type='session'
          AND payload.redacted_at IS NULL
          AND question.id IS NULL
    ) THEN
        RAISE EXCEPTION 'cannot recover free question version without exact typed question';
    END IF;

    IF EXISTS (
        SELECT question.id
        FROM tutoring_free_questions question
        LEFT JOIN learning_events event
          ON event.event_type='FreeQuestionAsked'
         AND event.aggregate_type='session'
         AND event.aggregate_id=question.session_id
        LEFT JOIN learning_event_payloads payload
          ON payload.id=event.payload_id
         AND payload.payload_hash=event.payload_hash
         AND payload.redacted_at IS NULL
         AND payload.payload->>'free_question_id'=question.id::text
         AND payload.payload->>'session_id'=question.session_id::text
         AND payload.payload->>'focus_frame_id'=question.focus_frame_id::text
        GROUP BY question.id
        HAVING count(payload.id)<>1
    ) THEN
        RAISE EXCEPTION 'cannot recover unique free question event association';
    END IF;
END
$$;

WITH question_events AS (
    SELECT question.id AS question_id,
           event.device_id,
           event.operation_id,
           event.aggregate_id AS session_id
    FROM tutoring_free_questions question
    JOIN learning_events event
      ON event.event_type='FreeQuestionAsked'
     AND event.aggregate_type='session'
     AND event.aggregate_id=question.session_id
    JOIN learning_event_payloads payload
      ON payload.id=event.payload_id
     AND payload.payload_hash=event.payload_hash
     AND payload.redacted_at IS NULL
     AND payload.payload->>'free_question_id'=question.id::text
     AND payload.payload->>'session_id'=question.session_id::text
     AND payload.payload->>'focus_frame_id'=question.focus_frame_id::text
), question_commits AS (
    SELECT question_events.question_id,
           max(batch.aggregate_version) AS session_aggregate_version
    FROM question_events
    JOIN learning_events batch
      ON batch.device_id=question_events.device_id
     AND batch.operation_id=question_events.operation_id
     AND batch.aggregate_type='session'
     AND batch.aggregate_id=question_events.session_id
    GROUP BY question_events.question_id
)
UPDATE tutoring_free_questions question
SET session_aggregate_version=commits.session_aggregate_version
FROM question_commits commits
WHERE question.id=commits.question_id;

CREATE TRIGGER tutoring_free_questions_immutable
    BEFORE UPDATE OR DELETE ON tutoring_free_questions
    FOR EACH ROW EXECUTE FUNCTION reject_tutoring_history_mutation();

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM tutoring_free_questions
        WHERE session_aggregate_version IS NULL
    ) THEN
        RAISE EXCEPTION 'cannot recover tutoring free question session aggregate version';
    END IF;
END
$$;

ALTER TABLE tutoring_focus_frames
    ADD CONSTRAINT tutoring_focus_frame_session_owner_unique UNIQUE(id,session_id);

ALTER TABLE tutoring_free_questions
    ALTER COLUMN session_aggregate_version SET NOT NULL,
    ADD CONSTRAINT tutoring_free_question_session_version_positive
        CHECK (session_aggregate_version >= 1),
    ADD CONSTRAINT tutoring_free_question_frame_owner
        FOREIGN KEY(focus_frame_id,session_id)
        REFERENCES tutoring_focus_frames(id,session_id),
    ADD CONSTRAINT tutoring_free_question_session_frame_version_unique
        UNIQUE(session_id,focus_frame_id,session_aggregate_version);

CREATE INDEX tutoring_free_question_current_lookup
    ON tutoring_free_questions(session_id,focus_frame_id,session_aggregate_version DESC,id DESC);
