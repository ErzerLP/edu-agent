-- Quiz jobs reuse encrypted plans/units and immutable section revisions. For a
-- quiz, section_id is the generation unit ID and ordinal is the formal question.
ALTER TABLE learning_studio_jobs DROP CONSTRAINT learning_studio_jobs_kind_check;
ALTER TABLE learning_studio_jobs ADD CHECK(kind IN ('guide','brief','note','quiz'));
