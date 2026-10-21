package mentorrun

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/jackc/pgx/v5"
	"slices"
	"time"
)

func (s *Service) SelectSourceResearch(ctx context.Context, actor identity.Credential, space, id string, c SourceResearchSelectionCommand) (SourceResearchJob, error) {
	result := SourceResearchJob{}
	if !validID(c.OperationID) || c.ExpectedVersion < 1 || len(c.Items) > 20 || c.ReportRevisionID != nil && *c.ReportRevisionID != "" && !validID(*c.ReportRevisionID) {
		return result, ErrInvalid
	}
	for _, item := range c.Items {
		if !validID(item.CandidateID) {
			return result, ErrInvalid
		}
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, true)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	item, _, version, _, _, err := s.sourceResearchRowTx(ctx, tx, actor, space, id, g, true)
	if err != nil {
		return result, err
	}
	hash := requestHash("source_research_selection", space, id, struct {
		Value         SourceResearchSelectionCommand
		ReportChanged bool
	}{c, c.ReportChanged})
	if found, e := s.webOperation(ctx, tx, actor.Device.ID, space, c.OperationID, g, hash, &result); e != nil || found {
		return result, e
	}
	if version != c.ExpectedVersion || item.body.SourceResearch.ResultSnapshotID == "" || sourceResearchActive(sourceResearchStatus(item)) {
		return result, ErrConflict
	}
	snapshot, err := s.sourceResearchResultTx(ctx, tx, item, item.body.SourceResearch.ResultSnapshotID)
	if err != nil {
		return result, err
	}
	for _, change := range c.Items {
		if !slices.Contains(snapshot.CandidateIDs, change.CandidateID) {
			return result, ErrInvalid
		}
		raw, e := s.sealWeb("source_research_selections", change.CandidateID, g, sourceResearchSelection{Selected: change.Selected, ExplicitlyChanged: true})
		if e != nil {
			return result, e
		}
		if _, e = tx.Exec(ctx, `UPDATE learning_source_research_selections SET ciphertext=$2 WHERE id=$1`, change.CandidateID, raw); e != nil {
			return result, e
		}
	}
	if c.ReportRevisionID != nil && *c.ReportRevisionID != "" {
		report, e := s.sourceResearchReportTx(ctx, tx, item, *c.ReportRevisionID, 0, 1)
		if e != nil || len(report.Sections) == 0 {
			return result, ErrConflict
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_source_research_jobs SET selection_version=selection_version+1,selected_report_revision_id=CASE WHEN $2::boolean THEN NULLIF($3,'')::uuid ELSE selected_report_revision_id END WHERE id=$1`, id, c.ReportChanged || c.ReportRevisionID != nil, func() string {
		if c.ReportRevisionID != nil {
			return *c.ReportRevisionID
		}
		return ""
	}()); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM learning_source_research_previews WHERE job_id=$1`, id); err != nil {
		return result, err
	}
	result, err = s.sourceResearchJobTx(ctx, tx, actor, space, id, g, false)
	if err != nil {
		return result, err
	}
	if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, "", item.RunID, c.OperationID, g, hash, result); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
func (s *Service) CommandSourceResearch(ctx context.Context, actor identity.Credential, space, id string, c SourceResearchCommand) (SourceResearchJob, error) {
	result := SourceResearchJob{}
	if len(c.ConfirmDestination) > 256 || c.Kind != "resume" && c.ConfirmDestination != "" || !validID(c.OperationID) || !validID(c.RoundID) || c.ExpectedVersion < 1 || c.Kind != "stop" && c.Kind != "resume" && c.Kind != "delete" {
		return result, ErrInvalid
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, true)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	item, version, _, _, _, err := s.sourceResearchRowTx(ctx, tx, actor, space, id, g, true)
	if err != nil {
		return result, err
	}
	hash := requestHash("source_research_command", space, id, c)
	if found, e := s.webOperation(ctx, tx, actor.Device.ID, space, c.OperationID, g, hash, &result); e != nil || found {
		return result, e
	}
	if c.ExpectedVersion != version || c.RoundID != item.SourceResearchRoundID {
		return result, ErrConflict
	}
	switch c.Kind {
	case "stop":
		if sourceResearchActive(sourceResearchStatus(item)) {
			item.Status = "cancelled"
			item.Reason = "user_stopped"
			item.lease = nil
			item.leaseUntil = nil
			item.callStarted = false
			item.body.Web.Progress.Notice = "搜集已停止；已校验的资料和报告部分可导入，未完成部分不会继续外发。"
			progress := &item.body.Web.Progress
			progress.ReadFailed += max(0, progress.ReadAttempted-progress.ReadSucceeded-progress.ReadFailed)
			if _, err = tx.Exec(ctx, `UPDATE learning_source_research_rounds SET execution_epoch=execution_epoch+1 WHERE id=$1`, c.RoundID); err != nil {
				return result, err
			}
		}
	case "resume":
		if item.Status != "paused_tool_steps" {
			return result, ErrConflict
		}
		view, fingerprint, search, e := s.sourceResearchConfigurationSnapshot()
		if e != nil {
			return result, e
		}
		if sourceResearchModelDestination(view) != item.body.SourceResearch.Destination || search != item.body.SourceResearch.SearchConfiguration {
			if c.ConfirmDestination != fingerprint+":"+search {
				return result, ErrInactive
			}
			item.body.SourceResearch.Destination = sourceResearchModelDestination(view)
			item.body.SourceResearch.SearchConfiguration = search
		}
		if err = s.forkSourceResearchReportTx(ctx, tx, &item); err != nil {
			return result, err
		}
		item.Configuration = fingerprint
		item.body.ToolSteps = 0
		item.body.SourceResearch.ResultSnapshotID = ""
		item.Status = "queued"
		item.Reason = ""
		item.callStarted = false
		item.body.Web.Progress.Notice = "继续这次搜集，沿用原阅读名额。"
		if err = tx.QueryRow(ctx, `UPDATE learning_source_research_rounds SET execution_epoch=execution_epoch+1 WHERE id=$1 RETURNING execution_epoch`, c.RoundID).Scan(&item.body.Web.Epoch); err != nil {
			return result, err
		}
		if err = s.touchSourceResearchTx(ctx, tx, &item); err != nil {
			return result, err
		}
	case "delete":
		item.BodyAvailable = false
		item.Status = "cancelled"
		item.Reason = "cleared"
		item.lease = nil
		item.leaseUntil = nil
		item.callStarted = false
		item.body = Body{}
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_source_research_jobs SET version=version+1 WHERE id=$1`, id); err != nil {
		return result, err
	}
	if err = s.save(ctx, tx, &item, "source_research_"+c.Kind); err != nil {
		return result, err
	}
	if c.Kind != "delete" {
		result, err = s.sourceResearchJobTx(ctx, tx, actor, space, id, g, false)
		if err != nil {
			return result, err
		}
		if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, "", item.RunID, c.OperationID, g, hash, result); err != nil {
			return result, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	if c.Kind == "stop" || c.Kind == "delete" {
		s.mu.Lock()
		for _, cancel := range s.webCancels[id] {
			cancel()
		}
		s.mu.Unlock()
	}
	return result, nil
}
func (s *Service) touchSourceResearchTx(ctx context.Context, tx pgx.Tx, item *row) error {
	item.ExpiresAt = time.Now().UTC().Add(7 * 24 * time.Hour)
	if _, err := tx.Exec(ctx, `UPDATE learning_source_research_jobs SET expires_at=$2 WHERE id=$1`, item.SourceResearchJobID, item.ExpiresAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=$2,state=jsonb_set(state,'{expires_at}',to_jsonb($2::timestamptz)) WHERE source_research_job_id=$1`, item.SourceResearchJobID, item.ExpiresAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_web_resources SET expires_at=$2 WHERE source_research_job_id=$1`, item.SourceResearchJobID, item.ExpiresAt); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE learning_web_documents SET expires_at=$2 WHERE source_research_job_id=$1`, item.SourceResearchJobID, item.ExpiresAt)
	return err
}
func (s *Service) scrubSourceResearchTx(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE learning_source_research_rounds SET status='stopped',execution_epoch=execution_epoch+1 WHERE job_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM learning_web_resources WHERE source_research_job_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM learning_web_operations WHERE run_id IN (SELECT id FROM learning_mentor_runs WHERE source_research_job_id=$1)`, id); err != nil {
		return err
	}
	for _, table := range []string{"imports", "previews", "events", "results", "sections", "reports", "selections", "candidates"} {
		if _, err := tx.Exec(ctx, `DELETE FROM learning_source_research_`+table+` WHERE job_id=$1`, id); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE learning_source_research_jobs SET ciphertext=NULL,deleted=TRUE,selected_report_revision_id=NULL WHERE id=$1`, id)
	return err
}
func (s *Service) SourceResearchJobs(ctx context.Context, actor identity.Credential, space, cursor string, limit int) ([]SourceResearchJob, string, error) {
	if !validID(space) || limit < 1 || limit > 20 || cursor != "" && !validID(cursor) {
		return nil, "", ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, false)
	if err != nil {
		return nil, "", err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM learning_source_research_jobs WHERE space_id=$1 AND device_id=$2 AND privacy_generation=$3 AND NOT deleted AND expires_at>clock_timestamp() AND ($4='' OR id<NULLIF($4,'')::uuid) ORDER BY id DESC LIMIT $5`, space, actor.Device.ID, g, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(ids) > limit {
		ids = ids[:limit]
		next = ids[len(ids)-1]
	}
	result := []SourceResearchJob{}
	for _, id := range ids {
		job, e := s.sourceResearchJobTx(ctx, tx, actor, space, id, g, false)
		if e != nil {
			return nil, "", e
		}
		result = append(result, job)
	}
	return result, next, nil
}
func (s *Service) SourceResearchEvents(ctx context.Context, actor identity.Credential, space, id string, after int64) ([]SourceResearchEvent, error) {
	if after < 0 {
		return nil, ErrInvalid
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, _, _, _, _, err = s.sourceResearchRowTx(ctx, tx, actor, space, id, g, false); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text,ciphertext FROM learning_source_research_events WHERE job_id=$1 AND seq>$2 ORDER BY seq LIMIT 100`, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SourceResearchEvent{}
	for rows.Next() {
		var eventID string
		var raw []byte
		var event SourceResearchEvent
		if err = rows.Scan(&eventID, &raw); err != nil {
			return nil, err
		}
		if err = s.openWeb("source_research_events", eventID, g, raw, &event); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}
