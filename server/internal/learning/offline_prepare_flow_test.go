package learning

import (
	"context"
	"errors"
	"testing"
	"time"
)

type offlinePrepareFlowStore struct {
	generation    OfflinePrepareGenerationRequest
	artifact      *OfflinePrepareArtifact
	prepared      OfflinePreparedPack
	published     bool
	artifactErr   error
	leaseToken    string
	claimCalls    int
	artifactCalls int
	publishCalls  int
	rejectCalls   int
}

func (s *offlinePrepareFlowStore) ClaimOfflinePrepare(context.Context, OfflinePrepareStoreRequest) (OfflinePrepareClaim, error) {
	s.claimCalls++
	if s.published {
		value := s.prepared
		value.Replayed = true
		return OfflinePrepareClaim{State: "published", Prepared: &value}, nil
	}
	leaseToken := s.leaseToken
	if leaseToken == "" {
		leaseToken = "90000000-0000-4000-8000-000000000001"
	}
	claim := OfflinePrepareClaim{State: "claimed", LeaseToken: leaseToken, Generation: &s.generation}
	if s.artifact != nil {
		value := *s.artifact
		claim.Artifact = &value
	}
	return claim, nil
}

func (s *offlinePrepareFlowStore) StoreOfflinePrepareArtifact(_ context.Context, _, _, leaseToken string, artifact OfflinePrepareArtifact) error {
	s.artifactCalls++
	if s.leaseToken != "" && leaseToken != s.leaseToken {
		return &Error{Code: CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	if s.artifactErr != nil {
		return s.artifactErr
	}
	value := artifact
	s.artifact = &value
	return nil
}

func (s *offlinePrepareFlowStore) PublishOfflinePrepare(_ context.Context, _ OfflinePrepareStoreRequest, leaseToken string, _ OfflineSigner) (OfflinePreparedPack, error) {
	s.publishCalls++
	if s.leaseToken != "" && leaseToken != s.leaseToken {
		return OfflinePreparedPack{}, &Error{Code: CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	s.published = true
	return s.prepared, nil
}

func (s *offlinePrepareFlowStore) RejectOfflinePrepare(_ context.Context, _, _, leaseToken string, _ error) error {
	s.rejectCalls++
	if s.leaseToken != "" && leaseToken != s.leaseToken {
		return &Error{Code: CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	return nil
}

func (*offlinePrepareFlowStore) IngestOffline(context.Context, OfflineIngestRequest) (OfflineIngestResult, error) {
	return OfflineIngestResult{}, errors.New("unexpected ingest")
}

func (*offlinePrepareFlowStore) OfflineOperationStatus(context.Context, string, string) (OfflineOperationStatus, error) {
	return OfflineOperationStatus{}, errors.New("unexpected status")
}

type offlinePrepareFlowGenerator struct {
	artifact OfflinePrepareArtifact
	calls    int
}

func (g *offlinePrepareFlowGenerator) GenerateOfflinePrepare(context.Context, OfflinePrepareGenerationRequest) (OfflinePrepareArtifact, error) {
	g.calls++
	return g.artifact, nil
}

func TestOfflinePrepareClaimGeneratePublishReplaysWithoutDuplicateFacts(t *testing.T) {
	signer := testOfflineSigner(t)
	pack, err := signer.Sign(OfflinePackDomain, map[string]any{"pack_id": "91000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	activity := Activity{ID: "91000000-0000-4000-8000-000000000002", Revision: 1}
	artifact := OfflinePrepareArtifact{
		ProtocolVersion: 1, SessionID: "91000000-0000-4000-8000-000000000003",
		SessionState: "RouteActive", ExpectedSessionVersion: 7,
		GoalRevisionID:      "91000000-0000-4000-8000-000000000004",
		RouteRevisionID:     "91000000-0000-4000-8000-000000000005",
		RouteStepID:         "91000000-0000-4000-8000-000000000006",
		KnowledgeRevisionID: "91000000-0000-4000-8000-000000000007",
		Activities:          []Activity{activity},
	}
	store := &offlinePrepareFlowStore{
		generation: OfflinePrepareGenerationRequest{Count: 1},
		prepared: OfflinePreparedPack{
			OperationID: "91000000-0000-4000-8000-000000000008",
			RequestHash: "1111111111111111111111111111111111111111111111111111111111111111",
			Pack:        pack, PackDigest: offlineBase64Digest(mustOfflineJSON(t, pack)),
			ManifestRevision: "1", ManifestDigest: signer.ManifestDigest(),
		},
	}
	generator := &offlinePrepareFlowGenerator{artifact: artifact}
	service, err := NewOfflineServiceWithGenerator(store, generator, signer, signer.Origin(), func() time.Time {
		return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	request := OfflinePrepareRequest{
		OperationID: store.prepared.OperationID, PayloadSchemaVersion: 1,
		ExpectedSessionVersion: "7", TrustedManifestRevision: "1",
		TrustedManifestDigest: signer.ManifestDigest(), RequestedCount: intPtrOffline(1),
	}
	first, err := service.Prepare(t.Context(), "91000000-0000-4000-8000-000000000009", request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Prepare(t.Context(), "91000000-0000-4000-8000-000000000009", request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || !second.Replayed || string(first.Pack.Payload) != string(second.Pack.Payload) {
		t.Fatalf("first replayed=%v second replayed=%v", first.Replayed, second.Replayed)
	}
	if generator.calls != 1 || store.artifactCalls != 1 || store.publishCalls != 1 || store.rejectCalls != 0 || store.claimCalls != 2 {
		t.Fatalf("generator=%d artifact=%d publish=%d reject=%d claims=%d", generator.calls, store.artifactCalls, store.publishCalls, store.rejectCalls, store.claimCalls)
	}
}

func TestOfflinePrepareReusesPersistedArtifactAfterTakeover(t *testing.T) {
	signer := testOfflineSigner(t)
	pack, err := signer.Sign(OfflinePackDomain, map[string]any{"pack_id": "92000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	artifact := OfflinePrepareArtifact{
		ProtocolVersion: 1, SessionID: "92000000-0000-4000-8000-000000000002", SessionState: "RouteActive",
		ExpectedSessionVersion: 3, GoalRevisionID: "92000000-0000-4000-8000-000000000003",
		RouteRevisionID: "92000000-0000-4000-8000-000000000004", RouteStepID: "92000000-0000-4000-8000-000000000005",
		KnowledgeRevisionID: "92000000-0000-4000-8000-000000000006",
		Activities:          []Activity{{ID: "92000000-0000-4000-8000-000000000007", Revision: 1}},
	}
	store := &offlinePrepareFlowStore{artifact: &artifact, prepared: OfflinePreparedPack{
		OperationID: "92000000-0000-4000-8000-000000000008",
		RequestHash: "2222222222222222222222222222222222222222222222222222222222222222",
		Pack:        pack, PackDigest: offlineBase64Digest(mustOfflineJSON(t, pack)), ManifestRevision: "1", ManifestDigest: signer.ManifestDigest(),
	}}
	generator := &offlinePrepareFlowGenerator{artifact: artifact}
	service, err := NewOfflineServiceWithGenerator(store, generator, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Prepare(t.Context(), "92000000-0000-4000-8000-000000000009", OfflinePrepareRequest{
		OperationID: store.prepared.OperationID, PayloadSchemaVersion: 1, ExpectedSessionVersion: "3",
		TrustedManifestRevision: "1", TrustedManifestDigest: signer.ManifestDigest(), RequestedCount: intPtrOffline(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != 0 || store.artifactCalls != 0 || store.publishCalls != 1 {
		t.Fatalf("artifact reuse generator=%d store_artifact=%d publish=%d", generator.calls, store.artifactCalls, store.publishCalls)
	}
}

func TestOfflinePrepareArtifactCASLossDoesNotPublish(t *testing.T) {
	signer := testOfflineSigner(t)
	artifact := OfflinePrepareArtifact{
		ProtocolVersion: 1, SessionID: "93000000-0000-4000-8000-000000000001", SessionState: "RouteActive",
		ExpectedSessionVersion: 4, GoalRevisionID: "93000000-0000-4000-8000-000000000002",
		RouteRevisionID: "93000000-0000-4000-8000-000000000003", RouteStepID: "93000000-0000-4000-8000-000000000004",
		KnowledgeRevisionID: "93000000-0000-4000-8000-000000000005",
		Activities:          []Activity{{ID: "93000000-0000-4000-8000-000000000006", Revision: 1}},
	}
	leaseLost := &Error{Code: CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	store := &offlinePrepareFlowStore{
		generation:  OfflinePrepareGenerationRequest{Count: 1},
		artifactErr: leaseLost,
		leaseToken:  "93000000-0000-4000-8000-000000000007",
	}
	generator := &offlinePrepareFlowGenerator{artifact: artifact}
	service, err := NewOfflineServiceWithGenerator(store, generator, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Prepare(t.Context(), "93000000-0000-4000-8000-000000000008", OfflinePrepareRequest{
		OperationID: "93000000-0000-4000-8000-000000000009", PayloadSchemaVersion: 1,
		ExpectedSessionVersion: "4", TrustedManifestRevision: "1",
		TrustedManifestDigest: signer.ManifestDigest(), RequestedCount: intPtrOffline(1),
	})
	if ErrorCode(err) != CodeStaleProposal || store.artifactCalls != 1 || store.publishCalls != 0 || store.published {
		t.Fatalf("error=%v artifact=%d publish=%d published=%v", err, store.artifactCalls, store.publishCalls, store.published)
	}
}

func intPtrOffline(value int) *int { return &value }
