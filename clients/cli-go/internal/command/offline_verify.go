package command

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/offline"
)

const (
	offlineAuthorizationDomain   = "edu-agent-offline-authorization-v1\n"
	offlinePackDomain            = "edu-agent-offline-pack-v1\n"
	offlinePrepareResponseDomain = "edu-agent-offline-prepare-response-v1\n"
	offlineSignerManifestDomain  = "edu-agent-signer-manifest-v1\n"
)

type offlineTrustedKey struct {
	keyID             string
	publicKey         ed25519.PublicKey
	notBefore         time.Time
	notAfter          time.Time
	statusEffectiveAt time.Time
	status            api.OfflineSignerKeyStatus
}

type offlineTrustRoot struct {
	canonicalEnvelope json.RawMessage
	envelope          api.OfflineSignerManifestEnvelope
	payloadCanonical  json.RawMessage
	manifestRevision  uint64
	manifestDigest    string
	issuedAt          time.Time
	keys              map[string]offlineTrustedKey
	activeKeyID       string
}

func loadOfflineTrust(value config.Config) (offlineTrustRoot, error) {
	if value.Offline == nil {
		return offlineTrustRoot{}, errors.New("paired server did not provide an offline trust root")
	}
	return loadOfflineTrustEnvelope(value.Offline.SignerManifest, value, true)
}

func loadOfflineTrustCheckpoint(raw json.RawMessage, value config.Config) (offlineTrustRoot, error) {
	return loadOfflineTrustEnvelope(raw, value, false)
}

func loadOfflineTrustEnvelope(raw json.RawMessage, value config.Config, requireRoot bool) (offlineTrustRoot, error) {
	canonical, err := offline.CanonicalizeJCS(raw)
	if err != nil {
		return offlineTrustRoot{}, errors.New("offline trust checkpoint is not canonical JSON")
	}
	var envelope api.OfflineSignerManifestEnvelope
	if err := decodeClosedJSON(canonical, &envelope); err != nil {
		return offlineTrustRoot{}, errors.New("offline trust checkpoint is not a closed manifest envelope")
	}
	payloadBytes, err := canonicalJSON(envelope.Payload)
	if err != nil {
		return offlineTrustRoot{}, err
	}
	payload := envelope.Payload
	manifestOrigin, err := config.ValidateServerURL(payload.ServerBaseURL, value.AllowInsecureHTTP)
	revision, revisionErr := parseOfflineUint63(payload.ManifestRevision, true)
	issuedAt, issuedErr := parseOfflineTime(payload.IssuedAt)
	if err != nil || revisionErr != nil || issuedErr != nil || payload.ProtocolVersion != 1 || payload.Issuer != "edu-agent" || manifestOrigin != value.ServerURL || !validOfflineDigest(payload.PreviousManifestDigest) || len(payload.Keys) == 0 || len(payload.Keys) > 16 {
		return offlineTrustRoot{}, errors.New("offline trust checkpoint metadata is invalid")
	}
	keys := make(map[string]offlineTrustedKey, len(payload.Keys))
	activeKeyID := ""
	for _, value := range payload.Keys {
		if _, exists := keys[value.KeyID]; exists {
			return offlineTrustRoot{}, errors.New("offline trust checkpoint contains a duplicate key")
		}
		key, keyErr := decodeOfflineTrustedKey(value)
		if keyErr != nil {
			return offlineTrustRoot{}, keyErr
		}
		if key.status == api.OfflineSignerKeyActive {
			if activeKeyID != "" || issuedAt.Before(key.notBefore) || !issuedAt.Before(key.notAfter) || issuedAt.Before(key.statusEffectiveAt) {
				return offlineTrustRoot{}, errors.New("offline trust checkpoint active key is invalid")
			}
			activeKeyID = key.keyID
		}
		keys[key.keyID] = key
	}
	if activeKeyID == "" {
		return offlineTrustRoot{}, errors.New("offline trust checkpoint has no active key")
	}
	if _, ok := keys[envelope.SignerKeyID]; !ok {
		return offlineTrustRoot{}, errors.New("offline manifest signer is absent from its key set")
	}
	if requireRoot {
		zeroDigest := base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))
		rootKey, ok := keys[envelope.SignerKeyID]
		if revision != 1 || payload.PreviousManifestDigest != zeroDigest || !ok || rootKey.status != api.OfflineSignerKeyActive {
			return offlineTrustRoot{}, errors.New("offline pairing trust is not a revision-1 root")
		}
		if err := verifyOfflineSignature(offlineSignerManifestDomain, payloadBytes, envelope.SignerKeyID, envelope.Signature, rootKey.keyID, rootKey.publicKey); err != nil {
			return offlineTrustRoot{}, err
		}
	}
	digest := sha256.Sum256(payloadBytes)
	return offlineTrustRoot{
		canonicalEnvelope: canonical, envelope: envelope, payloadCanonical: payloadBytes,
		manifestRevision: revision, manifestDigest: base64.RawURLEncoding.EncodeToString(digest[:]),
		issuedAt: issuedAt, keys: keys, activeKeyID: activeKeyID,
	}, nil
}

