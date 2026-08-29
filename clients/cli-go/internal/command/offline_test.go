package command

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/offline"
)

const (
	offlineTestPackID       = "20000000-0000-4000-8000-000000000001"
	offlineTestSessionID    = "20000000-0000-4000-8000-000000000002"
	offlineTestActivityID   = "20000000-0000-4000-8000-000000000003"
	offlineTestSubmissionID = "20000000-0000-4000-8000-000000000004"
	offlineTestOperationID  = "20000000-0000-4000-8000-000000000005"
)

type offlineCommandClient struct {
	APIClient
	privateKey      ed25519.PrivateKey
	manifest        api.OfflineSignerManifestEnvelope
	manifestChain   []api.OfflineSignerManifestEnvelope
	artifactKeyID   string
	origin          string
	now             time.Time
	replayed        bool
	prepareRequests []api.OfflinePrepareRequest
	revoked         bool
	purgeTask       *api.OfflinePurgeTask
	purgeAck        *api.OfflinePurgeAckRequest
	beforePurgeAck  func()
}

func (c *offlineCommandClient) CurrentSession(context.Context) (api.SessionView, error) {
	return api.SessionView{Session: api.TutoringSession{SessionID: offlineTestSessionID, AggregateVersion: 4}}, nil
}

func (c *offlineCommandClient) PrepareOffline(_ context.Context, request api.OfflinePrepareRequest) (api.OfflinePrepareResponse, int, error) {
	c.prepareRequests = append(c.prepareRequests, request)
	artifactKeyID := c.artifactKeyID
	if artifactKeyID == "" {
		artifactKeyID = c.manifest.SignerKeyID
	}
	activity := api.OfflineActivity{
		ActivityID: offlineTestActivityID, Revision: 1, SessionID: offlineTestSessionID,
		GoalRevisionID: "30000000-0000-4000-8000-000000000001", RouteRevisionID: "30000000-0000-4000-8000-000000000002",
		RouteStepID: "30000000-0000-4000-8000-000000000003", KnowledgeRevisionID: "30000000-0000-4000-8000-000000000004",
		TargetNodeID: "30000000-0000-4000-8000-000000000005", TargetNodeRevisionID: "30000000-0000-4000-8000-000000000006",
		KnowledgeReferences: []api.OfflineKnowledgeReference{{
			KnowledgeRevisionID: "30000000-0000-4000-8000-000000000004", NodeID: "30000000-0000-4000-8000-000000000005",
			NodeRevisionID: "30000000-0000-4000-8000-000000000006", Range: api.OfflineSourceRange{Start: 0, End: 4}, Slice: "fact", SliceSHA256: sha256Hex([]byte("fact")),
		}},
		Prompt: "What is two plus two?", Type: api.OfflineActivityObjective,
		Rubric:     api.OfflineRubric{RubricRevision: "objective-rule-v1", Items: []api.OfflineRubricItem{{RubricItemID: "answer", Criterion: "answer is four"}}, ObjectiveRule: &api.OfflineObjectiveRule{AcceptedAnswers: []string{"4"}, TrimSpace: true}},
		Difficulty: 1, AllowedHelp: []api.OfflineHelpLevel{api.OfflineHelpNone}, ActivityPolicyVersion: "activity-policy-v1",
		AssessmentPolicyVersion: "assessment-policy-v1", ReviewPolicyVersion: "review-policy-v1", CreatedAt: api.OfflineTimestamp(c.now.Format(time.RFC3339Nano)),
	}
	activityBytes, _ := canonicalJSON(activity)
	activityDigest := sha256.Sum256(activityBytes)
	originDigest := sha256.Sum256([]byte(c.origin))
	eligible := api.OfflineTimestamp(c.now.Add(time.Hour).Format(time.RFC3339Nano))
	archive := api.OfflineTimestamp(c.now.Add(25 * time.Hour).Format(time.RFC3339Nano))
	authorizationPayload := api.OfflineAuthorizationPayload{
		ProtocolVersion: 1, Format: "offline-authorization-v1", Issuer: "edu-agent", SignerKeyID: artifactKeyID,
		PackID: offlineTestPackID, DeviceID: testDeviceID, CredentialEpoch: "1", LearnerGeneration: "7",
		ServerOriginDigest: base64.RawURLEncoding.EncodeToString(originDigest[:]), OfflineActivityID: offlineTestActivityID,
		ActivityRevision: "1", SubmissionID: offlineTestSubmissionID, OperationID: offlineTestOperationID, DeviceSequence: "1", ExpectedVersion: "0",
		ActivityPayloadDigest: base64.RawURLEncoding.EncodeToString(activityDigest[:]), EligibleUntil: eligible, ArchiveUntil: archive,
	}
	authorization := signOfflineTestEnvelope(tContext{}, offlineAuthorizationDomain, authorizationPayload, artifactKeyID, c.privateKey)
	packPayload := api.OfflinePackPayload{
		ProtocolVersion: 1, PackID: offlineTestPackID, Revision: "1", DeviceID: testDeviceID, LearnerGeneration: "7", ParentSessionID: offlineTestSessionID,
		IssuedAt: api.OfflineTimestamp(c.now.Format(time.RFC3339Nano)), EligibleUntil: eligible, ArchiveUntil: archive,
		Items: []api.OfflinePackItem{{Activity: activity, ActivityPayloadDigest: authorizationPayload.ActivityPayloadDigest, Authorization: api.OfflineAuthorizationEnvelope{Payload: authorizationPayload, SignerKeyID: authorization.SignerKeyID, Signature: authorization.Signature}}},
	}
	packSigned := signOfflineTestEnvelope(tContext{}, offlinePackDomain, packPayload, artifactKeyID, c.privateKey)
	pack := api.OfflinePackEnvelope{Payload: packPayload, SignerKeyID: packSigned.SignerKeyID, Signature: packSigned.Signature}
	requestBytes, _ := canonicalJSON(request)
	requestDigest := sha256.Sum256(requestBytes)
	packBytes, _ := canonicalJSON(pack)
	packDigest := sha256.Sum256(packBytes)
	manifestPayloadBytes, _ := canonicalJSON(c.manifest.Payload)
	manifestDigest := sha256.Sum256(manifestPayloadBytes)
	responseSignaturePayload := api.OfflinePrepareResponseSignaturePayload{
		ProtocolVersion: 1, OperationID: request.OperationID, RequestHash: base64.RawURLEncoding.EncodeToString(requestDigest[:]), Replayed: c.replayed,
		PackDigest: base64.RawURLEncoding.EncodeToString(packDigest[:]), ManifestRevision: c.manifest.Payload.ManifestRevision, ManifestDigest: base64.RawURLEncoding.EncodeToString(manifestDigest[:]),
		ResponseAt: api.OfflineTimestamp(c.now.Format(time.RFC3339Nano)),
	}
	responseSigned := signOfflineTestEnvelope(tContext{}, offlinePrepareResponseDomain, responseSignaturePayload, artifactKeyID, c.privateKey)
	return api.OfflinePrepareResponse{
		OperationID: request.OperationID, Replayed: c.replayed, Pack: pack, ManifestChain: append([]api.OfflineSignerManifestEnvelope(nil), c.manifestChain...),
		ResponseSignature: api.OfflinePrepareResponseSignatureEnvelope{Payload: responseSignaturePayload, SignerKeyID: responseSigned.SignerKeyID, Signature: responseSigned.Signature},
	}, 201, nil
}

