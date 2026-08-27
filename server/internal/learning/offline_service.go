package learning

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
)

type OfflineRuntimeStore interface {
	OfflinePrepareStore
	IngestOffline(context.Context, OfflineIngestRequest) (OfflineIngestResult, error)
	OfflineOperationStatus(context.Context, string, string) (OfflineOperationStatus, error)
}

type OfflineService struct {
	store        OfflineRuntimeStore
	generator    OfflinePrepareGenerator
	assessments  OfflineAssessmentService
	signer       OfflineSigner
	origin       string
	originDigest string
	now          func() time.Time
}

func NewOfflineService(store OfflineRuntimeStore, signer OfflineSigner, rawOrigin string, now func() time.Time) (*OfflineService, error) {
	return NewOfflineServiceWithGenerator(store, nil, signer, rawOrigin, now)
}

func NewOfflineServiceWithGenerator(store OfflineRuntimeStore, generator OfflinePrepareGenerator, signer OfflineSigner, rawOrigin string, now func() time.Time) (*OfflineService, error) {
	if store == nil {
		return nil, errors.New("offline runtime store is required")
	}
	origin, err := normalizeOfflineOrigin(rawOrigin)
	if err != nil {
		return nil, err
	}
	if signer != nil && signer.Origin() != origin {
		return nil, errors.New("offline signer origin does not match service origin")
	}
	if now == nil {
		now = time.Now
	}
	var assessments OfflineAssessmentService
	if candidate, ok := generator.(OfflineAssessmentService); ok {
		assessments = candidate
	}
	return &OfflineService{store: store, generator: generator, assessments: assessments, signer: signer, origin: origin, originDigest: offlineBase64Digest([]byte(origin)), now: now}, nil
}

func (s *OfflineService) Available() bool       { return s != nil && s.store != nil }
func (s *OfflineService) SignerAvailable() bool { return s != nil && s.signer != nil }

type offlineBootstrapStore interface {
	OfflineLearnerGeneration(context.Context) (uint64, error)
}

func (s *OfflineService) PairingBootstrap(ctx context.Context) (OfflinePairingBootstrap, error) {
	if s == nil || s.signer == nil {
		return OfflinePairingBootstrap{}, &Error{Code: CodeOfflineSignerUnavailable, Reason: "offline_signer_unavailable"}
	}
	store, ok := s.store.(offlineBootstrapStore)
	if !ok {
		return OfflinePairingBootstrap{}, errors.New("offline bootstrap store is unavailable")
	}
	generation, err := store.OfflineLearnerGeneration(ctx)
	if err != nil {
		return OfflinePairingBootstrap{}, err
	}
	return OfflinePairingBootstrap{
		ProtocolVersion:   1,
		LearnerGeneration: strconv.FormatUint(generation, 10),
		ServerBaseURL:     s.origin,
		SignerManifest:    s.signer.RootManifestEnvelope(),
	}, nil
}

