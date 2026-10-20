package modelusage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/access"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BeginSearchTx uses the same task root as models without inventing model usage.
func (s *Store) BeginSearchTx(ctx context.Context, tx pgx.Tx, r BeginRequest) (CallRef, error) {
	ref := CallRef{ID: uuid.NewString(), Generation: r.Generation}
	if err := gate(ctx, tx, r.Generation); err != nil {
		return ref, err
	}
	var task string
	err := tx.QueryRow(ctx, `INSERT INTO learning_model_tasks(id,privacy_generation,space_id,source,actor_device_id,kind,origin_id) VALUES($1,$2,NULLIF($3,'')::uuid,$4,NULLIF($5,'')::uuid,$6,$7) ON CONFLICT(privacy_generation,kind,origin_id) DO UPDATE SET origin_id=excluded.origin_id RETURNING id::text`, uuid.NewString(), r.Generation, r.SpaceID, r.Source, r.ActorDeviceID, r.Kind, r.OriginID).Scan(&task)
	if err != nil {
		return ref, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO learning_search_calls(id,task_id,privacy_generation,attempt_key,attempt_no,lease_id,lease_until,provider,configuration,stage,state) SELECT $1,$2,$3,$4,COALESCE(max(attempt_no),0)+1,$5,$6,$7,$8,$9,'prepared' FROM learning_search_calls WHERE task_id=$2 ON CONFLICT(task_id,attempt_key) DO UPDATE SET attempt_key=excluded.attempt_key RETURNING id::text`, ref.ID, task, r.Generation, r.AttemptKey, r.LeaseID, r.LeaseUntil, r.Provider, r.Configuration, r.Stage).Scan(&ref.ID)
	return ref, err
}
func (s *Store) DispatchSearch(ctx context.Context, ref CallRef) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = gate(ctx, tx, ref.Generation); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE learning_search_calls SET state='dispatching',started_at=clock_timestamp() WHERE id=$1 AND privacy_generation=$2 AND state='prepared'`, ref.ID, ref.Generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInactive
	}
	return tx.Commit(ctx)
}
func (s *Store) FinishSearch(ctx context.Context, ref CallRef, state string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = gate(ctx, tx, ref.Generation); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE learning_search_calls SET state=CASE WHEN state='prepared' THEN 'not_dispatched' ELSE $3 END,finished_at=COALESCE(finished_at,clock_timestamp()) WHERE id=$1 AND privacy_generation=$2 AND state IN ('prepared','dispatching','outcome_unknown')`, ref.ID, ref.Generation, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_search_calls WHERE id=$1 AND privacy_generation=$2)`, ref.ID, ref.Generation).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrInactive
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) RecoverSearchTx(ctx context.Context, tx pgx.Tx, g int64) error {
	_, err := tx.Exec(ctx, `UPDATE learning_search_calls SET state=CASE WHEN state='prepared' THEN 'not_dispatched' ELSE 'outcome_unknown' END,finished_at=clock_timestamp() WHERE privacy_generation=$1 AND state IN ('prepared','dispatching') AND lease_until<clock_timestamp()`, g)
	return err
}
func (s *Store) SearchTransport(next http.RoundTripper, provider, configuration string) http.RoundTripper {
	return searchTransport{store: s, next: next, provider: provider, configuration: configuration}
}

type searchTransport struct {
	store                   *Store
	next                    http.RoundTripper
	provider, configuration string
}

func (t searchTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	task, ok := request.Context().Value(taskKey{}).(taskContext)
	if !ok {
		return nil, ErrContext
	}
	permit, err := t.store.permits.Acquire(request.Context(), privacy.OwnerIdentity, privacy.OwnerLearning)
	if err != nil {
		return nil, err
	}
	request = request.WithContext(permit.Context())
	ref, err := t.begin(request.Context(), task)
	settle := func(state string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 5*time.Second)
		defer cancel()
		_ = t.store.FinishSearch(ctx, ref, state)
	}
	if err == nil {
		err = t.store.DispatchSearch(request.Context(), ref)
	}
	if err != nil {
		if ref.ID != "" {
			settle("not_dispatched")
		}
		permit.Release()
		return nil, err
	}
	response, err := t.next.RoundTrip(request)
	if err != nil {
		state := "failed"
		if errors.Is(err, context.Canceled) {
			state = "cancelled"
		}
		settle(state)
		permit.Release()
		return response, err
	}
	state := "received"
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		state = "failed"
	}
	response.Body = &searchBody{ReadCloser: response.Body, finish: func(readErr error) {
		if readErr != nil && readErr != io.EOF {
			state = "failed"
		}
		if request.Context().Err() != nil {
			state = "cancelled"
		}
		settle(state)
		permit.Release()
	}}
	return response, nil
}

type searchBody struct {
	io.ReadCloser
	once   sync.Once
	finish func(error)
}

func (b *searchBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(func() { b.finish(err) })
	}
	return n, err
}
func (b *searchBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.finish(err) })
	return err
}
func (t searchTransport) begin(ctx context.Context, task taskContext) (CallRef, error) {
	tx, err := t.store.pool.Begin(ctx)
	if err != nil {
		return CallRef{}, err
	}
	defer tx.Rollback(context.Background())
	root := task.root
	root.mu.Lock()
	defer root.mu.Unlock()
	var g int64
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&g); err != nil {
		return CallRef{}, err
	}
	if root.Generation != 0 && root.Generation != g {
		return CallRef{}, ErrInactive
	}
	root.Generation = g
	source, device := "server", ""
	if actor, ok := access.CredentialFromContext(ctx); ok {
		var active, web bool
		err = tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL,EXISTS(SELECT 1 FROM identity_web_sessions w WHERE w.token_id=t.id) FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, actor.Device.ID, actor.TokenID).Scan(&active, &web)
		if err != nil {
			return CallRef{}, err
		}
		if !active {
			return CallRef{}, ErrInactive
		}
		device = actor.Device.ID
		source = "cli"
		if web {
			source = "web"
		}
	}
	until := time.Now().Add(45 * time.Second)
	ref, err := t.store.BeginSearchTx(ctx, tx, BeginRequest{Generation: g, SpaceID: root.SpaceID, Source: source, ActorDeviceID: device, Kind: root.Kind, OriginID: root.OriginID, Stage: task.stage, AttemptKey: uuid.NewString(), LeaseID: uuid.NewString(), LeaseUntil: &until, Provider: t.provider, Configuration: t.configuration})
	if err != nil {
		return ref, err
	}
	return ref, tx.Commit(ctx)
}