func (c *offlineCommandClient) SyncOfflineCanonical(_ context.Context, raw []byte) (api.OfflineSyncResponse, error) {
	var request api.OfflineSyncRequest
	if err := json.Unmarshal(raw, &request); err != nil || len(request.Operations) != 1 {
		return api.OfflineSyncResponse{}, errors.New("invalid test sync request")
	}
	operation := request.Operations[0]
	archivedAt := api.OfflineTimestamp(c.now.Add(2 * time.Hour).Format(time.RFC3339Nano))
	return api.OfflineSyncResponse{SyncRequestID: request.SyncRequestID, Results: []api.OfflineSyncItemResult{{
		ResultKind: api.OfflineResultArchived, OperationID: operation.OperationID, DeviceSequence: operation.DeviceSequence, SubmissionID: operation.SubmissionID,
		ArchiveStatus: api.OfflineArchivedSucceeded, AssessmentStatus: api.OfflineAssessmentNotRequested, EvidenceStatus: api.OfflineEvidenceProvisional,
		ReasonCodes: []api.OfflineReasonCode{}, IngestReceipt: &api.OfflineIngestReceipt{
			ReceiptID: "40000000-0000-4000-8000-000000000001", ArchivedAt: archivedAt, AggregateVersion: "1", FirstEventSequence: "1", LastEventSequence: "2", ProjectionAsOfEventSeq: "2", ArchiveStatus: api.OfflineArchivedSucceeded,
		}, StatusTicket: &api.OfflineStatusTicket{TicketID: "40000000-0000-4000-8000-000000000002", OperationID: operation.OperationID, Revision: "1", UpdatedAt: archivedAt},
	}}}, nil
}

