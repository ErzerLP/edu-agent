-- Existing receipts retain their exact v1 payload hash. New receipts use v2.
ALTER TABLE learning_mentor_operations
    ADD COLUMN hash_version SMALLINT NOT NULL DEFAULT 1 CHECK (hash_version IN (1,2));
