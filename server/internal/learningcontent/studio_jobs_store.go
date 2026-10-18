package learningcontent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

func studioJobAAD(j StudioJob, purpose, identity string) string {
	return fmt.Sprintf("studio-job:v1:%s:%s:%d:%s:%s", j.SpaceID, j.ID, j.Generation, purpose, identity)
}
func (s *Store) sealStudioJob(j StudioJob, purpose, identity string, value any) ([]byte, error) {
	if !s.Available() {
		return nil, ErrUnavailable
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	if len(raw) > 256<<10 {
		return nil, ErrInvalid
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, raw, []byte(studioJobAAD(j, purpose, identity))), nil
}
func (s *Store) openStudioJob(j StudioJob, purpose, identity string, raw []byte, value any) error {
	if !s.Available() || len(raw) < s.aead.NonceSize() {
		return ErrUnavailable
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(studioJobAAD(j, purpose, identity)))
	if err != nil {
		return ErrUnavailable
	}
	defer clear(plain)
	return json.Unmarshal(plain, value)
}

// These owner methods share the application's already gated transaction. They
// never open a second transaction across operation/CAS or stage publication.
func (s *Store) StudioJobTx(ctx context.Context, tx pgx.Tx, id string, g int64, lock bool) (StudioJob, error) {
	j := StudioJob{Messages: []StudioPlanMessage{}, SuggestedFocus: []string{}}
	var raw []byte
	q := `SELECT id,space_id,device_id,privacy_generation,version,kind,status,active_plan_revision,confirmed_plan_revision,COALESCE(run_id::text,''),ciphertext,created_at,updated_at FROM learning_studio_jobs WHERE id=$1 AND space_id=$2 AND privacy_generation=$3`
	if lock {
		q += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, q, id, learningspace.Scope(ctx), g).Scan(&j.ID, &j.SpaceID, &j.DeviceID, &j.Generation, &j.Version, &j.Kind, &j.Status, &j.ActivePlanRevision, &j.ConfirmedPlanRevision, &j.RunID, &raw, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if err = s.openStudioJob(j, "input", "", raw, &j.Input); err != nil {
		return j, err
	}
	j.Reason = j.Input.Reason
	j.SuggestedFocus = j.Input.LearningState.SuggestedFocus
	if j.ActivePlanRevision > 0 {
		if err = tx.QueryRow(ctx, `SELECT ciphertext FROM learning_studio_plans WHERE job_id=$1 AND revision=$2`, id, j.ActivePlanRevision).Scan(&raw); err != nil {
			return j, err
		}
		j.Plan = &StudioPlan{}
		if err = s.openStudioJob(j, "plan", fmt.Sprint(j.ActivePlanRevision), raw, j.Plan); err != nil {
			return j, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT operation_id::text,ciphertext FROM learning_studio_plan_messages WHERE job_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	for rows.Next() {
		var operation string
		var message StudioPlanMessage
		if err = rows.Scan(&operation, &raw); err != nil {
			return j, err
		}
		if err = s.openStudioJob(j, "message", operation, raw, &message); err != nil {
			return j, err
		}
		j.Messages = append(j.Messages, message)
	}
	return j, rows.Err()
}
func (s *Store) InsertStudioJobTx(ctx context.Context, tx pgx.Tx, j *StudioJob) error {
	raw, err := s.sealStudioJob(*j, "input", "", j.Input)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `INSERT INTO learning_studio_jobs(id,space_id,device_id,privacy_generation,version,kind,status,ciphertext) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at,updated_at`, j.ID, j.SpaceID, j.DeviceID, j.Generation, j.Version, j.Kind, j.Status, raw).Scan(&j.CreatedAt, &j.UpdatedAt)
}
func (s *Store) SaveStudioJobTx(ctx context.Context, tx pgx.Tx, j *StudioJob) error {
	j.Input.Reason = j.Reason
	raw, err := s.sealStudioJob(*j, "input", "", j.Input)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `UPDATE learning_studio_jobs SET version=$2,status=$3,active_plan_revision=$4,confirmed_plan_revision=$5,run_id=NULLIF($6,'')::uuid,ciphertext=$7,updated_at=clock_timestamp() WHERE id=$1 RETURNING updated_at`, j.ID, j.Version, j.Status, j.ActivePlanRevision, j.ConfirmedPlanRevision, j.RunID, raw).Scan(&j.UpdatedAt)
}
func (s *Store) AppendStudioPlanTx(ctx context.Context, tx pgx.Tx, j *StudioJob, p StudioPlan) error {
	p.Revision = j.ActivePlanRevision + 1
	raw, err := s.sealStudioJob(*j, "plan", fmt.Sprint(p.Revision), p)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_studio_plans(job_id,revision,ciphertext) VALUES($1,$2,$3)`, j.ID, p.Revision, raw); err != nil {
		return err
	}
	j.Plan = &p
	j.ActivePlanRevision = p.Revision
	j.ConfirmedPlanRevision = 0
	return nil
}
func (s *Store) SaveStudioPlanMessageTx(ctx context.Context, tx pgx.Tx, j StudioJob, m StudioPlanMessage) error {
	raw, err := s.sealStudioJob(j, "message", m.OperationID, m)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_studio_plan_messages(job_id,operation_id,ordinal,ciphertext) SELECT $1,$2,COALESCE(max(ordinal),0)+1,$3 FROM learning_studio_plan_messages WHERE job_id=$1 ON CONFLICT(job_id,operation_id) DO UPDATE SET ciphertext=excluded.ciphertext`, j.ID, m.OperationID, raw)
	return err
}

type studioJobReceipt struct {
	Version, ActivePlanRevision, ConfirmedPlanRevision int64
	Status, RunID, Reason                              string
	MessageCount                                       int
	UpdatedAt                                          time.Time
}

func (s *Store) StudioJobOperationTx(ctx context.Context, tx pgx.Tx, device, operation, hash string, g int64) (StudioJob, bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "studio-job-operation:"+device+":"+operation); err != nil {
		return StudioJob{}, false, err
	}
	var id, stored string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT job_id::text,encode(request_hash,'hex'),ciphertext FROM learning_studio_job_operations WHERE device_id=$1 AND operation_id=$2`, device, operation).Scan(&id, &stored, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return StudioJob{}, false, nil
	}
	if err != nil {
		return StudioJob{}, false, err
	}
	if stored != hash {
		return StudioJob{}, false, ErrConflict
	}
	current, err := s.StudioJobTx(ctx, tx, id, g, false)
	if err != nil {
		return current, false, err
	}
	var receipt studioJobReceipt
	if err = s.openStudioJob(current, "operation", device+":"+operation, raw, &receipt); err != nil {
		return StudioJob{}, false, err
	}
	// Receipts retain exact operation versions without copying the growing edit
	// history into every operation. Plans are immutable, messages stay with job.
	result := current
	result.Version, result.ActivePlanRevision, result.ConfirmedPlanRevision = receipt.Version, receipt.ActivePlanRevision, receipt.ConfirmedPlanRevision
	result.Status, result.RunID, result.Reason, result.UpdatedAt = receipt.Status, receipt.RunID, receipt.Reason, receipt.UpdatedAt
	result.Messages = result.Messages[:min(len(result.Messages), receipt.MessageCount)]
	result.Plan = nil
	if receipt.ActivePlanRevision > 0 {
		if err = tx.QueryRow(ctx, `SELECT ciphertext FROM learning_studio_plans WHERE job_id=$1 AND revision=$2`, result.ID, receipt.ActivePlanRevision).Scan(&raw); err != nil {
			return result, false, err
		}
		result.Plan = &StudioPlan{}
		if err = s.openStudioJob(result, "plan", fmt.Sprint(receipt.ActivePlanRevision), raw, result.Plan); err != nil {
			return result, false, err
		}
	}
	return result, true, nil
}
func (s *Store) RecordStudioJobOperationTx(ctx context.Context, tx pgx.Tx, j StudioJob, device, operation, hash string) error {
	receipt := studioJobReceipt{j.Version, j.ActivePlanRevision, j.ConfirmedPlanRevision, j.Status, j.RunID, j.Reason, len(j.Messages), j.UpdatedAt}
	raw, err := s.sealStudioJob(j, "operation", device+":"+operation, receipt)
	if err != nil {
		return err
	}
	digest, err := hex.DecodeString(hash)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_studio_job_operations(device_id,operation_id,job_id,request_hash,ciphertext) VALUES($1,$2,$3,$4,$5)`, device, operation, j.ID, digest, raw)
	return err
}