func decodeOfflineTrustedKey(value api.OfflineSignerKey) (offlineTrustedKey, error) {
	if value.KeyID == "" || len(value.KeyID) > 128 {
		return offlineTrustedKey{}, errors.New("offline trust key ID is invalid")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(value.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(publicKey) != value.PublicKey {
		return offlineTrustedKey{}, errors.New("offline trust public key is invalid")
	}
	fingerprint := sha256.Sum256(publicKey)
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(fingerprint[:])), []byte(value.Fingerprint)) != 1 {
		return offlineTrustedKey{}, errors.New("offline trust public key fingerprint does not match")
	}
	notBefore, err := parseOfflineTime(value.NotBefore)
	if err != nil {
		return offlineTrustedKey{}, err
	}
	notAfter, err := parseOfflineTime(value.NotAfter)
	if err != nil || !notAfter.After(notBefore) {
		return offlineTrustedKey{}, errors.New("offline trust key validity window is invalid")
	}
	effective, err := parseOfflineTime(value.StatusEffectiveAt)
	if err != nil || effective.Before(notBefore) || effective.After(notAfter) {
		return offlineTrustedKey{}, errors.New("offline trust key status time is invalid")
	}
	switch value.Status {
	case api.OfflineSignerKeyActive, api.OfflineSignerKeyVerifyOnly, api.OfflineSignerKeyRetired:
	default:
		return offlineTrustedKey{}, errors.New("offline trust key status is invalid")
	}
	return offlineTrustedKey{
		keyID: value.KeyID, publicKey: append(ed25519.PublicKey(nil), publicKey...),
		notBefore: notBefore, notAfter: notAfter, statusEffectiveAt: effective, status: value.Status,
	}, nil
}

func sameOfflineTrust(left, right offlineTrustRoot) bool {
	return left.manifestRevision == right.manifestRevision && left.manifestDigest == right.manifestDigest && bytes.Equal(left.canonicalEnvelope, right.canonicalEnvelope)
}

