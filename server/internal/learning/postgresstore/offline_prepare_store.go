package postgresstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	offlinePrepareLeaseDuration = 2 * time.Minute
	offlineMaxActivityBytes     = 1 << 20
	offlineMaxPackBytes         = 8 << 20
	offlinePrepareClaimFormatV1 = "offline-prepare-claim-v1"
)

type offlinePrepareClaimRequestV1 struct {
	Format          string                                   `json:"format"`
	ProtocolVersion int                                      `json:"protocol_version"`
	Request         learning.OfflinePrepareRequest           `json:"request"`
	Generation      learning.OfflinePrepareGenerationRequest `json:"generation"`
}

type offlinePrepareAuthority struct {
	generation      int64
	credentialEpoch int64
	session         tutoring.Session
	route           learning.RouteRevision
	currentActivity *learning.Activity
}

type offlinePrepareRejected struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

type offlinePreparedItem struct {
	activity               learning.Activity
	activityPayload        []byte
	activityHash           [sha256.Size]byte
	authorization          learning.OfflineSignedEnvelope
	authorizationHash      [sha256.Size]byte
	authorizationSignature []byte
	submissionID           string
	operationID            string
	deviceSequence         int64
}

func (s *Store) ClaimOfflinePrepare(ctx context.Context, request learning.OfflinePrepareStoreRequest) (learning.OfflinePrepareClaim, error) {
	if err := validateOfflinePrepareStoreRequest(request); err != nil {
		return learning.OfflinePrepareClaim{}, err
	}
	requestHash, err := request.Request.CanonicalHash()
	if err != nil {
		return learning.OfflinePrepareClaim{}, err
	}
	requestHashBytes, _ := hex.DecodeString(requestHash)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite})
	if err != nil {
		return learning.OfflinePrepareClaim{}, fmt.Errorf("begin offline prepare claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockOfflinePrepareOperation(ctx, tx, request.DeviceID, request.Request.OperationID); err != nil {
		return learning.OfflinePrepareClaim{}, err
	}
	generation, err := s.lockOfflinePrepareGates(ctx, tx)
	if err != nil {
		return learning.OfflinePrepareClaim{}, err
	}
	credentialEpoch, err := lockOfflinePrepareCredential(ctx, tx, request.DeviceID)
	if err != nil {
		return learning.OfflinePrepareClaim{}, err
	}
	var storedHash []byte
	var storedGeneration int64
	var status string
	var leaseToken *string
	var leaseExpires *time.Time
	var requestBody, artifactBody, resultBody []byte
	readErr := tx.QueryRow(ctx, `
		SELECT request_hash,learner_generation,status,lease_token::text,lease_expires_at,
		       request_body,model_artifact,result_body
		FROM offline_prepare_claims
		WHERE device_id=$1 AND operation_id=$2
		FOR UPDATE`, request.DeviceID, request.Request.OperationID).Scan(
		&storedHash, &storedGeneration, &status, &leaseToken, &leaseExpires, &requestBody,
		&artifactBody, &resultBody)
	if readErr == nil {
		if hex.EncodeToString(storedHash) != requestHash {
			return learning.OfflinePrepareClaim{}, &learning.Error{Code: learning.CodeIdempotencyConflict, Reason: learning.OfflineReasonIdempotencyConflict}
		}
		if storedGeneration != generation {
			return learning.OfflinePrepareClaim{}, &learning.Error{Code: learning.CodeContentRedacted, Reason: "offline_prepare_generation_changed"}
		}
		switch status {
		case "published":
			prepared, decodeErr := decodeStoredOfflinePrepared(resultBody)
			if decodeErr != nil {
				return learning.OfflinePrepareClaim{}, decodeErr
			}
			prepared.Replayed = true
			if err := tx.Commit(ctx); err != nil {
				return learning.OfflinePrepareClaim{}, fmt.Errorf("commit offline prepare replay claim: %w", err)
			}
			return learning.OfflinePrepareClaim{State: "published", Prepared: &prepared}, nil
		case "rejected":
			rejected, decodeErr := decodeOfflinePrepareRejection(resultBody)
			if decodeErr != nil {
				return learning.OfflinePrepareClaim{}, decodeErr
			}
			if err := tx.Commit(ctx); err != nil {
				return learning.OfflinePrepareClaim{}, fmt.Errorf("commit offline prepare rejection replay: %w", err)
			}
			return learning.OfflinePrepareClaim{}, &learning.Error{Code: rejected.Code, Reason: rejected.Reason}
		}
	} else if !errors.Is(readErr, pgx.ErrNoRows) {
		return learning.OfflinePrepareClaim{}, fmt.Errorf("read offline prepare claim: %w", readErr)
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return learning.OfflinePrepareClaim{}, fmt.Errorf("read offline prepare claim clock: %w", err)
	}
	databaseNow = databaseNow.UTC().Truncate(time.Microsecond)
	if readErr == nil && status == "processing" && leaseExpires != nil && leaseExpires.After(databaseNow) {
		if err := tx.Commit(ctx); err != nil {
			return learning.OfflinePrepareClaim{}, fmt.Errorf("commit busy offline prepare claim: %w", err)
		}
		return learning.OfflinePrepareClaim{State: "busy"}, nil
	}
	var generationRequest *learning.OfflinePrepareGenerationRequest
	if errors.Is(readErr, pgx.ErrNoRows) {
		authority, authorityErr := s.loadOfflinePrepareAuthority(ctx, tx, request, generation, credentialEpoch)
		if authorityErr != nil {
			return learning.OfflinePrepareClaim{}, authorityErr
		}
		frozen := offlinePrepareGenerationRequest(request, authority)
		requestBody, err = encodeOfflinePrepareClaimRequest(request, frozen)
		if err != nil {
			return learning.OfflinePrepareClaim{}, err
		}
		generationRequest = &frozen
	} else {
		generationRequest, err = recoverOfflinePrepareGenerationPlan(requestBody, artifactBody, request, requestHash)
		if err != nil {
			return learning.OfflinePrepareClaim{}, err
		}
	}
	newLease := uuid.NewString()
	leaseUntil := databaseNow.Add(offlinePrepareLeaseDuration)
	if errors.Is(readErr, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO offline_prepare_claims(
				device_id,operation_id,request_hash,learner_generation,status,lease_token,
				lease_expires_at,request_body,created_at,updated_at)
			VALUES($1,$2,$3,$4,'processing',$5,$6,$7,$8,$8)`, request.DeviceID,
			request.Request.OperationID, requestHashBytes, generation, newLease, leaseUntil,
			requestBody, databaseNow); err != nil {
			return learning.OfflinePrepareClaim{}, fmt.Errorf("insert offline prepare claim: %w", err)
		}
	} else {
		command, err := tx.Exec(ctx, `
			UPDATE offline_prepare_claims
			SET status='processing',lease_token=$3,lease_expires_at=$4,updated_at=$5
			WHERE device_id=$1 AND operation_id=$2 AND status IN ('pending','processing')`,
			request.DeviceID, request.Request.OperationID, newLease, leaseUntil, databaseNow)
		if err != nil {
			return learning.OfflinePrepareClaim{}, fmt.Errorf("take over offline prepare claim: %w", err)
		}
		if command.RowsAffected() != 1 {
			return learning.OfflinePrepareClaim{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_claim_changed"}
		}
	}
	claim := learning.OfflinePrepareClaim{State: "claimed", LeaseToken: newLease, Generation: generationRequest}
	if len(artifactBody) != 0 {
		var artifact learning.OfflinePrepareArtifact
		if err := json.Unmarshal(artifactBody, &artifact); err != nil {
			return learning.OfflinePrepareClaim{}, fmt.Errorf("decode offline prepare artifact: %w", err)
		}
		if err := validateOfflinePrepareArtifact(artifact); err != nil {
			return learning.OfflinePrepareClaim{}, fmt.Errorf("validate offline prepare artifact: %w", err)
		}
		claim.Artifact = &artifact
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.OfflinePrepareClaim{}, fmt.Errorf("commit offline prepare claim: %w", err)
	}
	return claim, nil
}

func (s *Store) StoreOfflinePrepareArtifact(ctx context.Context, deviceID, operationID, leaseToken string, artifact learning.OfflinePrepareArtifact) error {
	if uuid.Validate(deviceID) != nil || uuid.Validate(operationID) != nil || uuid.Validate(leaseToken) != nil {
		return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_prepare_artifact_claim"}
	}
	if err := validateOfflinePrepareArtifact(artifact); err != nil {
		return err
	}
	artifactBody, err := canonicalOfflineStoreValue(artifact)
	if err != nil {
		return err
	}
	command, err := s.pool.Exec(ctx, `
		UPDATE offline_prepare_claims
		SET model_artifact=$4,updated_at=clock_timestamp()
		WHERE device_id=$1 AND operation_id=$2 AND status='processing' AND lease_token=$3
		  AND lease_expires_at>clock_timestamp()
		  AND (model_artifact IS NULL OR model_artifact=$4::jsonb)`, deviceID, operationID,
		leaseToken, artifactBody)
	if err != nil {
		return fmt.Errorf("store offline prepare artifact: %w", err)
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	return nil
}

func (s *Store) RejectOfflinePrepare(ctx context.Context, deviceID, operationID, leaseToken string, cause error) error {
	if uuid.Validate(deviceID) != nil || uuid.Validate(operationID) != nil || uuid.Validate(leaseToken) != nil {
		return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_prepare_rejection"}
	}
	var domainErr *learning.Error
	if !errors.As(cause, &domainErr) {
		return cause
	}
	body, err := canonicalOfflineStoreValue(offlinePrepareRejected{Code: domainErr.Code, Reason: domainErr.Reason})
	if err != nil {
		return err
	}
	command, err := s.pool.Exec(ctx, `
		UPDATE offline_prepare_claims
		SET status='rejected',lease_token=NULL,lease_expires_at=NULL,result_body=$4,updated_at=clock_timestamp()
		WHERE device_id=$1 AND operation_id=$2 AND status='processing' AND lease_token=$3
		  AND lease_expires_at>clock_timestamp()`, deviceID, operationID, leaseToken, body)
	if err != nil {
		return fmt.Errorf("reject offline prepare claim: %w", err)
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	return nil
}

func (s *Store) PublishOfflinePrepare(ctx context.Context, request learning.OfflinePrepareStoreRequest, leaseToken string, signer learning.OfflineSigner) (learning.OfflinePreparedPack, error) {
	if err := validateOfflinePrepareStoreRequest(request); err != nil || signer == nil || uuid.Validate(leaseToken) != nil {
		if err != nil {
			return learning.OfflinePreparedPack{}, err
		}
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_prepare_publish"}
	}
	requestHash, err := request.Request.CanonicalHash()
	if err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite})
	if err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("begin offline prepare publish: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockOfflinePrepareOperation(ctx, tx, request.DeviceID, request.Request.OperationID); err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	generation, err := s.lockOfflinePrepareGates(ctx, tx)
	if err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	credentialEpoch, err := lockOfflinePrepareCredential(ctx, tx, request.DeviceID)
	if err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	var storedHash []byte
	var storedGeneration int64
	var status string
	var storedLease *string
	var leaseExpires *time.Time
	var artifactBody, resultBody []byte
	if err := tx.QueryRow(ctx, `
		SELECT request_hash,learner_generation,status,lease_token::text,lease_expires_at,
		       model_artifact,result_body
		FROM offline_prepare_claims
		WHERE device_id=$1 AND operation_id=$2
		FOR UPDATE`, request.DeviceID, request.Request.OperationID).Scan(
		&storedHash, &storedGeneration, &status, &storedLease, &leaseExpires, &artifactBody, &resultBody); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("lock offline prepare publish claim: %w", err)
	}
	if hex.EncodeToString(storedHash) != requestHash {
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeIdempotencyConflict, Reason: learning.OfflineReasonIdempotencyConflict}
	}
	if storedGeneration != generation {
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeContentRedacted, Reason: "offline_prepare_generation_changed"}
	}
	if status == "published" {
		prepared, decodeErr := decodeStoredOfflinePrepared(resultBody)
		if decodeErr != nil {
			return learning.OfflinePreparedPack{}, decodeErr
		}
		prepared.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return learning.OfflinePreparedPack{}, fmt.Errorf("commit offline prepare publish replay: %w", err)
		}
		return prepared, nil
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("read offline prepare publish clock: %w", err)
	}
	databaseNow = databaseNow.UTC().Truncate(time.Microsecond)
	if status != "processing" || storedLease == nil || *storedLease != leaseToken || leaseExpires == nil || !leaseExpires.After(databaseNow) || len(artifactBody) == 0 {
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	var artifact learning.OfflinePrepareArtifact
	if err := json.Unmarshal(artifactBody, &artifact); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("decode offline prepare publish artifact: %w", err)
	}
	if err := validateOfflinePrepareArtifact(artifact); err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	authority, err := s.loadOfflinePrepareAuthority(ctx, tx, request, generation, credentialEpoch)
	if err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	if err := validateOfflinePrepareArtifactAuthority(artifact, authority); err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	prepared, err := publishOfflinePreparePack(ctx, tx, request, signer, authority, artifact, requestHash, databaseNow)
	if err != nil {
		var domainErr *learning.Error
		if errors.As(err, &domainErr) && (domainErr.Code == learning.CodeOfflinePrepareUnavailable || domainErr.Code == learning.CodeModelUnavailable) {
			if rejectErr := rejectOfflinePrepareTx(ctx, tx, request.DeviceID, request.Request.OperationID, leaseToken, domainErr, databaseNow); rejectErr != nil {
				return learning.OfflinePreparedPack{}, rejectErr
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return learning.OfflinePreparedPack{}, fmt.Errorf("commit offline prepare publish rejection: %w", commitErr)
			}
		}
		return learning.OfflinePreparedPack{}, err
	}
	resultBody, err = canonicalOfflineStoreValue(prepared)
	if err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE offline_prepare_claims
		SET status='published',lease_token=NULL,lease_expires_at=NULL,result_body=$4,updated_at=$5
		WHERE device_id=$1 AND operation_id=$2 AND status='processing' AND lease_token=$3
		  AND lease_expires_at>$5`, request.DeviceID, request.Request.OperationID, leaseToken,
		resultBody, databaseNow)
	if err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("publish offline prepare claim: %w", err)
	}
	if command.RowsAffected() != 1 {
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("commit offline prepare publish: %w", err)
	}
	return prepared, nil
}

