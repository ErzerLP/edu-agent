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
	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/integrations/tutormodel"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/llmselector"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
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

	knowledgeOptions := []knowledgepostgres.Option(nil)
	if cfg.Notesync.Enabled {
		knowledgeOptions = append(knowledgeOptions, knowledgepostgres.WithNotesyncPublication(knowledgepostgres.NotesyncPublicationConfig{
			Vault: cfg.Notesync.Vault, PathPrefix: cfg.Notesync.PathPrefix,
		}))
	}
	stores := newApplicationStores(pool, knowledgeOptions...)
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
	canonicalizer := knowledge.NewCanonicalizer()
	knowledgeService, err := knowledge.NewService(
		stores.knowledge, canonicalizer, knowledge.ServiceOptions{
			Selector: selector, MaintenanceStore: stores.knowledge, EvidenceImpactReader: stores.learning,
		},
	)
	if err != nil {
		return fmt.Errorf("initialize knowledge service: %w", err)
	}
	learningComposition, err := composeLearningWithStores(stores.learning, stores.tutoring, knowledgeService, modelClient, cfg)
	if err != nil {
		return err
	}
	learningService := learningComposition.service
	offlineService := learningComposition.offline
	bridge, err := composeMemoryBridge(pool, stores, cfg, memoryBridgeDependencies{})
	if err != nil {
		return err
	}
	if err := verifyNocturneStartupPreflightForRuntime(ctx, pool, bridge.preflight); err != nil {
		return err
	}
	notesyncBridge, err := composeNotesync(cfg, notesyncDependencies{
		publicationStore: stores.knowledge, reviewStore: stores.knowledge, outboxStore: stores.outbox,
		importer: knowledgeService, canonicalizer: canonicalizer,
	})
	if err != nil {
		return err
	}
	runtimeWorkers := append([]workerSpec(nil), bridge.workers...)
	runtimeWorkers = append(runtimeWorkers, notesyncBridge.workers...)
	evaluationWorkerSpec, evaluationWorkerHealth, err := newOfflineEvaluationWorkerSpec(learningService, stores.learning, stores.outbox, cfg.Model.Timeout)
	if err != nil {
		return err
	}
	runtimeWorkers = append(runtimeWorkers, evaluationWorkerSpec)
	var modelProber httpapi.ModelProber
	if modelClient != nil {
		modelProber = modelClient
	}
	readiness := health.New(health.Options{
		Database: pool, ModelEnabled: cfg.Model.Enabled, ModelRequired: cfg.Model.Required,
		ModelProbe: modelHealthProbe(modelClient), OpenEvaluationWorkerProbe: evaluationWorkerHealth.Probe,
		OfflineSignerAvailable: offlineService.SignerAvailable(), OfflineProtocolAvailable: offlineService.Available(),
		NocturneEnabled: cfg.Nocturne.Enabled,
		NocturneProbe:   optionalNocturneHealthProbe(bridge.remote),
		NotesyncEnabled: cfg.Notesync.Enabled,
		NotesyncProbe:   notesyncBridge.probe,
		InsecureWarning: cfg.InsecureNonLoopbackWarning,
		Timeout:         minDuration(minDuration(minDuration(cfg.Model.Timeout, cfg.Nocturne.HTTPTimeout), cfg.Notesync.HTTPTimeout), 5*time.Second),
	})
	var migrationLeases httpapi.PrivacyMigrationLeaseService
	maintenanceToken := ""
	if cfg.Nocturne.Enabled {
		migrationLeases = bridge.privacyStore
		maintenanceToken = cfg.Nocturne.MaintenanceToken
	}
	authLimiter := httpapi.NewFixedWindowLimiter(cfg.AuthFailureLimitPerMinute, time.Minute)
	deviceLimiter := httpapi.NewFixedWindowLimiter(cfg.DeviceRateLimitPerMinute, time.Minute)
	handler, err := composeTransportHandler(httpapi.Options{
		Identity: identityService, Model: modelProber, Knowledge: knowledgeService, Notesync: notesyncBridge.review,
		Learning: learningService, Offline: offlineService,
		Memory: bridge.memoryService, MemoryExporter: bridge.memoryExporter,
		Privacy: bridge.privacyService, MigrationLeases: migrationLeases,
		MaintenanceToken: maintenanceToken, ReadPermits: bridge.readPermits,
		Readiness: readiness, Logger: logger,
		PairLimiter:           httpapi.NewFixedWindowLimiter(cfg.PairingRateLimitPerMinute, time.Minute),
		AuthLimiter:           authLimiter,
		DeviceLimiter:         deviceLimiter,
		PrivacyLimiter:        httpapi.NewFixedWindowLimiter(5, time.Minute),
		PrivacyBackupDeadline: cfg.Nocturne.BackupRetention,
		AdminUI: httpapi.AdminUIOptions{
			Enabled: cfg.AdminUI.Enabled, Identity: identityService, PublicBaseURL: cfg.PublicBaseURL, Token: cfg.AdminUI.Token,
			TrustedLoopbackProxy: cfg.AdminUI.TrustedLoopbackProxy, SettingsFile: cfg.AdminUI.SettingsFile,
			Notesync: cfg.Notesync, NotesyncSource: cfg.AdminUI.NotesyncSource, NotesyncSettingsSavedAt: cfg.AdminUI.NotesyncSettingsSavedAt,
			AuthLimiter: httpapi.NewFixedWindowLimiter(10, time.Minute), WriteLimiter: httpapi.NewFixedWindowLimiter(10, time.Minute),
		},
	})
	if err != nil {
		return err
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
	workers := startWorkerGroup(ctx, logger, runtimeWorkers)
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
	offline       *learning.OfflineService
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
	var signer learning.OfflineSigner
	origin := "http://127.0.0.1:8080/"
	if cfg.PublicBaseURL != nil {
		origin = cfg.PublicBaseURL.String()
	}
	if cfg.Offline.SignerEnabled() {
		signer, err = learning.NewEd25519OfflineSignerWithManifestChain(
			cfg.Offline.SignerKeyID, cfg.Offline.SignerPrivateKey, origin,
			cfg.Offline.SignerIssuedAt, cfg.Offline.SignerNotAfter,
			cfg.Offline.SignerManifestChain,
		)
		if err != nil {
			return learningComposition{}, fmt.Errorf("initialize offline signer: %w", err)
		}
	}
	composition.offline, err = learning.NewOfflineServiceWithGenerator(composition.learningStore, composition.service, signer, origin, time.Now)
	if err != nil {
		return learningComposition{}, fmt.Errorf("initialize offline service: %w", err)
	}
	return composition, nil
}

