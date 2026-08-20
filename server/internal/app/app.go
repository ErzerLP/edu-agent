package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitypostgres "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/integrations/tutormodel"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/llmselector"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	platformpostgres "github.com/edu-agent/edu-agent/server/internal/platform/postgres"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	learningtutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	pool, err := platformpostgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if cfg.MigrateOnStart {
		if err := migrations.Run(ctx, pool); err != nil {
			return fmt.Errorf("run database migrations: %w", err)
		}
	} else if err := migrations.Check(ctx, pool); err != nil {
		return fmt.Errorf("check database migrations: %w", err)
	}

	identityService, err := identity.NewService(identitypostgres.New(pool), identity.Options{
		PairingCodeTTL: cfg.PairingCodeTTL, PairingCodeMaxAttempts: cfg.PairingCodeMaxAttempts,
		LastUsedTouchInterval: cfg.TokenLastUsedTouchInterval,
	})
	if err != nil {
		return fmt.Errorf("initialize identity service: %w", err)
	}
	modelClient, err := buildModelClient(cfg)
	if err != nil {
		return err
	}
	var selector knowledge.Selector
	if modelClient != nil {
		selector = llmselector.New(modelClient)
	}
	knowledgeService, err := knowledge.NewService(
		knowledgepostgres.New(pool), knowledge.NewCanonicalizer(), knowledge.ServiceOptions{Selector: selector},
	)
	if err != nil {
		return fmt.Errorf("initialize knowledge service: %w", err)
	}
	learningComposition, err := composeLearning(pool, knowledgeService, modelClient, cfg)
	if err != nil {
		return err
	}
	learningService := learningComposition.service
	var modelProber httpapi.ModelProber
	if modelClient != nil {
		modelProber = modelClient
	}
	readiness := health.New(health.Options{
		Database: pool, ModelEnabled: cfg.Model.Enabled, ModelRequired: cfg.Model.Required,
		ModelProbe: modelHealthProbe(modelClient), InsecureWarning: cfg.InsecureNonLoopbackWarning,
		Timeout: minDuration(cfg.Model.Timeout, 5*time.Second),
	})
	handler, err := httpapi.New(httpapi.Options{
		Identity: identityService, Model: modelProber, Knowledge: knowledgeService, Learning: learningService,
		Readiness: readiness, Logger: logger,
		PairLimiter:   httpapi.NewFixedWindowLimiter(cfg.PairingRateLimitPerMinute, time.Minute),
		AuthLimiter:   httpapi.NewFixedWindowLimiter(cfg.AuthFailureLimitPerMinute, time.Minute),
		DeviceLimiter: httpapi.NewFixedWindowLimiter(cfg.DeviceRateLimitPerMinute, time.Minute),
	})
	if err != nil {
		return fmt.Errorf("initialize HTTP API: %w", err)
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on configured address: %w", err)
	}
	requestContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20,
		BaseContext: func(net.Listener) context.Context { return requestContext },
	}
	if cfg.InsecureNonLoopbackWarning {
		logger.Warn("insecure non-loopback HTTP is enabled", "warning", "insecure_non_loopback_http", "listen_addr", cfg.ListenAddr)
	}
	logger.Info("service listening", "listen_addr", cfg.ListenAddr, "public_base_url", cfg.PublicBaseURL.String())
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("graceful HTTP shutdown: %w", err)
	}
	if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP during shutdown: %w", err)
	}
	logger.Info("service stopped")
	return nil
}

type learningComposition struct {
	learningStore *learningpostgres.Store
	tutoringStore *learningtutoringpostgres.Store
	resolver      *learningknowledge.Adapter
	model         learning.TutorModel
	service       *learning.Service
}

func composeLearning(pool *pgxpool.Pool, reader learningknowledge.TreeReader, modelClient *llm.Client, cfg config.Config) (learningComposition, error) {
	tutoringStore := learningtutoringpostgres.New(pool)
	learningStore := learningpostgres.New(pool, tutoringStore)
	composition := learningComposition{learningStore: learningStore, tutoringStore: tutoringStore, resolver: learningknowledge.New(reader)}
	if modelClient != nil {
		composition.model = tutormodel.New(modelClient)
	}
	service, err := learning.NewService(composition.learningStore, composition.learningStore, composition.resolver, learning.ServiceOptions{
		Model: composition.model, ModelID: cfg.Model.Name,
		ModelParameters: map[string]any{"context_window": cfg.Model.ContextWindow},
	})
	if err != nil {
		return learningComposition{}, fmt.Errorf("initialize learning service: %w", err)
	}
	composition.service = service
	return composition, nil
}

func CreatePairingCode(ctx context.Context, cfg config.Config) (string, time.Time, error) {
	pool, err := platformpostgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return "", time.Time{}, err
	}
	defer pool.Close()
	if cfg.MigrateOnStart {
		if err := migrations.Run(ctx, pool); err != nil {
			return "", time.Time{}, fmt.Errorf("run database migrations: %w", err)
		}
	} else if err := migrations.Check(ctx, pool); err != nil {
		return "", time.Time{}, fmt.Errorf("check database migrations: %w", err)
	}
	service, err := identity.NewService(identitypostgres.New(pool), identity.Options{
		PairingCodeTTL: cfg.PairingCodeTTL, PairingCodeMaxAttempts: cfg.PairingCodeMaxAttempts,
		LastUsedTouchInterval: cfg.TokenLastUsedTouchInterval,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return service.CreatePairingCode(ctx)
}

func buildModelClient(cfg config.Config) (*llm.Client, error) {
	if !cfg.Model.Enabled {
		return nil, nil
	}
	client, err := llm.New(llm.Options{
		BaseURL: cfg.Model.BaseURL, Model: cfg.Model.Name, APIKey: cfg.Model.APIKey,
		ContextWindow: cfg.Model.ContextWindow, MinimumContext: cfg.Model.MinimumContext,
		Timeout: cfg.Model.Timeout, ProbeCacheTTL: cfg.Model.ProbeCacheTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize model client: %w", err)
	}
	return client, nil
}

func modelHealthProbe(client *llm.Client) health.ModelProbe {
	if client == nil {
		return nil
	}
	return func(ctx context.Context) (bool, string) {
		capabilities := client.Probe(ctx)
		if capabilities.Compatible {
			return true, ""
		}
		if len(capabilities.IncompatibilityReasons) == 0 {
			return false, "incompatible"
		}
		return false, capabilities.IncompatibilityReasons[0]
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}