func publishOfflinePreparePack(ctx context.Context, tx pgx.Tx, request learning.OfflinePrepareStoreRequest, signer learning.OfflineSigner, authority offlinePrepareAuthority, artifact learning.OfflinePrepareArtifact, requestHash string, issuedAt time.Time) (learning.OfflinePreparedPack, error) {
	generationText, _ := learning.FormatUint63Decimal(uint64(authority.generation))
	credentialEpochText, _ := learning.FormatUint63Decimal(uint64(authority.credentialEpoch))
	manifestRevision, _ := learning.FormatUint63Decimal(signer.ManifestRevision())
	eligibleUntil := issuedAt.Add(request.TTL)
	archiveUntil := eligibleUntil.Add(learning.OfflineArchiveExtension)
	packID := uuid.NewString()
	var highWater int64
	if err := tx.QueryRow(ctx, `SELECT high_water FROM offline_device_sequence_heads WHERE device_id=$1 FOR UPDATE`, request.DeviceID).Scan(&highWater); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("lock offline device sequence head: %w", err)
	}
	items := make([]offlinePreparedItem, 0, request.Count)
	truncatedReason := ""
	for _, source := range artifact.Activities {
		if len(items) == request.Count {
			break
		}
		activity := learning.CloneActivity(source)
		activity.CreatedAt = activity.CreatedAt.UTC().Truncate(time.Microsecond)
		var activeReservation bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM offline_attempt_heads
				WHERE device_id=$1 AND offline_activity_id=$2 AND activity_revision=$3 AND state='reserved')`,
			request.DeviceID, activity.ID, activity.Revision).Scan(&activeReservation); err != nil {
			return learning.OfflinePreparedPack{}, fmt.Errorf("check offline active reservation: %w", err)
		}
		if activeReservation {
			continue
		}
		activityPayload, err := canonicalOfflineStoreValue(activity)
		if err != nil {
			return learning.OfflinePreparedPack{}, err
		}
		if len(activityPayload) > offlineMaxActivityBytes {
			truncatedReason = "activity_size_limited"
			continue
		}
		deviceSequence := highWater + int64(len(items)) + 1
		if deviceSequence < 1 || uint64(deviceSequence) > learning.MaxUint63 {
			return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "device_sequence_exhausted"}
		}
		deviceSequenceText, _ := learning.FormatUint63Decimal(uint64(deviceSequence))
		activityRevisionText, _ := learning.FormatUint63Decimal(uint64(activity.Revision))
		activityDigest := base64Digest(activityPayload)
		submissionID, operationID := uuid.NewString(), uuid.NewString()
		authorizationPayload := learning.OfflineAuthorizationPayloadV1{
			ProtocolVersion: 1, Format: "offline-authorization-v1", Issuer: "edu-agent",
			SignerKeyID: signer.KeyID(), PackID: packID, DeviceID: request.DeviceID,
			CredentialEpoch: credentialEpochText, LearnerGeneration: generationText,
			ServerOriginDigest: base64Digest([]byte(signer.Origin())), OfflineActivityID: activity.ID,
			ActivityRevision: activityRevisionText, SubmissionID: submissionID,
			OperationID: operationID, DeviceSequence: deviceSequenceText, ExpectedVersion: "0",
			ActivityPayloadDigest: activityDigest, EligibleUntil: eligibleUntil, ArchiveUntil: archiveUntil,
		}
		authorization, err := signer.Sign(learning.OfflineAuthorizationDomain, authorizationPayload)
		if err != nil {
			return learning.OfflinePreparedPack{}, err
		}
		authorizationSignature, decodeErr := base64.RawURLEncoding.DecodeString(authorization.Signature)
		if decodeErr != nil || len(authorizationSignature) != 64 {
			return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeOfflineSignerUnavailable, Reason: "offline_signer_signature_invalid"}
		}
		item := offlinePreparedItem{
			activity: activity, activityPayload: activityPayload, activityHash: sha256.Sum256(activityPayload),
			authorization: authorization, authorizationHash: sha256.Sum256(authorization.Payload),
			authorizationSignature: authorizationSignature, submissionID: submissionID,
			operationID: operationID, deviceSequence: deviceSequence,
		}
		trial := append(append([]offlinePreparedItem(nil), items...), item)
		_, trialBody, buildErr := buildOfflinePackEnvelope(signer, packID, request.DeviceID, generationText, authority.session.ID, issuedAt, eligibleUntil, archiveUntil, trial, true, "pack_size_limited")
		if buildErr != nil {
			return learning.OfflinePreparedPack{}, buildErr
		}
		if len(trialBody) > offlineMaxPackBytes {
			truncatedReason = "pack_size_limited"
			break
		}
		items = trial
	}
	if len(items) == 0 {
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_empty_pack"}
	}
	truncated := len(items) < request.Count
	if truncated && truncatedReason == "" {
		if artifact.ModelPartial {
			truncatedReason = "model_partial"
		} else {
			truncatedReason = "route_exhausted"
		}
	}
	pack, packBody, err := buildOfflinePackEnvelope(signer, packID, request.DeviceID, generationText, authority.session.ID, issuedAt, eligibleUntil, archiveUntil, items, truncated, truncatedReason)
	if err != nil {
		return learning.OfflinePreparedPack{}, err
	}
	for len(packBody) > offlineMaxPackBytes && len(items) > 1 {
		items = items[:len(items)-1]
		truncated, truncatedReason = true, "pack_size_limited"
		pack, packBody, err = buildOfflinePackEnvelope(signer, packID, request.DeviceID, generationText, authority.session.ID, issuedAt, eligibleUntil, archiveUntil, items, truncated, truncatedReason)
		if err != nil {
			return learning.OfflinePreparedPack{}, err
		}
	}
	if len(packBody) > offlineMaxPackBytes {
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "pack_size_limited"}
	}
	packHash := sha256.Sum256(packBody)
	packSignature, decodeErr := base64.RawURLEncoding.DecodeString(pack.Signature)
	if decodeErr != nil || len(packSignature) != 64 {
		return learning.OfflinePreparedPack{}, &learning.Error{Code: learning.CodeOfflineSignerUnavailable, Reason: "offline_signer_signature_invalid"}
	}
	// The pack is the FK parent for authorizations and first possession. Insert it
	// before reserving any child facts; the surrounding transaction still makes
	// the complete publication atomic.
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_packs(
			id,revision,prepare_device_id,prepare_operation_id,learner_generation,parent_session_id,
			response_body,response_hash,signer_key_id,signature,issued_at,eligible_until,archive_until,created_at)
		VALUES($1,1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$10)`, packID, request.DeviceID,
		request.Request.OperationID, authority.generation, authority.session.ID, packBody, packHash[:],
		signer.KeyID(), packSignature, issuedAt, eligibleUntil, archiveUntil); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("insert offline pack: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE offline_device_sequence_heads SET high_water=$2,updated_at=$3 WHERE device_id=$1`, request.DeviceID, highWater+int64(len(items)), issuedAt); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("reserve offline device sequences: %w", err)
	}
	for _, item := range items {
		if err := persistOfflinePreparedItem(ctx, tx, request.DeviceID, packID, authority, item, issuedAt, eligibleUntil, archiveUntil, signer.KeyID()); err != nil {
			return learning.OfflinePreparedPack{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_device_possessions(id,device_id,learner_generation,first_pack_id,first_seen_at)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(device_id,learner_generation) DO NOTHING`, uuid.NewString(), request.DeviceID,
		authority.generation, packID, issuedAt); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("insert offline device possession: %w", err)
	}
	return learning.OfflinePreparedPack{
		OperationID: request.Request.OperationID, RequestHash: requestHash, Pack: pack,
		PackDigest: base64Digest(packBody), ManifestRevision: manifestRevision,
		ManifestDigest: signer.ManifestDigest(),
	}, nil
}

func buildOfflinePackEnvelope(signer learning.OfflineSigner, packID, deviceID, generation, sessionID string, issuedAt, eligibleUntil, archiveUntil time.Time, items []offlinePreparedItem, truncated bool, reason string) (learning.OfflineSignedEnvelope, []byte, error) {
	payload := learning.OfflinePackPayloadV1{
		ProtocolVersion: 1, PackID: packID, Revision: "1", DeviceID: deviceID,
		LearnerGeneration: generation, ParentSessionID: sessionID, IssuedAt: issuedAt,
		EligibleUntil: eligibleUntil, ArchiveUntil: archiveUntil, Truncated: truncated,
		Items: make([]learning.OfflinePackItemV1, 0, len(items)),
	}
	if truncated {
		payload.TruncatedReason = reason
	}
	for _, item := range items {
		payload.Items = append(payload.Items, learning.OfflinePackItemV1{
			Activity: item.activity, ActivityPayloadDigest: base64Digest(item.activityPayload),
			Authorization: item.authorization,
		})
	}
	pack, err := signer.Sign(learning.OfflinePackDomain, payload)
	if err != nil {
		return learning.OfflineSignedEnvelope{}, nil, err
	}
	body, err := canonicalOfflineStoreValue(pack)
	return pack, body, err
}

func persistOfflinePreparedItem(ctx context.Context, tx pgx.Tx, deviceID, packID string, authority offlinePrepareAuthority, item offlinePreparedItem, issuedAt, eligibleUntil, archiveUntil time.Time, signerKeyID string) error {
	activity := item.activity
	rubric, err := json.Marshal(activity.Rubric)
	if err != nil {
		return err
	}
	allowedHelp := make([]string, len(activity.AllowedHelp))
	for index, help := range activity.AllowedHelp {
		allowedHelp[index] = string(help)
	}
	practiceKind := "practice"
	if activity.Review {
		practiceKind = "review"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_activities(
			id,revision,learner_generation,parent_session_id,goal_revision_id,route_revision_id,
			route_step_id,knowledge_revision_id,target_node_id,target_node_revision_id,prompt,
			activity_type,rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,
			assessment_policy_version,review_policy_version,practice_kind,payload_hash,issued_at,
			eligible_until,archive_until,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$22)
		ON CONFLICT(id,revision) DO NOTHING`, activity.ID, activity.Revision, authority.generation,
		activity.SessionID, activity.GoalRevisionID, activity.RouteRevisionID, activity.RouteStepID,
		activity.KnowledgeRevisionID, activity.TargetNodeID, activity.TargetNodeRevisionID, activity.Prompt,
		activity.Type, activity.Rubric.Revision, rubric, activity.Difficulty, allowedHelp,
		activity.ActivityPolicyVersion, activity.AssessmentPolicyVersion, activity.ReviewPolicyVersion,
		practiceKind, item.activityHash[:], issuedAt, eligibleUntil, archiveUntil); err != nil {
		return fmt.Errorf("insert offline activity: %w", err)
	}
	for index, reference := range activity.References {
		sliceHash, decodeErr := hex.DecodeString(reference.SliceSHA256)
		if decodeErr != nil || len(sliceHash) != sha256.Size {
			return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: learning.OfflineReasonActivityInvalid}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO offline_activity_references(
				activity_id,activity_revision,ordinal,knowledge_revision_id,node_id,node_revision_id,
				document_revision_id,source_start,source_end,slice_text,slice_hash)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT(activity_id,activity_revision,ordinal) DO NOTHING`, activity.ID, activity.Revision,
			index, reference.KnowledgeRevisionID, reference.NodeID, reference.NodeRevisionID,
			reference.DocumentRevisionID, reference.Range.Start, reference.Range.End, reference.Slice, sliceHash); err != nil {
			return fmt.Errorf("insert offline activity reference: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_submission_authorizations(
			submission_id,pack_id,device_id,operation_id,device_seq,offline_activity_id,
			activity_revision,learner_generation,credential_epoch,expected_version,
			authorization_payload,authorization_hash,signer_key_id,signature,eligible_until,
			archive_until,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$11,$12,$13,$14,$15,$16)`, item.submissionID,
		packID, deviceID, item.operationID, item.deviceSequence, activity.ID, activity.Revision,
		authority.generation, authority.credentialEpoch, item.authorization.Payload, item.authorizationHash[:],
		signerKeyID, item.authorizationSignature, eligibleUntil, archiveUntil, issuedAt); err != nil {
		return fmt.Errorf("insert offline authorization: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_device_sequence_reservations(
			device_id,device_seq,operation_id,submission_id,authorization_hash,reserved_at)
		VALUES($1,$2,$3,$4,$5,$6)`, deviceID, item.deviceSequence, item.operationID,
		item.submissionID, item.authorizationHash[:], issuedAt); err != nil {
		return fmt.Errorf("insert offline sequence reservation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO offline_attempt_heads(
			submission_id,device_id,offline_activity_id,activity_revision,state,reserved_operation_id,
			aggregate_version,updated_at)
		VALUES($1,$2,$3,$4,'reserved',$5,0,$6)`, item.submissionID, deviceID, activity.ID,
		activity.Revision, item.operationID, issuedAt); err != nil {
		return fmt.Errorf("insert offline attempt reservation: %w", err)
	}
	return nil
}

func validateOfflinePrepareStoreRequest(request learning.OfflinePrepareStoreRequest) error {
	if uuid.Validate(request.DeviceID) != nil || request.Request.Validate() != nil || request.Count < 1 || request.Count > learning.OfflineMaxPackCount || request.TTL < learning.OfflineMinimumTTL || request.TTL > learning.OfflineMaximumTTL {
		return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_prepare_request"}
	}
	return nil
}

func validateOfflinePrepareArtifact(artifact learning.OfflinePrepareArtifact) error {
	if artifact.ProtocolVersion != 1 || uuid.Validate(artifact.SessionID) != nil || artifact.ExpectedSessionVersion < 1 || uuid.Validate(artifact.GoalRevisionID) != nil || uuid.Validate(artifact.RouteRevisionID) != nil || uuid.Validate(artifact.RouteStepID) != nil || uuid.Validate(artifact.KnowledgeRevisionID) != nil || len(artifact.Activities) == 0 || len(artifact.Activities) > learning.OfflineMaxPackCount {
		return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_prepare_artifact"}
	}
	seen := map[string]bool{}
	for _, activity := range artifact.Activities {
		if uuid.Validate(activity.ID) != nil || activity.Revision != 1 || seen[activity.ID] || activity.SessionID != artifact.SessionID || activity.GoalRevisionID != artifact.GoalRevisionID || activity.RouteRevisionID != artifact.RouteRevisionID || activity.KnowledgeRevisionID != artifact.KnowledgeRevisionID || (activity.Type != learning.ActivityObjective && activity.Type != learning.ActivityOpen) || len(activity.References) == 0 {
			return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_prepare_artifact_activity"}
		}
		seen[activity.ID] = true
	}
	return nil
}

func validateOfflinePrepareArtifactAuthority(artifact learning.OfflinePrepareArtifact, authority offlinePrepareAuthority) error {
	if artifact.SessionID != authority.session.ID || artifact.SessionState != string(authority.session.State) || artifact.ExpectedSessionVersion != authority.session.AggregateVer || artifact.GoalRevisionID != authority.session.Context.GoalRevisionID || artifact.RouteRevisionID != authority.route.ID || artifact.RouteStepID != authority.session.Context.RouteStepID || artifact.KnowledgeRevisionID != authority.route.KnowledgeRevisionID {
		return &learning.Error{Code: learning.CodeVersionConflict, AggregateType: "session", AggregateID: authority.session.ID, ExpectedVersion: artifact.ExpectedSessionVersion, CurrentVersion: authority.session.AggregateVer}
	}
	steps := make(map[string]learning.RouteStep, len(authority.route.Steps))
	for _, step := range authority.route.Steps {
		steps[step.ID] = step
	}
	for index, activity := range artifact.Activities {
		step, ok := steps[activity.RouteStepID]
		if !ok || step.NodeID != activity.TargetNodeID || step.NodeRevisionID != activity.TargetNodeRevisionID {
			return &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_route_changed"}
		}
		if index == 0 && authority.currentActivity != nil && activity.ID != authority.currentActivity.ID {
			return &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_current_activity_changed"}
		}
		found := false
		for _, reference := range activity.References {
			if reference.KnowledgeRevisionID == authority.route.KnowledgeRevisionID && reference.NodeID == step.NodeID && reference.NodeRevisionID == step.NodeRevisionID && reference.DocumentRevisionID != "" {
				found = true
				break
			}
		}
		if !found {
			return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Reason: learning.OfflineReasonActivityInvalid}
		}
	}
	return nil
}

