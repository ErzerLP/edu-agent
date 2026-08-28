package command

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/offline"
)

func TestOfflinePrepareReplaysRotationFromLegacyIntentAfterLocalAdvance(t *testing.T) {
	origin := "https://example.test/api"
	t1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	notAfter := t1.Add(365 * 24 * time.Hour)
	key1, key2 := commandSignerKey(0x40), commandSignerKey(0x50)
	rootPayload := api.OfflineSignerManifestPayload{
		ProtocolVersion: 1, ManifestRevision: "1", Issuer: "edu-agent", ServerBaseURL: origin,
		PreviousManifestDigest: base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)), IssuedAt: commandOfflineTime(t1),
		Keys: []api.OfflineSignerKey{commandManifestKey("key-1", key1.Public().(ed25519.PublicKey), t1, notAfter, t1, api.OfflineSignerKeyActive)},
	}
	rootManifest := commandSignManifest(t, rootPayload, "key-1", key1)
	rootPayloadBytes, _ := canonicalJSON(rootManifest.Payload)
	rootDigest := sha256.Sum256(rootPayloadBytes)
	rotatedPayload := api.OfflineSignerManifestPayload{
		ProtocolVersion: 1, ManifestRevision: "2", Issuer: "edu-agent", ServerBaseURL: origin,
		PreviousManifestDigest: base64.RawURLEncoding.EncodeToString(rootDigest[:]), IssuedAt: commandOfflineTime(t2),
		Keys: []api.OfflineSignerKey{
			commandManifestKey("key-1", key1.Public().(ed25519.PublicKey), t1, notAfter, t2, api.OfflineSignerKeyVerifyOnly),
			commandManifestKey("key-2", key2.Public().(ed25519.PublicKey), t2, notAfter, t2, api.OfflineSignerKeyActive),
		},
	}
	rotatedManifest := commandSignManifest(t, rotatedPayload, "key-1", key1)
	rootBytes := commandManifestJSON(t, rootManifest)
	rotatedBytes := commandManifestJSON(t, rotatedManifest)

	configStore, credentialStore := pairedStores(origin, "token")
	configStore.value.Offline = &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: origin, SignerManifest: rootBytes}
	binding, err := offline.NewBinding(origin, testDeviceID, 7)
	if err != nil {
		t.Fatal(err)
	}
	rootTrust, err := offline.NewTrustState(rootBytes)
	if err != nil {
		t.Fatal(err)
	}
	rotatedTrust, err := offline.NewTrustState(rotatedBytes)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "offline")
	store, err := offline.CreatePassphrase(t.Context(), root, offline.CreateOptions{Binding: binding, TrustState: rootTrust}, []byte("rotation replay passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	request := api.OfflinePrepareRequest{
		OperationID: "50000000-0000-4000-8000-000000000011", PayloadSchemaVersion: 1, ExpectedSessionVersion: "4",
		TrustedManifestRevision: "1", TrustedManifestDigest: base64.RawURLEncoding.EncodeToString(rootDigest[:]),
		RequestedCount: intPointer(1), RequestedTTLSeconds: intPointer(3600),
	}
	requestBytes, err := canonicalJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePrepareIntent(t.Context(), offline.PrepareIntent{RequestID: request.OperationID, CreatedAt: t2, Canonical: requestBytes}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTrustState(t.Context(), rotatedTrust); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rollbackClient := &offlineCommandClient{
		privateKey: key1, manifest: rootManifest, artifactKeyID: "key-1", origin: origin, now: t2, replayed: true,
	}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{secret: "rotation replay passphrase"})
	app.OfflineRoot = func() (string, error) { return root, nil }
	app.NewClient = func(string, string, time.Duration) APIClient { return rollbackClient }
	if exit := app.Run(t.Context(), []string{"offline", "prepare", "--count", "1"}); exit != ExitConflict {
		t.Fatalf("rollback replay exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "offline_signature_invalid") {
		t.Fatalf("rollback replay returned unstable error: %q", errOut.String())
	}

	client := &offlineCommandClient{
		privateKey: key2, manifest: rotatedManifest, manifestChain: []api.OfflineSignerManifestEnvelope{rotatedManifest},
		artifactKeyID: "key-2", origin: origin, now: t2, replayed: true,
	}
	out.Reset()
	errOut.Reset()
	app.NewClient = func(string, string, time.Duration) APIClient { return client }
	if exit := app.Run(t.Context(), []string{"offline", "prepare", "--count", "1"}); exit != ExitOK {
		t.Fatalf("rotation replay exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if len(client.prepareRequests) != 1 || client.prepareRequests[0].TrustedManifestRevision != "1" || client.prepareRequests[0].TrustedManifestDigest != request.TrustedManifestDigest {
		t.Fatalf("prepare did not replay the durable intent checkpoint: %#v", client.prepareRequests)
	}

	reopened, err := offline.OpenPassphrase(t.Context(), root, binding, rootTrust, []byte("rotation replay passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !bytes.Equal(reopened.TrustState().Bytes(), rotatedTrust.Bytes()) {
		t.Fatalf("replayed trust checkpoint=%s", reopened.TrustState().Bytes())
	}
	if _, err := reopened.GetPack(t.Context(), offlineTestPackID); err != nil {
		t.Fatalf("replayed pack was not published: %v", err)
	}
	if _, err := reopened.PendingPrepareIntent(t.Context()); !errors.Is(err, offline.ErrNotFound) {
		t.Fatalf("replayed prepare intent remains: %v", err)
	}
}

func TestOfflineTrustChainRejectsRollbackForkGapAndUnknownSigner(t *testing.T) {
	origin := "https://example.test/api"
	t1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t2, t3 := t1.Add(24*time.Hour), t1.Add(48*time.Hour)
	notAfter := t1.Add(365 * 24 * time.Hour)
	key1, key2, key3 := commandSignerKey(0x10), commandSignerKey(0x20), commandSignerKey(0x30)
	rootPayload := api.OfflineSignerManifestPayload{
		ProtocolVersion: 1, ManifestRevision: "1", Issuer: "edu-agent", ServerBaseURL: origin,
		PreviousManifestDigest: base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)), IssuedAt: commandOfflineTime(t1),
		Keys: []api.OfflineSignerKey{commandManifestKey("key-1", key1.Public().(ed25519.PublicKey), t1, notAfter, t1, api.OfflineSignerKeyActive)},
	}
	root := commandSignManifest(t, rootPayload, "key-1", key1)
	rootPayloadBytes, _ := canonicalJSON(root.Payload)
	rootDigest := sha256.Sum256(rootPayloadBytes)
	manifest2Payload := api.OfflineSignerManifestPayload{
		ProtocolVersion: 1, ManifestRevision: "2", Issuer: "edu-agent", ServerBaseURL: origin,
		PreviousManifestDigest: base64.RawURLEncoding.EncodeToString(rootDigest[:]), IssuedAt: commandOfflineTime(t2),
		Keys: []api.OfflineSignerKey{
			commandManifestKey("key-1", key1.Public().(ed25519.PublicKey), t1, notAfter, t2, api.OfflineSignerKeyVerifyOnly),
			commandManifestKey("key-2", key2.Public().(ed25519.PublicKey), t2, notAfter, t2, api.OfflineSignerKeyActive),
		},
	}
	manifest2 := commandSignManifest(t, manifest2Payload, "key-1", key1)
	manifest2Bytes, _ := canonicalJSON(manifest2.Payload)
	manifest2Digest := sha256.Sum256(manifest2Bytes)
	manifest3Payload := api.OfflineSignerManifestPayload{
		ProtocolVersion: 1, ManifestRevision: "3", Issuer: "edu-agent", ServerBaseURL: origin,
		PreviousManifestDigest: base64.RawURLEncoding.EncodeToString(manifest2Digest[:]), IssuedAt: commandOfflineTime(t3),
		Keys: []api.OfflineSignerKey{
			commandManifestKey("key-1", key1.Public().(ed25519.PublicKey), t1, notAfter, t2, api.OfflineSignerKeyRetired),
			commandManifestKey("key-2", key2.Public().(ed25519.PublicKey), t2, notAfter, t3, api.OfflineSignerKeyVerifyOnly),
			commandManifestKey("key-3", key3.Public().(ed25519.PublicKey), t3, notAfter, t3, api.OfflineSignerKeyActive),
		},
	}
	manifest3 := commandSignManifest(t, manifest3Payload, "key-2", key2)
	rootBytes, _ := canonicalJSON(root)
	value := config.Config{ServerURL: origin, Offline: &config.OfflineBinding{ProtocolVersion: 1, LearnerGeneration: "7", ServerBaseURL: origin, SignerManifest: rootBytes}}
	trust, err := loadOfflineTrust(value)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := advanceOfflineTrust(value, trust, []api.OfflineSignerManifestEnvelope{manifest2, manifest3})
	if err != nil || advanced.manifestRevision != 3 || advanced.activeKeyID != "key-3" {
		t.Fatalf("advanced revision=%d active=%s err=%v", advanced.manifestRevision, advanced.activeKeyID, err)
	}
	if _, err := advanceOfflineTrust(value, advanced, []api.OfflineSignerManifestEnvelope{manifest2}); err == nil {
		t.Fatal("manifest rollback was accepted")
	}
	forkPayload := manifest2Payload
	forkPayload.PreviousManifestDigest = base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))
	if _, err := advanceOfflineTrust(value, trust, []api.OfflineSignerManifestEnvelope{commandSignManifest(t, forkPayload, "key-1", key1)}); err == nil {
		t.Fatal("manifest fork was accepted")
	}
	if _, err := advanceOfflineTrust(value, trust, []api.OfflineSignerManifestEnvelope{manifest3}); err == nil {
		t.Fatal("manifest gap was accepted")
	}
	unknownPayload := manifest2Payload
	unknown := commandSignManifest(t, unknownPayload, "key-2", key2)
	if _, err := advanceOfflineTrust(value, trust, []api.OfflineSignerManifestEnvelope{unknown}); err == nil {
		t.Fatal("unknown manifest signer was accepted")
	}
}

