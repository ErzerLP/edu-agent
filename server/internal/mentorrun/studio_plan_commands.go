package mentorrun

import (
	"context"
	"encoding/json"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/jackc/pgx/v5"
)

func planFingerprint(input string, p learningcontent.StudioPlan) string {
	p.Revision = 0
	p.InputFingerprint = ""
	raw, _ := json.Marshal(struct {
		Input string
		Plan  learningcontent.StudioPlan
	}{input, p})
	return knowledge.SourceTextHash(string(raw))
}

func (s *Service) ChangeStudioPlan(ctx context.Context, actor identity.Credential, id, action string, c StudioPlanCommand) (learningcontent.StudioJob, error) {
	if !validID(id) || c.Validate(action) != nil {
		return learningcontent.StudioJob{}, ErrInvalid
	}
	tx, g, err := s.studioJobBegin(ctx, actor, true)
	if err != nil {
		return learningcontent.StudioJob{}, err
	}
	defer tx.Rollback(context.Background())
	hash := studioJobHash(action, "", id, c)
	if old, found, e := s.studio.StudioJobOperationTx(ctx, tx, actor.Device.ID, c.OperationID, hash, g); e != nil || found {
		if e == nil {
			e = s.studioJobSourcesTx(ctx, tx, old, actor.TokenID)
		}
		return old, e
	}
	j, err := s.studioCommandJobTx(ctx, tx, id, g, action)
	if err != nil {
		return j, err
	}
	versionMatches := j.Version == c.ExpectedVersion
	if action == "stop" || action == "resume" {
		// Progress advances the job version without changing the confirmed plan.
		versionMatches = c.ExpectedVersion <= j.Version
	}
	if !versionMatches || j.ActivePlanRevision != c.PlanRevision {
		return j, learningcontent.ErrConflict
	}
	if action == "stop" && j.Status == "completed" {
		if err = s.studio.RecordStudioJobOperationTx(ctx, tx, j, actor.Device.ID, c.OperationID, hash); err != nil {
			return j, err
		}
		return j, tx.Commit(ctx)
	}
	if action == "delete" {
		return s.deleteStudioJobTx(ctx, tx, j)
	}
	if err = s.studioJobSourcesTx(ctx, tx, j, actor.TokenID); err != nil {
		return learningcontent.StudioJob{}, err
	}
	if action == "stop" || action == "resume" || action == "save_as_new" || action == "revise" || action == "retry" && j.ConfirmedPlanRevision > 0 {
		if err = s.changeStudioContentTx(ctx, tx, actor, &j, action); err != nil {
			return j, err
		}
		if err = s.studio.SaveStudioJobTx(ctx, tx, &j); err != nil {
			return j, err
		}
		if err = s.studio.RecordStudioJobOperationTx(ctx, tx, j, actor.Device.ID, c.OperationID, hash); err != nil {
			return j, err
		}
		return j, tx.Commit(ctx)
	}
	if j.ConfirmedPlanRevision > 0 {
		return j, learningcontent.ErrConflict
	}
	if j.Status == "planning" {
		var status string
		var expired bool
		if err = tx.QueryRow(ctx, `SELECT state->>'status',expires_at<=clock_timestamp() FROM learning_mentor_runs WHERE id=$1`, j.RunID).Scan(&status, &expired); err != nil {
			return j, err
		}
		if terminal(status) || expired {
			j.Status = "failed"
		}
	}
	if action == "retry" {
		if j.Status != "failed" {
			return j, learningcontent.ErrConflict
		}
	} else if j.Plan == nil || (j.Status != "awaiting_confirmation" && j.Status != "queued" && j.Status != "failed") {
		return j, learningcontent.ErrConflict
	}
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return j, err
	}
	j.Version++
	j.Reason = ""
	switch action {
	case "message":
		m := learningcontent.StudioPlanMessage{OperationID: c.OperationID, Text: c.Text, Status: "pending", PlanRevision: j.ActivePlanRevision, CreatedAt: time.Now().UTC()}
		if err = s.studio.SaveStudioPlanMessageTx(ctx, tx, j, m); err != nil {
			return j, err
		}
		j.Messages = append(j.Messages, m)
		j.ConfirmedPlanRevision = 0
		if err = s.enqueueStudioPlanTx(ctx, tx, actor, &j, c.OperationID); err != nil {
			return j, err
		}
	case "retry":
		if err = s.enqueueStudioPlanTx(ctx, tx, actor, &j, c.OperationID); err != nil {
			return j, err
		}
	case "patch":
		p, e := patchStudioPlan(*j.Plan, c)
		if e != nil {
			return j, e
		}
		for _, section := range j.Plan.Sections {
			for _, id := range c.DeleteIDs {
				if section.ID == id {
					j.Input.ExcludedContent = append(j.Input.ExcludedContent, section.Topic+"："+section.Objective)
				}
			}
		}
		p.InputFingerprint = planFingerprint(j.Input.Fingerprint, p)
		if err = s.studio.AppendStudioPlanTx(ctx, tx, &j, p); err != nil {
			return j, err
		}
		j.Status = "awaiting_confirmation"
	case "confirm":
		if j.Plan.InputFingerprint != planFingerprint(j.Input.Fingerprint, *j.Plan) {
			return j, learningcontent.ErrConflict
		}
		if err = s.preflightStudioPlanTx(ctx, tx, j); err != nil {
			return j, err
		}
		j.ConfirmedPlanRevision = j.ActivePlanRevision
		if err = s.prepareStudioUnitsTx(ctx, tx, &j); err != nil {
			return j, err
		}
		if err = s.enqueueNextStudioUnitTx(ctx, tx, actor, &j); err != nil {
			return j, err
		}
		j.Status = "queued"
	}
	if err = s.studio.SaveStudioJobTx(ctx, tx, &j); err != nil {
		return j, err
	}
	if err = s.studio.RecordStudioJobOperationTx(ctx, tx, j, actor.Device.ID, c.OperationID, hash); err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}