func (s *Store) lockOfflinePrepareGates(ctx context.Context, tx pgx.Tx) (int64, error) {
	learningGeneration, err := lockCurrentLearningWriteGeneration(ctx, tx)
	if err != nil {
		return 0, err
	}
	tutoringGeneration, err := s.tutoring.LockReadWith(ctx, tx)
	if err != nil {
		return 0, err
	}
	if s.knowledge == nil {
		return 0, errors.New("offline knowledge owner is required")
	}
	knowledgeGeneration, err := s.knowledge.LockReadWith(ctx, tx)
	if err != nil {
		return 0, err
	}
	if tutoringGeneration != learningGeneration || knowledgeGeneration != learningGeneration {
		return 0, &learning.Error{Code: learning.CodeContentRedacted, Reason: "owner_generation_mismatch"}
	}
	return learningGeneration, nil
}

func lockOfflinePrepareOperation(ctx context.Context, tx pgx.Tx, deviceID, operationID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "offline-prepare:"+deviceID+":"+operationID); err != nil {
		return fmt.Errorf("lock offline prepare operation: %w", err)
	}
	return nil
}

func lockOfflinePrepareCredential(ctx context.Context, tx pgx.Tx, deviceID string) (int64, error) {
	var revokedAt *time.Time
	var credentialEpoch int64
	if err := tx.QueryRow(ctx, `
		SELECT device.revoked_at,credential.credential_epoch
		FROM devices device
		JOIN offline_device_credentials credential ON credential.device_id=device.id
		WHERE device.id=$1
		FOR UPDATE OF device,credential`, deviceID).Scan(&revokedAt, &credentialEpoch); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: learning.OfflineReasonDeviceRevoked}
		}
		return 0, fmt.Errorf("lock offline prepare credential: %w", err)
	}
	if revokedAt != nil {
		return 0, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: learning.OfflineReasonDeviceRevoked}
	}
	return credentialEpoch, nil
}

