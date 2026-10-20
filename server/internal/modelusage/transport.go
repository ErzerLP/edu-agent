package modelusage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/modeladmission"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/access"
	"github.com/google/uuid"
)

var ErrContext = errors.New("model_usage_context_required")

// ConfigureReadPermits is called by the composition root before serving work.
func (s *Store) ConfigureReadPermits(permits *privacy.ReadPermitManager) { s.permits = permits }

// Transport is the only accounting layer for the teaching/selector/probe
// client. Mentor workers use their existing gated budgetTransport instead.
func (s *Store) Transport(next http.RoundTripper, model Model, configuration string) http.RoundTripper {
	return usageTransport{store: s, next: next, model: model, configuration: configuration}
}

type usageTransport struct {
	store         *Store
	next          http.RoundTripper
	model         Model
	configuration string
}

func (t usageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// A cached capability's HEAD availability check does not invoke a model.
	if request.Method == http.MethodHead {
		return t.next.RoundTrip(request)
	}
	task, ok := request.Context().Value(taskKey{}).(taskContext)
	if !ok {
		return nil, ErrContext
	}
	permit, err := t.store.permits.Acquire(request.Context(), privacy.OwnerIdentity, privacy.OwnerLearning, privacy.OwnerKnowledge, privacy.OwnerTutoring)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(permit.Context())
	request = request.WithContext(ctx)
	ref, err := t.begin(request.Context(), task)
	var stopLease func()
	if err == nil {
		lease := modeladmission.Lease{ID: ref.LeaseID, Generation: ref.Generation, Until: ref.LeaseUntil}
		stopLease = modeladmission.Maintain(ctx, lease.Until, func(ctx context.Context) (time.Time, error) {
			tx, err := t.store.pool.Begin(ctx)
			if err != nil {
				return time.Time{}, err
			}
			defer tx.Rollback(context.Background())
			if err = gate(ctx, tx, ref.Generation); err != nil {
				return time.Time{}, err
			}
			until, err := modeladmission.RenewTx(ctx, tx, lease)
			if err != nil {
				return until, err
			}
			if _, err = tx.Exec(ctx, `UPDATE learning_model_calls SET lease_until=$2 WHERE id=$1`, ref.ID, until); err != nil {
				return until, err
			}
			return until, tx.Commit(ctx)
		}, cancel)
	}
	cleanup := func() {
		if stopLease != nil {
			stopLease()
		}
		cancel(nil)
		if ref.LeaseID != "" {
			bounded, end := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer end()
			_ = modeladmission.Release(bounded, t.store.pool, modeladmission.Lease{ID: ref.LeaseID, Generation: ref.Generation})
		}
		permit.Release()
	}
	if err == nil {
		err = t.store.Dispatch(request.Context(), ref)
	}
	if err != nil {
		if ref.ID != "" {
			t.store.settle(request.Context(), ref, "not_dispatched", Usage{})
		}
		cleanup()
		return nil, err
	}
	response, err := t.store.Observe(request.Context(), ref, t.next, request)
	if err != nil {
		cleanup()
		return response, err
	}
	response.Body = &permitBody{ReadCloser: response.Body, cleanup: cleanup}
	return response, nil
}

func (t usageTransport) begin(ctx context.Context, task taskContext) (CallRef, error) {
	tx, err := t.store.pool.Begin(ctx)
	if err != nil {
		return CallRef{}, err
	}
	defer tx.Rollback(context.Background())
	root := task.root
	root.mu.Lock()
	defer root.mu.Unlock()
	var generation int64
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&generation); err != nil {
		return CallRef{}, err
	}
	if root.Generation != 0 && root.Generation != generation {
		return CallRef{}, ErrInactive
	}
	root.Generation = generation
	source, device := "server", ""
	if actor, ok := access.CredentialFromContext(ctx); ok {
		var active, web bool
		err = tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL,EXISTS(SELECT 1 FROM identity_web_sessions w WHERE w.token_id=t.id)
		 FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, actor.Device.ID, actor.TokenID).Scan(&active, &web)
		if err != nil {
			return CallRef{}, err
		}
		if !active {
			return CallRef{}, ErrInactive
		}
		source, device = "cli", actor.Device.ID
		if web {
			source = "web"
		}
	}
	origin := root.OriginID
	// These user-supplied IDs are unique per device in their owning stores.
	// Derive a stable global UUID so another device cannot merge task ownership.
	if device != "" && (root.Kind == "teaching" || root.Kind == "planning" || root.Kind == "offline_prepare") {
		origin = uuid.NewSHA1(uuid.NameSpaceOID, []byte(root.Kind+"\n"+device+"\n"+root.OriginID)).String()
	}
	// Synchronous model calls share admission with mentor runs and CLI. Mentor
	// workers use their own transport and inherit their parent run slot.
	lease, err := modeladmission.AcquireTx(ctx, tx, generation, t.store.concurrency(), "server", device, "")
	if err != nil {
		return CallRef{}, err
	}
	ref, err := t.store.BeginTx(ctx, tx, BeginRequest{
		Generation: generation, SpaceID: root.SpaceID, Source: source, ActorDeviceID: device, Kind: root.Kind, OriginID: origin,
		Stage: task.stage, AttemptKey: uuid.NewString(), LeaseID: lease.ID, LeaseUntil: &lease.Until,
		Provider: t.model.Provider, Destination: t.model.Destination, Model: t.model.Model, APIStyle: t.model.APIStyle, Configuration: t.configuration,
	})
	if err != nil {
		return CallRef{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CallRef{}, err
	}
	ref.LeaseID, ref.LeaseUntil = lease.ID, lease.Until
	return ref, nil
}

type permitBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *permitBody) Close() error {
	err := b.ReadCloser.Close()
	b.cleanup()
	return err
}
