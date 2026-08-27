package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxUint63 = uint64(1<<63 - 1)

func validateOfflinePairingBootstrap(value OfflinePairingBootstrap) error {
	generation, generationErr := parseUint63(value.LearnerGeneration, true)
	if value.ProtocolVersion != 1 || generationErr != nil || generation == 0 || !validOfflineBaseURL(value.ServerBaseURL) {
		return errors.New("offline pairing bootstrap is invalid")
	}
	if err := validateOfflineManifestEnvelope(value.SignerManifest); err != nil {
		return err
	}
	manifest := value.SignerManifest.Payload
	if manifest.ManifestRevision != "1" || manifest.PreviousManifestDigest != base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)) || manifest.ServerBaseURL != value.ServerBaseURL {
		return errors.New("offline pairing bootstrap is not an initial trust root")
	}
	return nil
}

func validateOfflinePrepareRequest(value OfflinePrepareRequest) error {
	if !validLearningUUID(value.OperationID) || value.PayloadSchemaVersion != 1 {
		return errors.New("offline prepare identity is invalid")
	}
	if _, err := parseUint63(value.ExpectedSessionVersion, false); err != nil {
		return errors.New("offline prepare session version is invalid")
	}
	if _, err := parseUint63(value.TrustedManifestRevision, false); err != nil || !validBase64URLBytes(value.TrustedManifestDigest, sha256.Size) {
		return errors.New("offline prepare manifest trust is invalid")
	}
	if value.RequestedCount != nil && (*value.RequestedCount < 1 || *value.RequestedCount > 20) {
		return errors.New("offline prepare count is invalid")
	}
	if value.RequestedTTLSeconds != nil && (*value.RequestedTTLSeconds < 900 || *value.RequestedTTLSeconds > 604800) {
		return errors.New("offline prepare TTL is invalid")
	}
	return nil
}

func decodeOfflineSyncCanonical(body []byte) (OfflineSyncRequest, error) {
	if len(body) == 0 || len(body) > OfflineMaxSyncBodyBytes {
		return OfflineSyncRequest{}, errors.New("offline sync body size is invalid")
	}
	var request OfflineSyncRequest
	if err := decodeStrict(body, &request); err != nil {
		return OfflineSyncRequest{}, err
	}
	if err := validateOfflineSyncRequest(request); err != nil {
		return OfflineSyncRequest{}, err
	}
	return request, nil
}

func validateOfflineSyncRequest(value OfflineSyncRequest) error {
	if !validLearningUUID(value.SyncRequestID) || value.PayloadSchemaVersion != 1 || len(value.Operations) < 1 || len(value.Operations) > OfflineMaxSyncItems {
		return errors.New("offline sync request is invalid")
	}
	var previous uint64
	for index := range value.Operations {
		sequence, err := validateOfflineOperation(value.Operations[index])
		if err != nil {
			return fmt.Errorf("offline operation %d is invalid: %w", index, err)
		}
		if index > 0 && sequence <= previous {
			return errors.New("offline device sequences are not strictly increasing")
		}
		previous = sequence
	}
	return nil
}

func validateOfflineOperation(value OfflineOperation) (uint64, error) {
	sequence, sequenceErr := parseUint63(value.DeviceSequence, true)
	activityRevision, revisionErr := parseUint63(value.ActivityRevision, true)
	expectedVersion, expectedErr := parseUint63(value.ExpectedVersion, false)
	if !validLearningUUID(value.OperationID) || !validLearningUUID(value.DeviceID) || !validLearningUUID(value.SubmissionID) ||
		!validLearningUUID(value.AggregateID) || !validLearningUUID(value.OfflineActivityID) || sequenceErr != nil || revisionErr != nil ||
		expectedErr != nil || value.PayloadSchemaVersion != 1 || value.AggregateType != "offline_attempt" ||
		value.AggregateID != value.SubmissionID || expectedVersion != 0 || activityRevision != 1 {
		return 0, errors.New("offline operation envelope is invalid")
	}
	if !validBase64URLBytes(value.Signature, 64) {
		return 0, errors.New("offline operation signature is invalid")
	}
	if value.OccurredAt != nil && validateOfflineTimestamp(*value.OccurredAt) != nil {
		return 0, errors.New("offline operation time is invalid")
	}
	if err := validateOfflineAuthorization(value.Authorization); err != nil {
		return 0, err
	}
	authorization := value.Authorization
	if authorization.OperationID != value.OperationID || authorization.DeviceID != value.DeviceID || authorization.DeviceSequence != value.DeviceSequence ||
		authorization.SubmissionID != value.SubmissionID || authorization.OfflineActivityID != value.OfflineActivityID ||
		authorization.ActivityRevision != value.ActivityRevision || authorization.ExpectedVersion != value.ExpectedVersion {
		return 0, errors.New("offline authorization does not bind the operation")
	}
	switch value.OperationType {
	case OfflineAttemptCompleted:
		var payload OfflineAttemptPayload
		if err := decodeStrict(value.Payload, &payload); err != nil || validateOfflineAttemptPayload(payload) != nil {
			return 0, errors.New("offline attempt payload is invalid")
		}
	case OfflineActivitySkipped:
		var payload OfflineSkipPayload
		if err := decodeStrict(value.Payload, &payload); err != nil || !validOfflineSkipReason(payload.Reason) {
			return 0, errors.New("offline skip payload is invalid")
		}
	default:
		return 0, errors.New("offline operation type is invalid")
	}
	return sequence, nil
}

