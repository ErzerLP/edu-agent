package learningcontent

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func unitIdentity(j StudioJob, id string) string {
	return fmt.Sprintf("%d:%s", j.ConfirmedPlanRevision, id)
}
func (s *Store) SaveStudioUnitTx(ctx context.Context, tx pgx.Tx, j StudioJob, u StudioUnit) error {
	payload := studioUnitPayload{u.SectionID, u.Topic, u.Objective, u.InputFingerprint, u.Slice, u.Body}
	raw, err := s.sealStudioJob(j, "unit", unitIdentity(j, u.ID), payload)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO learning_studio_units(job_id,plan_revision,unit_id,ordinal,status,run_id,attempt,ciphertext) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,$7,$8) ON CONFLICT(job_id,plan_revision,unit_id) DO UPDATE SET status=excluded.status,run_id=excluded.run_id,attempt=excluded.attempt,ciphertext=excluded.ciphertext WHERE learning_studio_units.status<>'succeeded'`, j.ID, j.ConfirmedPlanRevision, u.ID, u.Ordinal, u.Status, u.RunID, u.Attempt, raw)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
func (s *Store) StudioUnitTx(ctx context.Context, tx pgx.Tx, j StudioJob, id string) (StudioUnit, error) {
	var u StudioUnit
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT unit_id,ordinal,status,COALESCE(run_id::text,''),attempt,ciphertext FROM learning_studio_units WHERE job_id=$1 AND plan_revision=$2 AND unit_id=$3`, j.ID, j.ConfirmedPlanRevision, id).Scan(&u.ID, &u.Ordinal, &u.Status, &u.RunID, &u.Attempt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	var payload studioUnitPayload
	if err = s.openStudioJob(j, "unit", unitIdentity(j, u.ID), raw, &payload); err != nil {
		return u, err
	}
	u.SectionID, u.Topic, u.Objective, u.InputFingerprint, u.Slice, u.Body = payload.SectionID, payload.Topic, payload.Objective, payload.InputFingerprint, payload.Slice, payload.Body
	return u, nil
}
func (s *Store) StudioUnitsTx(ctx context.Context, tx pgx.Tx, j StudioJob, after, limit int) (StudioUnitPage, error) {
	page := StudioUnitPage{Items: []StudioUnit{}}
	rows, err := tx.Query(ctx, `SELECT unit_id FROM learning_studio_units WHERE job_id=$1 AND plan_revision=$2 AND ordinal>$3 ORDER BY ordinal LIMIT $4`, j.ID, j.ConfirmedPlanRevision, after, limit+1)
	if err != nil {
		return page, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return page, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return page, err
	}
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	for _, id := range ids {
		u, e := s.StudioUnitTx(ctx, tx, j, id)
		if e != nil {
			return page, e
		}
		page.Items = append(page.Items, u)
	}
	if more {
		page.NextCursor = strconv.Itoa(page.Items[len(page.Items)-1].Ordinal)
	}
	return page, nil
}
func (s *Store) StudioProgressTx(ctx context.Context, tx pgx.Tx, j *StudioJob) error {
	j.Result = j.Input.Result
	if j.RunID != "" {
		err := tx.QueryRow(ctx, `SELECT COALESCE((state->>'result_unknown')::boolean,false),COALESCE((state->>'cost_unknown')::boolean,false) FROM learning_mentor_runs WHERE id=$1`, j.RunID).Scan(&j.ResultUnknown, &j.CostUnknown)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='succeeded'),COALESCE((array_agg(unit_id ORDER BY ordinal) FILTER(WHERE status<>'succeeded'))[1],'') FROM learning_studio_units WHERE job_id=$1 AND plan_revision=$2`, j.ID, j.ConfirmedPlanRevision).Scan(&j.TotalUnits, &j.CompletedUnits, &j.CurrentUnit)
}
func (s *Store) VerifyStudioUnitTx(ctx context.Context, tx pgx.Tx, token string, u StudioUnit) error {
	if u.Body == nil {
		return ErrInvalid
	}
	return s.studioSourcesTx(ctx, tx, token, *u.Body)
}
