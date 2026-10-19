ALTER TABLE learning_studio_jobs DROP CONSTRAINT learning_studio_jobs_status_check;
ALTER TABLE learning_studio_jobs ADD CHECK(status IN ('planning','awaiting_confirmation','queued','generating','summarizing','stopping','stopped','completed','failed','invalidated'));
CREATE TABLE learning_studio_units (
 job_id UUID NOT NULL REFERENCES learning_studio_jobs(id) ON DELETE CASCADE,
 plan_revision BIGINT NOT NULL,
 unit_id TEXT NOT NULL,
 ordinal INT NOT NULL CHECK(ordinal>0),
 status TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','failed','stopped')),
 run_id UUID,
 attempt INT NOT NULL DEFAULT 0,
 ciphertext BYTEA NOT NULL,
 PRIMARY KEY(job_id,plan_revision,unit_id),
 UNIQUE(job_id,plan_revision,ordinal),
 FOREIGN KEY(job_id,plan_revision) REFERENCES learning_studio_plans(job_id,revision)
);
CREATE TABLE learning_studio_section_revisions (
 artifact_id UUID NOT NULL,
 version BIGINT NOT NULL,
 section_id TEXT NOT NULL,
 ordinal INT NOT NULL CHECK(ordinal>0),
 content_version BIGINT NOT NULL,
 ciphertext BYTEA,
 PRIMARY KEY(artifact_id,version,section_id),
 UNIQUE(artifact_id,version,ordinal),
 FOREIGN KEY(artifact_id,version) REFERENCES learning_studio_revisions(artifact_id,version),
 FOREIGN KEY(artifact_id,content_version,section_id) REFERENCES learning_studio_section_revisions(artifact_id,version,section_id),
 CHECK((content_version=version AND ciphertext IS NOT NULL) OR (content_version<version AND ciphertext IS NULL))
);
CREATE TRIGGER learning_studio_units_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_studio_units FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_studio_section_revisions_privacy BEFORE INSERT OR UPDATE OR DELETE ON learning_studio_section_revisions FOR EACH ROW EXECUTE FUNCTION learning_mentor_write_gate();
CREATE TRIGGER learning_studio_section_revisions_immutable BEFORE UPDATE OR DELETE ON learning_studio_section_revisions FOR EACH ROW EXECUTE FUNCTION learning_content_immutable_revision();