func validateOfflineAttemptPayload(value OfflineAttemptPayload) error {
	if len(value.Answer) < 1 || len(value.Answer) > 262144 || !utf8.ValidString(value.Answer) || !validOfflineHelp(value.Help) ||
		value.Observations == nil || len(value.Observations) > 64 || !validHexSHA256(value.AnswerSHA256) {
		return errors.New("offline attempt payload is incomplete")
	}
	digest := sha256.Sum256([]byte(value.Answer))
	if !strings.EqualFold(hex.EncodeToString(digest[:]), value.AnswerSHA256) || value.AnswerSHA256 != strings.ToLower(value.AnswerSHA256) {
		return errors.New("offline answer digest does not match")
	}
	for _, observation := range value.Observations {
		if observation.Kind != OfflineActivityPresented && observation.Kind != OfflineAnswerRecorded {
			return errors.New("offline observation kind is invalid")
		}
		if observation.OccurredAt != nil && validateOfflineTimestamp(*observation.OccurredAt) != nil {
			return errors.New("offline observation time is invalid")
		}
	}
	return nil
}

func validateOfflineAuthorization(value OfflineAuthorizationPayload) error {
	credentialEpoch, credentialErr := parseUint63(value.CredentialEpoch, true)
	generation, generationErr := parseUint63(value.LearnerGeneration, true)
	sequence, sequenceErr := parseUint63(value.DeviceSequence, true)
	revision, revisionErr := parseUint63(value.ActivityRevision, true)
	expected, expectedErr := parseUint63(value.ExpectedVersion, false)
	if credentialErr != nil || generationErr != nil || sequenceErr != nil || revisionErr != nil || expectedErr != nil ||
		credentialEpoch == 0 || generation == 0 || sequence == 0 || revision != 1 || expected != 0 || value.ProtocolVersion != 1 ||
		value.Format != "offline-authorization-v1" || value.Issuer != "edu-agent" || !validSignerKeyID(value.SignerKeyID) ||
		!validLearningUUID(value.PackID) || !validLearningUUID(value.DeviceID) || !validLearningUUID(value.OfflineActivityID) ||
		!validLearningUUID(value.SubmissionID) || !validLearningUUID(value.OperationID) ||
		!validBase64URLBytes(value.ServerOriginDigest, sha256.Size) || !validBase64URLBytes(value.ActivityPayloadDigest, sha256.Size) {
		return errors.New("offline authorization is invalid")
	}
	eligible, err := parseOfflineTimestamp(value.EligibleUntil)
	if err != nil {
		return err
	}
	archive, err := parseOfflineTimestamp(value.ArchiveUntil)
	if err != nil || archive.Before(eligible) {
		return errors.New("offline authorization window is invalid")
	}
	return nil
}

func validateOfflinePrepareResponse(value OfflinePrepareResponse) error {
	if !validLearningUUID(value.OperationID) || value.ManifestChain == nil || len(value.ManifestChain) > 16 {
		return errors.New("offline prepare response is invalid")
	}
	for _, manifest := range value.ManifestChain {
		if err := validateOfflineManifestEnvelope(manifest); err != nil {
			return err
		}
	}
	if err := validateOfflinePackEnvelope(value.Pack); err != nil {
		return err
	}
	if err := validateOfflinePrepareSignature(value.ResponseSignature); err != nil {
		return err
	}
	signature := value.ResponseSignature.Payload
	if signature.OperationID != value.OperationID || signature.Replayed != value.Replayed {
		return errors.New("offline prepare signature does not bind the response")
	}
	return nil
}

func validateOfflinePrepareBinding(value OfflinePrepareResponse, request OfflinePrepareRequest, status int) error {
	if value.OperationID != request.OperationID {
		return errors.New("offline prepare response belongs to another operation")
	}
	if (status == 200) != value.Replayed || (status != 200 && status != 201) {
		return errors.New("offline prepare HTTP replay semantics are invalid")
	}
	return nil
}