func (s *Store) loadOfflinePrepareAuthority(ctx context.Context, tx pgx.Tx, request learning.OfflinePrepareStoreRequest, generation, credentialEpoch int64) (offlinePrepareAuthority, error) {
	metadata, _, _, _, err := metadataFrom(ctx, tx)
	if err != nil {
		return offlinePrepareAuthority{}, err
	}
	var sessionID string
	if err := tx.QueryRow(ctx, `
		SELECT session_id::text
		FROM learning_projection_sessions
		WHERE generation_id=$1 AND item->'session'->>'state'<>'Completed'
		ORDER BY updated_event_seq DESC,session_id DESC
		LIMIT 1`, metadata.GenerationID).Scan(&sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "active_session_missing"}
		}
		return offlinePrepareAuthority{}, fmt.Errorf("load offline prepare current session: %w", err)
	}
	session, err := s.tutoring.LoadSessionLockedWith(ctx, tx, sessionID)
	if err != nil {
		return offlinePrepareAuthority{}, fmt.Errorf("load offline prepare session authority: %w", err)
	}
	expectedSessionVersion, _ := learning.ParseUint63Decimal(request.Request.ExpectedSessionVersion)
	if uint64(session.AggregateVer) != expectedSessionVersion {
		return offlinePrepareAuthority{}, &learning.Error{
			Code: learning.CodeVersionConflict, AggregateType: "session", AggregateID: session.ID,
			ExpectedVersion: int64(expectedSessionVersion), CurrentVersion: session.AggregateVer,
		}
	}
	if session.State != tutoring.StateRouteActive && session.State != tutoring.StateAwaitingResponse {
		return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_session_state"}
	}
	contextValue := session.Context
	if contextValue.GoalRevisionID == "" || contextValue.RouteRevisionID == "" || contextValue.RouteStepID == "" || contextValue.KnowledgeRevisionID == "" || contextValue.FocusNodeRevisionID == "" {
		return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_context_incomplete"}
	}
	route, err := loadRouteRevisionForView(ctx, tx, contextValue.RouteRevisionID)
	if err != nil {
		return offlinePrepareAuthority{}, fmt.Errorf("load offline prepare route: %w", err)
	}
	if route.GoalRevisionID != contextValue.GoalRevisionID || route.KnowledgeRevisionID != contextValue.KnowledgeRevisionID || !learning.StableRouteSteps(route.Steps) {
		return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_route_changed"}
	}
	stepFound := false
	for _, step := range route.Steps {
		if step.ID == contextValue.RouteStepID && step.NodeRevisionID == contextValue.FocusNodeRevisionID {
			stepFound = true
			break
		}
	}
	if !stepFound {
		return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_route_step_missing"}
	}
	redacted, head, err := s.knowledge.RevisionHeadLockedWith(ctx, tx, contextValue.KnowledgeRevisionID)
	if err != nil {
		return offlinePrepareAuthority{}, err
	}
	if redacted {
		return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeContentRedacted, Reason: "offline_prepare_knowledge_redacted"}
	}
	if head != contextValue.KnowledgeRevisionID {
		return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: learning.OfflineReasonStaleKnowledge}
	}
	authority := offlinePrepareAuthority{generation: generation, credentialEpoch: credentialEpoch, session: session, route: route}
	if session.State == tutoring.StateAwaitingResponse {
		if session.Context.ActivityID == nil {
			return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_current_activity_missing"}
		}
		activity, err := loadActivityForView(ctx, tx, *session.Context.ActivityID)
		if err != nil {
			return offlinePrepareAuthority{}, fmt.Errorf("load offline prepare current activity: %w", err)
		}
		if (activity.Type != learning.ActivityObjective && activity.Type != learning.ActivityOpen) || activity.Revision != 1 || !activityOwnsSessionFocus(activity, session) {
			return offlinePrepareAuthority{}, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_current_activity_invalid"}
		}
		activity.CreatedAt = activity.CreatedAt.UTC().Truncate(time.Microsecond)
		authority.currentActivity = &activity
	}
	return authority, nil
}