func (c *offlineCommandClient) RevokeDevice(context.Context, string) error {
	c.revoked = true
	return nil
}

func (c *offlineCommandClient) OfflineDevicePurgeTask(context.Context, string) (*api.OfflinePurgeTask, error) {
	return c.purgeTask, nil
}

func (c *offlineCommandClient) AckOfflineDevicePurge(_ context.Context, task api.OfflinePurgeTask, request api.OfflinePurgeAckRequest) (api.OfflinePurgeAckResponse, error) {
	if c.beforePurgeAck != nil {
		c.beforePurgeAck()
	}
	c.purgeAck = &request
	return api.OfflinePurgeAckResponse{
		ErasureID: task.ErasureID, DeviceID: task.DeviceID,
		SourceGeneration: task.OldGeneration, CurrentGeneration: task.CurrentGeneration,
		ChallengeRevision: task.ChallengeRevision, Status: request.Outcome,
		UpdatedAt: task.IssuedAt, StableReason: "device_acknowledged",
	}, nil
}

func TestPairPersistsOfflineTrustBootstrap(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pairings/exchange" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, manifest, _ := newOfflineTestTrust(t, server.URL+"/", time.Now().UTC().Truncate(time.Second))
		writeJSONTest(w, http.StatusCreated, api.IssuedCredential{
			Device: testDevice("Laptop"), Token: "device-token",
			Offline: &api.OfflinePairingBootstrap{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: server.URL + "/", SignerManifest: manifest},
		})
	}))
	defer server.Close()
	configStore := &memoryConfigStore{}
	credentialStore := &memoryCredentialStore{}
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{secret: "pairing-code"})
	if exit := app.Run(t.Context(), []string{"pair", "--server", server.URL, "--name", "Laptop"}); exit != ExitOK {
		t.Fatalf("pair exit=%d err=%q", exit, errOut.String())
	}
	if configStore.value.Offline == nil || configStore.value.Offline.LearnerGeneration != "7" {
		t.Fatalf("offline trust bootstrap was not persisted: %+v", configStore.value.Offline)
	}
	if _, err := loadOfflineTrust(configStore.value); err != nil {
		t.Fatalf("persisted trust bootstrap is invalid: %v", err)
	}
}

