// Package modelgateway coordinates device-only model calls without persisting content.
package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Authenticator interface {
	Authenticate(context.Context, string, string) (identity.Credential, error)
}
type Options struct {
	Pool           *pgxpool.Pool
	Settings       *settings.Service
	Identity       Authenticator
	Usage          *modelusage.Store
	Permits        *privacy.ReadPermitManager
	RequestTimeout time.Duration
}
type Service struct {
	Options
	ctx    context.Context
	stop   context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	ready  bool
	active map[*execution]context.CancelCauseFunc
	schema string
}
type Config struct {
	ProtocolVersion   int                        `json:"protocol_version"`
	InvokeAllowed     bool                       `json:"invoke_allowed"`
	Available         bool                       `json:"available"`
	UnavailableReason *string                    `json:"unavailable_reason"`
	PrivacyGeneration int64                      `json:"privacy_generation"`
	Models            []settings.ModelProjection `json:"models"`
	Limits            struct {
		MaxToolSteps          *int `json:"max_tool_steps"`
		IdleTimeoutSeconds    int  `json:"idle_timeout_seconds"`
		RequestTimeoutSeconds int  `json:"request_timeout_seconds"`
	} `json:"limits"`
	Capabilities struct {
		Stream          bool `json:"stream"`
		Tools           bool `json:"tools"`
		ReasoningDeltas bool `json:"reasoning_deltas"`
	} `json:"capabilities"`
	Admission struct {
		Concurrency int  `json:"concurrency"`
		PerDevice   int  `json:"per_device"`
		CLICapacity int  `json:"cli_capacity"`
		Queue       bool `json:"queue"`
	} `json:"admission"`
}
type gatewayError string

func (e gatewayError) Error() string       { return string(e) }
func (e gatewayError) GatewayCode() string { return string(e) }

func New(ctx context.Context, o Options) (*Service, error) {
	if o.Pool == nil || o.Settings == nil || o.Identity == nil || o.Usage == nil {
		return nil, errors.New("model gateway dependencies required")
	}
	if o.Permits == nil {
		o.Permits = privacy.DefaultReadPermits
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 15 * time.Minute
	}
	s := &Service{Options: o, done: make(chan struct{}), active: map[*execution]context.CancelCauseFunc{}}
	s.ctx, s.stop = context.WithCancel(ctx)
	var key string
	if err := o.Pool.QueryRow(ctx, `SELECT current_schema(),secret FROM model_gateway_revision_key`).Scan(&s.schema, &key); err != nil {
		s.stop()
		return nil, err
	}
	o.Settings.ConfigureGatewayRevisionKey([]byte(key))
	conn, err := s.listen(ctx)
	if err != nil {
		s.stop()
		return nil, err
	}
	s.ready = true
	go s.notifications(conn)
	return s, nil
}

// Sweep only recovers metadata and counters. It never replays a model request.
func (s *Service) Sweep(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var g int64
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&g); err != nil {
		return err
	}
	if err = s.recover(ctx, tx, g); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM learning_model_execution_leases WHERE privacy_generation=$1 AND lease_until<=clock_timestamp()`, g); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) Close() { s.stop(); s.cancelAll(gatewayError("gateway_unavailable")); <-s.done }
func (s *Service) listen(ctx context.Context) (*pgx.Conn, error) {
	c, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	if _, err = c.Exec(ctx, `LISTEN model_gateway_cancel`); err != nil {
		_ = c.Close(ctx)
		return nil, err
	}
	return c, nil
}
func (s *Service) cancelAll(reason error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = false
	for _, cancel := range s.active {
		cancel(reason)
	}
}
func (s *Service) notifications(conn *pgx.Conn) {
	defer close(s.done)
	for {
		n, err := conn.WaitForNotification(s.ctx)
		if err == nil {
			var value struct {
				Schema           string `json:"schema"`
				Device           string `json:"device"`
				Request          string `json:"request"`
				Reason           string `json:"reason"`
				Generation       int64  `json:"generation"`
				BeforeGeneration int64  `json:"before_generation"`
			}
			if json.Unmarshal([]byte(n.Payload), &value) == nil && value.Schema == s.schema {
				s.mu.Lock()
				for e, cancel := range s.active {
					if (value.Device == "" || e.actor.Device.ID == value.Device) && (value.Request == "" || e.meta.RequestID == value.Request) && (value.Generation == 0 || e.meta.Generation == value.Generation) && (value.BeforeGeneration == 0 || e.meta.Generation < value.BeforeGeneration) {
						cancel(gatewayError(value.Reason))
					}
				}
				s.mu.Unlock()
			}
			continue
		}
		s.cancelAll(gatewayError("gateway_unavailable"))
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = conn.Close(closeCtx)
		closeCancel()
		if s.ctx.Err() != nil {
			return
		}
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(time.Second):
			}
			bounded, end := context.WithTimeout(s.ctx, 5*time.Second)
			conn, err = s.listen(bounded)
			end()
			if err == nil {
				s.mu.Lock()
				s.ready = true
				s.mu.Unlock()
				break
			}
		}
	}
}
func (s *Service) register(e *execution, cancel context.CancelCauseFunc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return gatewayError("gateway_unavailable")
	}
	s.active[e] = cancel
	return nil
}
func (s *Service) unregister(e *execution) { s.mu.Lock(); delete(s.active, e); s.mu.Unlock() }

func (s *Service) configuration(generation int64, actor identity.Credential) Config {
	l := s.Settings.View().Limits
	c := Config{ProtocolVersion: 1, PrivacyGeneration: generation, Models: s.Settings.GatewayModels()}
	for _, scope := range actor.Scopes {
		if scope == "model:invoke" {
			c.InvokeAllowed = true
		}
	}
	c.Available = true
	reason := ""
	if len(c.Models) == 0 {
		reason = "no_selectable_model"
	}
	if l.Concurrency < 2 {
		reason = "gateway_capacity_disabled"
	}
	if reason != "" {
		c.Available = false
		c.UnavailableReason = &reason
	}
	c.Limits.MaxToolSteps = l.MaxToolSteps
	c.Limits.IdleTimeoutSeconds = l.IdleTimeoutSeconds
	c.Limits.RequestTimeoutSeconds = int(s.RequestTimeout / time.Second)
	c.Capabilities.Stream = true
	c.Capabilities.Tools = true
	c.Capabilities.ReasoningDeltas = true
	c.Admission.Concurrency = l.Concurrency
	c.Admission.PerDevice = 1
	c.Admission.CLICapacity = max(0, l.Concurrency-1)
	return c
}
