package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/modeladmission"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type metadata struct {
	Generation                                                  int64
	SessionID, TurnID, RequestID, Stage, SpaceID, Configuration string
}
type execution struct {
	service    *Service
	actor      identity.Credential
	meta       metadata
	connection settings.FrozenConnection
	lease      modeladmission.Lease
	task       string
	ref        modelusage.CallRef
	attempt    int
}
type requestState struct {
	RequestID  string            `json:"request_id"`
	TaskID     *string           `json:"task_id"`
	State      string            `json:"state"`
	Dispatched bool              `json:"dispatched"`
	Usage      modelusage.Totals `json:"usage"`
	Reason     *string           `json:"reason"`
}

// guard locks the same device/token rows as revocation and the persistent
// privacy gates. Output/dispatch commits must retain this short transaction.
func (s *Service) guard(ctx context.Context, actor identity.Credential, generation int64, invoke bool) (pgx.Tx, int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tx.Rollback(context.Background())
		}
	}()
	var current int64
	for _, owner := range []string{"identity", "learning"} {
		if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate($1,'write',NULL)`, owner).Scan(&current); err != nil {
			return nil, 0, gatewayError("privacy_generation_changed")
		}
	}
	if generation != 0 && current != generation {
		return nil, current, gatewayError("privacy_generation_changed")
	}
	var active, allowed, web bool
	err = tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL,'model:invoke'=ANY(t.scopes),EXISTS(SELECT 1 FROM identity_web_sessions w WHERE w.token_id=t.id) FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, actor.Device.ID, actor.TokenID).Scan(&active, &allowed, &web)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!active || web) {
		return nil, current, gatewayError("device_unauthorized")
	}
	if err != nil {
		return nil, current, err
	}
	if invoke && !allowed {
		return nil, current, gatewayError("model_scope_required")
	}
	ok = true
	return tx, current, nil
}

func (e *execution) guard(ctx context.Context) (pgx.Tx, error) {
	return e.guardState(ctx, false)
}
func (e *execution) guardState(ctx context.Context, completed bool) (pgx.Tx, error) {
	tx, _, err := e.service.guard(ctx, e.actor, e.meta.Generation, true)
	if err != nil {
		return nil, err
	}
	var state string
	var reason *string
	var valid bool
	err = tx.QueryRow(ctx, `SELECT r.state,r.reason,EXISTS(SELECT 1 FROM learning_model_execution_leases l WHERE l.id=r.lease_id AND l.privacy_generation=r.privacy_generation AND l.lease_until>clock_timestamp()) FROM learning_cli_model_requests r WHERE r.privacy_generation=$1 AND r.actor_device_id=$2 AND r.request_id=$3 FOR SHARE OF r`, e.meta.Generation, e.actor.Device.ID, e.meta.RequestID).Scan(&state, &reason, &valid)
	allowed := (state == "accepted" || state == "running") && valid
	if completed {
		allowed = state == "succeeded" || state == "failed" && reason != nil && (*reason == "output_limit_exceeded" || *reason == "upstream_error")
	}
	if err != nil || !allowed {
		_ = tx.Rollback(context.Background())
		if reason != nil {
			return nil, gatewayError(*reason)
		}
		return nil, gatewayError("request_cancelled")
	}
	return tx, nil
}
func (e *execution) beginRequest() modelusage.BeginRequest {
	origin := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("cli_gateway\n%d\n%s\n%s\n%s", e.meta.Generation, e.actor.Device.ID, e.meta.SessionID, e.meta.TurnID))).String()
	m := e.connection.Model
	return modelusage.BeginRequest{Generation: e.meta.Generation, SpaceID: e.meta.SpaceID, Source: "cli", ActorDeviceID: e.actor.Device.ID, Kind: "cli_gateway", OriginID: origin, Stage: e.meta.Stage, AttemptKey: uuid.NewSHA1(uuid.MustParse(e.meta.RequestID), []byte(fmt.Sprint(e.attempt))).String(), LeaseID: e.lease.ID, LeaseUntil: &e.lease.Until, Provider: m.Provider, Destination: m.Destination, Model: m.Model, APIStyle: m.APIStyle, Configuration: e.connection.Configuration}
}
func (s *Service) accept(ctx context.Context, e *execution) error {
	tx, _, err := s.guard(ctx, e.actor, e.meta.Generation, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(210021)`); err != nil {
		return err
	}
	var duplicate bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_cli_model_requests WHERE privacy_generation=$1 AND actor_device_id=$2 AND request_id=$3)`, e.meta.Generation, e.actor.Device.ID, e.meta.RequestID).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return gatewayError("request_already_accepted")
	}
	if e.meta.SpaceID != "" {
		var allowed bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_spaces WHERE id=$1 AND status='active') AND EXISTS(SELECT 1 FROM device_tokens WHERE id=$2 AND 'learning:read'=ANY(scopes))`, e.meta.SpaceID, e.actor.TokenID).Scan(&allowed)
		if err != nil {
			return err
		}
		if !allowed {
			return gatewayError("invalid_request")
		}
	}
	var space *string
	err = tx.QueryRow(ctx, `SELECT space_id::text FROM learning_cli_model_turns WHERE privacy_generation=$1 AND actor_device_id=$2 AND session_id=$3 AND turn_id=$4`, e.meta.Generation, e.actor.Device.ID, e.meta.SessionID, e.meta.TurnID).Scan(&space)
	if err == nil {
		old := ""
		if space != nil {
			old = *space
		}
		if old != e.meta.SpaceID {
			return gatewayError("turn_binding_conflict")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	capacity := s.Settings.View().Limits.Concurrency
	if capacity < 2 {
		return gatewayError("gateway_capacity_disabled")
	}
	if e.lease, err = modeladmission.AcquireTx(ctx, tx, e.meta.Generation, capacity, "cli", e.actor.Device.ID, e.meta.RequestID); err != nil {
		return err
	}
	if e.ref, err = s.Usage.BeginTx(ctx, tx, e.beginRequest()); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT task_id::text FROM learning_model_calls WHERE id=$1`, e.ref.ID).Scan(&e.task); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_cli_model_turns(privacy_generation,actor_device_id,session_id,turn_id,space_id,task_id) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6) ON CONFLICT DO NOTHING`, e.meta.Generation, e.actor.Device.ID, e.meta.SessionID, e.meta.TurnID, e.meta.SpaceID, e.task)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_cli_model_requests(privacy_generation,actor_device_id,request_id,task_id,target,configuration_revision,state,lease_id,lease_until) VALUES($1,$2,$3,$4,$5,$6,'accepted',$7,$8)`, e.meta.Generation, e.actor.Device.ID, e.meta.RequestID, e.task, e.connection.Projection().Target, e.meta.Configuration, e.lease.ID, e.lease.Until)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (e *execution) renew(ctx context.Context) (time.Time, error) {
	tx, err := e.guard(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(context.Background())
	// Capacity changes take effect for the newest CLI requests first.
	var rank int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM learning_model_execution_leases WHERE source='cli' AND lease_until>clock_timestamp() AND (created_at,id)<=(SELECT created_at,id FROM learning_model_execution_leases WHERE id=$1)`, e.lease.ID).Scan(&rank)
	if err != nil {
		return time.Time{}, err
	}
	if rank > max(0, e.service.Settings.View().Limits.Concurrency-1) {
		return time.Time{}, gatewayError("configuration_changed")
	}
	until, err := modeladmission.RenewTx(ctx, tx, e.lease)
	if err != nil {
		return until, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_model_calls SET lease_until=$2 WHERE lease_id=$1 AND dispatch_state IN ('prepared','dispatching')`, e.lease.ID, until); err != nil {
		return until, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_cli_model_requests SET lease_until=$2 WHERE lease_id=$1`, e.lease.ID, until); err != nil {
		return until, err
	}
	return until, tx.Commit(ctx)
}
func (e *execution) finish(ctx context.Context, state, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// A prepared attempt is not dispatched. Observe already settled real calls.
	if err := e.service.Usage.Finish(ctx, e.ref, "outcome_unknown", modelusage.Usage{}); err != nil {
		return err
	}
	tx, err := e.service.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var g int64
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&g); err != nil || g != e.meta.Generation {
		return gatewayError("privacy_generation_changed")
	}
	tag, err := tx.Exec(ctx, `UPDATE learning_cli_model_requests SET state=$4,reason=NULLIF($5,''),finished_at=clock_timestamp() WHERE privacy_generation=$1 AND actor_device_id=$2 AND request_id=$3 AND state IN ('accepted','running') AND ($4<>'succeeded' OR EXISTS(SELECT 1 FROM learning_model_execution_leases l WHERE l.id=lease_id AND l.lease_until>clock_timestamp()))`, e.meta.Generation, e.actor.Device.ID, e.meta.RequestID, state, reason)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM learning_model_execution_leases WHERE id=$1`, e.lease.ID); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if state == "succeeded" && tag.RowsAffected() != 1 {
		return gatewayError("request_cancelled")
	}
	return nil
}
func (s *Service) recover(ctx context.Context, tx pgx.Tx, g int64) error {
	if err := s.Usage.RecoverTx(ctx, tx, g); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE learning_cli_model_requests SET state='outcome_unknown',reason='gateway_unavailable',finished_at=clock_timestamp() WHERE privacy_generation=$1 AND state IN ('accepted','running') AND lease_until<=clock_timestamp()`, g)
	return err
}
func (s *Service) state(ctx context.Context, tx pgx.Tx, actor identity.Credential, g int64, id string) (requestState, error) {
	r := requestState{RequestID: id}
	if err := s.recover(ctx, tx, g); err != nil {
		return r, err
	}
	var lease *string
	err := tx.QueryRow(ctx, `SELECT task_id::text,state,dispatched,reason,lease_id::text FROM learning_cli_model_requests WHERE privacy_generation=$1 AND actor_device_id=$2 AND request_id=$3`, g, actor.Device.ID, id).Scan(&r.TaskID, &r.State, &r.Dispatched, &r.Reason, &lease)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, gatewayError("request_not_found")
	}
	if err != nil {
		return r, err
	}
	if lease == nil {
		return r, nil
	}
	t := &r.Usage
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE dispatch_state='dispatching'),count(*) FILTER(WHERE dispatch_state='outcome_unknown'),COALESCE(sum(input_tokens),0)::bigint,count(*) FILTER(WHERE input_tokens IS NULL),COALESCE(sum(output_tokens),0)::bigint,count(*) FILTER(WHERE output_tokens IS NULL),COALESCE(sum(reasoning_tokens),0)::bigint,count(*) FILTER(WHERE reasoning_tokens IS NULL),COALESCE(sum(total_tokens),0)::bigint,count(*) FILTER(WHERE total_tokens IS NULL) FROM learning_model_calls WHERE privacy_generation=$1 AND lease_id=$2 AND started_at IS NOT NULL`, g, *lease).Scan(&t.Calls, &t.PendingCalls, &t.OutcomeUnknownCalls, &t.Input.KnownSum, &t.Input.UnknownCalls, &t.Output.KnownSum, &t.Output.UnknownCalls, &t.Reasoning.KnownSum, &t.Reasoning.UnknownCalls, &t.Total.KnownSum, &t.Total.UnknownCalls)
	return r, err
}
