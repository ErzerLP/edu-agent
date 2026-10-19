package mentorrun

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

// S-A confirmation saved queued without a content run. Upgrade only that exact
// state, under the original authorization and frozen plan; ordinary stage runs
// continue through their existing lease/checkpoint recovery.
func (s *Service) scheduleConfirmedStudio(ctx context.Context) error {
	if s.studio == nil || !s.studio.Available() {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, true)
	if err != nil {
		return err
	}
	var id, space, device, token, configuration string
	err = tx.QueryRow(ctx, `SELECT j.id::text,j.space_id::text,r.device_id::text,r.token_id::text,r.state->>'configuration'
 FROM learning_studio_jobs j JOIN learning_mentor_runs r ON r.id=j.run_id
 WHERE j.privacy_generation=$1 AND j.status='queued' AND j.confirmed_plan_revision>0
 AND NOT EXISTS(SELECT 1 FROM learning_studio_units u WHERE u.job_id=j.id AND u.plan_revision=j.confirmed_plan_revision)
 ORDER BY j.created_at LIMIT 1 FOR UPDATE OF r SKIP LOCKED`, g).Scan(&id, &space, &device, &token, &configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	ctx, err = learningspace.WithScope(ctx, space)
	if err != nil {
		return err
	}
	j, err := s.studio.StudioJobTx(ctx, tx, id, g, true)
	if err != nil {
		return err
	}
	if j.Status != "queued" || j.TotalUnits != 0 {
		return tx.Commit(ctx)
	}
	actor := identity.Credential{Device: identity.Device{ID: device}, TokenID: token}
	err = actorGate(ctx, tx, device, token, true)
	if err == nil {
		err = sourceActor(ctx, tx, token)
	}
	if err == nil {
		err = s.studioJobSourcesTx(ctx, tx, j, actor.TokenID)
	}
	j.Input.Configuration = configuration
	if err == nil {
		_, _, current, e := s.settings.MentorClient()
		err = e
		if err == nil && current != configuration {
			err = ErrInactive
		}
	}
	if err != nil {
		if !sourceUnavailable(err) && !errors.Is(err, ErrInactive) && !errors.Is(err, ErrForbidden) {
			return err
		}
		j.Status = "failed"
		j.Reason = studioFailureCategory(err)
		j.Version++
		if sourceUnavailable(err) {
			j.Status = "invalidated"
		}
		if e := s.studio.SaveStudioJobTx(ctx, tx, &j); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	j.Input.Destination = s.studioDestination()
	if j.Plan == nil || j.Plan.InputFingerprint != planFingerprint(j.Input.Fingerprint, *j.Plan) {
		return learningcontent.ErrConflict
	}
	if err = s.preflightStudioPlanTx(ctx, tx, j); err != nil {
		return err
	}
	if err = s.prepareStudioUnitsTx(ctx, tx, &j); err != nil {
		return err
	}
	j.Version++
	if err = s.enqueueNextStudioUnitTx(ctx, tx, actor, &j); err != nil {
		return err
	}
	if err = s.studio.SaveStudioJobTx(ctx, tx, &j); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