// alignOfflinePrepareReplay trims only a duplicated chain prefix that reaches
// the exact durable local checkpoint. It is used for legacy prepare intents
// that predate storing the full verification checkpoint.
func alignOfflinePrepareReplay(value config.Config, request api.OfflinePrepareRequest, current offlineTrustRoot, chain []api.OfflineSignerManifestEnvelope) ([]api.OfflineSignerManifestEnvelope, error) {
	requestRevision, err := parseOfflineUint63(request.TrustedManifestRevision, true)
	if err != nil || !validOfflineDigest(request.TrustedManifestDigest) {
		return nil, errors.New("offline prepare intent trust checkpoint is invalid")
	}
	if requestRevision == current.manifestRevision {
		if request.TrustedManifestDigest != current.manifestDigest {
			return nil, errors.New("offline prepare intent forks from the trusted checkpoint")
		}
		return append([]api.OfflineSignerManifestEnvelope(nil), chain...), nil
	}
	if requestRevision > current.manifestRevision {
		return nil, errors.New("offline prepare intent trust checkpoint is ahead of local trust")
	}
	prefixLength := current.manifestRevision - requestRevision
	if uint64(len(chain)) < prefixLength {
		return nil, errors.New("offline signer manifest replay does not reach the trusted checkpoint")
	}
	expectedPrevious := request.TrustedManifestDigest
	var previousIssuedAt time.Time
	for index := uint64(0); index < prefixLength; index++ {
		canonical, err := canonicalJSON(chain[index])
		if err != nil {
			return nil, err
		}
		checkpoint, err := loadOfflineTrustEnvelope(canonical, value, false)
		if err != nil {
			return nil, err
		}
		if checkpoint.manifestRevision != requestRevision+index+1 || checkpoint.envelope.Payload.PreviousManifestDigest != expectedPrevious {
			return nil, errors.New("offline signer manifest replay has a rollback, gap, or fork")
		}
		if index != 0 && checkpoint.issuedAt.Before(previousIssuedAt) {
			return nil, errors.New("offline signer manifest replay time rolled back")
		}
		expectedPrevious = checkpoint.manifestDigest
		previousIssuedAt = checkpoint.issuedAt
		if index == prefixLength-1 && !sameOfflineTrust(checkpoint, current) {
			return nil, errors.New("offline signer manifest replay does not match the trusted checkpoint")
		}
	}
	return append([]api.OfflineSignerManifestEnvelope(nil), chain[prefixLength:]...), nil
}

func offlineTrustOnVerifiedPath(value config.Config, base, checkpoint offlineTrustRoot, chain []api.OfflineSignerManifestEnvelope) (bool, error) {
	if sameOfflineTrust(base, checkpoint) {
		return true, nil
	}
	for index := range chain {
		canonical, err := canonicalJSON(chain[index])
		if err != nil {
			return false, err
		}
		candidate, err := loadOfflineTrustEnvelope(canonical, value, false)
		if err != nil {
			return false, err
		}
		if sameOfflineTrust(candidate, checkpoint) {
			return true, nil
		}
	}
	return false, nil
}

func advanceOfflineTrust(value config.Config, trust offlineTrustRoot, chain []api.OfflineSignerManifestEnvelope) (offlineTrustRoot, error) {
	current := trust
	for index := range chain {
		nextBytes, err := canonicalJSON(chain[index])
		if err != nil {
			return offlineTrustRoot{}, err
		}
		next, err := loadOfflineTrustEnvelope(nextBytes, value, false)
		if err != nil {
			return offlineTrustRoot{}, err
		}
		if next.manifestRevision != current.manifestRevision+1 {
			return offlineTrustRoot{}, errors.New("offline signer manifest chain has a rollback or gap")
		}
		if next.envelope.Payload.PreviousManifestDigest != current.manifestDigest {
			return offlineTrustRoot{}, errors.New("offline signer manifest chain forks from the trusted checkpoint")
		}
		if next.issuedAt.Before(current.issuedAt) {
			return offlineTrustRoot{}, errors.New("offline signer manifest time rolled back")
		}
		signingKey, ok := current.keys[next.envelope.SignerKeyID]
		if !ok || signingKey.status != api.OfflineSignerKeyActive || !signingKey.canSign(next.issuedAt) {
			return offlineTrustRoot{}, errors.New("offline signer manifest uses an unknown or inactive signer")
		}
		if err := verifyOfflineSignature(offlineSignerManifestDomain, next.payloadCanonical, next.envelope.SignerKeyID, next.envelope.Signature, signingKey.keyID, signingKey.publicKey); err != nil {
			return offlineTrustRoot{}, err
		}
		current = next
	}
	return current, nil
}