func TestOfflineKeyMigrateRewrapsExistingPassphraseProfile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake Secret Service command is Linux-specific")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	signedOrigin := "https://example.test/api/"
	privateKey, manifest, manifestBytes := newOfflineTestTrust(t, signedOrigin, time.Now().UTC().Truncate(time.Second))
	configStore, credentialStore := pairedStores("https://example.test/api", "token")
	configStore.value.Offline = &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: signedOrigin, SignerManifest: manifestBytes}
	client := &offlineCommandClient{privateKey: privateKey, manifest: manifest, origin: signedOrigin, now: time.Now().UTC().Truncate(time.Second)}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{secret: "legacy offline passphrase"})
	root := filepath.Join(t.TempDir(), "offline")
	app.OfflineRoot = func() (string, error) { return root, nil }
	app.NewClient = func(string, string, time.Duration) APIClient { return client }
	app.NewUUID = uuidSequence(t, "50000000-0000-4000-8000-000000000001")
	if exit := app.Run(t.Context(), []string{"offline", "prepare", "--count", "1"}); exit != ExitOK {
		t.Fatalf("prepare exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}

	bin := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "protected-key")
	secretTool := filepath.Join(bin, "secret-tool")
	script := `#!/bin/sh
case "$1" in
  lookup) [ -f "$EDU_AGENT_TEST_KEY_FILE" ] || exit 1; /bin/cat "$EDU_AGENT_TEST_KEY_FILE" ;;
  store) /bin/cat > "$EDU_AGENT_TEST_KEY_FILE" ;;
  clear) /bin/rm -f "$EDU_AGENT_TEST_KEY_FILE" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(secretTool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/edu-agent-fake-secret-service")
	t.Setenv("EDU_AGENT_TEST_KEY_FILE", keyFile)
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "key-migrate"}); exit != ExitOK {
		t.Fatalf("key-migrate exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	protected, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(protected, []byte("legacy offline passphrase")) {
		t.Fatal("legacy passphrase appeared in the protected system-key entry")
	}

	binding, err := offline.NewBinding(configStore.value.ServerURL, configStore.value.DeviceID, 7)
	if err != nil {
		t.Fatal(err)
	}
	trustState, err := offline.NewTrustState(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if legacyStore, openErr := offline.OpenPassphrase(t.Context(), root, binding, trustState, []byte("legacy offline passphrase")); openErr == nil {
		_ = legacyStore.Close()
		t.Fatal("legacy passphrase still opened the migrated profile")
	}

	app.Terminal = &fakeTerminal{secret: "wrong fallback passphrase"}
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "status"}); exit != ExitOK {
		t.Fatalf("system-key open exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	erasureID := "60000000-0000-4000-8000-000000000020"
	client.purgeTask = &api.OfflinePurgeTask{
		ErasureID: erasureID, DeviceID: testDeviceID,
		OldGeneration: 7, CurrentGeneration: 8, ChallengeRevision: 1,
		Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)),
		IssuedAt:  api.OfflineTimestamp(time.Now().UTC().Format(time.RFC3339Nano)), Status: "pending",
	}
	out.Reset()
	errOut.Reset()
	backendFailureScript := `#!/bin/sh
case "$1" in
  lookup|clear) echo 'secret service unavailable' >&2; exit 1 ;;
  store) /bin/cat > "$EDU_AGENT_TEST_KEY_FILE" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(secretTool, []byte(backendFailureScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run(t.Context(), []string{"offline", "purge", erasureID}); exit == ExitOK {
		t.Fatalf("purge unexpectedly succeeded while system key backend was unavailable: out=%q err=%q", out.String(), errOut.String())
	}
	if client.purgeAck == nil || client.purgeAck.Outcome != "failed" || client.purgeAck.FailureCode != "key_delete_failed" {
		t.Fatalf("backend outage produced an incorrect purge acknowledgment: %#v", client.purgeAck)
	}
	if _, err := os.Lstat(keyFile); err != nil {
		t.Fatalf("system key was lost during backend outage: %v", err)
	}
	if err := os.WriteFile(secretTool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "purge", erasureID}); exit != ExitOK {
		t.Fatalf("system-key purge retry exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if _, err := os.Lstat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("system key remains after acknowledged purge: %v", err)
	}
}

type fakeOfflineKeyStore struct {
	available bool
	secret    []byte
	loadErr   error
	storeErr  error
	deleteErr error
}

func (f *fakeOfflineKeyStore) Available(string) bool { return f.available }
func (f *fakeOfflineKeyStore) Generate() ([]byte, error) {
	if len(f.secret) == 0 {
		f.secret = bytes.Repeat([]byte{0x42}, 32)
	}
	return append([]byte(nil), f.secret...), nil
}
func (f *fakeOfflineKeyStore) Load(string) ([]byte, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if len(f.secret) == 0 {
		return nil, keybackend.ErrNotFound
	}
	return append([]byte(nil), f.secret...), nil
}
func (f *fakeOfflineKeyStore) Store(_ string, secret []byte) error {
	if f.storeErr != nil {
		return f.storeErr
	}
	f.secret = append([]byte(nil), secret...)
	return nil
}
func (f *fakeOfflineKeyStore) Delete(string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.secret = nil
	return nil
}

func TestSystemBoundOfflineProfileNeverFallsBackToPassphrase(t *testing.T) {
	signedOrigin := "https://example.test/api/"
	_, _, manifestBytes := newOfflineTestTrust(t, signedOrigin, time.Now().UTC().Truncate(time.Second))
	configStore, credentialStore := pairedStores("https://example.test/api", "token")
	configStore.value.Offline = &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: signedOrigin, SignerManifest: manifestBytes}
	binding, err := offline.NewBinding(configStore.value.ServerURL, configStore.value.DeviceID, 7)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := offline.NewTrustState(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "offline")
	systemSecret := bytes.Repeat([]byte{0x31}, 32)
	store, err := offline.CreateSystem(t.Context(), root, offline.CreateOptions{Binding: binding, TrustState: trust}, systemSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		keyStore *fakeOfflineKeyStore
	}{
		{name: "backend unavailable", keyStore: &fakeOfflineKeyStore{available: true, loadErr: keybackend.ErrUnavailable}},
		{name: "identity mismatch", keyStore: &fakeOfflineKeyStore{available: true, secret: bytes.Repeat([]byte{0x32}, 32)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			terminal := &fakeTerminal{secret: "fallback must not be read"}
			app, out, errOut := newTestApp(configStore, credentialStore, terminal)
			app.OfflineRoot = func() (string, error) { return root, nil }
			app.OfflineKeys = test.keyStore
			if exit := app.Run(t.Context(), []string{"offline", "status"}); exit != ExitUnavailable {
				t.Fatalf("status exit=%d out=%q err=%q", exit, out.String(), errOut.String())
			}
			if !strings.Contains(errOut.String(), "offline_key_backend_unavailable") {
				t.Fatalf("unstable error=%q", errOut.String())
			}
			if terminal.secretCalls != 0 {
				t.Fatalf("system-bound profile prompted for passphrase %d time(s)", terminal.secretCalls)
			}
		})
	}
}

