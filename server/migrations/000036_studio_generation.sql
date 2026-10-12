-- Only extend the mentor task discriminator; classroom artifact constraints stay intact.
ALTER TABLE learning_mentor_sessions DROP CONSTRAINT learning_mentor_sessions_kind_check;
ALTER TABLE learning_mentor_sessions ADD CHECK(kind IN ('mentor','research','start_learning','content_edit','studio'));