func (k offlineTrustedKey) canSign(at time.Time) bool {
	return k.status == api.OfflineSignerKeyActive && !at.Before(k.notBefore) && at.Before(k.notAfter) && !at.Before(k.statusEffectiveAt)
}

func (k offlineTrustedKey) canVerifyArtifact(at time.Time) bool {
	if at.Before(k.notBefore) || !at.Before(k.notAfter) {
		return false
	}
	if k.status == api.OfflineSignerKeyActive {
		return !at.Before(k.statusEffectiveAt)
	}
	return at.Before(k.statusEffectiveAt)
}

func verifyPreparedPack(response api.OfflinePrepareResponse, request api.OfflinePrepareRequest, value config.Config, trust offlineTrustRoot) (json.RawMessage, offlineTrustRoot, error) {
	nextTrust, err := advanceOfflineTrust(value, trust, response.ManifestChain)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	requestBytes, err := canonicalJSON(request)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	requestDigest := sha256.Sum256(requestBytes)
	packBytes, err := canonicalJSON(response.Pack)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	packDigest := sha256.Sum256(packBytes)
	responsePayloadBytes, err := canonicalJSON(response.ResponseSignature.Payload)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	signaturePayload := response.ResponseSignature.Payload
	manifestRevision, revisionErr := parseOfflineUint63(signaturePayload.ManifestRevision, true)
	if revisionErr != nil || signaturePayload.ProtocolVersion != 1 || signaturePayload.OperationID != request.OperationID || signaturePayload.RequestHash != base64.RawURLEncoding.EncodeToString(requestDigest[:]) || signaturePayload.Replayed != response.Replayed || signaturePayload.PackDigest != base64.RawURLEncoding.EncodeToString(packDigest[:]) || manifestRevision != nextTrust.manifestRevision || signaturePayload.ManifestDigest != nextTrust.manifestDigest {
		return nil, offlineTrustRoot{}, errors.New("offline prepare response signature payload does not bind the request, pack, and trust checkpoint")
	}
	responseAt, err := parseOfflineTime(signaturePayload.ResponseAt)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	responseKey, ok := nextTrust.keys[response.ResponseSignature.SignerKeyID]
	if !ok || responseKey.keyID != nextTrust.activeKeyID || !responseKey.canSign(responseAt) {
		return nil, offlineTrustRoot{}, errors.New("offline prepare response uses an unknown or inactive signer")
	}
	if err := verifyOfflineSignature(offlinePrepareResponseDomain, responsePayloadBytes, response.ResponseSignature.SignerKeyID, response.ResponseSignature.Signature, responseKey.keyID, responseKey.publicKey); err != nil {
		return nil, offlineTrustRoot{}, err
	}
	packIssuedAt, err := parseOfflineTime(response.Pack.Payload.IssuedAt)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	packKey, ok := nextTrust.keys[response.Pack.SignerKeyID]
	if !ok || !packKey.canVerifyArtifact(packIssuedAt) {
		return nil, offlineTrustRoot{}, errors.New("offline pack uses an unknown, backdated, or expired signer")
	}
	packPayloadBytes, err := canonicalJSON(response.Pack.Payload)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	if err := verifyOfflineSignature(offlinePackDomain, packPayloadBytes, response.Pack.SignerKeyID, response.Pack.Signature, packKey.keyID, packKey.publicKey); err != nil {
		return nil, offlineTrustRoot{}, err
	}
	pack := response.Pack.Payload
	if pack.ProtocolVersion != 1 || pack.DeviceID != value.DeviceID || string(pack.LearnerGeneration) != value.Offline.LearnerGeneration || len(pack.Items) == 0 || len(pack.Items) > 20 {
		return nil, offlineTrustRoot{}, errors.New("offline pack binding is invalid")
	}
	originDigest := sha256.Sum256([]byte(nextTrust.envelope.Payload.ServerBaseURL))
	encodedOriginDigest := base64.RawURLEncoding.EncodeToString(originDigest[:])
	for index := range pack.Items {
		item := pack.Items[index]
		activityBytes, err := canonicalJSON(item.Activity)
		if err != nil {
			return nil, offlineTrustRoot{}, err
		}
		activityDigest := sha256.Sum256(activityBytes)
		if item.ActivityPayloadDigest != base64.RawURLEncoding.EncodeToString(activityDigest[:]) {
			return nil, offlineTrustRoot{}, fmt.Errorf("offline pack item %d activity digest does not match", index)
		}
		authorizationBytes, err := canonicalJSON(item.Authorization.Payload)
		if err != nil {
			return nil, offlineTrustRoot{}, err
		}
		authorizationKey, ok := nextTrust.keys[item.Authorization.SignerKeyID]
		if !ok || !authorizationKey.canVerifyArtifact(packIssuedAt) {
			return nil, offlineTrustRoot{}, fmt.Errorf("offline pack item %d uses an unknown signer", index)
		}
		if err := verifyOfflineSignature(offlineAuthorizationDomain, authorizationBytes, item.Authorization.SignerKeyID, item.Authorization.Signature, authorizationKey.keyID, authorizationKey.publicKey); err != nil {
			return nil, offlineTrustRoot{}, err
		}
		authorization := item.Authorization.Payload
		if authorization.ProtocolVersion != 1 || authorization.Format != "offline-authorization-v1" || authorization.Issuer != "edu-agent" || authorization.SignerKeyID != item.Authorization.SignerKeyID || item.Authorization.SignerKeyID != response.Pack.SignerKeyID || authorization.PackID != pack.PackID || authorization.DeviceID != value.DeviceID || string(authorization.LearnerGeneration) != value.Offline.LearnerGeneration || authorization.ServerOriginDigest != encodedOriginDigest || authorization.OfflineActivityID != item.Activity.ActivityID || authorization.ActivityPayloadDigest != item.ActivityPayloadDigest || authorization.EligibleUntil != pack.EligibleUntil || authorization.ArchiveUntil != pack.ArchiveUntil {
			return nil, offlineTrustRoot{}, fmt.Errorf("offline pack item %d authorization binding is invalid", index)
		}
	}
	return packBytes, nextTrust, nil
}