func TestOfflinePrepareDoesNotSilentlyDowngradeWhenSystemBackendFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake Secret Service command is Linux-specific")
	}
	bin := t.TempDir()
	secretTool := filepath.Join(bin, "secret-tool")
	if err := os.WriteFile(secretTool, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/edu-agent-fake-secret-service")

	signedOrigin := "https://example.test/api/"
	privateKey, manifest, manifestBytes := newOfflineTestTrust(t, signedOrigin, time.Now().UTC().Truncate(time.Second))
	configStore, credentialStore := pairedStores("https://example.test/api", "token")
	configStore.value.Offline = &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: signedOrigin, SignerManifest: manifestBytes}
	client := &offlineCommandClient{privateKey: privateKey, manifest: manifest, origin: signedOrigin, now: time.Now().UTC().Truncate(time.Second)}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{secret: "fallback must not be used"})
	root := filepath.Join(t.TempDir(), "offline")
	app.OfflineRoot = func() (string, error) { return root, nil }
	app.NewClient = func(string, string, time.Duration) APIClient { return client }
	app.NewUUID = uuidSequence(t, "50000000-0000-4000-8000-000000000001")

	if exit := app.Run(t.Context(), []string{"offline", "prepare", "--count", "1"}); exit != ExitUnavailable {
		t.Fatalf("prepare exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if exists, err := offline.Exists(root); err != nil || exists {
		t.Fatalf("profile should not be created after backend failure: exists=%t err=%v", exists, err)
	}
}