func (s *OfflineService) Prepare(ctx context.Context, deviceID string, request OfflinePrepareRequest) (OfflinePrepareResponse, error) {
	if s == nil || s.store == nil {
		return OfflinePrepareResponse{}, &Error{Code: CodeOfflinePrepareUnavailable}
	}
	if s.signer == nil {
		return OfflinePrepareResponse{}, &Error{Code: CodeOfflineSignerUnavailable}
	}
	if uuid.Validate(deviceID) != nil || request.Validate() != nil {
		return OfflinePrepareResponse{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_prepare_request"}
	}
	trustedRevision, _ := ParseUint63Decimal(request.TrustedManifestRevision)
	manifestChain, err := s.signer.ManifestChain(trustedRevision, request.TrustedManifestDigest)
	if err != nil {
		return OfflinePrepareResponse{}, err
	}
	count, ttl := request.Limits()
	storeRequest := OfflinePrepareStoreRequest{DeviceID: deviceID, Request: request, Count: count, TTL: ttl}
	claim, err := s.store.ClaimOfflinePrepare(ctx, storeRequest)
	if err != nil {
		return OfflinePrepareResponse{}, err
	}
	var prepared OfflinePreparedPack
	switch claim.State {
	case "published":
		if claim.Prepared == nil {
			return OfflinePrepareResponse{}, errors.New("offline prepare replay omitted the published pack")
		}
		prepared = *claim.Prepared
	case "busy":
		return OfflinePrepareResponse{}, &Error{Code: CodeOfflinePrepareUnavailable, Reason: "offline_prepare_in_progress"}
	case "claimed":
		artifact := claim.Artifact
		if artifact == nil {
			if claim.Generation == nil {
				return OfflinePrepareResponse{}, errors.New("offline prepare claim omitted generation context")
			}
			var generated OfflinePrepareArtifact
			if s.generator == nil {
				if claim.Generation.CurrentActivity == nil {
					err = &Error{Code: CodeModelUnavailable, Reason: OfflineReasonModelUnavailable}
				} else {
					generated = offlineCurrentActivityArtifact(*claim.Generation)
				}
			} else {
				generated, err = s.generator.GenerateOfflinePrepare(ctx, *claim.Generation)
			}
			if err != nil {
				if rejectErr := s.store.RejectOfflinePrepare(ctx, deviceID, request.OperationID, claim.LeaseToken, err); rejectErr != nil {
					return OfflinePrepareResponse{}, rejectErr
				}
				return OfflinePrepareResponse{}, err
			}
			if err := s.store.StoreOfflinePrepareArtifact(ctx, deviceID, request.OperationID, claim.LeaseToken, generated); err != nil {
				return OfflinePrepareResponse{}, err
			}
			artifact = &generated
		}
		if artifact.ProtocolVersion != 1 || len(artifact.Activities) == 0 {
			return OfflinePrepareResponse{}, errors.New("offline prepare artifact is invalid")
		}
		prepared, err = s.store.PublishOfflinePrepare(ctx, storeRequest, claim.LeaseToken, s.signer)
		if err != nil {
			return OfflinePrepareResponse{}, err
		}
	default:
		return OfflinePrepareResponse{}, errors.New("offline prepare claim state is invalid")
	}
	requestHash, err := decodeOfflineRequestHash(prepared.RequestHash)
	if err != nil {
		return OfflinePrepareResponse{}, err
	}
	manifestRevision, err := FormatUint63Decimal(s.signer.ManifestRevision())
	if err != nil {
		return OfflinePrepareResponse{}, err
	}
	responseAt := s.now().UTC().Truncate(time.Microsecond)
	responseSignature, err := s.signer.Sign(OfflinePrepareResponseDomain, OfflinePrepareResponseSignaturePayloadV1{
		ProtocolVersion:  1,
		OperationID:      prepared.OperationID,
		RequestHash:      requestHash,
		Replayed:         prepared.Replayed,
		PackDigest:       prepared.PackDigest,
		ManifestRevision: manifestRevision,
		ManifestDigest:   s.signer.ManifestDigest(),
		ResponseAt:       responseAt,
	})
	if err != nil {
		return OfflinePrepareResponse{}, err
	}
	return OfflinePrepareResponse{
		OperationID:       prepared.OperationID,
		Replayed:          prepared.Replayed,
		Pack:              cloneOfflineEnvelope(prepared.Pack),
		ManifestChain:     manifestChain,
		ResponseSignature: responseSignature,
	}, nil
}

func offlineCurrentActivityArtifact(request OfflinePrepareGenerationRequest) OfflinePrepareArtifact {
	activities := []Activity{CloneActivity(*request.CurrentActivity)}
	return OfflinePrepareArtifact{
		ProtocolVersion: 1, SessionID: request.SessionID, SessionState: request.SessionState,
		ExpectedSessionVersion: request.ExpectedSessionVersion, GoalRevisionID: request.GoalRevisionID,
		RouteRevisionID: request.Route.ID, RouteStepID: request.RouteStepID,
		KnowledgeRevisionID: request.KnowledgeRevisionID,
		Activities:          activities, ModelPartial: request.Count > len(activities),
	}
}

func (s *OfflineService) Sync(ctx context.Context, deviceID string, request OfflineSyncRequest) (OfflineSyncResponse, error) {
	if s == nil || s.store == nil || uuid.Validate(deviceID) != nil {
		return OfflineSyncResponse{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_sync_request"}
	}
	if err := request.Validate(); err != nil {
		return OfflineSyncResponse{}, err
	}
	operations := make([]OfflineOperation, len(request.Operations))
	var previous uint64
	for index, raw := range request.Operations {
		operation, err := DecodeOfflineOperationWire(raw)
		if err != nil {
			return OfflineSyncResponse{}, err
		}
		if _, err := BindOfflineAuthorization(&operation, deviceID, s.origin); err != nil {
			return OfflineSyncResponse{}, err
		}
		if index > 0 && operation.DeviceSequence <= previous {
			return OfflineSyncResponse{}, &Error{Code: CodeInvalidRequest, Reason: "offline_operations_not_sorted"}
		}
		previous = operation.DeviceSequence
		operations[index] = operation
	}

	response := OfflineSyncResponse{SyncRequestID: request.SyncRequestID, Results: make([]OfflineIngestResult, 0, len(operations))}
	for index, operation := range operations {
		result, err := s.store.IngestOffline(ctx, OfflineIngestRequest{Operation: operation})
		if err != nil {
			result = offlineRuntimeFailure(operation, err)
			response.Results = append(response.Results, result)
			for _, pending := range operations[index+1:] {
				response.Results = append(response.Results, offlineNotProcessedResult(pending))
			}
			break
		}
		response.Results = append(response.Results, result)
		if result.ResultKind == OfflineResultBlocked {
			for _, pending := range operations[index+1:] {
				response.Results = append(response.Results, offlineNotProcessedResult(pending))
			}
			break
		}
	}
	return response, nil
}

func (s *OfflineService) Status(ctx context.Context, deviceID, operationID string) (OfflineOperationStatus, error) {
	if s == nil || s.store == nil || uuid.Validate(deviceID) != nil || uuid.Validate(operationID) != nil || operationID != stringsLower(operationID) {
		return OfflineOperationStatus{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_id"}
	}
	return s.store.OfflineOperationStatus(ctx, deviceID, operationID)
}

func (s *OfflineService) ListOfflineAssessments(ctx context.Context, deviceID string, query OfflineAssessmentQuery) (OfflineAssessmentPage, error) {
	if s == nil || s.assessments == nil {
		return OfflineAssessmentPage{}, &Error{Code: CodeProjectionUnavailable, Reason: "offline_assessment_service_unavailable"}
	}
	return s.assessments.ListOfflineAssessments(ctx, deviceID, query)
}

func (s *OfflineService) OfflineAssessment(ctx context.Context, deviceID, assessmentID string) (OfflineAssessmentView, error) {
	if s == nil || s.assessments == nil {
		return OfflineAssessmentView{}, &Error{Code: CodeProjectionUnavailable, Reason: "offline_assessment_service_unavailable"}
	}
	return s.assessments.OfflineAssessment(ctx, deviceID, assessmentID)
}

func (s *OfflineService) DecideOfflineAssessment(ctx context.Context, deviceID, assessmentID string, command OfflineAssessmentDecisionCommand) (OfflineAssessmentDecisionReceipt, error) {
	if s == nil || s.assessments == nil {
		return OfflineAssessmentDecisionReceipt{}, &Error{Code: CodeProjectionUnavailable, Reason: "offline_assessment_service_unavailable"}
	}
	return s.assessments.DecideOfflineAssessment(ctx, deviceID, assessmentID, command)
}

func offlineRuntimeFailure(operation OfflineOperation, err error) OfflineIngestResult {
	deviceSequence, _ := FormatUint63Decimal(operation.DeviceSequence)
	reason := OfflineReasonInternalError
	kind := OfflineResultRetryable
	archive := OfflineNotArchivedRetry
	var privacyError *privacy.Error
	if errors.As(err, &privacyError) {
		kind = OfflineResultBlocked
		archive = OfflineNotArchivedBlocked
		switch privacyError.Code {
		case privacy.CodeContentRedacted:
			reason = OfflineReasonContentRedacted
		case privacy.CodePrivacyClearInProgress:
			reason = OfflineReasonPrivacyClearing
		}
	}
	return OfflineIngestResult{
		ResultKind:     kind,
		OperationID:    operation.OperationID,
		DeviceSequence: deviceSequence,
		SubmissionID:   operation.SubmissionID,
		ArchiveStatus:  archive,
		ReasonCodes:    []string{reason},
	}
}

func offlineNotProcessedResult(operation OfflineOperation) OfflineIngestResult {
	deviceSequence, _ := FormatUint63Decimal(operation.DeviceSequence)
	return OfflineIngestResult{
		ResultKind:     OfflineResultNotProcessed,
		OperationID:    operation.OperationID,
		DeviceSequence: deviceSequence,
		SubmissionID:   operation.SubmissionID,
		ArchiveStatus:  OfflineNotProcessed,
		ReasonCodes:    []string{OfflineReasonNotProcessed},
	}
}

func stringsLower(value string) string {
	for index := 0; index < len(value); index++ {
		if value[index] >= 'A' && value[index] <= 'Z' {
			return strings.ToLower(value)
		}
	}
	return value
}
