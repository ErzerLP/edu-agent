package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StudioContentStage struct {
	JobID        string `json:"job_id"`
	PlanRevision int64  `json:"plan_revision"`
	UnitID       string `json:"unit_id,omitempty"`
}

func (s *Service) studioDestination() string {
	c := s.settings.View().EffectiveMentor
	return knowledge.SourceTextHash(c.Provider + "\n" + c.Endpoint)
}

func (s *Service) prepareStudioUnitsTx(ctx context.Context, tx pgx.Tx, j *learningcontent.StudioJob) error {
	if j.Kind == "quiz" {
		return s.prepareStudioQuestionsTx(ctx, tx, j)
	}
	store := knowledgepg.New(s.pool)
	ordinal := 0
	for _, section := range j.Plan.Sections {
		part := 0
		for _, n := range section.Chapters {
			for start := n.Range.Start; start < n.Range.End; {
				q := knowledge.SourceSliceRequest{SourceIdentity: n.SourceIdentity, NodeID: n.NodeID, NodeRevisionID: n.NodeRevisionID, Start: start, End: min(n.Range.End, start+j.Plan.SliceBytes)}
				source, err := store.ReadSourceSliceTx(ctx, tx, j.Input.Manifest, q)
				if err != nil {
					return err
				}
				q.Start, q.End = source.Fragments[0].Range.Start, source.Fragments[0].Range.End
				ordinal++
				part++
				raw, _ := json.Marshal(struct {
					Plan  string
					Slice knowledge.SourceSliceRequest
				}{j.Plan.InputFingerprint, q})
				u := learningcontent.StudioUnit{ID: fmt.Sprintf("%s-%d", section.ID, part), SectionID: section.ID, Ordinal: ordinal, Status: "pending", Topic: section.Topic, Objective: section.Objective, Slice: q, InputFingerprint: knowledge.SourceTextHash(string(raw))}
				if err = s.studio.SaveStudioUnitTx(ctx, tx, *j, u); err != nil {
					return err
				}
				start = q.End
			}
		}
	}
	return s.studio.StudioProgressTx(ctx, tx, j)
}
func (s *Service) enqueueNextStudioUnitTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, j *learningcontent.StudioJob) error {
	if err := s.quota(ctx, tx, 512<<10); err != nil {
		return err
	}
	if err := s.studio.StudioProgressTx(ctx, tx, j); err != nil {
		return err
	}
	stage := &StudioContentStage{j.ID, j.ConfirmedPlanRevision, j.CurrentUnit}
	if err := s.enqueueStudioStageTx(ctx, tx, actor, j, uuid.NewString(), stage); err != nil {
		return err
	}
	if stage.UnitID != "" {
		u, err := s.studio.StudioUnitTx(ctx, tx, *j, stage.UnitID)
		if err != nil {
			return err
		}
		u.Status, u.RunID = "running", j.RunID
		u.Attempt++
		return s.studio.SaveStudioUnitTx(ctx, tx, *j, u)
	}
	return nil
}
func (s *Service) studioContentGateTx(ctx context.Context, tx pgx.Tx, item row) error {
	ctx, err := learningspace.WithScope(ctx, item.SpaceID)
	if err != nil {
		return err
	}
	stage := item.body.StudioContent
	j, err := s.studio.StudioJobTx(ctx, tx, stage.JobID, item.Generation, false)
	if err != nil {
		return err
	}
	if j.RunID != item.RunID || j.ConfirmedPlanRevision != stage.PlanRevision || (j.Status != "queued" && j.Status != "generating" && j.Status != "summarizing") {
		return ErrInactive
	}
	return s.studioJobSourcesTx(ctx, tx, j)
}
func (s *Service) changeStudioContentTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, j *learningcontent.StudioJob, action string) error {
	switch action {
	case "stop":
		if j.Status == "stopped" {
			return nil
		}
		if j.Status != "planning" && j.Status != "queued" && j.Status != "generating" && j.Status != "summarizing" {
			return learningcontent.ErrConflict
		}
		// All existing run rows were locked before the job. If a completion enqueued
		// a new run while locks were acquired, reject a stale command via job CAS.
		item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 FOR UPDATE`, j.RunID))
		if err != nil {
			return err
		}
		if err = s.decode(&item); err != nil {
			return err
		}
		item.Status, item.Reason, item.Stage = "cancelled", "user_stopped", "stopped"
		item.ResultUnknown = item.ResultUnknown || item.callStarted
		item.CostUnknown = item.CostUnknown || item.callStarted
		j.ResultUnknown, j.CostUnknown = item.ResultUnknown, item.CostUnknown
		item.lease, item.leaseUntil = nil, nil
		// Save the stopped job first so the run failure hook cannot overwrite it.
		j.Status = "stopped"
		j.Reason = "user_stopped"
		j.Version++
		if err = s.studio.SaveStudioJobTx(ctx, tx, j); err != nil {
			return err
		}
		if err = s.save(ctx, tx, &item, "stopped"); err != nil {
			return err
		}
		if j.CurrentUnit != "" {
			u, e := s.studio.StudioUnitTx(ctx, tx, *j, j.CurrentUnit)
			if e != nil {
				return e
			}
			u.Status = "stopped"
			if e = s.studio.SaveStudioUnitTx(ctx, tx, *j, u); e != nil {
				return e
			}
		}
		return nil
	case "resume", "retry":
		if j.Status != "stopped" && j.Status != "failed" {
			if j.Status != "queued" && j.Status != "generating" && j.Status != "summarizing" {
				return learningcontent.ErrConflict
			}
			var state string
			if err := tx.QueryRow(ctx, `SELECT state->>'status' FROM learning_mentor_runs WHERE id=$1`, j.RunID).Scan(&state); err != nil {
				return err
			}
			if state != "failed" && state != "cancelled" {
				return learningcontent.ErrConflict
			}
		}
		if j.ConfirmedPlanRevision == 0 {
			j.Version++
			if j.Plan != nil {
				j.Status = "awaiting_confirmation"
				j.Reason = ""
				return nil
			}
			return s.enqueueStudioPlanTx(ctx, tx, actor, j, uuid.NewString())
		}
		if j.Input.ArtifactID != "" {
			target := learningcontent.StudioTarget{ArtifactID: &j.Input.ArtifactID, ExpectedVersion: j.Input.ExpectedArtifactVersion, Generation: j.Generation, Kind: j.Kind}
			target, err := s.studio.StudioDocumentTargetTx(ctx, tx, target)
			if err != nil {
				return err
			}
			if err := s.studio.StudioTargetTx(ctx, tx, actor, target); err != nil {
				return err
			}
		}
		if j.Input.Destination != "" && j.Input.Destination == s.studioDestination() {
			_, _, configuration, err := s.settings.MentorClient()
			if err != nil {
				return err
			}
			j.Input.Configuration = configuration
		}
		if j.TotalUnits == 0 {
			if err := s.prepareStudioUnitsTx(ctx, tx, j); err != nil {
				return err
			}
		}
	case "save_as_new":
		if j.Status != "failed" || j.Reason != "studio_version_conflict" || j.CompletedUnits != j.TotalUnits || j.Input.Summary == nil {
			return learningcontent.ErrConflict
		}
		j.Input.ArtifactID = ""
		j.Input.ExpectedArtifactVersion = 0
	case "revise":
		if j.Status != "stopped" && j.Status != "failed" && j.Status != "completed" {
			return learningcontent.ErrConflict
		}
		if j.Plan == nil {
			return learningcontent.ErrConflict
		}
		if err := s.studio.AppendStudioPlanTx(ctx, tx, j, *j.Plan); err != nil {
			return err
		}
		j.Input.Summary = nil
		j.Input.Result = nil
		j.Result = nil
		j.CompletedUnits = 0
		j.TotalUnits = 0
		j.CurrentUnit = ""
		j.Status = "awaiting_confirmation"
		j.Reason = ""
		j.Version++
		return nil
	}
	j.Version++
	j.Reason = ""
	return s.enqueueNextStudioUnitTx(ctx, tx, actor, j)
}
func (s *Service) failStudioContentTx(ctx context.Context, tx pgx.Tx, item row) error {
	ctx, err := learningspace.WithScope(ctx, item.SpaceID)
	if err != nil {
		return err
	}
	j, err := s.studio.StudioJobTx(ctx, tx, item.body.StudioContent.JobID, item.Generation, true)
	if errors.Is(err, learningcontent.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.RunID != item.RunID || j.Status == "stopped" || j.Status == "completed" {
		return nil
	}
	j.Status = "failed"
	j.Reason = item.Reason
	j.Version++
	if item.Reason == "source_unavailable" || item.Reason == "context_changed" {
		j.Status = "invalidated"
	}
	if id := item.body.StudioContent.UnitID; id != "" {
		u, e := s.studio.StudioUnitTx(ctx, tx, j, id)
		if e != nil {
			return e
		}
		if u.Status != "succeeded" {
			u.Status = "failed"
			if e = s.studio.SaveStudioUnitTx(ctx, tx, j, u); e != nil {
				return e
			}
		}
	}
	return s.studio.SaveStudioJobTx(ctx, tx, &j)
}