func newOfflineEvaluationWorkerSpec(service *learning.Service, evaluationStore learning.OfflineEvaluationStore, messageStore outbox.Store, modelTimeout time.Duration) (workerSpec, *workerHealth, error) {
	consumer, err := learning.NewOfflineEvaluationConsumer(service, evaluationStore)
	if err != nil {
		return workerSpec{}, nil, fmt.Errorf("initialize offline evaluation consumer: %w", err)
	}
	lease := 3 * modelTimeout
	if lease < time.Minute {
		lease = time.Minute
	}
	worker, err := outbox.NewWorker(messageStore, map[string]outbox.Consumer{
		"learning.offline-evaluation": consumer,
	}, outbox.WorkerOptions{BatchSize: 10, Lease: lease, BaseBackoff: time.Second, MaxBackoff: time.Minute})
	if err != nil {
		return workerSpec{}, nil, fmt.Errorf("initialize offline evaluation worker: %w", err)
	}
	health := &workerHealth{}
	return periodicWorker("offline_evaluation", time.Second, 10, health.track(worker.RunOnce)), health, nil
}

func CreatePairingCode(ctx context.Context, cfg config.Config, profile identity.PairingProfile) (string, time.Time, error) {
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
	return service.CreatePairingCodeForProfile(ctx, profile)
}

type notesyncBridgeRemote interface {
	notesync.Remote
	notesync.ReviewRemote
}

type notesyncComposition struct {
	client  *notesync.Client
	remote  notesyncBridgeRemote
	review  httpapi.NotesyncReviewService
	probe   health.NotesyncProbe
	workers []workerSpec
	lease   time.Duration
}

type notesyncDependencies struct {
	publicationStore notesync.PublicationStore
	reviewStore      notesync.ReviewStore
	outboxStore      outbox.Store
	importer         notesync.KnowledgeImporter
	canonicalizer    *knowledge.Canonicalizer
	remote           notesyncBridgeRemote
}