func TestOfflinePrepareLearnSyncAndSafeLogoutLoop(t *testing.T) {
	signedOrigin := "https://example.test/api/"
	privateKey, manifest, manifestBytes := newOfflineTestTrust(t, signedOrigin, time.Now().UTC().Truncate(time.Second))
	configStore, credentialStore := pairedStores("https://example.test/api", "token")
	configStore.value.Offline = &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: signedOrigin, SignerManifest: manifestBytes}
	client := &offlineCommandClient{privateKey: privateKey, manifest: manifest, origin: signedOrigin, now: time.Now().UTC().Truncate(time.Second)}
	terminal := &fakeTerminal{secret: "correct horse battery staple", lines: []string{"4"}, confirmed: true}
	app, out, errOut := newTestApp(configStore, credentialStore, terminal)
	root := filepath.Join(t.TempDir(), "offline")
	app.OfflineRoot = func() (string, error) { return root, nil }
	app.NewClient = func(string, string, time.Duration) APIClient { return client }
	app.NewUUID = uuidSequence(t, "50000000-0000-4000-8000-000000000001", "50000000-0000-4000-8000-000000000002")

	if exit := app.Run(t.Context(), []string{"offline", "prepare", "--count", "1"}); exit != ExitOK {
		t.Fatalf("prepare exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	assertOfflineFilesDoNotContain(t, root, "What is two plus two?", "correct horse battery staple")
	out.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "learn"}); exit != ExitOK {
		t.Fatalf("learn exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	assertOfflineFilesDoNotContain(t, root, "What is two plus two?", "correct horse battery staple", `"answer":"4"`)
	out.Reset()
	if exit := app.Run(t.Context(), []string{"logout"}); exit != ExitConflict || client.revoked {
		t.Fatalf("queued logout exit=%d revoked=%t err=%q", exit, client.revoked, errOut.String())
	}
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "sync"}); exit != ExitOK {
		t.Fatalf("sync exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	out.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "status"}); exit != ExitOK || !strings.Contains(out.String(), "Terminal: 1") {
		t.Fatalf("status exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	out.Reset()
	if exit := app.Run(t.Context(), []string{"logout"}); exit != ExitOK || !client.revoked || !configStore.present || configStore.value.HasPairingBinding() || credentialStore.present {
		t.Fatalf("logout exit=%d revoked=%t config=%+v credential=%t err=%q", exit, client.revoked, configStore.value, credentialStore.present, errOut.String())
	}
	if exists, err := offline.Exists(root); err != nil || exists {
		t.Fatalf("offline profile remains after safe logout: exists=%t err=%v", exists, err)
	}
}

func TestOfflinePurgeDeletesManagedProfileAndAcknowledges(t *testing.T) {
	signedOrigin := "https://example.test/api/"
	now := time.Now().UTC().Truncate(time.Second)
	privateKey, manifest, manifestBytes := newOfflineTestTrust(t, signedOrigin, now)
	configStore, credentialStore := pairedStores("https://example.test/api", "token")
	configStore.value.Offline = &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: signedOrigin, SignerManifest: manifestBytes}
	client := &offlineCommandClient{privateKey: privateKey, manifest: manifest, origin: signedOrigin, now: now}
	terminal := &fakeTerminal{secret: "correct horse battery staple"}
	app, out, errOut := newTestApp(configStore, credentialStore, terminal)
	root := filepath.Join(t.TempDir(), "offline")
	app.OfflineRoot = func() (string, error) { return root, nil }
	app.NewClient = func(string, string, time.Duration) APIClient { return client }
	app.NewUUID = uuidSequence(t, "50000000-0000-4000-8000-000000000011")

	if exit := app.Run(t.Context(), []string{"offline", "prepare", "--count", "1"}); exit != ExitOK {
		t.Fatalf("prepare exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if exists, err := offline.Exists(root); err != nil || !exists {
		t.Fatalf("offline profile missing before purge: exists=%t err=%v", exists, err)
	}
	erasureID := "60000000-0000-4000-8000-000000000001"
	client.purgeTask = &api.OfflinePurgeTask{
		ErasureID: erasureID, DeviceID: testDeviceID,
		OldGeneration: 7, CurrentGeneration: 8, ChallengeRevision: 1,
		Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)),
		IssuedAt:  api.OfflineTimestamp(now.Format(time.RFC3339Nano)), Status: "pending",
	}
	client.beforePurgeAck = func() {
		leaseContext, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
		defer cancel()
		lease, err := offline.AcquireLease(leaseContext, root, offline.LeaseExclusive, 25*time.Millisecond)
		if lease != nil {
			_ = lease.Close()
		}
		if !errors.Is(err, offline.ErrProfileBusy) {
			t.Fatalf("purge did not retain the exclusive profile lease through acknowledgment: %v", err)
		}
	}
	out.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "purge", erasureID}); exit != ExitOK {
		t.Fatalf("purge exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if exists, err := offline.Exists(root); err != nil || exists {
		t.Fatalf("offline profile remains after purge: exists=%t err=%v", exists, err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed offline root remains after purge: %v", err)
	}
	if client.purgeAck == nil || client.purgeAck.Outcome != "succeeded" || client.purgeAck.ManagedObjectsAbsent == nil || !*client.purgeAck.ManagedObjectsAbsent {
		t.Fatalf("unexpected purge acknowledgment: %#v", client.purgeAck)
	}
}

func TestVerifyPreparedPackRejectsTamperedSignature(t *testing.T) {
	privateKey, manifest, manifestBytes := newOfflineTestTrust(t, "https://example.test/api", time.Now().UTC().Truncate(time.Second))
	value := config.Config{ServerURL: "https://example.test/api", DeviceID: testDeviceID, DisplayName: "Laptop", Timeout: "2s", Color: "never", Offline: &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: "https://example.test/api", SignerManifest: manifestBytes}}
	trust, err := loadOfflineTrust(value)
	if err != nil {
		t.Fatal(err)
	}
	client := &offlineCommandClient{privateKey: privateKey, manifest: manifest, origin: value.ServerURL, now: time.Now().UTC().Truncate(time.Second)}
	request := api.OfflinePrepareRequest{OperationID: "50000000-0000-4000-8000-000000000001", PayloadSchemaVersion: 1, ExpectedSessionVersion: "4", TrustedManifestRevision: "1", TrustedManifestDigest: trust.manifestDigest, RequestedCount: intPointer(1), RequestedTTLSeconds: intPointer(3600)}
	response, _, err := client.PrepareOffline(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	response.Pack.Signature = strings.Repeat("A", 86)
	if _, _, err := verifyPreparedPack(response, request, value, trust); err == nil {
		t.Fatal("tampered pack signature was accepted")
	}
}

type tContext struct{}

func signOfflineTestEnvelope(_ tContext, domain string, payload any, keyID string, privateKey ed25519.PrivateKey) api.OfflineSignerManifestEnvelope {
	canonical, err := canonicalJSON(payload)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	message := append([]byte(domain), digest[:]...)
	return api.OfflineSignerManifestEnvelope{SignerKeyID: keyID, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))}
}