func encodeOfflinePrepareClaimRequest(request learning.OfflinePrepareStoreRequest, generation learning.OfflinePrepareGenerationRequest) ([]byte, error) {
	if err := validateOfflinePrepareGenerationPlan(request, generation); err != nil {
		return nil, err
	}
	return canonicalOfflineStoreValue(offlinePrepareClaimRequestV1{
		Format: offlinePrepareClaimFormatV1, ProtocolVersion: 1,
		Request: request.Request, Generation: generation,
	})
}

func recoverOfflinePrepareGenerationPlan(raw, artifactBody []byte, request learning.OfflinePrepareStoreRequest, expectedHash string) (*learning.OfflinePrepareGenerationRequest, error) {
	frozen, legacy, err := decodeOfflinePrepareClaimRequest(raw, request, expectedHash)
	if err != nil {
		return nil, err
	}
	if legacy {
		if len(artifactBody) == 0 {
			return nil, &learning.Error{Code: learning.CodeOfflinePrepareUnavailable, Reason: "offline_prepare_claim_plan_missing"}
		}
		return nil, nil
	}
	return &frozen, nil
}

func decodeOfflinePrepareClaimRequest(raw []byte, request learning.OfflinePrepareStoreRequest, expectedHash string) (learning.OfflinePrepareGenerationRequest, bool, error) {
	var envelope offlinePrepareClaimRequestV1
	if err := decodeStrictOfflinePrepareClaimJSON(raw, &envelope); err == nil {
		if envelope.Format != offlinePrepareClaimFormatV1 || envelope.ProtocolVersion != 1 {
			return learning.OfflinePrepareGenerationRequest{}, false, errors.New("offline prepare claim request envelope version is invalid")
		}
		embeddedHash, hashErr := envelope.Request.CanonicalHash()
		if hashErr != nil || embeddedHash != expectedHash {
			return learning.OfflinePrepareGenerationRequest{}, false, errors.New("offline prepare claim request envelope hash is invalid")
		}
		if err := validateOfflinePrepareGenerationPlan(request, envelope.Generation); err != nil {
			return learning.OfflinePrepareGenerationRequest{}, false, fmt.Errorf("validate offline prepare claim generation plan: %w", err)
		}
		return envelope.Generation, false, nil
	}
	var legacy learning.OfflinePrepareRequest
	if err := decodeStrictOfflinePrepareClaimJSON(raw, &legacy); err == nil && legacy.Validate() == nil {
		legacyHash, hashErr := legacy.CanonicalHash()
		if hashErr != nil || legacyHash != expectedHash {
			return learning.OfflinePrepareGenerationRequest{}, false, errors.New("legacy offline prepare claim request hash is invalid")
		}
		return learning.OfflinePrepareGenerationRequest{}, true, nil
	}
	return learning.OfflinePrepareGenerationRequest{}, false, errors.New("offline prepare claim request body is invalid")
}

