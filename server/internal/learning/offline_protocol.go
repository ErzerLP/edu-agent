package learning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	OfflineAuthorizationDomain   = "edu-agent-offline-authorization-v1\n"
	OfflinePackDomain            = "edu-agent-offline-pack-v1\n"
	OfflinePrepareResponseDomain = "edu-agent-offline-prepare-response-v1\n"
	OfflineSignerManifestDomain  = "edu-agent-signer-manifest-v1\n"

	OfflineDefaultPackCount = 5
	OfflineMaxPackCount     = 20
	OfflineDefaultTTL       = 72 * time.Hour
	OfflineMinimumTTL       = 15 * time.Minute
	OfflineMaximumTTL       = 7 * 24 * time.Hour
	OfflineArchiveExtension = 30 * 24 * time.Hour
	OfflineMaxSyncItems     = 50
)

var OfflineZeroDigest = base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))

type OfflineSignedEnvelope struct {
	Payload     json.RawMessage `json:"payload"`
	SignerKeyID string          `json:"signer_key_id"`
	Signature   string          `json:"signature"`
}

type OfflineSignerKeyV1 struct {
	KeyID             string    `json:"key_id"`
	PublicKey         string    `json:"public_key"`
	Fingerprint       string    `json:"fingerprint"`
	NotBefore         time.Time `json:"not_before"`
	NotAfter          time.Time `json:"not_after"`
	StatusEffectiveAt time.Time `json:"status_effective_at"`
	Status            string    `json:"status"`
}

type OfflinePairingBootstrap struct {
	ProtocolVersion   int                   `json:"protocol_version"`
	LearnerGeneration string                `json:"learner_generation"`
	ServerBaseURL     string                `json:"server_base_url"`
	SignerManifest    OfflineSignedEnvelope `json:"signer_manifest"`
}

type OfflineSignerManifestPayloadV1 struct {
	ProtocolVersion  int                  `json:"protocol_version"`
	ManifestRevision string               `json:"manifest_revision"`
	Issuer           string               `json:"issuer"`
	ServerBaseURL    string               `json:"server_base_url"`
	PreviousDigest   string               `json:"previous_manifest_digest"`
	IssuedAt         time.Time            `json:"issued_at"`
	Keys             []OfflineSignerKeyV1 `json:"keys"`
}

type OfflineSigner interface {
	KeyID() string
	Origin() string
	ManifestRevision() uint64
	ManifestDigest() string
	RootManifestEnvelope() OfflineSignedEnvelope
	ManifestEnvelope() OfflineSignedEnvelope
	ManifestChain(uint64, string) ([]OfflineSignedEnvelope, error)
	Sign(string, any) (OfflineSignedEnvelope, error)
}

type Ed25519OfflineSigner struct {
	keyID           string
	origin          string
	privateKey      ed25519.PrivateKey
	manifests       []OfflineSignedEnvelope
	manifestPayload []OfflineSignerManifestPayloadV1
	manifestDigests []string
}

func NewEd25519OfflineSigner(keyID string, privateKey ed25519.PrivateKey, rawOrigin string, issuedAt, notAfter time.Time) (*Ed25519OfflineSigner, error) {
	return NewEd25519OfflineSignerWithManifestChain(keyID, privateKey, rawOrigin, issuedAt, notAfter, nil)
}