func validateOfflineManifestEnvelope(value OfflineSignerManifestEnvelope) error {
	if !validSignerKeyID(value.SignerKeyID) || !validBase64URLBytes(value.Signature, 64) {
		return errors.New("offline manifest envelope is invalid")
	}
	payload := value.Payload
	revision, revisionErr := parseUint63(payload.ManifestRevision, true)
	issuedAt, timeErr := parseOfflineTimestamp(payload.IssuedAt)
	if revisionErr != nil || revision == 0 || timeErr != nil || payload.ProtocolVersion != 1 || payload.Issuer != "edu-agent" ||
		!validOfflineBaseURL(payload.ServerBaseURL) || !validBase64URLBytes(payload.PreviousManifestDigest, sha256.Size) || len(payload.Keys) < 1 || len(payload.Keys) > 16 {
		return errors.New("offline manifest payload is invalid")
	}
	seen := map[string]bool{}
	for _, key := range payload.Keys {
		if seen[key.KeyID] || validateOfflineSignerKey(key, issuedAt) != nil {
			return errors.New("offline manifest key is invalid")
		}
		seen[key.KeyID] = true
	}
	return nil
}

func validateOfflineSignerKey(value OfflineSignerKey, issuedAt time.Time) error {
	notBefore, beforeErr := parseOfflineTimestamp(value.NotBefore)
	notAfter, afterErr := parseOfflineTimestamp(value.NotAfter)
	effective, effectiveErr := parseOfflineTimestamp(value.StatusEffectiveAt)
	if beforeErr != nil || afterErr != nil || effectiveErr != nil || !validSignerKeyID(value.KeyID) ||
		!validBase64URLBytes(value.PublicKey, 32) || !validBase64URLBytes(value.Fingerprint, sha256.Size) ||
		!validOfflineSignerStatus(value.Status) || !notAfter.After(notBefore) || issuedAt.Before(notBefore) || effective.After(notAfter) {
		return errors.New("offline signer key is invalid")
	}
	return nil
}

func validateOfflinePackEnvelope(value OfflinePackEnvelope) error {
	if !validSignerKeyID(value.SignerKeyID) || !validBase64URLBytes(value.Signature, 64) {
		return errors.New("offline pack envelope is invalid")
	}
	payload := value.Payload
	revision, revisionErr := parseUint63(payload.Revision, true)
	generation, generationErr := parseUint63(payload.LearnerGeneration, true)
	issuedAt, issuedErr := parseOfflineTimestamp(payload.IssuedAt)
	eligible, eligibleErr := parseOfflineTimestamp(payload.EligibleUntil)
	archive, archiveErr := parseOfflineTimestamp(payload.ArchiveUntil)
	if revisionErr != nil || revision != 1 || generationErr != nil || generation == 0 || issuedErr != nil || eligibleErr != nil || archiveErr != nil ||
		payload.ProtocolVersion != 1 || !validLearningUUID(payload.PackID) || !validLearningUUID(payload.DeviceID) || !validLearningUUID(payload.ParentSessionID) ||
		eligible.Before(issuedAt) || archive.Before(eligible) || len(payload.Items) < 1 || len(payload.Items) > 20 {
		return errors.New("offline pack payload is invalid")
	}
	if payload.Truncated {
		if !validOfflineTruncatedReason(payload.TruncatedReason) {
			return errors.New("offline pack truncation reason is invalid")
		}
	} else if payload.TruncatedReason != "" {
		return errors.New("untruncated offline pack has a reason")
	}
	var previous uint64
	seenOperations := map[string]bool{}
	seenSubmissions := map[string]bool{}
	for index, item := range payload.Items {
		if err := validateOfflineActivity(item.Activity); err != nil {
			return fmt.Errorf("offline pack item %d activity is invalid: %w", index, err)
		}
		if !validBase64URLBytes(item.ActivityPayloadDigest, sha256.Size) {
			return fmt.Errorf("offline pack item %d activity digest is invalid", index)
		}
		if !validSignerKeyID(item.Authorization.SignerKeyID) || !validBase64URLBytes(item.Authorization.Signature, 64) {
			return fmt.Errorf("offline pack item %d authorization envelope is invalid", index)
		}
		if err := validateOfflineAuthorization(item.Authorization.Payload); err != nil {
			return fmt.Errorf("offline pack item %d authorization is invalid: %w", index, err)
		}
		authorization := item.Authorization.Payload
		sequence, _ := parseUint63(authorization.DeviceSequence, true)
		if item.Authorization.SignerKeyID != authorization.SignerKeyID || item.Authorization.SignerKeyID != value.SignerKeyID ||
			authorization.PackID != payload.PackID || authorization.DeviceID != payload.DeviceID || authorization.LearnerGeneration != payload.LearnerGeneration ||
			authorization.OfflineActivityID != item.Activity.ActivityID || authorization.ActivityRevision != Uint63Decimal(strconv.FormatInt(item.Activity.Revision, 10)) ||
			authorization.ActivityPayloadDigest != item.ActivityPayloadDigest || authorization.EligibleUntil != payload.EligibleUntil || authorization.ArchiveUntil != payload.ArchiveUntil ||
			(index > 0 && sequence <= previous) || seenOperations[authorization.OperationID] || seenSubmissions[authorization.SubmissionID] {
			return errors.New("offline pack item binding is invalid")
		}
		previous = sequence
		seenOperations[authorization.OperationID] = true
		seenSubmissions[authorization.SubmissionID] = true
	}
	return nil
}