func decodeStrictOfflinePrepareClaimJSON(raw []byte, target any) error {
	canonical, err := learning.CanonicalizeJCS(raw)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("offline prepare claim request body has trailing data")
	}
	return nil
}

func validateOfflinePrepareGenerationPlan(request learning.OfflinePrepareStoreRequest, generation learning.OfflinePrepareGenerationRequest) error {
	expectedVersion, err := learning.ParseUint63Decimal(request.Request.ExpectedSessionVersion)
	if err != nil {
		return err
	}
	if uuid.Validate(generation.DeviceID) != nil || uuid.Validate(generation.OperationID) != nil || uuid.Validate(generation.SessionID) != nil ||
		uuid.Validate(generation.GoalRevisionID) != nil || uuid.Validate(generation.Route.ID) != nil || uuid.Validate(generation.RouteStepID) != nil ||
		uuid.Validate(generation.KnowledgeRevisionID) != nil || generation.DeviceID != request.DeviceID ||
		generation.OperationID != request.Request.OperationID || generation.Count != request.Count ||
		generation.ExpectedSessionVersion != int64(expectedVersion) || generation.GoalRevisionID != generation.Route.GoalRevisionID ||
		generation.KnowledgeRevisionID != generation.Route.KnowledgeRevisionID || !learning.StableRouteSteps(generation.Route.Steps) {
		return errors.New("offline prepare generation plan identity is invalid")
	}
	var activeStep *learning.RouteStep
	for index := range generation.Route.Steps {
		if generation.Route.Steps[index].ID == generation.RouteStepID {
			activeStep = &generation.Route.Steps[index]
			break
		}
	}
	if activeStep == nil {
		return errors.New("offline prepare generation plan route step is missing")
	}
	switch generation.SessionState {
	case string(tutoring.StateRouteActive):
		if generation.CurrentActivity != nil {
			return errors.New("route-active offline prepare plan has a current activity")
		}
	case string(tutoring.StateAwaitingResponse):
		activity := generation.CurrentActivity
		if activity == nil || uuid.Validate(activity.ID) != nil || activity.Revision != 1 ||
			activity.SessionID != generation.SessionID || activity.GoalRevisionID != generation.GoalRevisionID ||
			activity.RouteRevisionID != generation.Route.ID || activity.RouteStepID != generation.RouteStepID ||
			activity.KnowledgeRevisionID != generation.KnowledgeRevisionID || activity.TargetNodeID != activeStep.NodeID ||
			activity.TargetNodeRevisionID != activeStep.NodeRevisionID ||
			(activity.Type != learning.ActivityObjective && activity.Type != learning.ActivityOpen) || len(activity.References) == 0 {
			return errors.New("awaiting-response offline prepare plan current activity is invalid")
		}
	default:
		return errors.New("offline prepare generation plan session state is invalid")
	}
	return nil
}