func NewEd25519OfflineSignerWithManifestChain(keyID string, privateKey ed25519.PrivateKey, rawOrigin string, issuedAt, notAfter time.Time, configured []json.RawMessage) (*Ed25519OfflineSigner, error) {
	origin, err := normalizeOfflineOrigin(rawOrigin)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(keyID) == "" || len(keyID) > 128 || strings.TrimSpace(keyID) != keyID {
		return nil, errors.New("offline signer key ID is invalid")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("offline signer private key must contain 64 bytes")
	}
	issuedAt = issuedAt.UTC().Truncate(time.Microsecond)
	notAfter = notAfter.UTC().Truncate(time.Microsecond)
	if issuedAt.IsZero() || !notAfter.After(issuedAt) {
		return nil, errors.New("offline signer validity window is invalid")
	}
	signer := &Ed25519OfflineSigner{keyID: keyID, origin: origin, privateKey: append(ed25519.PrivateKey(nil), privateKey...)}
	if len(configured) == 0 {
		publicKey := privateKey.Public().(ed25519.PublicKey)
		fingerprint := sha256.Sum256(publicKey)
		manifestPayload := OfflineSignerManifestPayloadV1{
			ProtocolVersion:  1,
			ManifestRevision: "1",
			Issuer:           "edu-agent",
			ServerBaseURL:    origin,
			PreviousDigest:   OfflineZeroDigest,
			IssuedAt:         issuedAt,
			Keys: []OfflineSignerKeyV1{{
				KeyID:             keyID,
				PublicKey:         base64.RawURLEncoding.EncodeToString(publicKey),
				Fingerprint:       base64.RawURLEncoding.EncodeToString(fingerprint[:]),
				NotBefore:         issuedAt,
				NotAfter:          notAfter,
				StatusEffectiveAt: issuedAt,
				Status:            "active",
			}},
		}
		manifest, signErr := signer.Sign(OfflineSignerManifestDomain, manifestPayload)
		if signErr != nil {
			return nil, signErr
		}
		signer.manifests = []OfflineSignedEnvelope{manifest}
		signer.manifestPayload = []OfflineSignerManifestPayloadV1{manifestPayload}
		signer.manifestDigests = []string{offlineBase64Digest(manifest.Payload)}
		return signer, nil
	}
	if err := signer.loadManifestChain(configured, issuedAt, notAfter); err != nil {
		return nil, err
	}
	return signer, nil
}

func (s *Ed25519OfflineSigner) loadManifestChain(configured []json.RawMessage, issuedAt, notAfter time.Time) error {
	if len(configured) > 16 {
		return errors.New("offline signer manifest chain is too long")
	}
	for index, raw := range configured {
		envelope, payload, err := decodeOfflineManifestEnvelope(raw)
		if err != nil {
			return fmt.Errorf("offline signer manifest revision %d is invalid: %w", index+1, err)
		}
		revision, err := ParseUint63Decimal(payload.ManifestRevision)
		if err != nil || revision != uint64(index+1) || payload.ProtocolVersion != 1 || payload.Issuer != "edu-agent" || payload.ServerBaseURL != s.origin {
			return fmt.Errorf("offline signer manifest revision %d metadata is invalid", index+1)
		}
		if err := validateOfflineManifestKeys(payload); err != nil {
			return fmt.Errorf("offline signer manifest revision %d keys are invalid: %w", index+1, err)
		}
		if index == 0 {
			if payload.PreviousDigest != OfflineZeroDigest {
				return errors.New("offline signer root previous digest is invalid")
			}
			rootKey, ok := offlineManifestKey(payload, envelope.SignerKeyID)
			if !ok || rootKey.Status != "active" || !verifyOfflineManifestSignature(envelope, rootKey) {
				return errors.New("offline signer root signature is invalid")
			}
		} else {
			if payload.PreviousDigest != s.manifestDigests[index-1] {
				return fmt.Errorf("offline signer manifest revision %d previous digest is invalid", index+1)
			}
			previousKey, ok := offlineManifestKey(s.manifestPayload[index-1], envelope.SignerKeyID)
			if !ok || previousKey.Status != "active" || payload.IssuedAt.Before(previousKey.NotBefore) || !payload.IssuedAt.Before(previousKey.NotAfter) || payload.IssuedAt.Before(previousKey.StatusEffectiveAt) || !verifyOfflineManifestSignature(envelope, previousKey) {
				return fmt.Errorf("offline signer manifest revision %d continuity signature is invalid", index+1)
			}
		}
		s.manifests = append(s.manifests, envelope)
		s.manifestPayload = append(s.manifestPayload, payload)
		s.manifestDigests = append(s.manifestDigests, offlineBase64Digest(envelope.Payload))
	}
	current := s.manifestPayload[len(s.manifestPayload)-1]
	active, ok := offlineManifestKey(current, s.keyID)
	publicKey := s.privateKey.Public().(ed25519.PublicKey)
	if !ok || active.Status != "active" || active.NotBefore != issuedAt || active.NotAfter != notAfter || active.PublicKey != base64.RawURLEncoding.EncodeToString(publicKey) {
		return errors.New("offline signer private key does not match the current active manifest key")
	}
	return nil
}