func commandSignerKey(fill byte) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = fill + byte(index)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func commandManifestKey(keyID string, publicKey ed25519.PublicKey, notBefore, notAfter, effective time.Time, status api.OfflineSignerKeyStatus) api.OfflineSignerKey {
	fingerprint := sha256.Sum256(publicKey)
	return api.OfflineSignerKey{
		KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(publicKey), Fingerprint: base64.RawURLEncoding.EncodeToString(fingerprint[:]),
		NotBefore: commandOfflineTime(notBefore), NotAfter: commandOfflineTime(notAfter), StatusEffectiveAt: commandOfflineTime(effective), Status: status,
	}
}

func commandSignManifest(t *testing.T, payload api.OfflineSignerManifestPayload, keyID string, privateKey ed25519.PrivateKey) api.OfflineSignerManifestEnvelope {
	t.Helper()
	payloadBytes, err := canonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payloadBytes)
	message := append(append([]byte(nil), offlineSignerManifestDomain...), digest[:]...)
	return api.OfflineSignerManifestEnvelope{Payload: payload, SignerKeyID: keyID, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))}
}

func commandOfflineTime(value time.Time) api.OfflineTimestamp {
	return api.OfflineTimestamp(value.UTC().Format(time.RFC3339Nano))
}

func commandManifestJSON(t *testing.T, envelope api.OfflineSignerManifestEnvelope) json.RawMessage {
	t.Helper()
	value, err := canonicalJSON(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
