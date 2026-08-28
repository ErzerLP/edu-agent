package learning

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type offlineRuntimeStoreStub struct {
	prepared   learningPrepared
	ingest     func(OfflineOperation) (OfflineIngestResult, error)
	status     OfflineOperationStatus
	generation uint64
}

type learningPrepared struct {
	value OfflinePreparedPack
	err   error
}

func (s *offlineRuntimeStoreStub) ClaimOfflinePrepare(context.Context, OfflinePrepareStoreRequest) (OfflinePrepareClaim, error) {
	if s.prepared.err != nil {
		return OfflinePrepareClaim{}, s.prepared.err
	}
	value := s.prepared.value
	return OfflinePrepareClaim{State: "published", Prepared: &value}, nil
}

func (s *offlineRuntimeStoreStub) StoreOfflinePrepareArtifact(context.Context, string, string, string, OfflinePrepareArtifact) error {
	return errors.New("unexpected offline prepare artifact")
}

func (s *offlineRuntimeStoreStub) PublishOfflinePrepare(context.Context, OfflinePrepareStoreRequest, string, OfflineSigner) (OfflinePreparedPack, error) {
	return OfflinePreparedPack{}, errors.New("unexpected offline prepare publish")
}

func (s *offlineRuntimeStoreStub) RejectOfflinePrepare(context.Context, string, string, string, error) error {
	return errors.New("unexpected offline prepare rejection")
}

func (s *offlineRuntimeStoreStub) IngestOffline(_ context.Context, request OfflineIngestRequest) (OfflineIngestResult, error) {
	if s.ingest == nil {
		return OfflineIngestResult{}, errors.New("unexpected ingest")
	}
	return s.ingest(request.Operation)
}

func (s *offlineRuntimeStoreStub) OfflineOperationStatus(context.Context, string, string) (OfflineOperationStatus, error) {
	return s.status, nil
}

func (s *offlineRuntimeStoreStub) OfflineLearnerGeneration(context.Context) (uint64, error) {
	return s.generation, nil
}