func decodeOfflineManifestEnvelope(raw json.RawMessage) (OfflineSignedEnvelope, OfflineSignerManifestPayloadV1, error) {
	canonical, err := CanonicalizeJCS(raw)
	if err != nil {
		return OfflineSignedEnvelope{}, OfflineSignerManifestPayloadV1{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var envelope OfflineSignedEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return envelope, OfflineSignerManifestPayloadV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return envelope, OfflineSignerManifestPayloadV1{}, errors.New("multiple manifest JSON values")
	}
	payloadCanonical, err := CanonicalizeJCS(envelope.Payload)
	if err != nil {
		return envelope, OfflineSignerManifestPayloadV1{}, err
	}
	envelope.Payload = payloadCanonical
	payloadDecoder := json.NewDecoder(bytes.NewReader(payloadCanonical))
	payloadDecoder.DisallowUnknownFields()
	var payload OfflineSignerManifestPayloadV1
	if err := payloadDecoder.Decode(&payload); err != nil {
		return envelope, payload, err
	}
	if err := payloadDecoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return envelope, payload, errors.New("multiple manifest payload values")
	}
	if envelope.SignerKeyID == "" || !validOfflineSignature(envelope.Signature) {
		return envelope, payload, errors.New("invalid manifest envelope")
	}
	return envelope, payload, nil
}

func validateOfflineManifestKeys(payload OfflineSignerManifestPayloadV1) error {
	if payload.IssuedAt.IsZero() || len(payload.Keys) == 0 || len(payload.Keys) > 16 || !validOfflineDigest(payload.PreviousDigest) {
		return errors.New("invalid manifest payload")
	}
	seen := make(map[string]bool, len(payload.Keys))
	active := 0
	for _, key := range payload.Keys {
		publicKey, err := base64.RawURLEncoding.DecodeString(key.PublicKey)
		fingerprint, fingerprintErr := base64.RawURLEncoding.DecodeString(key.Fingerprint)
		expectedFingerprint := sha256.Sum256(publicKey)
		if key.KeyID == "" || len(key.KeyID) > 128 || strings.TrimSpace(key.KeyID) != key.KeyID || seen[key.KeyID] || err != nil || len(publicKey) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(publicKey) != key.PublicKey || fingerprintErr != nil || len(fingerprint) != sha256.Size || base64.RawURLEncoding.EncodeToString(fingerprint) != key.Fingerprint || !bytes.Equal(fingerprint, expectedFingerprint[:]) || key.NotBefore.IsZero() || !key.NotAfter.After(key.NotBefore) || key.StatusEffectiveAt.Before(key.NotBefore) || key.StatusEffectiveAt.After(key.NotAfter) {
			return errors.New("invalid manifest key")
		}
		switch key.Status {
		case "active":
			active++
			if payload.IssuedAt.Before(key.NotBefore) || !payload.IssuedAt.Before(key.NotAfter) || payload.IssuedAt.Before(key.StatusEffectiveAt) {
				return errors.New("active manifest key is outside its validity window")
			}
		case "verify_only", "retired":
		default:
			return errors.New("invalid manifest key status")
		}
		seen[key.KeyID] = true
	}
	if active != 1 {
		return errors.New("manifest must contain exactly one active key")
	}
	return nil
}

func offlineManifestKey(payload OfflineSignerManifestPayloadV1, keyID string) (OfflineSignerKeyV1, bool) {
	for _, key := range payload.Keys {
		if key.KeyID == keyID {
			return key, true
		}
	}
	return OfflineSignerKeyV1{}, false
}