func verifyOfflineSignature(domain string, payload []byte, signerKeyID, encodedSignature, trustedKeyID string, publicKey ed25519.PublicKey) error {
	if signerKeyID != trustedKeyID {
		return errors.New("offline signature uses an untrusted key")
	}
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != encodedSignature {
		return errors.New("offline signature encoding is invalid")
	}
	digest := sha256.Sum256(payload)
	message := make([]byte, 0, len(domain)+len(digest))
	message = append(message, domain...)
	message = append(message, digest[:]...)
	if !ed25519.Verify(publicKey, message, signature) {
		return errors.New("offline signature verification failed")
	}
	return nil
}

func validOfflineDigest(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func parseOfflineUint63(value api.Uint63Decimal, positive bool) (uint64, error) {
	text := string(value)
	if text == "" || (len(text) > 1 && text[0] == '0') {
		return 0, errors.New("offline decimal is not canonical")
	}
	parsed, err := strconv.ParseUint(text, 10, 63)
	if err != nil || (positive && parsed == 0) {
		return 0, errors.New("offline decimal is outside the supported range")
	}
	return parsed, nil
}

func canonicalJSON(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := offline.CanonicalizeJCS(encoded)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func decodeClosedJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected JSON value")
	}
	return nil
}

func parseOfflineTime(value api.OfflineTimestamp) (time.Time, error) {
	text := string(value)
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != text {
		return time.Time{}, errors.New("offline timestamp is not canonical UTC")
	}
	return parsed, nil
}