func TestOfflinePairingBootstrapBindsGenerationOriginAndSignerRoot(t *testing.T) {
	signer := testOfflineSigner(t)
	service, err := NewOfflineService(&offlineRuntimeStoreStub{generation: 7}, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := service.PairingBootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.ProtocolVersion != 1 || bootstrap.LearnerGeneration != "7" || bootstrap.ServerBaseURL != signer.Origin() {
		t.Fatalf("offline pairing bootstrap=%+v", bootstrap)
	}
	if bootstrap.SignerManifest.SignerKeyID != signer.KeyID() || string(bootstrap.SignerManifest.Payload) != string(signer.ManifestEnvelope().Payload) {
		t.Fatalf("offline pairing signer root=%+v", bootstrap.SignerManifest)
	}
	verifyOfflineEnvelope(t, signer, OfflineSignerManifestDomain, bootstrap.SignerManifest)

	withoutSigner, err := NewOfflineService(&offlineRuntimeStoreStub{generation: 7}, nil, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutSigner.PairingBootstrap(context.Background()); ErrorCode(err) != CodeOfflineSignerUnavailable {
		t.Fatalf("missing signer bootstrap error=%v", err)
	}
}

func TestOfflineSignerUsesCanonicalDomainSeparatedPayloads(t *testing.T) {
	signer := testOfflineSigner(t)
	manifest := signer.ManifestEnvelope()
	if signer.ManifestDigest() != offlineBase64Digest(manifest.Payload) {
		t.Fatalf("manifest digest=%s", signer.ManifestDigest())
	}
	verifyOfflineEnvelope(t, signer, OfflineSignerManifestDomain, manifest)
	chain, err := signer.ManifestChain(0, OfflineZeroDigest)
	if err != nil || len(chain) != 1 {
		t.Fatalf("initial manifest chain=%+v err=%v", chain, err)
	}
	chain, err = signer.ManifestChain(1, signer.ManifestDigest())
	if err != nil || len(chain) != 0 {
		t.Fatalf("current manifest chain=%+v err=%v", chain, err)
	}
	if _, err := signer.ManifestChain(1, OfflineZeroDigest); ErrorCode(err) != CodeOfflinePrepareUnavailable {
		t.Fatalf("manifest fork error=%v", err)
	}

	payload := OfflinePrepareResponseSignaturePayloadV1{
		ProtocolVersion:  1,
		OperationID:      "10000000-0000-4000-8000-000000000001",
		RequestHash:      strings.Repeat("A", 43),
		PackDigest:       strings.Repeat("B", 43),
		ManifestRevision: "1",
		ManifestDigest:   signer.ManifestDigest(),
		ResponseAt:       time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
	}
	envelope, err := signer.Sign(OfflinePrepareResponseDomain, payload)
	if err != nil {
		t.Fatal(err)
	}
	verifyOfflineEnvelope(t, signer, OfflinePrepareResponseDomain, envelope)
	tampered := append([]byte(nil), envelope.Payload...)
	tampered[len(tampered)-2] ^= 1
	if verifyOfflineSignature(signer, OfflinePrepareResponseDomain, tampered, envelope.Signature) {
		t.Fatal("tampered payload retained a valid signature")
	}
}

func TestOfflineServicePrepareAndSyncFreezeWireSemantics(t *testing.T) {
	signer := testOfflineSigner(t)
	pack, err := signer.Sign(OfflinePackDomain, map[string]any{"pack_id": "20000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	requestHash := strings.Repeat("1", 64)
	store := &offlineRuntimeStoreStub{prepared: learningPrepared{value: OfflinePreparedPack{
		OperationID:      "10000000-0000-4000-8000-000000000001",
		RequestHash:      requestHash,
		Pack:             pack,
		PackDigest:       offlineBase64Digest(mustOfflineJSON(t, pack)),
		ManifestRevision: "1",
		ManifestDigest:   signer.ManifestDigest(),
	}}}
	fixedNow := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	service, err := NewOfflineService(store, signer, "https://EXAMPLE.test:443/api/", func() time.Time { return fixedNow })
	if err != nil {
		t.Fatal(err)
	}
	prepare := OfflinePrepareRequest{
		OperationID:             "10000000-0000-4000-8000-000000000001",
		PayloadSchemaVersion:    1,
		ExpectedSessionVersion:  "7",
		TrustedManifestRevision: "0",
		TrustedManifestDigest:   OfflineZeroDigest,
	}
	response, err := service.Prepare(context.Background(), "30000000-0000-4000-8000-000000000001", prepare)
	if err != nil {
		t.Fatal(err)
	}
	if response.Replayed || len(response.ManifestChain) != 1 || response.ResponseSignature.SignerKeyID != signer.KeyID() {
		t.Fatalf("prepare response=%+v", response)
	}
	verifyOfflineEnvelope(t, signer, OfflinePrepareResponseDomain, response.ResponseSignature)

	deviceID := "30000000-0000-4000-8000-000000000001"
	operations := []json.RawMessage{
		testOfflineWire(t, signer, deviceID, 1, "40000000-0000-4000-8000-000000000001", "50000000-0000-4000-8000-000000000001", "60000000-0000-4000-8000-000000000001"),
		testOfflineWire(t, signer, deviceID, 2, "40000000-0000-4000-8000-000000000002", "50000000-0000-4000-8000-000000000002", "60000000-0000-4000-8000-000000000002"),
		testOfflineWire(t, signer, deviceID, 3, "40000000-0000-4000-8000-000000000003", "50000000-0000-4000-8000-000000000003", "60000000-0000-4000-8000-000000000003"),
	}
	calls := 0
	store.ingest = func(operation OfflineOperation) (OfflineIngestResult, error) {
		calls++
		if calls == 2 {
			return OfflineIngestResult{}, errors.New("database unavailable")
		}
		sequence, _ := FormatUint63Decimal(operation.DeviceSequence)
		return OfflineIngestResult{
			ResultKind:     OfflineResultConflict,
			OperationID:    operation.OperationID,
			DeviceSequence: sequence,
			SubmissionID:   operation.SubmissionID,
			ArchiveStatus:  OfflineIdempotencyConflict,
			ReasonCodes:    []string{OfflineReasonIdempotencyConflict},
		}, nil
	}
	syncResponse, err := service.Sync(context.Background(), deviceID, OfflineSyncRequest{
		SyncRequestID:        "70000000-0000-4000-8000-000000000001",
		PayloadSchemaVersion: 1,
		Operations:           operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(syncResponse.Results) != 3 || syncResponse.Results[0].ResultKind != OfflineResultConflict || syncResponse.Results[1].ResultKind != OfflineResultRetryable || syncResponse.Results[2].ResultKind != OfflineResultNotProcessed {
		t.Fatalf("sync response=%+v calls=%d", syncResponse, calls)
	}
	encoded, err := json.Marshal(syncResponse)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"device_seq":"1"`) || strings.Contains(string(encoded), `"device_seq":1`) {
		t.Fatalf("device sequence is not canonical decimal text: %s", encoded)
	}
}

func TestOfflineServiceRejectsBodyDeviceAndSequenceReorderingBeforeIngest(t *testing.T) {
	signer := testOfflineSigner(t)
	store := &offlineRuntimeStoreStub{}
	service, err := NewOfflineService(store, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	deviceID := "30000000-0000-4000-8000-000000000001"
	wrongDevice := testOfflineWire(t, signer, "30000000-0000-4000-8000-000000000002", 1, "40000000-0000-4000-8000-000000000001", "50000000-0000-4000-8000-000000000001", "60000000-0000-4000-8000-000000000001")
	_, err = service.Sync(context.Background(), deviceID, OfflineSyncRequest{SyncRequestID: "70000000-0000-4000-8000-000000000001", PayloadSchemaVersion: 1, Operations: []json.RawMessage{wrongDevice}})
	if ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("device mismatch error=%v", err)
	}
	outOfOrder := []json.RawMessage{
		testOfflineWire(t, signer, deviceID, 2, "40000000-0000-4000-8000-000000000002", "50000000-0000-4000-8000-000000000002", "60000000-0000-4000-8000-000000000002"),
		testOfflineWire(t, signer, deviceID, 1, "40000000-0000-4000-8000-000000000001", "50000000-0000-4000-8000-000000000001", "60000000-0000-4000-8000-000000000001"),
	}
	_, err = service.Sync(context.Background(), deviceID, OfflineSyncRequest{SyncRequestID: "70000000-0000-4000-8000-000000000002", PayloadSchemaVersion: 1, Operations: outOfOrder})
	if ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("sequence ordering error=%v", err)
	}
}

func testOfflineSigner(t *testing.T) *Ed25519OfflineSigner {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	signer, err := NewEd25519OfflineSigner(
		"offline-test-key", ed25519.NewKeyFromSeed(seed), "https://EXAMPLE.test:443/api/",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if signer.Origin() != "https://example.test/api" {
		t.Fatalf("normalized origin=%q", signer.Origin())
	}
	return signer
}

func verifyOfflineEnvelope(t *testing.T, signer *Ed25519OfflineSigner, domain string, envelope OfflineSignedEnvelope) {
	t.Helper()
	if !verifyOfflineSignature(signer, domain, envelope.Payload, envelope.Signature) {
		t.Fatalf("invalid %q signature", domain)
	}
}

func verifyOfflineSignature(signer *Ed25519OfflineSigner, domain string, payload []byte, encodedSignature string) bool {
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(payload)
	message := append(append([]byte(nil), domain...), digest[:]...)
	return ed25519.Verify(signer.privateKey.Public().(ed25519.PublicKey), message, signature)
}

func testOfflineWire(t *testing.T, signer *Ed25519OfflineSigner, deviceID string, sequence uint64, operationID, submissionID, activityID string) json.RawMessage {
	t.Helper()
	sequenceText, _ := FormatUint63Decimal(sequence)
	authorizationPayload := OfflineAuthorizationPayloadV1{
		ProtocolVersion:       1,
		Format:                "offline-authorization-v1",
		Issuer:                "edu-agent",
		SignerKeyID:           signer.KeyID(),
		PackID:                "80000000-0000-4000-8000-000000000001",
		DeviceID:              deviceID,
		CredentialEpoch:       "1",
		LearnerGeneration:     "1",
		ServerOriginDigest:    offlineBase64Digest([]byte(signer.Origin())),
		OfflineActivityID:     activityID,
		ActivityRevision:      "1",
		SubmissionID:          submissionID,
		OperationID:           operationID,
		DeviceSequence:        sequenceText,
		ExpectedVersion:       "0",
		ActivityPayloadDigest: offlineBase64Digest([]byte(activityID)),
		EligibleUntil:         time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		ArchiveUntil:          time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
	}
	authorization, err := signer.Sign(OfflineAuthorizationDomain, authorizationPayload)
	if err != nil {
		t.Fatal(err)
	}
	answer := "four"
	wire := OfflineOperationWireV1{
		OperationID:          operationID,
		DeviceID:             deviceID,
		DeviceSequence:       sequenceText,
		SubmissionID:         submissionID,
		PayloadSchemaVersion: 1,
		AggregateType:        "offline_attempt",
		AggregateID:          submissionID,
		ExpectedVersion:      "0",
		OfflineActivityID:    activityID,
		ActivityRevision:     "1",
		Authorization:        authorization.Payload,
		Signature:            authorization.Signature,
		OccurredAt:           nil,
		OperationType:        OfflineAttemptCompleted,
		Payload:              mustOfflineJSON(t, OfflineAttemptPayload{Answer: answer, AnswerSHA256: SHA256([]byte(answer)), Help: HelpNone, Observations: []OfflineObservation{}}),
	}
	return mustOfflineJSON(t, wire)
}

func mustOfflineJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
