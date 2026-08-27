package learning

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestOfflineSignerManifestChainAdvancesOneTwoThree(t *testing.T) {
	origin := "https://example.test/api"
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	t3 := t2.Add(24 * time.Hour)
	notAfter := t1.Add(365 * 24 * time.Hour)
	key1 := offlineSignerTestKey(0x11)
	key2 := offlineSignerTestKey(0x22)
	key3 := offlineSignerTestKey(0x33)
	rootSigner, err := NewEd25519OfflineSigner("key-1", key1, origin, t1, notAfter)
	if err != nil {
		t.Fatal(err)
	}
	var rootPayload OfflineSignerManifestPayloadV1
	if err := json.Unmarshal(rootSigner.RootManifestEnvelope().Payload, &rootPayload); err != nil {
		t.Fatal(err)
	}
	manifest2Payload := OfflineSignerManifestPayloadV1{
		ProtocolVersion: 1, ManifestRevision: "2", Issuer: "edu-agent", ServerBaseURL: rootSigner.Origin(),
		PreviousDigest: rootSigner.ManifestDigest(), IssuedAt: t2,
		Keys: []OfflineSignerKeyV1{
			offlineSignerManifestKey("key-1", key1.Public().(ed25519.PublicKey), t1, notAfter, t2, "verify_only"),
			offlineSignerManifestKey("key-2", key2.Public().(ed25519.PublicKey), t2, notAfter, t2, "active"),
		},
	}
	manifest2 := signOfflineManifestForTest(t, manifest2Payload, "key-1", key1)
	manifest2Digest := offlineBase64Digest(manifest2.Payload)
	manifest3Payload := OfflineSignerManifestPayloadV1{
		ProtocolVersion: 1, ManifestRevision: "3", Issuer: "edu-agent", ServerBaseURL: rootSigner.Origin(),
		PreviousDigest: manifest2Digest, IssuedAt: t3,
		Keys: []OfflineSignerKeyV1{
			offlineSignerManifestKey("key-1", key1.Public().(ed25519.PublicKey), t1, notAfter, t2, "retired"),
			offlineSignerManifestKey("key-2", key2.Public().(ed25519.PublicKey), t2, notAfter, t3, "verify_only"),
			offlineSignerManifestKey("key-3", key3.Public().(ed25519.PublicKey), t3, notAfter, t3, "active"),
		},
	}
	manifest3 := signOfflineManifestForTest(t, manifest3Payload, "key-2", key2)
	chain := []json.RawMessage{mustOfflineCanonical(t, rootSigner.RootManifestEnvelope()), mustOfflineCanonical(t, manifest2), mustOfflineCanonical(t, manifest3)}
	signer, err := NewEd25519OfflineSignerWithManifestChain("key-3", key3, origin, t3, notAfter, chain)
	if err != nil {
		t.Fatal(err)
	}
	if signer.ManifestRevision() != 3 || signer.ManifestDigest() != offlineBase64Digest(manifest3.Payload) || signer.KeyID() != "key-3" {
		t.Fatalf("current signer revision=%d digest=%s key=%s", signer.ManifestRevision(), signer.ManifestDigest(), signer.KeyID())
	}
	fromRoot, err := signer.ManifestChain(1, rootSigner.ManifestDigest())
	if err != nil || len(fromRoot) != 2 || string(fromRoot[0].Payload) != string(manifest2.Payload) || string(fromRoot[1].Payload) != string(manifest3.Payload) {
		t.Fatalf("chain from root=%+v err=%v", fromRoot, err)
	}
	fromZero, err := signer.ManifestChain(0, OfflineZeroDigest)
	if err != nil || len(fromZero) != 3 {
		t.Fatalf("chain from zero len=%d err=%v", len(fromZero), err)
	}
	if _, err := signer.ManifestChain(2, OfflineZeroDigest); ErrorCode(err) != CodeOfflinePrepareUnavailable {
		t.Fatalf("fork accepted: %v", err)
	}
	if string(rootPayload.ManifestRevision) != "1" || string(signer.RootManifestEnvelope().Payload) != string(rootSigner.RootManifestEnvelope().Payload) {
		t.Fatal("rotation changed the pairing revision-1 root")
	}
}

func TestOfflineSignerLegacyConstructorRemainsRevisionOneByteCompatible(t *testing.T) {
	key := offlineSignerTestKey(0x44)
	issuedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	notAfter := issuedAt.Add(365 * 24 * time.Hour)
	legacy, err := NewEd25519OfflineSigner("legacy-key", key, "https://example.test/api", issuedAt, notAfter)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := NewEd25519OfflineSignerWithManifestChain("legacy-key", key, "https://example.test/api", issuedAt, notAfter, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyBytes := mustOfflineCanonical(t, legacy.RootManifestEnvelope())
	explicitBytes := mustOfflineCanonical(t, explicit.RootManifestEnvelope())
	if string(legacyBytes) != string(explicitBytes) || legacy.ManifestRevision() != 1 || explicit.ManifestRevision() != 1 {
		t.Fatalf("legacy root changed\nlegacy=%s\nexplicit=%s", legacyBytes, explicitBytes)
	}
}

func offlineSignerTestKey(fill byte) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = fill + byte(index)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func offlineSignerManifestKey(keyID string, publicKey ed25519.PublicKey, notBefore, notAfter, effective time.Time, status string) OfflineSignerKeyV1 {
	fingerprint := sha256.Sum256(publicKey)
	return OfflineSignerKeyV1{
		KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		Fingerprint: base64.RawURLEncoding.EncodeToString(fingerprint[:]),
		NotBefore:   notBefore, NotAfter: notAfter, StatusEffectiveAt: effective, Status: status,
	}
}

func signOfflineManifestForTest(t *testing.T, payload OfflineSignerManifestPayloadV1, keyID string, privateKey ed25519.PrivateKey) OfflineSignedEnvelope {
	t.Helper()
	canonical, err := offlineCanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	message := append(append([]byte(nil), OfflineSignerManifestDomain...), digest[:]...)
	return OfflineSignedEnvelope{Payload: canonical, SignerKeyID: keyID, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))}
}

func mustOfflineCanonical(t *testing.T, value any) json.RawMessage {
	t.Helper()
	canonical, err := offlineCanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
