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
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	platformpostgres "github.com/edu-agent/edu-agent/server/internal/platform/postgres"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	learningtutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func verifyNocturneStartupPreflightForRuntime(ctx context.Context, pool *pgxpool.Pool, gate *nocturnePreflightGate) error {
	err := verifyNocturneStartupPreflight(ctx, gate)
	if err == nil || pool == nil {
		return err
	}
	var activeErasure bool
	if queryErr := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM privacy_erasure_heads WHERE status<>'verified')`).Scan(&activeErasure); queryErr != nil {
		return fmt.Errorf("check privacy erasure before Nocturne startup: %w", queryErr)
	}
	if activeErasure {
		return nil
	}
	return err
}

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

	stores := newApplicationStores(pool)
	identityService, err := identity.NewService(stores.identity, identity.Options{
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
		stores.knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{Selector: selector},
	)
	if err != nil {
		return fmt.Errorf("initialize knowledge service: %w", err)
	}
	learningComposition, err := composeLearningWithStores(stores.learning, stores.tutoring, knowledgeService, modelClient, cfg)
	if err != nil {
		return err
	}
	learningService := learningComposition.service
	bridge, err := composeMemoryBridge(pool, stores, cfg, memoryBridgeDependencies{})
	if err != nil {
		return err
	}
	if err := verifyNocturneStartupPreflightForRuntime(ctx, pool, bridge.preflight); err != nil {
		return err
	}
	var modelProber httpapi.ModelProber
	if modelClient != nil {
		modelProber = modelClient
	}
	readiness := health.New(health.Options{
		Database: pool, ModelEnabled: cfg.Model.Enabled, ModelRequired: cfg.Model.Required,
		ModelProbe: modelHealthProbe(modelClient), NocturneEnabled: cfg.Nocturne.Enabled,
		NocturneProbe: optionalNocturneHealthProbe(bridge.remote), InsecureWarning: cfg.InsecureNonLoopbackWarning,
		Timeout: minDuration(minDuration(cfg.Model.Timeout, cfg.Nocturne.HTTPTimeout), 5*time.Second),
	})
	var migrationLeases httpapi.PrivacyMigrationLeaseService
	maintenanceToken := ""
	if cfg.Nocturne.Enabled {
		migrationLeases = bridge.privacyStore
		maintenanceToken = cfg.Nocturne.MaintenanceToken
	}
	handler, err := httpapi.New(httpapi.Options{
		Identity: identityService, Model: modelProber, Knowledge: knowledgeService, Learning: learningService,
		Memory: bridge.memoryService, MemoryExporter: bridge.memoryExporter,
		Privacy: bridge.privacyService, MigrationLeases: migrationLeases,
		MaintenanceToken: maintenanceToken, ReadPermits: bridge.readPermits,
		Readiness: readiness, Logger: logger,
		PairLimiter:           httpapi.NewFixedWindowLimiter(cfg.PairingRateLimitPerMinute, time.Minute),
		AuthLimiter:           httpapi.NewFixedWindowLimiter(cfg.AuthFailureLimitPerMinute, time.Minute),
		DeviceLimiter:         httpapi.NewFixedWindowLimiter(cfg.DeviceRateLimitPerMinute, time.Minute),
		PrivacyLimiter:        httpapi.NewFixedWindowLimiter(5, time.Minute),
		PrivacyBackupDeadline: cfg.Nocturne.BackupRetention,
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
	if cfg.Nocturne.Enabled {
		logger.Warn("Nocturne workers use fixed claim leases; one remote operation must complete within the lease", "warning", "fixed_worker_lease_limit", "worker_lease", cfg.Nocturne.WorkerLeaseDuration)
	}
	workers := startWorkerGroup(ctx, logger, bridge.workers)
	logger.Info("service listening", "listen_addr", cfg.ListenAddr, "public_base_url", cfg.PublicBaseURL.String())
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	var serveErr error
	serveFinished := false
	select {
	case serveErr = <-serveResult:
		serveFinished = true
		if !expectedServeClose(serveErr) {
			logger.Error("HTTP server stopped unexpectedly", "error_category", "http_serve_failed")
		}
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}
	shutdownErr := shutdownRuntime(listener, server, workers, cfg.ShutdownTimeout, logger)
	if !serveFinished {
		serveErr = <-serveResult
	}
	if !expectedServeClose(serveErr) {
		serveErr = fmt.Errorf("serve HTTP: %w", serveErr)
	} else {
		serveErr = nil
	}
	if err := errors.Join(serveErr, shutdownErr); err != nil {
		return err
	}
	logger.Info("service stopped")
	return nil
}

var (
	errWorkerShutdownTimeout = errors.New("worker shutdown timed out")
	errWorkerShutdownFailed  = errors.New("worker shutdown failed")
	errHTTPShutdownTimeout   = errors.New("HTTP shutdown timed out")
	errHTTPShutdownFailed    = errors.New("HTTP shutdown failed")
)

func shutdownRuntime(listener net.Listener, server *http.Server, workers *workerGroup, timeout time.Duration, logger *slog.Logger) error {
	var listenerErr error
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		listenerErr = fmt.Errorf("stop HTTP listener: %w", err)
	}

	workerCtx, cancelWorkers := context.WithTimeout(context.Background(), timeout)
	defer cancelWorkers()
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), timeout)
	defer cancelHTTP()
	workerResult := make(chan error, 1)
	httpResult := make(chan error, 1)
	go func() { workerResult <- workers.Stop(workerCtx) }()
	go func() {
		err := server.Shutdown(httpCtx)
		if err != nil {
			if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				err = errors.Join(err, closeErr)
			}
		}
		httpResult <- err
	}()

	workerErr := <-workerResult
	httpErr := <-httpResult
	var classifiedWorkerErr, classifiedHTTPErr error
	if workerErr != nil {
		if errors.Is(workerErr, context.DeadlineExceeded) {
			logger.Error("background workers did not stop within their shutdown budget", "error_category", "worker_shutdown_timeout")
			classifiedWorkerErr = fmt.Errorf("%w: %v", errWorkerShutdownTimeout, workerErr)
		} else {
			logger.Error("background worker shutdown failed", "error_category", "worker_shutdown_failed")
			classifiedWorkerErr = fmt.Errorf("%w: %v", errWorkerShutdownFailed, workerErr)
		}
	}
	if httpErr != nil {
		if errors.Is(httpErr, context.DeadlineExceeded) {
			logger.Error("HTTP requests did not drain within their shutdown budget", "error_category", "http_shutdown_timeout")
			classifiedHTTPErr = fmt.Errorf("%w: %v", errHTTPShutdownTimeout, httpErr)
		} else {
			logger.Error("HTTP shutdown failed", "error_category", "http_shutdown_failed")
			classifiedHTTPErr = fmt.Errorf("%w: %v", errHTTPShutdownFailed, httpErr)
		}
	}
	return errors.Join(listenerErr, classifiedWorkerErr, classifiedHTTPErr)
}

func expectedServeClose(err error) bool {
	return err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)
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
	return composeLearningWithStores(learningpostgres.New(pool, tutoringStore), tutoringStore, reader, modelClient, cfg)
}

func composeLearningWithStores(learningStore *learningpostgres.Store, tutoringStore *learningtutoringpostgres.Store, reader learningknowledge.TreeReader, modelClient *llm.Client, cfg config.Config) (learningComposition, error) {
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

type optionalHealthDependency interface {
	Health(context.Context) error
	Capabilities(context.Context) (memory.NocturneCapabilities, error)
}

func optionalNocturneHealthProbe(dependency optionalHealthDependency) health.OptionalProbe {
	if dependency == nil {
		return nil
	}
	return func(ctx context.Context) error {
		if err := dependency.Health(ctx); err != nil {
			return err
		}
		capabilities, err := dependency.Capabilities(ctx)
		if err != nil {
			return err
		}
		if capabilities.UpstreamCommit != memory.NocturneUpstreamCommit || capabilities.CompatRevision != memory.NocturneCompatRevision || capabilities.BootEpoch == "" {
			return errors.New("Nocturne capability lock mismatch")
		}
		return nil
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}
