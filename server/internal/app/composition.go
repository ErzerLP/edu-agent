package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	identitypostgres "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/nocturne"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	memorypostgres "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypostgres "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultNocturneParentPath = "edu-agent"
	nocturneResponseBodyLimit = 16 << 20
)

type applicationStores struct {
	identity  *identitypostgres.Store
	knowledge *knowledgepostgres.Store
	learning  *learningpostgres.Store
	tutoring  *tutoringpostgres.Store
	memory    *memorypostgres.Store
	outbox    *outboxpostgres.Store
}

func newApplicationStores(pool *pgxpool.Pool) applicationStores {
	tutoringStore := tutoringpostgres.New(pool)
	return applicationStores{
		identity: identitypostgres.New(pool), knowledge: knowledgepostgres.New(pool),
		tutoring: tutoringStore, learning: learningpostgres.New(pool, tutoringStore),
		memory: memorypostgres.New(pool), outbox: outboxpostgres.New(pool),
	}
}

func (s applicationStores) localOwnerPorts() []privacy.LocalOwnerPort {
	return []privacy.LocalOwnerPort{s.identity, s.knowledge, s.learning, s.tutoring, s.memory, s.outbox}
}

type memoryBridgeDependencies struct {
	remote           memory.NocturneRemote
	backupRepository privacy.ManagedBackupRepository
	dumpSource       nocturne.DumpSource
}

type managedBackupProducer interface {
	Produce(context.Context, int64) (privacy.ManagedBackupArtifact, error)
}

type managedBackupInventoryReader interface {
	ManagedBackupInventory(context.Context) ([]privacy.ManagedBackupArtifact, error)
}

type nocturnePreflightProbe interface {
	Preflight(context.Context) error
}

type nocturnePreflightGate struct {
	mu            sync.Mutex
	probe         nocturnePreflightProbe
	validFor      time.Duration
	verifiedUntil time.Time
	mismatch      error
}

func newNocturnePreflightGate(probe nocturnePreflightProbe, validFor time.Duration) (*nocturnePreflightGate, error) {
	if probe == nil || validFor <= 0 {
		return nil, fmt.Errorf("Nocturne preflight probe and positive validity interval are required")
	}
	return &nocturnePreflightGate{probe: probe, validFor: validFor}, nil
}

func (g *nocturnePreflightGate) Check(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mismatch != nil {
		return g.mismatch
	}
	if time.Now().Before(g.verifiedUntil) {
		return nil
	}
	err := g.probe.Preflight(ctx)
	if err == nil {
		g.verifiedUntil = time.Now().Add(g.validFor)
		return nil
	}
	if nocturne.IsContractMismatch(err) {
		g.mismatch = err
	}
	return err
}

func (g *nocturnePreflightGate) protect(spec workerSpec) workerSpec {
	runOnce := spec.runOnce
	spec.runOnce = func(ctx context.Context) (int, error) {
		if err := g.Check(ctx); err != nil {
			return 0, err
		}
		return runOnce(ctx)
	}
	return spec
}

func verifyNocturneStartupPreflight(ctx context.Context, gate *nocturnePreflightGate) error {
	if gate == nil {
		return nil
	}
	if err := gate.Check(ctx); err != nil && nocturne.IsContractMismatch(err) {
		return fmt.Errorf("Nocturne preflight contract mismatch: %w", err)
	}
	return nil
}

type backupProductionState struct {
	forcedForUnavailable bool
}

func (s *backupProductionState) run(
	ctx context.Context,
	generation int64,
	interval time.Duration,
	now func() time.Time,
	inventory managedBackupInventoryReader,
	producer managedBackupProducer,
	health interface{ Health(context.Context) error },
) (int, error) {
	healthErr := health.Health(ctx)
	force := healthErr != nil && !s.forcedForUnavailable
	produced, err := produceManagedBackupIfDue(ctx, generation, interval, now, inventory, producer, force)
	if err != nil {
		return 0, err
	}
	if healthErr == nil {
		s.forcedForUnavailable = false
	} else if produced > 0 {
		s.forcedForUnavailable = true
	}
	return produced, nil
}

