-- The turn and the complete learning exercise are published in one transaction.
ALTER TABLE learning_tutor_turns
 ADD COLUMN exercise_id UUID UNIQUE REFERENCES learning_exercises(id) ON DELETE SET NULL,
 ADD COLUMN exercise_deferred BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX learning_tutor_exercise_conversation ON learning_tutor_turns(conversation_id)
 WHERE exercise_id IS NOT NULL;