func validateOfflineActivity(value OfflineActivity) error {
	if !validLearningUUID(value.ActivityID) || value.Revision != 1 || !validLearningUUID(value.SessionID) || !validLearningUUID(value.GoalRevisionID) ||
		!validLearningUUID(value.RouteRevisionID) || !validLearningUUID(value.RouteStepID) || !validLearningUUID(value.KnowledgeRevisionID) ||
		!validLearningUUID(value.TargetNodeID) || !validLearningUUID(value.TargetNodeRevisionID) {
		return errors.New("offline activity identity is invalid")
	}
	if (value.Type != OfflineActivityObjective && value.Type != OfflineActivityOpen) || strings.TrimSpace(value.Prompt) == "" || len(value.Prompt) > 8000 || !utf8.ValidString(value.Prompt) ||
		value.Difficulty < 1 || value.Difficulty > 5 {
		return errors.New("offline activity presentation is invalid")
	}
	if len(value.AllowedHelp) < 1 || len(value.AllowedHelp) > 4 {
		return errors.New("offline activity help metadata is invalid")
	}
	if value.ActivityPolicyVersion == "" || value.AssessmentPolicyVersion == "" || value.ReviewPolicyVersion == "" {
		return errors.New("offline activity policy version is invalid")
	}
	if validateOfflineTimestamp(value.CreatedAt) != nil {
		return errors.New("offline activity creation time is invalid")
	}
	for _, optionalID := range []string{value.SourceProposalID, value.AttachedFreeQuestionID, value.AttachedFreeAnswerID} {
		if optionalID != "" && !validLearningUUID(optionalID) {
			return errors.New("offline activity optional ID is invalid")
		}
	}
	if len(value.KnowledgeReferences) < 1 || len(value.KnowledgeReferences) > 100 || validateOfflineRubric(value.Rubric, value.Type) != nil {
		return errors.New("offline activity content is invalid")
	}
	helpSeen := map[OfflineHelpLevel]bool{}
	targetFound := false
	for _, help := range value.AllowedHelp {
		if !validOfflineHelp(help) || helpSeen[help] {
			return errors.New("offline activity help is invalid")
		}
		helpSeen[help] = true
	}
	for _, reference := range value.KnowledgeReferences {
		if validateOfflineKnowledgeReference(reference, value.KnowledgeRevisionID) != nil {
			return errors.New("offline activity reference is invalid")
		}
		targetFound = targetFound || reference.NodeRevisionID == value.TargetNodeRevisionID
	}
	if !targetFound {
		return errors.New("offline activity target reference is missing")
	}
	return nil
}

func validateOfflineRubric(value OfflineRubric, activityType OfflineActivityType) error {
	if strings.TrimSpace(value.RubricRevision) == "" || len(value.Items) < 1 || len(value.Items) > 64 {
		return errors.New("offline rubric is invalid")
	}
	if activityType == OfflineActivityObjective && (value.ObjectiveRule == nil || len(value.ObjectiveRule.AcceptedAnswers) < 1) {
		return errors.New("offline objective rubric is invalid")
	}
	if activityType == OfflineActivityOpen && value.ObjectiveRule != nil {
		return errors.New("offline open rubric carries an objective rule")
	}
	seen := map[string]bool{}
	for _, item := range value.Items {
		if strings.TrimSpace(item.RubricItemID) == "" || strings.TrimSpace(item.Criterion) == "" || !utf8.ValidString(item.Criterion) || seen[item.RubricItemID] {
			return errors.New("offline rubric item is invalid")
		}
		seen[item.RubricItemID] = true
		referenceSeen := map[string]bool{}
		for _, id := range item.RequiredReferenceIDs {
			if !validLearningUUID(id) || referenceSeen[id] {
				return errors.New("offline rubric reference is invalid")
			}
			referenceSeen[id] = true
		}
	}
	if value.ObjectiveRule != nil {
		for _, answer := range value.ObjectiveRule.AcceptedAnswers {
			if !utf8.ValidString(answer) || strings.TrimSpace(answer) == "" {
				return errors.New("offline objective answer is invalid")
			}
		}
	}
	return nil
}

func validateOfflineKnowledgeReference(value OfflineKnowledgeReference, knowledgeRevisionID string) error {
	if value.KnowledgeRevisionID != knowledgeRevisionID || !validLearningUUID(value.NodeID) || !validLearningUUID(value.NodeRevisionID) ||
		(value.DocumentRevisionID != "" && !validLearningUUID(value.DocumentRevisionID)) || value.Range.Start < 0 || value.Range.End < value.Range.Start ||
		!validHexSHA256(value.SliceSHA256) || !utf8.ValidString(value.Slice) {
		return errors.New("offline knowledge reference is invalid")
	}
	return nil
}