func produceManagedBackupIfDue(
	ctx context.Context,
	generation int64,
	interval time.Duration,
	now func() time.Time,
	inventory managedBackupInventoryReader,
	producer managedBackupProducer,
	force bool,
) (int, error) {
	if generation < 1 || interval <= 0 || now == nil || inventory == nil || producer == nil {
		return 0, fmt.Errorf("invalid managed backup producer configuration")
	}
	artifacts, err := inventory.ManagedBackupInventory(ctx)
	if err != nil {
		return 0, err
	}
	var latest time.Time
	for _, artifact := range artifacts {
		if artifact.LearnerGeneration == generation && artifact.CreatedAt.After(latest) {
			latest = artifact.CreatedAt
		}
	}
	if current := now().UTC(); !force && !latest.IsZero() && !current.After(latest.Add(interval)) {
		return 0, nil
	}
	if _, err := producer.Produce(ctx, generation); err != nil {
		return 0, err
	}
	return 1, nil
}

// memoryBridgeComposition owns the shared privacy gate and every optional
// Nocturne component that is started by app.Run.
type memoryBridgeComposition struct {
	readPermits      *privacy.ReadPermitManager
	memoryService    *memory.Service
	memoryExporter   *nocturne.MemoryExporter
	privacyStore     *privacypostgres.Store
	privacyService   *privacyHTTPAdapter
	privacyGrant     *privacy.ErasureGrantService
	remote           memory.NocturneRemote
	preflight        *nocturnePreflightGate
	backupController *nocturne.BackupController
	workers          []workerSpec
}

func composeMemoryBridge(pool *pgxpool.Pool, stores applicationStores, cfg config.Config, dependencies memoryBridgeDependencies) (memoryBridgeComposition, error) {
	permits := privacy.NewReadPermitManager()
	memoryService, err := memory.NewService(stores.memory, memory.ServiceOptions{
		ReadPermits: permits, DeliveryTTL: cfg.Nocturne.DeliveryTTL,
	})
	if err != nil {
		return memoryBridgeComposition{}, fmt.Errorf("initialize memory service: %w", err)
	}
	parentPath := cfg.Nocturne.Namespace
	if parentPath == "" {
		parentPath = defaultNocturneParentPath
	}

	remote := dependencies.remote
	if cfg.Nocturne.Enabled && remote == nil {
		remote, err = nocturne.New(nocturne.Options{
			BaseURL: cfg.Nocturne.BaseURL, APIToken: cfg.Nocturne.APIToken,
			MaintenanceToken: cfg.Nocturne.MaintenanceToken, Timeout: cfg.Nocturne.HTTPTimeout,
			BodyLimit: nocturneResponseBodyLimit, Namespace: cfg.Nocturne.Namespace,
			Domain: cfg.Nocturne.Domain, ParentPath: parentPath, Priority: 0,
			Disclosure: "edu-agent managed memory",
		})
		if err != nil {
			return memoryBridgeComposition{}, fmt.Errorf("initialize Nocturne client: %w", err)
		}
	}
	if !cfg.Nocturne.Enabled {
		remote = nil
	}

	exporter, err := nocturne.NewMemoryExporter(nocturne.MemoryExporterOptions{
		Service: memoryService, Remote: remote, ReadPermits: permits, ParentPath: parentPath,
	})
	if err != nil {
		return memoryBridgeComposition{}, fmt.Errorf("initialize memory exporter: %w", err)
	}

	privacyOptions := []privacypostgres.Option{privacypostgres.WithReadPermits(permits)}
	for _, owner := range stores.localOwnerPorts() {
		if err := privacy.ValidateOwnerPort(owner); err != nil {
			return memoryBridgeComposition{}, fmt.Errorf("initialize privacy owner port: %w", err)
		}
		privacyOptions = append(privacyOptions, privacypostgres.WithLocalOwner(owner))
	}
	privacyStore := privacypostgres.New(pool, privacyOptions...)
	grantService, err := privacy.NewErasureGrantService(privacypostgres.NewGrantStore(pool), privacy.ErasureGrantOptions{
		TTL: cfg.Privacy.ErasureGrantTTL, MaxAttempts: cfg.Privacy.ErasureGrantMaxAttempts,
	})
	if err != nil {
		return memoryBridgeComposition{}, fmt.Errorf("initialize privacy erasure grant service: %w", err)
	}

	composition := memoryBridgeComposition{
		readPermits: permits, memoryService: memoryService, memoryExporter: exporter,
		privacyStore: privacyStore, privacyGrant: grantService, remote: remote,
	}
	composition.privacyService = &privacyHTTPAdapter{store: privacyStore}
	if !cfg.Nocturne.Enabled {
		disabledVerifier, verifierErr := privacy.NewDisabledNocturneVerifier(disabledNocturneEvidenceReader{pool: pool}, nil)
		if verifierErr != nil {
			return memoryBridgeComposition{}, fmt.Errorf("initialize disabled Nocturne verifier: %w", verifierErr)
		}
		composition.privacyService.eraser = disabledVerifier
		composition.privacyService.verifier = disabledVerifier
	}
	composition.workers = append(composition.workers,
		periodicWorker("candidate_expiry", cfg.Nocturne.CandidateSweepInterval, cfg.Nocturne.WorkerBatchSize, func(ctx context.Context) (int, error) {
			return stores.memory.ExpireCandidates(ctx, time.Time{}, cfg.Nocturne.WorkerBatchSize)
		}),
		periodicWorker("delivery_expiry", cfg.Nocturne.DeliverySweepInterval, cfg.Nocturne.WorkerBatchSize, func(ctx context.Context) (int, error) {
			return stores.memory.ExpireDeliveries(ctx, time.Time{}, cfg.Nocturne.WorkerBatchSize)
		}),
	)

	if cfg.Nocturne.Enabled {
		probe, ok := remote.(nocturnePreflightProbe)
		if !ok {
			return memoryBridgeComposition{}, fmt.Errorf("initialize Nocturne preflight: remote does not implement the fixed deployment probe")
		}
		composition.preflight, err = newNocturnePreflightGate(probe, cfg.Nocturne.ReconciliationInterval)
		if err != nil {
			return memoryBridgeComposition{}, fmt.Errorf("initialize Nocturne preflight: %w", err)
		}
		if err := composition.composeEnabledNocturne(pool, stores, cfg, dependencies); err != nil {
			return memoryBridgeComposition{}, err
		}
	}
	privacyResume := periodicWorker("privacy_erasure_resume", cfg.Nocturne.ReconciliationInterval, 1, func(ctx context.Context) (int, error) {
		return resumeActivePrivacyErasure(ctx, pool, composition.privacyService)
	})
	if composition.preflight != nil {
		privacyResume = composition.preflight.protect(privacyResume)
	}
	composition.workers = append(composition.workers, privacyResume)
	return composition, nil
}