func (s *Service) studioCommandJobTx(ctx context.Context, tx pgx.Tx, id string, generation int64, action string) (learningcontent.StudioJob, error) {
	if action != "delete" && action != "stop" && action != "resume" && action != "retry" && action != "save_as_new" && action != "revise" {
		return s.studio.StudioJobTx(ctx, tx, id, generation, true)
	}
	for {
		// Worker completion locks run -> job. A completion while this query waits
		// can enqueue a run outside its snapshot. Release the job lock and retry
		// in that case, so accepting an observed version cannot invert lock order.
		locked, err := tx.Begin(ctx)
		if err != nil {
			return learningcontent.StudioJob{}, err
		}
		rows, err := locked.Query(ctx, `SELECT id FROM learning_mentor_runs WHERE studio_job_id=$1 ORDER BY id FOR UPDATE`, id)
		if err != nil {
			return learningcontent.StudioJob{}, err
		}
		runs := map[string]bool{}
		for rows.Next() {
			var runID string
			if err = rows.Scan(&runID); err != nil {
				rows.Close()
				return learningcontent.StudioJob{}, err
			}
			runs[runID] = true
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return learningcontent.StudioJob{}, err
		}
		j, err := s.studio.StudioJobTx(ctx, locked, id, generation, true)
		if err != nil {
			return j, err
		}
		if j.RunID == "" || runs[j.RunID] {
			return j, locked.Commit(ctx)
		}
		if err = locked.Rollback(ctx); err != nil {
			return j, err
		}
	}
}

func patchStudioPlan(p learningcontent.StudioPlan, c StudioPlanCommand) (learningcontent.StudioPlan, error) {
	available := map[string]learningcontent.StudioPlanSection{}
	for _, section := range p.Sections {
		available[section.ID] = section
	}
	for _, id := range c.DeleteIDs {
		if _, ok := available[id]; !ok {
			return p, ErrInvalid
		}
		delete(available, id)
	}
	if len(available) == 0 {
		return p, ErrInvalid
	}
	sections := []learningcontent.StudioPlanSection{}
	if c.OrderedIDs != nil {
		if len(c.OrderedIDs) != len(available) {
			return p, ErrInvalid
		}
		for _, id := range c.OrderedIDs {
			section, ok := available[id]
			if !ok {
				return p, ErrInvalid
			}
			sections = append(sections, section)
			delete(available, id)
		}
	} else {
		for _, section := range p.Sections {
			if _, ok := available[section.ID]; ok {
				sections = append(sections, section)
			}
		}
	}
	p.Sections = sections
	if len(c.DeleteIDs) > 0 {
		p.Coverage = "partial"
		p.Uncovered = append([]string(nil), p.Uncovered...)
		p.Uncovered = append(p.Uncovered, "已删除的部分不在本次大纲覆盖范围内。")
	}
	return p, nil
}
func (s *Service) preflightStudioPlanTx(ctx context.Context, tx pgx.Tx, j learningcontent.StudioJob) error {
	// Re-read directory metadata at confirmation; no whole source bodies are
	// materialized. The exact byte ranges define finite future generation units.
	wanted := map[string]knowledge.SourceDirectoryEntry{}
	for _, section := range j.Plan.Sections {
		for _, n := range section.Chapters {
			wanted[n.NodeRevisionID] = n
		}
	}
	store := knowledgepg.New(s.pool)
	cursor := ""
	for {
		page, err := store.ReadSourceDirectoryTx(ctx, tx, j.Input.Manifest, cursor, 64)
		if err != nil {
			return err
		}
		for _, node := range page.Items {
			if old, ok := wanted[node.NodeRevisionID]; ok {
				if node != old {
					return errCitation
				}
				delete(wanted, node.NodeRevisionID)
			}
		}
		if len(wanted) == 0 {
			return nil
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return errCitation
}
func (s *Service) deleteStudioJobTx(ctx context.Context, tx pgx.Tx, j learningcontent.StudioJob) (learningcontent.StudioJob, error) {
	if err := removeStudioJobTx(ctx, tx, j.ID); err != nil {
		return j, err
	}
	j.Status = "deleted"
	j.Plan = nil
	j.Messages = []learningcontent.StudioPlanMessage{}
	j.SuggestedFocus = []string{}
	j.Input = learningcontent.StudioJobInput{}
	j.RunID = ""
	j.Version++
	return j, tx.Commit(ctx)
}
func removeStudioJobTx(ctx context.Context, tx pgx.Tx, id string) error {
	// Deleting a planning task removes its input, all revisions, sent messages,
	// operation payloads and stage checkpoints. Published artifacts are separate.
	for _, query := range []string{
		`DELETE FROM learning_mentor_events WHERE run_id IN(SELECT id FROM learning_mentor_runs WHERE studio_job_id=$1)`,
		`DELETE FROM learning_mentor_operations WHERE run_id IN(SELECT id FROM learning_mentor_runs WHERE studio_job_id=$1)`,
		`DELETE FROM learning_mentor_runs WHERE studio_job_id=$1`,
		`DELETE FROM learning_mentor_sessions WHERE studio_job_id=$1`,
		`DELETE FROM learning_studio_jobs WHERE id=$1`,
	} {
		if _, err := tx.Exec(ctx, query, id); err != nil {
			return err
		}
	}
	return nil
}