func validateOfflinePrepareSignature(value OfflinePrepareResponseSignatureEnvelope) error {
	payload := value.Payload
	if !validSignerKeyID(value.SignerKeyID) || !validBase64URLBytes(value.Signature, 64) || payload.ProtocolVersion != 1 ||
		!validLearningUUID(payload.OperationID) || !validBase64URLBytes(payload.RequestHash, sha256.Size) || !validBase64URLBytes(payload.PackDigest, sha256.Size) ||
		!validBase64URLBytes(payload.ManifestDigest, sha256.Size) || validateOfflineTimestamp(payload.ResponseAt) != nil {
		return errors.New("offline prepare response signature is invalid")
	}
	if _, err := parseUint63(payload.ManifestRevision, true); err != nil {
		return errors.New("offline prepare manifest revision is invalid")
	}
	return nil
}

func validateOfflineSyncResponse(value OfflineSyncResponse) error {
	if !validLearningUUID(value.SyncRequestID) || len(value.Results) < 1 || len(value.Results) > OfflineMaxSyncItems {
		return errors.New("offline sync response is invalid")
	}
	for index := range value.Results {
		if err := validateOfflineSyncItemResult(value.Results[index]); err != nil {
			return fmt.Errorf("offline sync result %d is invalid: %w", index, err)
		}
	}
	return nil
}

func validateOfflineSyncBinding(value OfflineSyncResponse, request OfflineSyncRequest) error {
	if value.SyncRequestID != request.SyncRequestID || len(value.Results) != len(request.Operations) {
		return errors.New("offline sync response does not bind the request")
	}
	for index := range value.Results {
		result, operation := value.Results[index], request.Operations[index]
		if result.OperationID != operation.OperationID || result.SubmissionID != operation.SubmissionID || result.DeviceSequence != operation.DeviceSequence {
			return errors.New("offline sync result order or identity changed")
		}
	}
	return nil
}

func validateOfflineSyncItemResult(value OfflineSyncItemResult) error {
	if !validLearningUUID(value.OperationID) || !validLearningUUID(value.SubmissionID) || value.ReasonCodes == nil || len(value.ReasonCodes) > 16 {
		return errors.New("offline sync result identity is invalid")
	}
	if _, err := parseUint63(value.DeviceSequence, true); err != nil || !validOfflineReasons(value.ReasonCodes) {
		return errors.New("offline sync result sequence or reasons are invalid")
	}
	archived := value.ResultKind == OfflineResultArchived
	if archived {
		if value.IngestReceipt == nil || validateOfflineReceipt(*value.IngestReceipt) != nil || value.IngestReceipt.ArchiveStatus != value.ArchiveStatus ||
			validateOfflineStatusCombination(value.ArchiveStatus, value.AssessmentStatus, value.EvidenceStatus) != nil {
			return errors.New("archived offline sync result is invalid")
		}
		if value.ArchiveStatus == OfflineArchivedSucceeded {
			if value.StatusTicket == nil || validateOfflineTicket(*value.StatusTicket, value.OperationID) != nil {
				return errors.New("successful archive status ticket is invalid")
			}
		} else if value.StatusTicket != nil {
			return errors.New("rejected archive carries a status ticket")
		}
		return validateOptionalOfflineEntityIDs(value.AssessmentID, value.EvidenceID)
	}
	if value.AssessmentStatus != "" || value.EvidenceStatus != "" || value.IngestReceipt != nil || value.StatusTicket != nil || value.AssessmentID != "" || value.EvidenceID != "" {
		return errors.New("non-archived result carries archive fields")
	}
	switch value.ResultKind {
	case OfflineResultRetryable:
		if value.ArchiveStatus != OfflineNotArchivedRetryable {
			return errors.New("retryable archive status is invalid")
		}
	case OfflineResultBlocked:
		if value.ArchiveStatus != OfflineNotArchivedBlocked {
			return errors.New("blocked archive status is invalid")
		}
	case OfflineResultConflict:
		if value.ArchiveStatus != OfflineIdempotencyConflict && value.ArchiveStatus != OfflineSequenceConflict {
			return errors.New("conflict archive status is invalid")
		}
	case OfflineResultNotProcessed:
		if value.ArchiveStatus != OfflineNotProcessed {
			return errors.New("not-processed archive status is invalid")
		}
	default:
		return errors.New("offline result kind is invalid")
	}
	return nil
}

func validateOfflineOperationStatus(value OfflineOperationStatus) error {
	if !validLearningUUID(value.OperationID) || !validLearningUUID(value.SubmissionID) || value.ReasonCodes == nil || len(value.ReasonCodes) > 16 ||
		!validOfflineReasons(value.ReasonCodes) || validateOfflineReceipt(value.IngestReceipt) != nil || value.IngestReceipt.ArchiveStatus != value.ArchiveStatus ||
		validateOfflineTicket(value.StatusTicket, value.OperationID) != nil || validateOfflineStatusCombination(value.ArchiveStatus, value.AssessmentStatus, value.EvidenceStatus) != nil {
		return errors.New("offline operation status is invalid")
	}
	return validateOptionalOfflineEntityIDs(value.AssessmentID, value.EvidenceID)
}