func offlinePrepareGenerationRequest(request learning.OfflinePrepareStoreRequest, authority offlinePrepareAuthority) learning.OfflinePrepareGenerationRequest {
	value := learning.OfflinePrepareGenerationRequest{
		DeviceID: request.DeviceID, OperationID: request.Request.OperationID, Count: request.Count,
		SessionID: authority.session.ID, SessionState: string(authority.session.State),
		ExpectedSessionVersion: authority.session.AggregateVer,
		GoalRevisionID:         authority.session.Context.GoalRevisionID, Route: authority.route,
		RouteStepID: authority.session.Context.RouteStepID, KnowledgeRevisionID: authority.route.KnowledgeRevisionID,
	}
	if authority.currentActivity != nil {
		current := learning.CloneActivity(*authority.currentActivity)
		value.CurrentActivity = &current
	}
	return value
}

func decodeStoredOfflinePrepared(raw []byte) (learning.OfflinePreparedPack, error) {
	if len(raw) == 0 {
		return learning.OfflinePreparedPack{}, errors.New("offline prepare result is missing")
	}
	var prepared learning.OfflinePreparedPack
	if err := json.Unmarshal(raw, &prepared); err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("decode offline prepare result: %w", err)
	}
	canonicalPackPayload, err := learning.CanonicalizeJCS(prepared.Pack.Payload)
	if err != nil {
		return learning.OfflinePreparedPack{}, fmt.Errorf("canonicalize offline prepare replay: %w", err)
	}
	prepared.Pack.Payload = canonicalPackPayload
	return prepared, nil
}