func pgDumpEnvironment(raw string) ([]string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" || parsed.User == nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("initialize managed backup dump environment: invalid PostgreSQL DSN")
	}
	password, ok := parsed.User.Password()
	if !ok || password == "" {
		return nil, fmt.Errorf("initialize managed backup dump environment: PostgreSQL password is required")
	}
	database, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil || database == "" || strings.Contains(database, "/") {
		return nil, fmt.Errorf("initialize managed backup dump environment: PostgreSQL database is invalid")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	sslMode := parsed.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "require"
	}
	blocked := []string{"PGHOST=", "PGPORT=", "PGUSER=", "PGPASSWORD=", "PGDATABASE=", "PGSSLMODE="}
	environment := make([]string, 0, len(os.Environ())+6)
	for _, item := range os.Environ() {
		skip := false
		for _, prefix := range blocked {
			if strings.HasPrefix(item, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			environment = append(environment, item)
		}
	}
	return append(environment,
		"PGHOST="+parsed.Hostname(), "PGPORT="+port, "PGUSER="+parsed.User.Username(),
		"PGPASSWORD="+password, "PGDATABASE="+database, "PGSSLMODE="+sslMode,
	), nil
}

func (c *memoryBridgeComposition) composeEnabledNocturne(pool *pgxpool.Pool, stores applicationStores, cfg config.Config, dependencies memoryBridgeDependencies) error {
	consumer, err := nocturne.NewConsumer(stores.memory, c.remote, nocturne.ConsumerOptions{
		Lease: cfg.Nocturne.WorkerLeaseDuration, Namespace: cfg.Nocturne.Namespace,
		Domain: cfg.Nocturne.Domain, ParentPath: cfg.Nocturne.Namespace,
	})
	if err != nil {
		return fmt.Errorf("initialize Nocturne memory consumer: %w", err)
	}
	purger, err := nocturne.NewPurger(stores.memory, c.remote, cfg.Nocturne.Namespace, cfg.Nocturne.Domain, cfg.Nocturne.Namespace, nil)
	if err != nil {
		return fmt.Errorf("initialize Nocturne purger: %w", err)
	}
	remoteEraser, err := nocturne.NewRemoteEraser(stores.memory, c.remote, purger, nocturne.RemoteEraserOptions{
		Lease: cfg.Nocturne.WorkerLeaseDuration, MaxReconciliations: cfg.Nocturne.WorkerBatchSize,
	})
	if err != nil {
		return fmt.Errorf("initialize Nocturne remote eraser: %w", err)
	}

	repository := dependencies.backupRepository
	if repository == nil {
		repository, err = privacypostgres.NewManagedBackupRepository(pool, cfg.Nocturne.MasterWrappingKey)
		if err != nil {
			return fmt.Errorf("initialize managed backup repository: %w", err)
		}
	}
	dumpSource := dependencies.dumpSource
	if dumpSource == nil {
		dumpEnvironment, environmentErr := pgDumpEnvironment(cfg.Nocturne.PGDumpDSN)
		if environmentErr != nil {
			return environmentErr
		}
		dumpSource = nocturne.CommandDumpSource{
			Args: []string{"--format=custom", "--no-owner", "--no-privileges"},
			Env:  dumpEnvironment,
		}
	}
	controller, err := nocturne.NewBackupController(nocturne.BackupControllerOptions{
		Root: cfg.Nocturne.BackupRoot, DumpSource: dumpSource, Keys: repository,
		Inventory: repository, Maintenance: c.remote,
	})
	if err != nil {
		return fmt.Errorf("initialize managed backup controller: %w", err)
	}
	c.backupController = controller
	c.privacyService.eraser = remoteEraser
	c.privacyService.verifier = controller

	outboxWorker, err := outbox.NewWorker(stores.outbox, map[string]outbox.Consumer{"memory.delivery": consumer}, outbox.WorkerOptions{
		BatchSize: cfg.Nocturne.WorkerBatchSize, Lease: cfg.Nocturne.WorkerLeaseDuration,
		BaseBackoff: cfg.Nocturne.WorkerPollInterval, MaxBackoff: cfg.Nocturne.WorkerPollInterval * 8,
	})
	if err != nil {
		return fmt.Errorf("initialize memory outbox worker: %w", err)
	}
	attemptReconciler, err := nocturne.NewAttemptReconciler(consumer, cfg.Nocturne.ReconciliationInterval)
	if err != nil {
		return fmt.Errorf("initialize Nocturne attempt reconciler: %w", err)
	}
	expiryReconciler, err := nocturne.NewExpiryReconciler(stores.memory, c.remote, purger,
		cfg.Nocturne.WorkerLeaseDuration, cfg.Nocturne.ReconciliationInterval, nil)
	if err != nil {
		return fmt.Errorf("initialize Nocturne expiry reconciler: %w", err)
	}
	remoteWorkers := []workerSpec{
		periodicWorker("memory_outbox", cfg.Nocturne.WorkerPollInterval, cfg.Nocturne.WorkerBatchSize, outboxWorker.RunOnce),
		periodicWorker("attempt_reconciler", cfg.Nocturne.ReconciliationInterval, 1, attemptReconciler.RunOnce),
		periodicWorker("expiry_remote_reconciler", cfg.Nocturne.ReconciliationInterval, 1, expiryReconciler.RunOnce),
	}
	for _, spec := range remoteWorkers {
		c.workers = append(c.workers, c.preflight.protect(spec))
	}
	backupState := &backupProductionState{}
	c.workers = append(c.workers, workerSpec{name: "backup_producer", interval: cfg.Nocturne.BackupControllerInterval, retryInterval: cfg.Nocturne.WorkerPollInterval, batch: 1, runOnce: func(ctx context.Context) (int, error) {
		generation, generationErr := currentMemoryGeneration(ctx, pool)
		if generationErr != nil {
			return 0, generationErr
		}
		return backupState.run(ctx, generation, cfg.Nocturne.BackupControllerInterval, time.Now, repository, controller, c.remote)
	}})
	c.workers = append(c.workers, c.preflight.protect(workerSpec{
		name: "backup_prune", interval: cfg.Nocturne.BackupControllerInterval,
		retryInterval: cfg.Nocturne.WorkerPollInterval, batch: 1,
		runOnce: func(ctx context.Context) (int, error) {
			return 1, controller.Prune(ctx, time.Now().UTC().Add(-cfg.Nocturne.BackupRetention), c.remote)
		},
	}))
	return nil
}