func validateOfflineReceipt(value OfflineIngestReceipt) error {
	aggregate, aggregateErr := parseUint63(value.AggregateVersion, true)
	first, firstErr := parseUint63(value.FirstEventSequence, true)
	last, lastErr := parseUint63(value.LastEventSequence, true)
	projection, projectionErr := parseUint63(value.ProjectionAsOfEventSeq, true)
	if !validLearningUUID(value.ReceiptID) || validateOfflineTimestamp(value.ArchivedAt) != nil || aggregateErr != nil || firstErr != nil || lastErr != nil || projectionErr != nil ||
		aggregate == 0 || first == 0 || last < first || projection < last || (value.ArchiveStatus != OfflineArchivedSucceeded && value.ArchiveStatus != OfflineArchivedRejected) {
		return errors.New("offline ingest receipt is invalid")
	}
	return nil
}

func validateOfflineTicket(value OfflineStatusTicket, operationID string) error {
	if !validLearningUUID(value.TicketID) || value.OperationID != operationID || validateOfflineTimestamp(value.UpdatedAt) != nil {
		return errors.New("offline status ticket is invalid")
	}
	if _, err := parseUint63(value.Revision, true); err != nil {
		return errors.New("offline status ticket revision is invalid")
	}
	return nil
}

func validateOfflineStatusCombination(archive OfflineArchiveStatus, assessment OfflineAssessmentStatus, evidence OfflineEvidenceStatus) error {
	valid := false
	switch archive {
	case OfflineArchivedRejected:
		valid = assessment == OfflineAssessmentNotRequested && evidence == OfflineEvidenceUnchanged
	case OfflineArchivedSucceeded:
		switch assessment {
		case OfflineAssessmentNotRequested:
			valid = evidence == OfflineEvidenceProvisional || evidence == OfflineEvidenceNotEligible || evidence == OfflineEvidenceNotApplicable
		case OfflineAssessmentQueued, OfflineAssessmentProcessing, OfflineAssessmentPendingRetry:
			valid = evidence == OfflineEvidencePendingEvaluation
		case OfflineAssessmentCompleted:
			valid = evidence == OfflineEvidenceAccepted || evidence == OfflineEvidenceProvisional || evidence == OfflineEvidenceNotEligible
		case OfflineAssessmentFailed:
			valid = evidence == OfflineEvidenceUnchanged
		}
	}
	if !valid {
		return errors.New("offline archive, assessment, and evidence combination is invalid")
	}
	return nil
}

func validateOptionalOfflineEntityIDs(assessmentID, evidenceID string) error {
	if assessmentID != "" && !validLearningUUID(assessmentID) {
		return errors.New("offline assessment ID is invalid")
	}
	if evidenceID != "" && !validLearningUUID(evidenceID) {
		return errors.New("offline evidence ID is invalid")
	}
	return nil
}

func validateOfflineAssessmentPageContract(value OfflineAssessmentPage) error {
	if err := validateProjectionPageCursor(value.Metadata, value.Items, value.NextCursor); err != nil {
		return err
	}
	for _, item := range value.Items {
		if !validLearningUUID(item.AssessmentID) || !validLearningUUID(item.AttemptID) || !validLearningUUID(item.ActivityID) ||
			!validLearningUUID(item.SubmissionID) || item.Confidence < 0 || item.Confidence > 1000 ||
			item.Disposition != "provisional" || item.AttemptReceivedAt.IsZero() || item.AssessmentCreatedAt.IsZero() {
			return errors.New("offline assessment summary is invalid")
		}
		for _, version := range []Uint63Decimal{item.ActivityRevision, item.AggregateVersion, item.DispositionVersion} {
			if parsed, err := parseUint63(version, true); err != nil || parsed == 0 {
				return errors.New("offline assessment summary version is invalid")
			}
		}
		if !validOfflineAssessmentAllowed(item.AllowedDecisions, item.Confirmable) {
			return errors.New("offline assessment summary decisions are invalid")
		}
	}
	return nil
}