func verifyOfflineManifestSignature(envelope OfflineSignedEnvelope, key OfflineSignerKeyV1) bool {
	publicKey, err := base64.RawURLEncoding.DecodeString(key.PublicKey)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || signatureErr != nil || len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize {
		return false
	}
	digest := sha256.Sum256(envelope.Payload)
	message := append(append([]byte(nil), OfflineSignerManifestDomain...), digest[:]...)
	return ed25519.Verify(ed25519.PublicKey(publicKey), message, signature)
}

func validOfflineSignature(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == ed25519.SignatureSize && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func (s *Ed25519OfflineSigner) KeyID() string  { return s.keyID }
func (s *Ed25519OfflineSigner) Origin() string { return s.origin }
func (s *Ed25519OfflineSigner) ManifestRevision() uint64 {
	return uint64(len(s.manifests))
}
func (s *Ed25519OfflineSigner) ManifestDigest() string {
	if len(s.manifestDigests) == 0 {
		return ""
	}
	return s.manifestDigests[len(s.manifestDigests)-1]
}
func (s *Ed25519OfflineSigner) RootManifestEnvelope() OfflineSignedEnvelope {
	if len(s.manifests) == 0 {
		return OfflineSignedEnvelope{}
	}
	return cloneOfflineEnvelope(s.manifests[0])
}
func (s *Ed25519OfflineSigner) ManifestEnvelope() OfflineSignedEnvelope {
	if len(s.manifests) == 0 {
		return OfflineSignedEnvelope{}
	}
	return cloneOfflineEnvelope(s.manifests[len(s.manifests)-1])
}

func (s *Ed25519OfflineSigner) ManifestChain(revision uint64, digest string) ([]OfflineSignedEnvelope, error) {
	if revision > uint64(len(s.manifests)) || !validOfflineDigest(digest) {
		return nil, &Error{Code: CodeOfflinePrepareUnavailable, Reason: "signer_manifest_conflict"}
	}
	if revision == 0 {
		if digest != OfflineZeroDigest {
			return nil, &Error{Code: CodeOfflinePrepareUnavailable, Reason: "signer_manifest_conflict"}
		}
	} else if s.manifestDigests[revision-1] != digest {
		return nil, &Error{Code: CodeOfflinePrepareUnavailable, Reason: "signer_manifest_conflict"}
	}
	chain := make([]OfflineSignedEnvelope, 0, len(s.manifests)-int(revision))
	for index := int(revision); index < len(s.manifests); index++ {
		chain = append(chain, cloneOfflineEnvelope(s.manifests[index]))
	}
	return chain, nil
}

func (s *Ed25519OfflineSigner) Sign(domain string, value any) (OfflineSignedEnvelope, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize || domain == "" {
		return OfflineSignedEnvelope{}, &Error{Code: CodeOfflineSignerUnavailable}
	}
	payload, err := offlineCanonicalJSON(value)
	if err != nil {
		return OfflineSignedEnvelope{}, err
	}
	digest := sha256.Sum256(payload)
	message := make([]byte, 0, len(domain)+len(digest))
	message = append(message, domain...)
	message = append(message, digest[:]...)
	signature := ed25519.Sign(s.privateKey, message)
	return OfflineSignedEnvelope{
		Payload:     payload,
		SignerKeyID: s.keyID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
	}, nil
}

func cloneOfflineEnvelope(value OfflineSignedEnvelope) OfflineSignedEnvelope {
	value.Payload = append(json.RawMessage(nil), value.Payload...)
	return value
}

func normalizeOfflineOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("offline server origin must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("offline server origin must not include credentials, query, or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	if (parsed.Scheme == "http" && strings.HasSuffix(parsed.Host, ":80")) || (parsed.Scheme == "https" && strings.HasSuffix(parsed.Host, ":443")) {
		parsed.Host = strings.TrimSuffix(strings.TrimSuffix(parsed.Host, ":80"), ":443")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	} else if parsed.Path != "/" {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}
	return parsed.String(), nil
}

func offlineCanonicalJSON(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalizeJCS(encoded)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func offlineBase64Digest(value []byte) string {
	digest := sha256.Sum256(value)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func validOfflineDigest(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

type OfflinePrepareRequest struct {
	OperationID             string `json:"operation_id"`
	PayloadSchemaVersion    int    `json:"payload_schema_version"`
	ExpectedSessionVersion  string `json:"expected_session_version"`
	TrustedManifestRevision string `json:"trusted_manifest_revision"`
	TrustedManifestDigest   string `json:"trusted_manifest_digest"`
	RequestedCount          *int   `json:"requested_count,omitempty"`
	RequestedTTLSeconds     *int   `json:"requested_ttl_seconds,omitempty"`
}

func (r OfflinePrepareRequest) Validate() error {
	if uuid.Validate(r.OperationID) != nil || strings.ToLower(r.OperationID) != r.OperationID || r.PayloadSchemaVersion != 1 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_prepare_request"}
	}
	if _, err := ParseUint63Decimal(r.ExpectedSessionVersion); err != nil {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_expected_session_version"}
	}
	if _, err := ParseUint63Decimal(r.TrustedManifestRevision); err != nil || !validOfflineDigest(r.TrustedManifestDigest) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_trusted_manifest"}
	}
	count, ttl := r.Limits()
	if count < 1 || count > OfflineMaxPackCount || ttl < OfflineMinimumTTL || ttl > OfflineMaximumTTL {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_prepare_limits"}
	}
	return nil
}

func (r OfflinePrepareRequest) Limits() (int, time.Duration) {
	count := OfflineDefaultPackCount
	if r.RequestedCount != nil {
		count = *r.RequestedCount
	}
	ttl := OfflineDefaultTTL
	if r.RequestedTTLSeconds != nil {
		ttl = time.Duration(*r.RequestedTTLSeconds) * time.Second
	}
	return count, ttl
}

func (r OfflinePrepareRequest) CanonicalHash() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	payload, err := offlineCanonicalJSON(r)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

type OfflineAuthorizationPayloadV1 struct {
	ProtocolVersion       int       `json:"protocol_version"`
	Format                string    `json:"format"`
	Issuer                string    `json:"issuer"`
	SignerKeyID           string    `json:"signer_key_id"`
	PackID                string    `json:"pack_id"`
	DeviceID              string    `json:"device_id"`
	CredentialEpoch       string    `json:"credential_epoch"`
	LearnerGeneration     string    `json:"learner_generation"`
	ServerOriginDigest    string    `json:"server_origin_digest"`
	OfflineActivityID     string    `json:"offline_activity_id"`
	ActivityRevision      string    `json:"activity_revision"`
	SubmissionID          string    `json:"submission_id"`
	OperationID           string    `json:"operation_id"`
	DeviceSequence        string    `json:"device_seq"`
	ExpectedVersion       string    `json:"expected_version"`
	ActivityPayloadDigest string    `json:"activity_payload_digest"`
	EligibleUntil         time.Time `json:"eligible_until"`
	ArchiveUntil          time.Time `json:"archive_until"`
}

func BindOfflineAuthorization(operation *OfflineOperation, expectedDeviceID, normalizedOrigin string) (OfflineAuthorizationPayloadV1, error) {
	if operation == nil || uuid.Validate(expectedDeviceID) != nil || operation.DeviceID != expectedDeviceID {
		return OfflineAuthorizationPayloadV1{}, &Error{Code: CodeInvalidRequest, Reason: "offline_device_mismatch"}
	}
	decoder := json.NewDecoder(strings.NewReader(string(operation.Authorization)))
	decoder.DisallowUnknownFields()
	var authorization OfflineAuthorizationPayloadV1
	if err := decoder.Decode(&authorization); err != nil {
		return authorization, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_authorization", Cause: err}
	}
	credentialEpoch, err := ParseUint63Decimal(authorization.CredentialEpoch)
	if err != nil || credentialEpoch == 0 {
		return authorization, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_authorization"}
	}
	learnerGeneration, err := ParseUint63Decimal(authorization.LearnerGeneration)
	if err != nil || learnerGeneration == 0 {
		return authorization, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_authorization"}
	}
	deviceSequence, sequenceErr := ParseUint63Decimal(authorization.DeviceSequence)
	expectedVersion, versionErr := ParseUint63Decimal(authorization.ExpectedVersion)
	activityRevision, revisionErr := ParseUint63Decimal(authorization.ActivityRevision)
	origin, originErr := normalizeOfflineOrigin(normalizedOrigin)
	originDigest := offlineBase64Digest([]byte(origin))
	if sequenceErr != nil || versionErr != nil || revisionErr != nil || originErr != nil ||
		authorization.ProtocolVersion != 1 || authorization.Format != "offline-authorization-v1" ||
		authorization.Issuer != "edu-agent" || authorization.SignerKeyID == "" ||
		operation.DeviceID != authorization.DeviceID || operation.OperationID != authorization.OperationID ||
		operation.SubmissionID != authorization.SubmissionID || operation.DeviceSequence != deviceSequence ||
		operation.OfflineActivityID != authorization.OfflineActivityID || uint64(operation.ActivityRevision) != activityRevision ||
		uint64(operation.ExpectedVersion) != expectedVersion || authorization.ServerOriginDigest != originDigest ||
		!validOfflineDigest(authorization.ActivityPayloadDigest) || authorization.EligibleUntil.IsZero() ||
		authorization.ArchiveUntil.Before(authorization.EligibleUntil) {
		return authorization, &Error{Code: CodeInvalidRequest, Reason: "offline_authorization_mismatch"}
	}
	operation.CredentialEpoch = int64(credentialEpoch)
	operation.LearnerGeneration = int64(learnerGeneration)
	return authorization, nil
}

type OfflinePackItemV1 struct {
	Activity              Activity              `json:"activity"`
	ActivityPayloadDigest string                `json:"activity_payload_digest"`
	Authorization         OfflineSignedEnvelope `json:"authorization"`
}

type OfflinePackPayloadV1 struct {
	ProtocolVersion   int                 `json:"protocol_version"`
	PackID            string              `json:"pack_id"`
	Revision          string              `json:"revision"`
	DeviceID          string              `json:"device_id"`
	LearnerGeneration string              `json:"learner_generation"`
	ParentSessionID   string              `json:"parent_session_id"`
	IssuedAt          time.Time           `json:"issued_at"`
	EligibleUntil     time.Time           `json:"eligible_until"`
	ArchiveUntil      time.Time           `json:"archive_until"`
	Truncated         bool                `json:"truncated"`
	TruncatedReason   string              `json:"truncated_reason,omitempty"`
	Items             []OfflinePackItemV1 `json:"items"`
}

type OfflinePreparedPack struct {
	OperationID      string                `json:"operation_id"`
	RequestHash      string                `json:"request_hash"`
	Pack             OfflineSignedEnvelope `json:"pack"`
	PackDigest       string                `json:"pack_digest"`
	ManifestRevision string                `json:"manifest_revision"`
	ManifestDigest   string                `json:"manifest_digest"`
	Replayed         bool                  `json:"replayed"`
}

type OfflinePrepareResponseSignaturePayloadV1 struct {
	ProtocolVersion  int       `json:"protocol_version"`
	OperationID      string    `json:"operation_id"`
	RequestHash      string    `json:"request_hash"`
	Replayed         bool      `json:"replayed"`
	PackDigest       string    `json:"pack_digest"`
	ManifestRevision string    `json:"manifest_revision"`
	ManifestDigest   string    `json:"manifest_digest"`
	ResponseAt       time.Time `json:"response_at"`
}

type OfflinePrepareResponse struct {
	OperationID       string                  `json:"operation_id"`
	Replayed          bool                    `json:"replayed"`
	Pack              OfflineSignedEnvelope   `json:"pack"`
	ManifestChain     []OfflineSignedEnvelope `json:"manifest_chain"`
	ResponseSignature OfflineSignedEnvelope   `json:"response_signature"`
}

type OfflinePrepareStoreRequest struct {
	DeviceID string
	Request  OfflinePrepareRequest
	Count    int
	TTL      time.Duration
}

type OfflinePrepareClaim struct {
	State      string
	LeaseToken string
	Prepared   *OfflinePreparedPack
	Generation *OfflinePrepareGenerationRequest
	Artifact   *OfflinePrepareArtifact
}

type OfflinePrepareGenerationRequest struct {
	DeviceID               string        `json:"device_id"`
	OperationID            string        `json:"operation_id"`
	Count                  int           `json:"count"`
	SessionID              string        `json:"session_id"`
	SessionState           string        `json:"session_state"`
	ExpectedSessionVersion int64         `json:"expected_session_version"`
	GoalRevisionID         string        `json:"goal_revision_id"`
	Route                  RouteRevision `json:"route"`
	RouteStepID            string        `json:"route_step_id"`
	KnowledgeRevisionID    string        `json:"knowledge_revision_id"`
	CurrentActivity        *Activity     `json:"current_activity,omitempty"`
}

type OfflinePrepareArtifact struct {
	ProtocolVersion        int        `json:"protocol_version"`
	SessionID              string     `json:"session_id"`
	SessionState           string     `json:"session_state"`
	ExpectedSessionVersion int64      `json:"expected_session_version"`
	GoalRevisionID         string     `json:"goal_revision_id"`
	RouteRevisionID        string     `json:"route_revision_id"`
	RouteStepID            string     `json:"route_step_id"`
	KnowledgeRevisionID    string     `json:"knowledge_revision_id"`
	Activities             []Activity `json:"activities"`
	ModelPartial           bool       `json:"model_partial"`
}

type OfflinePrepareStore interface {
	ClaimOfflinePrepare(context.Context, OfflinePrepareStoreRequest) (OfflinePrepareClaim, error)
	StoreOfflinePrepareArtifact(context.Context, string, string, string, OfflinePrepareArtifact) error
	PublishOfflinePrepare(context.Context, OfflinePrepareStoreRequest, string, OfflineSigner) (OfflinePreparedPack, error)
	RejectOfflinePrepare(context.Context, string, string, string, error) error
}

type OfflinePrepareGenerator interface {
	GenerateOfflinePrepare(context.Context, OfflinePrepareGenerationRequest) (OfflinePrepareArtifact, error)
}

type OfflineSyncRequest struct {
	SyncRequestID        string            `json:"sync_request_id"`
	PayloadSchemaVersion int               `json:"payload_schema_version"`
	Operations           []json.RawMessage `json:"operations"`
}

func (r OfflineSyncRequest) Validate() error {
	if uuid.Validate(r.SyncRequestID) != nil || strings.ToLower(r.SyncRequestID) != r.SyncRequestID ||
		r.PayloadSchemaVersion != 1 || len(r.Operations) == 0 || len(r.Operations) > OfflineMaxSyncItems {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_sync_request"}
	}
	return nil
}

type OfflineSyncResponse struct {
	SyncRequestID string                `json:"sync_request_id"`
	Results       []OfflineIngestResult `json:"results"`
}

type OfflineOperationStatus struct {
	OperationID      string                  `json:"operation_id"`
	SubmissionID     string                  `json:"submission_id"`
	ArchiveStatus    OfflineArchiveStatus    `json:"archive_status"`
	AssessmentStatus OfflineAssessmentStatus `json:"assessment_status"`
	EvidenceStatus   OfflineEvidenceStatus   `json:"evidence_status"`
	ReasonCodes      []string                `json:"reason_codes"`
	Receipt          OfflineIngestReceipt    `json:"ingest_receipt"`
	StatusTicket     OfflineStatusTicket     `json:"status_ticket"`
	AssessmentID     string                  `json:"assessment_id,omitempty"`
	EvidenceID       string                  `json:"evidence_id,omitempty"`
}

func decodeOfflineRequestHash(value string) (string, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(value) != value {
		return "", fmt.Errorf("invalid offline request hash")
	}
	return base64.RawURLEncoding.EncodeToString(decoded), nil
}
