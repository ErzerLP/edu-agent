package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"go.yaml.in/yaml/v3"
)

type appDumpSource struct{}

func (appDumpSource) Dump(context.Context, io.Writer) error { return nil }

type appBackupRepository struct{}

func (appBackupRepository) WithGenerationKey(context.Context, int64, func(privacy.GenerationKeyLease) error) error {
	return errors.New("not used")
}
func (appBackupRepository) WithExistingGenerationKey(context.Context, int64, string, func(privacy.GenerationKeyLease) error) error {
	return errors.New("not used")
}
func (appBackupRepository) VerifyGenerationKeyDestroyed(context.Context, int64, string) error {
	return errors.New("not used")
}
func (appBackupRepository) RecordManagedBackup(context.Context, privacy.ManagedBackupArtifact) error {
	return nil
}
func (appBackupRepository) DiscardManagedBackupPublication(context.Context, privacy.ManagedBackupArtifact, time.Time) error {
	return nil
}
func (appBackupRepository) ManagedBackupInventory(context.Context) ([]privacy.ManagedBackupArtifact, error) {
	return nil, nil
}
func (appBackupRepository) MarkManagedBackupsPruned(context.Context, []string, time.Time) error {
	return nil
}

type appRemote struct{}

func (appRemote) EnsureParent(context.Context) error { return nil }

func (appRemote) Health(context.Context) error    { return nil }
func (appRemote) Preflight(context.Context) error { return nil }
func (appRemote) Capabilities(context.Context) (memory.NocturneCapabilities, error) {
	return memory.NocturneCapabilities{
		UpstreamCommit: memory.NocturneUpstreamCommit,
		CompatRevision: memory.NocturneCompatRevision,
		BootEpoch:      "test-boot",
	}, nil
}
func (appRemote) GetNode(context.Context, string) (memory.RemoteNode, error) {
	return memory.RemoteNode{}, nil
}
func (appRemote) CreateNode(context.Context, string, string) (memory.RemoteMutation, error) {
	return memory.RemoteMutation{}, nil
}
func (appRemote) UpdateNode(context.Context, string, string) (memory.RemoteMutation, error) {
	return memory.RemoteMutation{}, nil
}
func (appRemote) DeletePath(context.Context, string) error { return nil }
func (appRemote) Search(context.Context, string) ([]memory.RemoteSearchResult, error) {
	return nil, nil
}
func (appRemote) ListOrphans(context.Context) ([]memory.RemoteOrphan, error) { return nil, nil }
func (appRemote) OrphanDetail(context.Context, int64) (memory.RemoteOrphan, error) {
	return memory.RemoteOrphan{}, nil
}
func (appRemote) PermanentDelete(context.Context, int64) (memory.RemoteDeleteResult, error) {
	return memory.RemoteDeleteResult{}, nil
}
func (appRemote) References(context.Context, string) (memory.RemoteReferences, error) {
	return memory.RemoteReferences{}, nil
}
func (appRemote) ClearReviewReferences(context.Context, string) error { return nil }
func (appRemote) Backups(context.Context) (memory.BackupInventory, error) {
	return memory.BackupInventory{}, nil
}
func (appRemote) PruneBackups(context.Context, memory.BackupPruneRequest) (memory.BackupPruneResult, error) {
	return memory.BackupPruneResult{}, nil
}