func validateOfflineAssessmentViewContract(value OfflineAssessmentView) error {
	if err := validateProjectionMetadata(value.Metadata); err != nil {
		return err
	}
	if !validLearningUUID(value.SubmissionID) || value.AllowedDecisions == nil || !validActivity(value.Activity) ||
		!validAttempt(value.Attempt) || !validAssessmentArtifact(value.Assessment) || !validAssessmentDecision(value.Decision) {
		return errors.New("offline assessment view is invalid")
	}
	if version, err := parseUint63(value.AggregateVersion, true); err != nil || version == 0 {
		return errors.New("offline assessment aggregate version is invalid")
	}
	if value.Activity.Type != "open" || value.Attempt.ArchiveDisposition != "offline_succeeded" ||
		value.Attempt.OfflineSubmissionID != value.SubmissionID || value.Attempt.ActivityID != value.Activity.ActivityID ||
		value.Attempt.ActivityRevision != value.Activity.Revision || value.Assessment.AttemptID != value.Attempt.AttemptID ||
		value.Assessment.ActivityID != value.Activity.ActivityID || value.Assessment.ActivityRevision != value.Activity.Revision ||
		value.Decision.AssessmentID != value.Assessment.AssessmentID || !assessmentDecisionMatchesArtifact(value.Assessment, value.Decision) {
		return errors.New("offline assessment ownership binding is invalid")
	}
	if !validOfflineAssessmentAllowed(value.AllowedDecisions, value.Confirmable) {
		return errors.New("offline assessment allowed decisions are invalid")
	}
	if value.Decision.Disposition == "provisional" && value.Assessment.EvidenceEligibility && value.Attempt.EvidenceEligibility {
		if len(value.AllowedDecisions) < 2 {
			return errors.New("offline provisional assessment decisions are incomplete")
		}
	} else if len(value.AllowedDecisions) != 0 || value.Confirmable {
		return errors.New("resolved offline assessment still allows decisions")
	}
	return nil
}

func validateOfflineAssessmentDecisionRequestContract(assessmentID string, request OfflineAssessmentDecisionRequest) error {
	if !validLearningUUID(assessmentID) || request == nil {
		return errors.New("offline assessment decision identity is invalid")
	}
	base, kind, reason, items, ok := offlineAssessmentDecisionParts(request)
	if !ok || !validLearningUUID(base.OperationID) || !validLearningUUID(base.AttemptID) || base.PayloadSchemaVersion != 1 || base.Kind != kind {
		return errors.New("offline assessment decision envelope is invalid")
	}
	if expected, err := parseUint63(base.ExpectedVersion, true); err != nil || expected == 0 {
		return errors.New("offline assessment expected version is invalid")
	}
	if expected, err := parseUint63(base.ExpectedDispositionVersion, true); err != nil || expected == 0 {
		return errors.New("offline assessment disposition version is invalid")
	}
	if !utf8.ValidString(reason) || strings.TrimSpace(reason) != reason || utf8.RuneCountInString(reason) > MaxOfflineAssessmentDecisionReasonRunes {
		return errors.New("offline assessment reason is invalid")
	}
	switch kind {
	case "confirm":
		if reason != "" || len(items) != 0 {
			return errors.New("offline assessment confirm shape is invalid")
		}
	case "override":
		if reason == "" || len(items) < 1 || len(items) > 64 {
			return errors.New("offline assessment override shape is invalid")
		}
		seen := map[string]bool{}
		for _, item := range items {
			if !utf8.ValidString(item.RubricItemID) || strings.TrimSpace(item.RubricItemID) == "" || utf8.RuneCountInString(item.RubricItemID) > MaxOfflineAssessmentRubricItemIDRunes || seen[item.RubricItemID] ||
				(item.Conclusion != "pass" && item.Conclusion != "partial" && item.Conclusion != "fail") ||
				!utf8.ValidString(item.MisconceptionCandidate) || utf8.RuneCountInString(item.MisconceptionCandidate) > MaxOfflineAssessmentMisconceptionRunes {
				return errors.New("offline assessment override item is invalid")
			}
			seen[item.RubricItemID] = true
		}
	case "void":
		if reason == "" || len(items) != 0 {
			return errors.New("offline assessment void shape is invalid")
		}
	default:
		return errors.New("offline assessment decision kind is invalid")
	}
	return nil
}

func validateOfflineAssessmentDecisionReceiptContract(value OfflineAssessmentDecisionReceipt, assessmentID string, request OfflineAssessmentDecisionRequest, status int) error {
	base, kind, _, _, ok := offlineAssessmentDecisionParts(request)
	if !ok || value.OperationID != base.OperationID || value.Decision.DecisionID != base.OperationID ||
		value.AssessmentID != assessmentID || value.Decision.AssessmentID != assessmentID || value.AttemptID != base.AttemptID ||
		!validLearningUUID(value.SubmissionID) || !validAssessmentDecision(value.Decision) ||
		(status == http.StatusOK) != value.Replayed || (status != http.StatusOK && status != http.StatusCreated) {
		return errors.New("offline assessment decision receipt binding is invalid")
	}
	expectedDisposition, _ := parseUint63(base.ExpectedDispositionVersion, true)
	if value.Decision.Version != int64(expectedDisposition)+1 {
		return errors.New("offline assessment decision receipt version is invalid")
	}
	for _, version := range []Uint63Decimal{value.AggregateVersion, value.FirstEventSequence, value.LastEventSequence, value.ProjectionAsOfEventSequence} {
		if parsed, err := parseUint63(version, true); err != nil || parsed == 0 {
			return errors.New("offline assessment decision receipt sequence is invalid")
		}
	}
	first, _ := parseUint63(value.FirstEventSequence, true)
	last, _ := parseUint63(value.LastEventSequence, true)
	projection, _ := parseUint63(value.ProjectionAsOfEventSequence, true)
	if last < first || projection < last {
		return errors.New("offline assessment decision receipt range is invalid")
	}
	switch kind {
	case "confirm":
		if value.Decision.Disposition != "accepted" || value.Decision.ProducedEvidenceID == "" {
			return errors.New("offline assessment confirmation receipt is invalid")
		}
	case "override":
		if value.Decision.Disposition != "overridden" || value.Decision.ProducedEvidenceID == "" {
			return errors.New("offline assessment override receipt is invalid")
		}
	case "void":
		if value.Decision.Disposition != "voided" || value.Decision.ProducedEvidenceID != "" {
			return errors.New("offline assessment void receipt is invalid")
		}
	}
	return nil
}