func decodeOfflinePrepareRejection(raw []byte) (offlinePrepareRejected, error) {
	var rejected offlinePrepareRejected
	if len(raw) == 0 || json.Unmarshal(raw, &rejected) != nil || rejected.Code == "" {
		return rejected, errors.New("offline prepare rejection is invalid")
	}
	return rejected, nil
}

func rejectOfflinePrepareTx(ctx context.Context, tx pgx.Tx, deviceID, operationID, leaseToken string, cause *learning.Error, now time.Time) error {
	body, err := canonicalOfflineStoreValue(offlinePrepareRejected{Code: cause.Code, Reason: cause.Reason})
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		UPDATE offline_prepare_claims
		SET status='rejected',lease_token=NULL,lease_expires_at=NULL,result_body=$4,updated_at=$5
		WHERE device_id=$1 AND operation_id=$2 AND status='processing' AND lease_token=$3
		  AND lease_expires_at>$5`, deviceID, operationID, leaseToken, body, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeStaleProposal, Reason: "offline_prepare_lease_lost"}
	}
	return nil
}

func lockCurrentLearningWriteGeneration(ctx context.Context, tx pgx.Tx) (int64, error) {
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&generation); err != nil {
		switch databaseMessage(err) {
		case "content_redacted":
			return 0, &privacy.Error{Code: privacy.CodeContentRedacted, Reason: "learning_write_gate_closed", Cause: err}
		case "privacy_clear_in_progress", "privacy owner generation changed":
			return 0, &privacy.Error{Code: privacy.CodePrivacyClearInProgress, Reason: "learning_write_gate_closed", Cause: err}
		default:
			return 0, fmt.Errorf("lock current learning privacy write generation: %w", err)
		}
	}
	return generation, nil
}

func (s *Store) OfflineOperationStatus(ctx context.Context, deviceID, operationID string) (learning.OfflineOperationStatus, error) {
	if uuid.Validate(deviceID) != nil || uuid.Validate(operationID) != nil {
		return learning.OfflineOperationStatus{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_offline_operation_id"}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return learning.OfflineOperationStatus{}, fmt.Errorf("begin offline status read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	learningGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return learning.OfflineOperationStatus{}, err
	}
	tutoringGeneration, err := s.tutoring.LockReadWith(ctx, tx)
	if err != nil {
		return learning.OfflineOperationStatus{}, err
	}
	if s.knowledge == nil {
		return learning.OfflineOperationStatus{}, errors.New("offline knowledge owner is required")
	}
	knowledgeGeneration, err := s.knowledge.LockReadWith(ctx, tx)
	if err != nil {
		return learning.OfflineOperationStatus{}, err
	}
	if learningGeneration != tutoringGeneration || learningGeneration != knowledgeGeneration {
		return learning.OfflineOperationStatus{}, &learning.Error{Code: learning.CodeContentRedacted, Reason: "owner_generation_mismatch"}
	}
	var result learning.OfflineOperationStatus
	var receiptJSON []byte
	var revision int64
	var assessmentID, evidenceID *string
	err = tx.QueryRow(ctx, `
		SELECT status.submission_id::text,revision.archive_status,revision.assessment_status,
		       revision.evidence_status,revision.reason_codes,inbox.result,status.ticket_id::text,
		       head.current_revision,revision.updated_at,revision.assessment_id::text,revision.evidence_id::text
		FROM offline_operation_statuses status
		JOIN offline_operation_status_heads head ON head.ticket_id=status.ticket_id
		JOIN offline_operation_status_revisions revision ON revision.id=head.current_revision_id
		JOIN learning_inbox inbox ON inbox.device_id=status.device_id AND inbox.operation_id=status.operation_id
		WHERE status.device_id=$1 AND status.operation_id=$2`, deviceID, operationID).Scan(
		&result.SubmissionID, &result.ArchiveStatus, &result.AssessmentStatus, &result.EvidenceStatus,
		&result.ReasonCodes, &receiptJSON, &result.StatusTicket.TicketID, &revision,
		&result.StatusTicket.UpdatedAt, &assessmentID, &evidenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.OfflineOperationStatus{}, &learning.Error{Code: learning.CodeOfflineOperationNotFound}
	}
	if err != nil {
		return learning.OfflineOperationStatus{}, fmt.Errorf("read offline operation status: %w", err)
	}
	var archived learning.OfflineIngestResult
	if err := json.Unmarshal(receiptJSON, &archived); err != nil {
		return learning.OfflineOperationStatus{}, fmt.Errorf("decode offline operation receipt: %w", err)
	}
	if archived.Receipt == nil {
		return learning.OfflineOperationStatus{}, errors.New("offline operation receipt is missing")
	}
	result.OperationID = operationID
	result.Receipt = *archived.Receipt
	result.StatusTicket.OperationID = operationID
	revisionText, _ := learning.FormatUint63Decimal(uint64(revision))
	result.StatusTicket.Revision = revisionText
	if assessmentID != nil {
		result.AssessmentID = *assessmentID
	}
	if evidenceID != nil {
		result.EvidenceID = *evidenceID
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.OfflineOperationStatus{}, fmt.Errorf("commit offline status read: %w", err)
	}
	return result, nil
}

func canonicalOfflineStoreValue(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return learning.CanonicalizeJCS(encoded)
}

func base64Digest(value []byte) string {
	digest := sha256.Sum256(value)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
