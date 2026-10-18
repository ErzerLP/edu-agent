package mentorrun

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

func (s *Service) failStudioPlanTx(ctx context.Context, tx pgx.Tx, item row) error {
	ctx, err := learningspace.WithScope(ctx, item.SpaceID)
	if err != nil {
		return err
	}
	j, err := s.studio.StudioJobTx(ctx, tx, item.body.StudioPlan.JobID, item.Generation, true)
	if errors.Is(err, learningcontent.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.RunID != item.RunID || j.Status != "planning" {
		return nil
	}
	j.Status = "failed"
	j.Reason = item.Reason
	j.Version++
	if item.Reason == "source_unavailable" {
		j.Status = "invalidated"
	}
	if len(j.Messages) > 0 {
		m := j.Messages[len(j.Messages)-1]
		if m.Status != "succeeded" {
			m.Status = "failed"
			m.Explanation = "本次修改未成功，已保留上次大纲。"
			if err = s.studio.SaveStudioPlanMessageTx(ctx, tx, j, m); err != nil {
				return err
			}
		}
	}
	return s.studio.SaveStudioJobTx(ctx, tx, &j)
}