func offlineAssessmentDecisionParts(request OfflineAssessmentDecisionRequest) (OfflineAssessmentDecisionBase, string, string, []OfflineAssessmentOverrideItem, bool) {
	switch value := request.(type) {
	case OfflineAssessmentConfirmRequest:
		return value.OfflineAssessmentDecisionBase, "confirm", "", nil, true
	case OfflineAssessmentOverrideRequest:
		return value.OfflineAssessmentDecisionBase, "override", value.Reason, value.Items, true
	case OfflineAssessmentVoidRequest:
		return value.OfflineAssessmentDecisionBase, "void", value.Reason, nil, true
	default:
		return OfflineAssessmentDecisionBase{}, "", "", nil, false
	}
}

func validOfflineAssessmentAllowed(values []string, confirmable bool) bool {
	if values == nil {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] || (value != "confirm" && value != "override" && value != "void") {
			return false
		}
		seen[value] = true
	}
	if confirmable != seen["confirm"] {
		return false
	}
	return true
}

func parseUint63(value Uint63Decimal, positive bool) (uint64, error) {
	text := string(value)
	if text == "" || (len(text) > 1 && text[0] == '0') || len(text) > 19 {
		return 0, errors.New("uint63 decimal is not canonical")
	}
	parsed, err := strconv.ParseUint(text, 10, 63)
	if err != nil || parsed > maxUint63 || (positive && parsed == 0) {
		return 0, errors.New("uint63 decimal is out of range")
	}
	return parsed, nil
}

func parseOfflineTimestamp(value OfflineTimestamp) (time.Time, error) {
	text := string(value)
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || !strings.HasSuffix(text, "Z") || parsed.UTC().Format(time.RFC3339Nano) != text {
		return time.Time{}, errors.New("timestamp is not canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func validateOfflineTimestamp(value OfflineTimestamp) error {
	_, err := parseOfflineTimestamp(value)
	return err
}

func validBase64URLBytes(value string, size int) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validHexSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func validSignerKeyID(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value && utf8.ValidString(value)
}

func validOfflineBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Scheme == strings.ToLower(parsed.Scheme) && parsed.Host != "" &&
		parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validOfflineSignerStatus(value OfflineSignerKeyStatus) bool {
	return value == OfflineSignerKeyActive || value == OfflineSignerKeyVerifyOnly || value == OfflineSignerKeyRetired
}

func validOfflineHelp(value OfflineHelpLevel) bool {
	return value == OfflineHelpNone || value == OfflineHelpHint || value == OfflineHelpScaffold || value == OfflineHelpAnswerRevealed
}

func validOfflineTruncatedReason(value OfflinePackTruncatedReason) bool {
	switch value {
	case OfflinePackCurrentActivityOnly, OfflinePackRequestedLimited, OfflinePackActivitySizeLimited,
		OfflinePackSizeLimited, OfflinePackModelPartial, OfflinePackRouteExhausted, OfflinePackReviewExhausted:
		return true
	default:
		return false
	}
}

func validOfflineSkipReason(value OfflineSkipReason) bool {
	return value == OfflineUserSkipped || value == OfflineExpiredLocally || value == OfflineUnreadableLocalItem
}

func validOfflineReasons(values []OfflineReasonCode) bool {
	seen := map[OfflineReasonCode]bool{}
	for _, value := range values {
		if seen[value] || !validOfflineReason(value) {
			return false
		}
		seen[value] = true
	}
	return true
}

func validOfflineReason(value OfflineReasonCode) bool {
	switch value {
	case OfflineReasonDuplicateActivity, OfflineReasonStaleKnowledge, OfflineReasonExpiredActivity, OfflineReasonStaleContext,
		OfflineReasonStalePolicy, OfflineReasonAnswerRevealed, OfflineReasonModelUnavailable, OfflineReasonEvaluationInvalid,
		OfflineReasonActivityInvalid, OfflineReasonContentRedacted, OfflineReasonPrivacyClearing, OfflineReasonDeviceRevoked,
		OfflineReasonAuthorizationExpired, OfflineReasonAuthorizationInvalid, OfflineReasonVersionConflict,
		OfflineReasonIdempotencyConflict, OfflineReasonSequenceConflict, OfflineReasonNotProcessed, OfflineReasonInternalError:
		return true
	default:
		return false
	}
}