func TestComposeMemoryBridgeDisabledAndEnabled(t *testing.T) {
	stores := newApplicationStores(nil)
	disabledConfig := bridgeTestConfig(t, false)
	disabled, err := composeMemoryBridge(nil, stores, disabledConfig, memoryBridgeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.readPermits == nil || disabled.memoryService == nil || disabled.memoryExporter == nil || disabled.privacyStore == nil || disabled.privacyService == nil || disabled.privacyGrant == nil {
		t.Fatalf("incomplete disabled composition: %+v", disabled)
	}
	if disabled.remote != nil || disabled.backupController != nil || disabled.privacyService.eraser == nil || disabled.privacyService.verifier == nil || workerNames(disabled.workers) != "candidate_expiry,delivery_expiry,privacy_erasure_resume" {
		t.Fatalf("unexpected disabled remote composition: remote=%T backup=%v workers=%s", disabled.remote, disabled.backupController, workerNames(disabled.workers))
	}
	owners := stores.localOwnerPorts()
	seenOwners := map[privacy.OwnerKind]bool{}
	for _, owner := range owners {
		seenOwners[owner.Owner()] = true
	}
	if len(owners) != 6 || len(seenOwners) != 6 {
		t.Fatalf("owner ports=%d unique=%d", len(owners), len(seenOwners))
	}

	enabledConfig := bridgeTestConfig(t, true)
	enabled, err := composeMemoryBridge(nil, stores, enabledConfig, memoryBridgeDependencies{
		remote: appRemote{}, backupRepository: appBackupRepository{}, dumpSource: appDumpSource{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if enabled.remote == nil || enabled.backupController == nil || enabled.privacyService.eraser == nil || enabled.privacyService.verifier == nil {
		t.Fatalf("incomplete enabled composition: %+v", enabled)
	}
	wantWorkers := "candidate_expiry,delivery_expiry,memory_outbox,attempt_reconciler,expiry_remote_reconciler,backup_producer,backup_prune,privacy_erasure_resume"
	if workerNames(enabled.workers) != wantWorkers {
		t.Fatalf("enabled workers=%s want=%s", workerNames(enabled.workers), wantWorkers)
	}
}

type incompatibleAppRemote struct{ appRemote }

func (incompatibleAppRemote) Capabilities(context.Context) (memory.NocturneCapabilities, error) {
	return memory.NocturneCapabilities{UpstreamCommit: "wrong", CompatRevision: memory.NocturneCompatRevision, BootEpoch: "test-boot"}, nil
}

type appPreflightError struct{ category string }

func (e appPreflightError) Error() string    { return "Nocturne preflight failed" }
func (e appPreflightError) Category() string { return e.category }

type appPreflightProbe struct {
	err   error
	calls int
}

func (p *appPreflightProbe) Preflight(context.Context) error {
	p.calls++
	return p.err
}

func TestNocturneStartupPreflightFailsClosedAndRetriesUnavailable(t *testing.T) {
	mismatchProbe := &appPreflightProbe{err: appPreflightError{category: "contract_mismatch"}}
	mismatchGate, err := newNocturnePreflightGate(mismatchProbe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyNocturneStartupPreflight(context.Background(), mismatchGate); err == nil {
		t.Fatal("contract mismatch did not block startup")
	}

	unavailableProbe := &appPreflightProbe{err: errors.New("network unavailable")}
	unavailableGate, err := newNocturnePreflightGate(unavailableProbe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyNocturneStartupPreflight(context.Background(), unavailableGate); err != nil {
		t.Fatalf("optional network outage blocked startup: %v", err)
	}
	mutationCalls := 0
	protected := unavailableGate.protect(periodicWorker("remote_mutation", time.Second, 1, func(context.Context) (int, error) {
		mutationCalls++
		return 1, nil
	}))
	if _, err := protected.runOnce(context.Background()); err == nil || mutationCalls != 0 {
		t.Fatalf("unavailable preflight reached mutation: calls=%d err=%v", mutationCalls, err)
	}
	unavailableProbe.err = nil
	if _, err := protected.runOnce(context.Background()); err != nil || mutationCalls != 1 {
		t.Fatalf("recovered preflight did not release mutation: calls=%d err=%v", mutationCalls, err)
	}
	unavailableProbe.err = appPreflightError{category: "contract_mismatch"}
	unavailableGate.mu.Lock()
	unavailableGate.verifiedUntil = time.Time{}
	unavailableGate.mu.Unlock()
	if _, err := protected.runOnce(context.Background()); err == nil || mutationCalls != 1 {
		t.Fatalf("post-start contract drift reached mutation: calls=%d err=%v", mutationCalls, err)
	}
}

func TestOptionalNocturneHealthProbeIncludesCapabilityLock(t *testing.T) {
	if err := optionalNocturneHealthProbe(appRemote{})(context.Background()); err != nil {
		t.Fatalf("valid capability probe failed: %v", err)
	}
	if err := optionalNocturneHealthProbe(incompatibleAppRemote{})(context.Background()); err == nil {
		t.Fatal("capability lock mismatch was accepted")
	}
}

func TestWorkerGroupStopsClaimingAndWaitsForGoroutines(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	group := startWorkerGroup(context.Background(), logger, []workerSpec{periodicWorker("blocking", time.Hour, 1, func(ctx context.Context) (int, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return 0, ctx.Err()
	})})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := group.Stop(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("worker group returned before the worker exited")
	}
}

func TestComposeStaticNocturneIsolationContract(t *testing.T) {
	_, testFile, _, _ := runtime.Caller(0)
	composePath := filepath.Join(filepath.Dir(testFile), "..", "..", "..", "deploy", "compose.yaml")
	payload, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Build       any               `yaml:"build"`
			Ports       []string          `yaml:"ports"`
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
			Networks    []string          `yaml:"networks"`
		} `yaml:"services"`
		Volumes  map[string]any `yaml:"volumes"`
		Networks map[string]struct {
			Internal bool `yaml:"internal"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	nocturne := document.Services["nocturne"]
	server := document.Services["server"]
	nocturneDB := document.Services["nocturne-postgres"]
	mainDB := document.Services["postgres"]
	if !strings.HasPrefix(nocturne.Image, "${NOCTURNE_IMAGE:?") || nocturne.Build != nil || len(nocturne.Ports) != 0 {
		t.Fatalf("Nocturne must use only the supplied image without build/host ports: %+v", nocturne)
	}
	if len(nocturne.Networks) != 1 || nocturne.Networks[0] != "nocturne-internal" || !document.Networks["nocturne-internal"].Internal {
		t.Fatalf("Nocturne network is not isolated: %+v", nocturne.Networks)
	}
	if !containsMount(nocturne.Volumes, "nocturne-backups:") || !containsMount(server.Volumes, "nocturne-backups:") {
		t.Fatalf("server and Nocturne do not share the backup volume")
	}
	if _, ok := document.Volumes["nocturne-postgres-data"]; !ok || !containsMount(nocturneDB.Volumes, "nocturne-postgres-data:") {
		t.Fatalf("Nocturne PostgreSQL volume is missing")
	}
	if nocturneDB.Environment["POSTGRES_PASSWORD"] == mainDB.Environment["POSTGRES_PASSWORD"] || !strings.Contains(nocturneDB.Environment["POSTGRES_PASSWORD"], "NOCTURNE_POSTGRES_PASSWORD") {
		t.Fatalf("Nocturne PostgreSQL credentials are not independent")
	}
	if nocturne.Environment["API_TOKEN"] == nocturne.Environment["EDU_AGENT_MAINTENANCE_TOKEN"] || nocturne.Environment["API_TOKEN"] != server.Environment["NOCTURNE_API_TOKEN"] {
		t.Fatalf("bridge and maintenance credentials are not separated")
	}
	if nocturne.Environment["EDU_AGENT_SERVER_INTERNAL_URL"] != "http://server:8080" ||
		nocturne.Environment["EDU_AGENT_SERVER_MAINTENANCE_TOKEN"] != server.Environment["NOCTURNE_MAINTENANCE_TOKEN"] {
		t.Fatalf("Nocturne migration guard is not bound to the internal server maintenance endpoint")
	}
	if server.Environment["NOCTURNE_IMAGE_LOCK_REFERENCE"] != nocturne.Image {
		t.Fatalf("server image lock %q does not use the same required value as Nocturne image %q", server.Environment["NOCTURNE_IMAGE_LOCK_REFERENCE"], nocturne.Image)
	}
}

func TestManagedBackupProducerSkipsRecentPersistentGenerationOnRestart(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	inventory := &appBackupInventory{artifacts: []privacy.ManagedBackupArtifact{
		{LearnerGeneration: 4, CreatedAt: now.Add(-30 * time.Minute)},
		{LearnerGeneration: 3, CreatedAt: now.Add(-2 * time.Hour)},
	}}
	producer := &appBackupProducer{}
	for restart := 0; restart < 2; restart++ {
		produced, err := produceManagedBackupIfDue(context.Background(), 4, time.Hour, func() time.Time { return now }, inventory, producer, false)
		if err != nil || produced != 0 {
			t.Fatalf("restart %d produced=%d err=%v", restart, produced, err)
		}
	}
	if producer.calls != 0 || inventory.calls != 2 {
		t.Fatalf("producer calls=%d inventory calls=%d", producer.calls, inventory.calls)
	}
	produced, err := produceManagedBackupIfDue(context.Background(), 4, time.Hour, func() time.Time { return now.Add(31 * time.Minute) }, inventory, producer, false)
	if err != nil || produced != 1 || producer.calls != 1 {
		t.Fatalf("due backup produced=%d calls=%d err=%v", produced, producer.calls, err)
	}
}

type appHealthProbe struct{ err error }

func (p *appHealthProbe) Health(context.Context) error { return p.err }

func TestManagedBackupProducerForcesOncePerNocturneOutage(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	inventory := &appBackupInventory{artifacts: []privacy.ManagedBackupArtifact{{LearnerGeneration: 4, CreatedAt: now.Add(-time.Minute)}}}
	producer := &appBackupProducer{}
	health := &appHealthProbe{err: errors.New("Nocturne unavailable")}
	state := &backupProductionState{}
	produced, err := state.run(context.Background(), 4, time.Hour, func() time.Time { return now }, inventory, producer, health)
	if err != nil || produced != 1 || producer.calls != 1 {
		t.Fatalf("first outage produced=%d calls=%d err=%v", produced, producer.calls, err)
	}
	produced, err = state.run(context.Background(), 4, time.Hour, func() time.Time { return now }, inventory, producer, health)
	if err != nil || produced != 0 || producer.calls != 1 {
		t.Fatalf("same outage produced=%d calls=%d err=%v", produced, producer.calls, err)
	}
	health.err = nil
	if _, err := state.run(context.Background(), 4, time.Hour, func() time.Time { return now }, inventory, producer, health); err != nil {
		t.Fatal(err)
	}
	health.err = errors.New("Nocturne unavailable again")
	produced, err = state.run(context.Background(), 4, time.Hour, func() time.Time { return now }, inventory, producer, health)
	if err != nil || produced != 1 || producer.calls != 2 {
		t.Fatalf("second outage produced=%d calls=%d err=%v", produced, producer.calls, err)
	}
}

func TestPrivacyResumeRoutesFromCurrentReceiptStage(t *testing.T) {
	ctx := context.Background()
	initial := appErasureReceipt(privacy.StatusBarrierCommitted, privacy.StepPending, privacy.StepPending)
	store := &appPrivacyRouteStore{
		localResult:  appErasureReceipt(privacy.StatusLocalScrubbed, privacy.StepPending, privacy.StepPending),
		remoteResult: appErasureReceipt(privacy.StatusRemotePurged, privacy.StepNotApplicable, privacy.StepPending),
		backupResult: appErasureReceipt(privacy.StatusVerified, privacy.StepNotApplicable, privacy.StepNotApplicable),
	}
	adapter := &privacyHTTPAdapter{store: store, eraser: appRouteEraser{}, verifier: appRouteVerifier{}}
	result, err := resumePrivacyErasure(ctx, adapter, initial)
	if err != nil || result.Status != privacy.StatusVerified || store.localCalls != 1 || store.remoteCalls != 1 || store.backupCalls != 1 {
		t.Fatalf("full resume result=%+v calls=%d/%d/%d err=%v", result, store.localCalls, store.remoteCalls, store.backupCalls, err)
	}

	store = &appPrivacyRouteStore{backupResult: appErasureReceipt(privacy.StatusVerified, privacy.StepSucceeded, privacy.StepSucceeded)}
	adapter = &privacyHTTPAdapter{store: store, eraser: appRouteEraser{}, verifier: appRouteVerifier{}}
	partialBackup := appErasureReceipt(privacy.StatusPartial, privacy.StepSucceeded, privacy.StepUnknown)
	result, err = resumePrivacyErasure(ctx, adapter, partialBackup)
	if err != nil || result.Status != privacy.StatusVerified || store.remoteCalls != 0 || store.backupCalls != 1 {
		t.Fatalf("backup resume result=%+v remote=%d backup=%d err=%v", result, store.remoteCalls, store.backupCalls, err)
	}

	verified := appErasureReceipt(privacy.StatusVerified, privacy.StepSucceeded, privacy.StepSucceeded)
	result, err = resumePrivacyErasure(ctx, adapter, verified)
	if err != nil || result.Status != privacy.StatusVerified || store.remoteCalls != 0 || store.backupCalls != 1 {
		t.Fatalf("verified replay result=%+v remote=%d backup=%d err=%v", result, store.remoteCalls, store.backupCalls, err)
	}
}

type appBackupInventory struct {
	artifacts []privacy.ManagedBackupArtifact
	calls     int
}

func (i *appBackupInventory) ManagedBackupInventory(context.Context) ([]privacy.ManagedBackupArtifact, error) {
	i.calls++
	return append([]privacy.ManagedBackupArtifact(nil), i.artifacts...), nil
}

type appBackupProducer struct{ calls int }

func (p *appBackupProducer) Produce(context.Context, int64) (privacy.ManagedBackupArtifact, error) {
	p.calls++
	return privacy.ManagedBackupArtifact{}, nil
}

type appRouteEraser struct{}

func (appRouteEraser) Erase(context.Context, privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
	return privacy.RemoteEraseResult{}, nil
}

type appRouteVerifier struct{}

func (appRouteVerifier) VerifyManagedBackups(context.Context, privacy.ManagedBackupVerificationRequest) (privacy.ManagedBackupVerificationResult, error) {
	return privacy.ManagedBackupVerificationResult{}, nil
}

type appPrivacyRouteStore struct {
	receipt      privacy.ErasureReceipt
	localResult  privacy.ErasureReceipt
	remoteResult privacy.ErasureReceipt
	backupResult privacy.ErasureReceipt
	localCalls   int
	remoteCalls  int
	backupCalls  int
}

func (s *appPrivacyRouteStore) CommitBarrierAuthorized(context.Context, privacy.ErasureRequest, privacy.ErasureGrantAuthorization) (privacy.ErasureReceipt, error) {
	return s.receipt, nil
}
func (s *appPrivacyRouteStore) Receipt(context.Context, string) (privacy.ErasureReceipt, error) {
	return s.receipt, nil
}
func (s *appPrivacyRouteStore) RunLocalScrub(context.Context, string) (privacy.ErasureReceipt, error) {
	s.localCalls++
	return s.localResult, nil
}
func (s *appPrivacyRouteStore) RunNocturneErase(context.Context, string, privacy.RemoteEraser) (privacy.ErasureReceipt, error) {
	s.remoteCalls++
	return s.remoteResult, nil
}
func (s *appPrivacyRouteStore) RunManagedBackupVerification(context.Context, string, privacy.ManagedBackupVerifier) (privacy.ErasureReceipt, error) {
	s.backupCalls++
	return s.backupResult, nil
}
func (s *appPrivacyRouteStore) CurrentOfflineDevicePurge(context.Context, string) (privacy.OfflinePurgeChallenge, bool, error) {
	return privacy.OfflinePurgeChallenge{}, false, nil
}
func (s *appPrivacyRouteStore) AcknowledgeOfflineDevicePurge(context.Context, string, string, privacy.OfflineDevicePurgeAcknowledgment) (privacy.OfflineDeviceChildReceipt, error) {
	return privacy.OfflineDeviceChildReceipt{}, nil
}

func appErasureReceipt(status privacy.ErasureStatus, remoteStatus, backupStatus privacy.StepStatus) privacy.ErasureReceipt {
	return privacy.ErasureReceipt{
		ErasureID: "10000000-0000-4000-8000-000000000001", Status: status,
		Steps: []privacy.StepReceipt{
			{Store: privacy.StoreNocturnePaths, Status: remoteStatus},
			{Store: privacy.StoreNocturneOrphanHistory, Status: remoteStatus},
			{Store: privacy.StoreNocturneSnapshotChangeset, Status: remoteStatus},
			{Store: privacy.StoreManagedBackup, Status: backupStatus},
		},
	}
}

func bridgeTestConfig(t *testing.T, enabled bool) config.Config {
	t.Helper()
	base, err := url.Parse("http://127.0.0.1:8233")
	if err != nil {
		t.Fatal(err)
	}
	return config.Config{
		Nocturne: config.NocturneConfig{
			Enabled: enabled, BaseURL: base, APIToken: strings.Repeat("a", 32), MaintenanceToken: strings.Repeat("b", 32),
			Namespace: "edu-agent", Domain: "core", HTTPTimeout: time.Second, ReconciliationInterval: time.Second,
			WorkerPollInterval: time.Second, WorkerLeaseDuration: 12 * time.Second, WorkerBatchSize: 2, DeliveryTTL: time.Hour,
			CandidateSweepInterval: time.Minute, DeliverySweepInterval: time.Minute, BackupRoot: t.TempDir(),
			BackupControllerInterval: time.Hour, BackupRetention: 24 * time.Hour,
		},
		Privacy: config.PrivacyConfig{ErasureGrantTTL: time.Minute, ErasureGrantMaxAttempts: 2},
	}
}

func workerNames(specs []workerSpec) string {
	names := make([]string, len(specs))
	for index, spec := range specs {
		names[index] = spec.name
	}
	return strings.Join(names, ",")
}

func containsMount(mounts []string, prefix string) bool {
	for _, mount := range mounts {
		if strings.HasPrefix(mount, prefix) {
			return true
		}
	}
	return false
}