func composeNotesync(cfg config.Config, supplied ...notesyncDependencies) (notesyncComposition, error) {
	if !cfg.Notesync.Enabled {
		return notesyncComposition{}, nil
	}
	var dependencies notesyncDependencies
	if len(supplied) == 1 {
		dependencies = supplied[0]
	} else if len(supplied) > 1 {
		return notesyncComposition{}, fmt.Errorf("initialize NoteSync: duplicate dependency sets")
	}
	remote := dependencies.remote
	var client *notesync.Client
	if remote == nil {
		var err error
		client, err = notesync.New(notesync.Options{
			BaseURL: cfg.Notesync.BaseURL, APIToken: cfg.Notesync.APIToken,
			Timeout: cfg.Notesync.HTTPTimeout, BodyLimit: notesync.DefaultBodyLimit,
		})
		if err != nil {
			return notesyncComposition{}, fmt.Errorf("initialize NoteSync client: %w", err)
		}
		remote = client
	}
	if dependencies.publicationStore == nil || dependencies.reviewStore == nil || dependencies.outboxStore == nil ||
		dependencies.importer == nil || dependencies.canonicalizer == nil {
		return notesyncComposition{}, fmt.Errorf("initialize NoteSync publication and review services: dependencies are required")
	}
	review, err := notesync.NewReviewService(notesync.ReviewServiceOptions{
		Store: dependencies.reviewStore, Remote: remote, Importer: dependencies.importer,
		Canonicalizer: dependencies.canonicalizer, Vault: cfg.Notesync.Vault, PathPrefix: cfg.Notesync.PathPrefix,
		ScanPageSize: cfg.Notesync.ScanPageSize, ScanMaxPages: cfg.Notesync.ScanMaxPages,
	})
	if err != nil {
		return notesyncComposition{}, fmt.Errorf("initialize NoteSync review service: %w", err)
	}
	consumer, err := notesync.NewConsumer(notesync.ConsumerOptions{
		Store: dependencies.publicationStore, Remote: remote, Vault: cfg.Notesync.Vault,
		PathPrefix: cfg.Notesync.PathPrefix, RetryBackoff: cfg.Notesync.WorkerInterval,
	})
	if err != nil {
		return notesyncComposition{}, fmt.Errorf("initialize NoteSync publication consumer: %w", err)
	}
	lease := notesyncWorkerLease(cfg.Notesync.HTTPTimeout)
	worker, err := outbox.NewWorker(dependencies.outboxStore, map[string]outbox.Consumer{
		notesync.PublicationBusinessType: consumer,
	}, outbox.WorkerOptions{
		BatchSize: cfg.Notesync.WorkerBatch, Lease: lease,
		BaseBackoff: cfg.Notesync.WorkerInterval, MaxBackoff: cfg.Notesync.WorkerInterval * 8,
	})
	if err != nil {
		return notesyncComposition{}, fmt.Errorf("initialize NoteSync publication worker: %w", err)
	}
	bootstrapper, ok := dependencies.publicationStore.(interface {
		BootstrapNotesyncPublications(context.Context) (int, error)
	})
	if !ok {
		return notesyncComposition{}, fmt.Errorf("initialize NoteSync publication worker: bootstrap store is required")
	}
	probe := func(ctx context.Context) (bool, string) {
		capability := remote.Probe(ctx, cfg.Notesync.Vault)
		return capability.Compatible, capability.Reason
	}
	return notesyncComposition{
		client: client, remote: remote, review: review, probe: probe, lease: lease,
		workers: []workerSpec{periodicWorker(
			"notesync_outbox", cfg.Notesync.WorkerInterval, cfg.Notesync.WorkerBatch,
			runAfterNotesyncBootstrap(bootstrapper.BootstrapNotesyncPublications, worker.RunOnce),
		)},
	}, nil
}

func runAfterNotesyncBootstrap(
	bootstrap func(context.Context) (int, error),
	runOnce func(context.Context) (int, error),
) func(context.Context) (int, error) {
	bootstrapped := false
	return func(ctx context.Context) (int, error) {
		count := 0
		if !bootstrapped {
			inserted, err := bootstrap(ctx)
			if err != nil {
				return 0, fmt.Errorf("bootstrap NoteSync publications: %w", err)
			}
			count += inserted
			bootstrapped = true
		}
		processed, err := runOnce(ctx)
		return count + processed, err
	}
}

func notesyncWorkerLease(httpTimeout time.Duration) time.Duration {
	lease := 12 * httpTimeout
	if lease < time.Minute {
		return time.Minute
	}
	return lease
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