func newOfflineTestTrust(t *testing.T, origin string, now time.Time) (ed25519.PrivateKey, api.OfflineSignerManifestEnvelope, json.RawMessage) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(publicKey)
	payload := api.OfflineSignerManifestPayload{
		ProtocolVersion: 1, ManifestRevision: "1", Issuer: "edu-agent", ServerBaseURL: origin,
		PreviousManifestDigest: base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)), IssuedAt: api.OfflineTimestamp(now.Add(-time.Hour).Format(time.RFC3339Nano)),
		Keys: []api.OfflineSignerKey{{KeyID: "offline-test-key", PublicKey: base64.RawURLEncoding.EncodeToString(publicKey), Fingerprint: base64.RawURLEncoding.EncodeToString(fingerprint[:]),
			NotBefore: api.OfflineTimestamp(now.Add(-time.Hour).Format(time.RFC3339Nano)), NotAfter: api.OfflineTimestamp(now.Add(24 * time.Hour).Format(time.RFC3339Nano)),
			StatusEffectiveAt: api.OfflineTimestamp(now.Add(-time.Hour).Format(time.RFC3339Nano)), Status: api.OfflineSignerKeyActive}},
	}
	signed := signOfflineTestEnvelope(tContext{}, offlineSignerManifestDomain, payload, "offline-test-key", privateKey)
	envelope := api.OfflineSignerManifestEnvelope{Payload: payload, SignerKeyID: signed.SignerKeyID, Signature: signed.Signature}
	canonical, err := canonicalJSON(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, envelope, canonical
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func assertOfflineFilesDoNotContain(t *testing.T, root string, forbidden ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range forbidden {
			if strings.Contains(string(content), value) {
				t.Fatalf("offline file %s contains forbidden plaintext %q", path, value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
