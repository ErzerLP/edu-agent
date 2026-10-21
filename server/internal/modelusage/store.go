package modelusage

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool        *pgxpool.Pool
	permits     *privacy.ReadPermitManager
	concurrency func() int
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, permits: privacy.DefaultReadPermits, concurrency: func() int { return 10 }}
}

func (s *Store) ConfigureConcurrency(current func() int) { s.concurrency = current }

func gate(ctx context.Context, tx pgx.Tx, generation int64) error {
	var current int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&current); err != nil {
		return err
	}
	if current != generation {
		return ErrInactive
	}
	return nil
}

// BeginTx is called after the business gates in the same transaction. Locking
// the root task serializes attempt numbering without a mutable usage total.
func (s *Store) BeginTx(ctx context.Context, tx pgx.Tx, r BeginRequest) (CallRef, error) {
	ref := CallRef{ID: uuid.NewString(), Generation: r.Generation, APIStyle: r.APIStyle}
	if err := gate(ctx, tx, r.Generation); err != nil {
		return ref, err
	}
	var task string
	err := tx.QueryRow(ctx, `INSERT INTO learning_model_tasks(id,privacy_generation,space_id,source,actor_device_id,kind,origin_id)
	 VALUES($1,$2,NULLIF($3,'')::uuid,$4,NULLIF($5,'')::uuid,$6,$7)
	 ON CONFLICT(privacy_generation,kind,origin_id) DO UPDATE SET origin_id=excluded.origin_id RETURNING id::text`, uuid.NewString(), r.Generation, r.SpaceID, r.Source, r.ActorDeviceID, r.Kind, r.OriginID).Scan(&task)
	if err != nil {
		return ref, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO learning_model_calls(id,task_id,privacy_generation,stage,attempt_key,attempt_no,lease_id,dispatch_state,provider,destination,model,api_style,configuration,lease_until)
	 SELECT $1,$2,$3,$4,$5,COALESCE(max(attempt_no),0)+1,$6,'prepared',$7,$8,$9,$10,$11,$12 FROM learning_model_calls WHERE task_id=$2
	 ON CONFLICT(task_id,attempt_key) DO UPDATE SET attempt_key=excluded.attempt_key RETURNING id::text`, ref.ID, task, r.Generation, r.Stage, r.AttemptKey, r.LeaseID, r.Provider, r.Destination, r.Model, r.APIStyle, r.Configuration, r.LeaseUntil).Scan(&ref.ID)
	return ref, err
}

func (s *Store) Dispatch(ctx context.Context, ref CallRef) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = gate(ctx, tx, ref.Generation); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE learning_model_calls SET dispatch_state='dispatching',started_at=clock_timestamp() WHERE id=$1 AND privacy_generation=$2 AND dispatch_state='prepared'`, ref.ID, ref.Generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInactive
	}
	return tx.Commit(ctx)
}

// Finish only fills previously unknown counters on this call. A lost worker
// lease cannot commit content, but can still settle counters in its generation.
func (s *Store) Finish(ctx context.Context, ref CallRef, state string, u Usage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = gate(ctx, tx, ref.Generation); err != nil {
		return err
	}
	var old Usage
	var oldState string
	err = tx.QueryRow(ctx, `SELECT dispatch_state,input_tokens,output_tokens,reasoning_tokens,provider_total_tokens,usage_inconsistent FROM learning_model_calls WHERE id=$1 AND privacy_generation=$2 FOR UPDATE`, ref.ID, ref.Generation).Scan(&oldState, &old.Input, &old.Output, &old.Reasoning, &old.ProviderTotal, &old.Inconsistent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInactive
	}
	if err != nil {
		return err
	}
	if oldState == "not_dispatched" {
		return nil
	}
	if oldState == "prepared" {
		state = "not_dispatched"
	} else {
		if old.Input == nil {
			old.Input = u.Input
		}
		if old.Output == nil {
			old.Output = u.Output
		}
		if old.Reasoning == nil {
			old.Reasoning = u.Reasoning
		}
		if old.ProviderTotal == nil {
			old.ProviderTotal = u.ProviderTotal
		}
		old.Inconsistent = old.Inconsistent || u.Inconsistent
		old.normalize()
		if oldState != "dispatching" && oldState != "outcome_unknown" {
			state = oldState
		}
	}
	_, err = tx.Exec(ctx, `UPDATE learning_model_calls SET dispatch_state=$3,finished_at=COALESCE(finished_at,clock_timestamp()),input_tokens=$4,output_tokens=$5,reasoning_tokens=$6,provider_total_tokens=$7,total_tokens=$8,usage_inconsistent=$9 WHERE id=$1 AND privacy_generation=$2`, ref.ID, ref.Generation, state, old.Input, old.Output, old.Reasoning, old.ProviderTotal, old.Total(), old.Inconsistent)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecoverTx uses run leases (saved and temporary) or a synchronous HTTP deadline.
// Assessment task origins refer to attempts, which retain their run binding.
// It never replays an outbound HTTP request to repair statistics.
func (s *Store) RecoverTx(ctx context.Context, tx pgx.Tx, generation int64) error {
	if err := s.RecoverSearchTx(ctx, tx, generation); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE learning_model_calls c SET dispatch_state=CASE WHEN c.dispatch_state='prepared' THEN 'not_dispatched' ELSE 'outcome_unknown' END,finished_at=clock_timestamp()
	 FROM learning_model_tasks t WHERE c.task_id=t.id AND c.privacy_generation=$1 AND c.dispatch_state IN ('prepared','dispatching') AND (
	 c.lease_until<clock_timestamp() OR c.lease_until IS NULL AND NOT EXISTS (
	 SELECT 1 FROM learning_mentor_runs r WHERE (r.id=CASE WHEN t.kind='assessment' THEN (SELECT a.run_id FROM learning_assessment_tasks a WHERE a.attempt_id=t.origin_id) ELSE t.origin_id END OR t.kind='topic_index' AND r.session_id=t.origin_id AND r.state->>'kind'='topic_index' OR t.kind='studio' AND r.studio_job_id=t.origin_id OR t.kind='source_research' AND r.source_research_job_id=t.origin_id)
	 AND r.lease_id=c.lease_id AND r.lease_until>clock_timestamp() AND r.state->>'status' IN ('running','cancelling')))`, generation)
	return err
}

// The deployment marker is independent of learner data and survives erasure.
func (s *Store) EnableFullCoverage(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE model_usage_coverage SET full_started_at=clock_timestamp() WHERE full_started_at IS NULL`)
	return err
}
