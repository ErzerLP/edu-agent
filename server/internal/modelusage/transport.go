package modelusage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

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
	request = request.WithContext(permit.Context())
	ref, err := t.begin(request.Context(), task)
	if err == nil {
		err = t.store.Dispatch(request.Context(), ref)
	}
	if err != nil {
		if ref.ID != "" {
			t.store.settle(request.Context(), ref, "not_dispatched", Usage{})
		}
		permit.Release()
		return nil, err
	}
	response, err := t.store.Observe(request.Context(), ref, t.next, request)
	if err != nil {
		permit.Release()
		return response, err
	}
	response.Body = &permitBody{ReadCloser: response.Body, permit: permit}
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
	// llm.Client always supplies its configured HTTP deadline. Keep enough time
	// for the bounded detached receipt write before recovery marks it unknown.
	deadline, _ := ctx.Deadline()
	deadline = deadline.Add(5 * time.Second)
	ref, err := t.store.BeginTx(ctx, tx, BeginRequest{
		Generation: generation, SpaceID: root.SpaceID, Source: source, ActorDeviceID: device, Kind: root.Kind, OriginID: origin,
		Stage: task.stage, AttemptKey: uuid.NewString(), LeaseID: uuid.NewString(), LeaseUntil: &deadline,
		Provider: t.model.Provider, Destination: t.model.Destination, Model: t.model.Model, APIStyle: t.model.APIStyle, Configuration: t.configuration,
	})
	if err != nil {
		return CallRef{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CallRef{}, err
	}
	return ref, nil
}

type permitBody struct {
	io.ReadCloser
	permit *privacy.ReadPermit
}

func (b *permitBody) Close() error {
	err := b.ReadCloser.Close()
	b.permit.Release()
	return err
}
