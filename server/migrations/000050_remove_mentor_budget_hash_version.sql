-- Mentor operations use the current request contract without budget compatibility.
ALTER TABLE learning_mentor_operations DROP COLUMN hash_version;
