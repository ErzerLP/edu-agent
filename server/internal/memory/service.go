package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
)

const defaultDeliveryTTL = 24 * time.Hour

type DevicePrincipal struct {
	DeviceID string
}

func (p DevicePrincipal) Validate() error {
	if !validUUID(p.DeviceID) {
		return invalid("invalid_device_principal")
	}
	return nil
}

type ModelPrincipal struct {
	DeviceID       string
	ProposerID     string
	ModelID        string
	PromptRevision string
}

func (p ModelPrincipal) Validate() error {
	if !validUUID(p.DeviceID) || !validUUID(p.ProposerID) || !validText(p.ModelID, MaxReferenceBytes, MaxReferenceBytes) || !validText(p.PromptRevision, MaxReferenceBytes, MaxReferenceBytes) {
		return invalid("invalid_model_principal")
	}
	return nil
}

type CreateCandidateCommand struct {
	OperationID       string
	Content           string
	SourceEventID     string
	SourceOperationID string
	Reason            string
	Category          Category
	Sensitivity       Sensitivity
	Stability         Stability
	ValidUntil        time.Time
}

type CreateModelCandidateCommand struct {
	OperationID              string
	LogicalMemoryID          string
	ExpectedRevision         int64
	ExpectedRecordGeneration int64
	Content                  string
	Source                   SourceKind
	SourceEventID            string
	SourceOperationID        string
	SourceHashes             []string
	Reason                   string
	Category                 Category
	Sensitivity              Sensitivity
	Stability                Stability
	ValidUntil               time.Time
}

type CreateCorrectionCandidateCommand struct {
	OperationID              string
	LogicalMemoryID          string
	ExpectedRevision         int64
	ExpectedRecordGeneration int64
	Content                  string
	SourceEventID            string
	SourceOperationID        string
	Reason                   string
	Category                 Category
	Sensitivity              Sensitivity
	Stability                Stability
	ValidUntil               time.Time
}

type DecideCandidateCommand struct {
	OperationID              string
	CandidateID              string
	ExpectedRevision         int64
	ExpectedRecordRevision   int64
	ExpectedRecordGeneration int64
	Decision                 Decision
	Reason                   string
}

type DeleteRecordCommand struct {
	OperationID              string
	LogicalMemoryID          string
	ExpectedRevision         int64
	ExpectedRecordGeneration int64
}

type ReplayDeliveryCommand struct {
	OperationID string
	DeliveryID  string
}

type CreatePlan struct {
	Operation                Operation
	Candidate                Candidate
	Content                  string
	Correction               bool
	ExpectedRecordRevision   int64
	ExpectedRecordGeneration int64
	AutomaticDecision        *CandidateDecision
	LogicalMemoryID          string
	RecordRevisionID         string
	DeliveryID               string
	DeliveryPayloadID        string
	ReceiptID                string
	OutboxID                 string
}

type DecisionPlan struct {
	Operation                Operation
	CandidateID              string
	ExpectedRevision         int64
	ExpectedRecordRevision   int64
	ExpectedRecordGeneration int64
	Decision                 CandidateDecision
	LogicalMemoryID          string
	RecordRevisionID         string
	DeliveryID               string
	DeliveryPayloadID        string
	ReceiptID                string
	OutboxID                 string
}

type DeletePlan struct {
	Operation                Operation
	LogicalMemoryID          string
	ExpectedRevision         int64
	ExpectedRecordGeneration int64
	DeliveryID               string
	DeliveryPayloadID        string
	ReceiptID                string
	OutboxID                 string
	ValidUntil               time.Time
}

type ServiceOptions struct {
	Now            func() time.Time
	NewID          func() string
	ModelPrincipal *ModelPrincipal
	ReadPermits    *privacy.ReadPermitManager
	DeliveryTTL    time.Duration
}

type Service struct {
	store          Store
	now            func() time.Time
	newID          func() string
	modelPrincipal *ModelPrincipal
	readPermits    *privacy.ReadPermitManager
	deliveryTTL    time.Duration
}

func NewService(store Store, options ServiceOptions) (*Service, error) {
	if store == nil {
		return nil, errors.New("memory store is required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewID == nil {
		options.NewID = func() string { return uuid.NewString() }
	}
	if options.ReadPermits == nil {
		options.ReadPermits = privacy.DefaultReadPermits
	}
	if options.DeliveryTTL == 0 {
		options.DeliveryTTL = defaultDeliveryTTL
	}
	if options.DeliveryTTL < 0 {
		return nil, errors.New("memory delivery TTL must be positive")
	}
	if options.ModelPrincipal != nil {
		if err := options.ModelPrincipal.Validate(); err != nil {
			return nil, err
		}
		principal := *options.ModelPrincipal
		options.ModelPrincipal = &principal
	}
	return &Service{
		store: store, now: options.Now, newID: options.NewID,
		modelPrincipal: options.ModelPrincipal, readPermits: options.ReadPermits,
		deliveryTTL: options.DeliveryTTL,
	}, nil
}

func (s *Service) CreateCandidate(ctx context.Context, principal DevicePrincipal, command CreateCandidateCommand) (OperationResult, error) {
	if err := principal.Validate(); err != nil {
		return OperationResult{}, err
	}
	reference := SourceReference{EventID: command.SourceEventID, OperationID: command.SourceOperationID}
	return s.createCandidate(ctx, principal.DeviceID, command.OperationID, command.Content, SourceUserStatement,
		reference, principal.DeviceID, command.Reason, command.Category, command.Sensitivity,
		command.Stability, command.ValidUntil, false, "", 0, 0)
}

func (s *Service) CreateModelCandidate(ctx context.Context, command CreateModelCandidateCommand) (OperationResult, error) {
	if s.modelPrincipal == nil {
		return OperationResult{}, &Error{Code: CodeMemoryUnavailable, Reason: "model_principal_not_configured"}
	}
	if command.Source != SourceModelInference && command.Source != SourceLongTermBackground && command.Source != SourceGeneratedSummary {
		return OperationResult{}, invalid("invalid_model_source")
	}
	reference := SourceReference{
		EventID: command.SourceEventID, OperationID: command.SourceOperationID,
		ModelID: s.modelPrincipal.ModelID, PromptRevision: s.modelPrincipal.PromptRevision,
		SourceHashes: append([]string(nil), command.SourceHashes...),
	}
	correction := command.LogicalMemoryID != "" || command.ExpectedRevision != 0 || command.ExpectedRecordGeneration != 0
	return s.createCandidate(ctx, s.modelPrincipal.DeviceID, command.OperationID, command.Content, command.Source,
		reference, s.modelPrincipal.ProposerID, command.Reason, command.Category, command.Sensitivity,
		command.Stability, command.ValidUntil, correction, command.LogicalMemoryID,
		command.ExpectedRevision, command.ExpectedRecordGeneration)
}

func (s *Service) CreateCorrectionCandidate(
	ctx context.Context,
	principal DevicePrincipal,
	command CreateCorrectionCandidateCommand,
) (OperationResult, error) {
	if err := principal.Validate(); err != nil {
		return OperationResult{}, err
	}
	reference := SourceReference{EventID: command.SourceEventID, OperationID: command.SourceOperationID}
	return s.createCandidate(ctx, principal.DeviceID, command.OperationID, command.Content, SourceUserStatement,
		reference, principal.DeviceID, command.Reason, command.Category, command.Sensitivity,
		command.Stability, command.ValidUntil, true, command.LogicalMemoryID,
		command.ExpectedRevision, command.ExpectedRecordGeneration)
}

type createCandidateRequestV1 struct {
	SchemaVersion            string          `json:"schema_version"`
	OperationKind            OperationKind   `json:"operation_kind"`
	Content                  string          `json:"content"`
	Source                   SourceKind      `json:"source_kind"`
	SourceReference          SourceReference `json:"source_reference"`
	ProposerID               string          `json:"proposer_id"`
	Reason                   string          `json:"reason"`
	Category                 Category        `json:"category"`
	Sensitivity              Sensitivity     `json:"sensitivity"`
	Stability                Stability       `json:"stability"`
	ValidUntil               time.Time       `json:"valid_until"`
	LogicalMemoryID          string          `json:"logical_memory_id,omitempty"`
	ExpectedRecordRevision   int64           `json:"expected_record_revision,omitempty"`
	ExpectedRecordGeneration int64           `json:"expected_record_generation,omitempty"`
}

func (s *Service) createCandidate(
	ctx context.Context,
	deviceID, operationID, content string,
	source SourceKind,
	reference SourceReference,
	proposerID, reason string,
	category Category,
	sensitivity Sensitivity,
	stability Stability,
	validUntil time.Time,
	correction bool,
	logicalMemoryID string,
	expectedRecordRevision, expectedRecordGeneration int64,
) (OperationResult, error) {
	if !validUUID(deviceID) || !validUUID(operationID) || !validUUID(proposerID) || validUntil.IsZero() || !isUTC(validUntil) {
		return OperationResult{}, invalid("invalid_create_candidate_command")
	}
	if correction {
		if !validUUID(logicalMemoryID) || expectedRecordRevision < 1 || expectedRecordGeneration < 1 {
			return OperationResult{}, invalid("invalid_correction_candidate_target")
		}
	} else if logicalMemoryID != "" || expectedRecordRevision != 0 || expectedRecordGeneration != 0 {
		return OperationResult{}, invalid("unexpected_correction_candidate_target")
	}
	if err := ValidateProposedContent(category, content); err != nil {
		return OperationResult{}, err
	}
	schemaVersion := "memory-create-candidate-v1"
	if correction {
		schemaVersion = "memory-create-correction-candidate-v1"
	}
	requestHash, err := CanonicalRequestHash(createCandidateRequestV1{
		SchemaVersion: schemaVersion, OperationKind: OperationCreateCandidate,
		Content: content, Source: source, SourceReference: reference, ProposerID: proposerID,
		Reason: reason, Category: category, Sensitivity: sensitivity, Stability: stability, ValidUntil: validUntil,
		LogicalMemoryID: logicalMemoryID, ExpectedRecordRevision: expectedRecordRevision,
		ExpectedRecordGeneration: expectedRecordGeneration,
	})
	if err != nil {
		return OperationResult{}, fmt.Errorf("hash memory candidate request: %w", err)
	}
	operation := Operation{DeviceID: deviceID, OperationID: operationID, RequestHash: requestHash, Kind: OperationCreateCandidate}
	now := s.now().UTC()
	candidateID := s.newID()
	candidate := Candidate{
		ID: candidateID, URI: CandidateURI(candidateID), LogicalMemoryID: logicalMemoryID,
		PayloadID: s.newID(), ContentHash: SHA256String(content),
		Source: source, SourceReference: reference, ProposerID: proposerID,
		Reason: reason, Category: category, Sensitivity: sensitivity, Stability: stability,
		ValidUntil: validUntil, PolicyVersion: AdmissionPolicyVersion,
		Status: CandidatePending, Revision: 1, CreatedAt: now,
	}
	if err := candidate.Validate(); err != nil {
		return OperationResult{}, err
	}
	plan := CreatePlan{
		Operation: operation, Candidate: candidate, Content: content, Correction: correction,
		ExpectedRecordRevision: expectedRecordRevision, ExpectedRecordGeneration: expectedRecordGeneration,
		LogicalMemoryID: logicalMemoryID,
	}
	automaticStatus := EvaluateAdmission(candidate, content, now)
	if automaticStatus != CandidatePending {
		candidate.Status = automaticStatus
		candidate.Revision = 2
		decisionKind := DecisionAdmit
		decisionReason := "automatic_policy_match"
		if automaticStatus == CandidateRejected {
			decisionKind = DecisionReject
			decisionReason = "automatic_policy_forbidden_content"
		}
		decision := CandidateDecision{
			ID: s.newID(), CandidateID: candidate.ID, Revision: 2, Decision: decisionKind,
			Reason: decisionReason, ActorKind: "system", OperationID: operation.OperationID,
			RequestHash: operation.RequestHash, CreatedAt: now,
		}
		if err := decision.Validate(); err != nil {
			return OperationResult{}, err
		}
		plan.Candidate = candidate
		plan.AutomaticDecision = &decision
		if automaticStatus == CandidateAdmitted {
			if correction {
				plan.LogicalMemoryID = logicalMemoryID
			} else {
				plan.LogicalMemoryID = s.newID()
			}
			plan.RecordRevisionID = s.newID()
			plan.DeliveryID = s.newID()
			plan.DeliveryPayloadID = s.newID()
			plan.ReceiptID = s.newID()
			plan.OutboxID = s.newID()
		}
	}
	return s.store.CreateCandidate(ctx, plan)
}

type decideCandidateRequestV1 struct {
	SchemaVersion            string        `json:"schema_version"`
	OperationKind            OperationKind `json:"operation_kind"`
	CandidateID              string        `json:"candidate_id"`
	ExpectedRevision         int64         `json:"expected_revision"`
	ExpectedRecordRevision   int64         `json:"expected_record_revision,omitempty"`
	ExpectedRecordGeneration int64         `json:"expected_record_generation,omitempty"`
	Decision                 Decision      `json:"decision"`
	Reason                   string        `json:"reason"`
	ActorID                  string        `json:"actor_id"`
}

func (s *Service) DecideCandidate(ctx context.Context, principal DevicePrincipal, command DecideCandidateCommand) (OperationResult, error) {
	if err := principal.Validate(); err != nil {
		return OperationResult{}, err
	}
	if !validUUID(command.OperationID) || !validUUID(command.CandidateID) || command.ExpectedRevision < 1 ||
		(command.Decision != DecisionAdmit && command.Decision != DecisionReject) ||
		(command.ExpectedRecordRevision == 0) != (command.ExpectedRecordGeneration == 0) ||
		command.ExpectedRecordRevision < 0 || command.ExpectedRecordGeneration < 0 {
		return OperationResult{}, invalid("invalid_decision_command")
	}
	requestHash, err := CanonicalRequestHash(decideCandidateRequestV1{
		SchemaVersion: "memory-candidate-decision-v1", OperationKind: OperationCandidateDecision,
		CandidateID: command.CandidateID, ExpectedRevision: command.ExpectedRevision,
		ExpectedRecordRevision:   command.ExpectedRecordRevision,
		ExpectedRecordGeneration: command.ExpectedRecordGeneration,
		Decision:                 command.Decision, Reason: command.Reason, ActorID: principal.DeviceID,
	})
	if err != nil {
		return OperationResult{}, fmt.Errorf("hash memory decision request: %w", err)
	}
	operation := Operation{DeviceID: principal.DeviceID, OperationID: command.OperationID, RequestHash: requestHash, Kind: OperationCandidateDecision}
	now := s.now().UTC()
	decision := CandidateDecision{
		ID: s.newID(), CandidateID: command.CandidateID, Revision: command.ExpectedRevision + 1,
		Decision: command.Decision, Reason: command.Reason, ActorID: principal.DeviceID, ActorKind: "device",
		OperationID: operation.OperationID, RequestHash: operation.RequestHash, CreatedAt: now,
	}
	if err := decision.Validate(); err != nil {
		return OperationResult{}, err
	}
	plan := DecisionPlan{
		Operation: operation, CandidateID: command.CandidateID, ExpectedRevision: command.ExpectedRevision,
		ExpectedRecordRevision:   command.ExpectedRecordRevision,
		ExpectedRecordGeneration: command.ExpectedRecordGeneration, Decision: decision,
	}
	if command.Decision == DecisionAdmit {
		plan.LogicalMemoryID = s.newID()
		plan.RecordRevisionID = s.newID()
		plan.DeliveryID = s.newID()
		plan.DeliveryPayloadID = s.newID()
		plan.ReceiptID = s.newID()
		plan.OutboxID = s.newID()
	}
	return s.store.DecideCandidate(ctx, plan)
}

type deleteRecordRequestV1 struct {
	SchemaVersion            string        `json:"schema_version"`
	OperationKind            OperationKind `json:"operation_kind"`
	LogicalMemoryID          string        `json:"logical_memory_id"`
	ExpectedRevision         int64         `json:"expected_revision"`
	ExpectedRecordGeneration int64         `json:"expected_record_generation"`
	ActorID                  string        `json:"actor_id"`
}

func (s *Service) DeleteRecord(ctx context.Context, principal DevicePrincipal, command DeleteRecordCommand) (OperationResult, error) {
	if err := principal.Validate(); err != nil {
		return OperationResult{}, err
	}
	if !validUUID(command.OperationID) || !validUUID(command.LogicalMemoryID) ||
		command.ExpectedRevision < 1 || command.ExpectedRecordGeneration < 1 {
		return OperationResult{}, invalid("invalid_record_delete_command")
	}
	requestHash, err := CanonicalRequestHash(deleteRecordRequestV1{
		SchemaVersion: "memory-record-delete-v1", OperationKind: OperationRecordDelete,
		LogicalMemoryID: command.LogicalMemoryID, ExpectedRevision: command.ExpectedRevision,
		ExpectedRecordGeneration: command.ExpectedRecordGeneration, ActorID: principal.DeviceID,
	})
	if err != nil {
		return OperationResult{}, fmt.Errorf("hash memory record delete request: %w", err)
	}
	now := s.now().UTC()
	return s.store.DeleteRecord(ctx, DeletePlan{
		Operation:       Operation{DeviceID: principal.DeviceID, OperationID: command.OperationID, RequestHash: requestHash, Kind: OperationRecordDelete},
		LogicalMemoryID: command.LogicalMemoryID, ExpectedRevision: command.ExpectedRevision,
		ExpectedRecordGeneration: command.ExpectedRecordGeneration, DeliveryID: s.newID(),
		DeliveryPayloadID: s.newID(), ReceiptID: s.newID(), OutboxID: s.newID(), ValidUntil: now.Add(s.deliveryTTL),
	})
}

type replayDeliveryRequestV1 struct {
	SchemaVersion string        `json:"schema_version"`
	OperationKind OperationKind `json:"operation_kind"`
	DeliveryID    string        `json:"delivery_id"`
	ActorID       string        `json:"actor_id"`
}

func (s *Service) ReplayDelivery(
	ctx context.Context,
	principal DevicePrincipal,
	command ReplayDeliveryCommand,
) (OperationResult, error) {
	if err := principal.Validate(); err != nil {
		return OperationResult{}, err
	}
	if !validUUID(command.OperationID) || !validUUID(command.DeliveryID) {
		return OperationResult{}, invalid("invalid_delivery_replay_command")
	}
	requestHash, err := CanonicalRequestHash(replayDeliveryRequestV1{
		SchemaVersion: "memory-delivery-replay-v1", OperationKind: OperationDeliveryReplay,
		DeliveryID: command.DeliveryID, ActorID: principal.DeviceID,
	})
	if err != nil {
		return OperationResult{}, fmt.Errorf("hash memory delivery replay request: %w", err)
	}
	return s.store.ReplayDelivery(ctx, ReplayPlan{
		Operation: Operation{
			DeviceID: principal.DeviceID, OperationID: command.OperationID,
			RequestHash: requestHash, Kind: OperationDeliveryReplay,
		},
		DeliveryID: command.DeliveryID,
	})
}

func (s *Service) Candidate(ctx context.Context, candidateID string) (CandidateView, error) {
	if !validUUID(candidateID) {
		return CandidateView{}, invalid("invalid_candidate_id")
	}
	permit, err := s.readPermits.Acquire(ctx, privacy.OwnerMemory)
	if err != nil {
		return CandidateView{}, memoryReadError(err)
	}
	defer permit.Release()
	value, err := s.store.Candidate(permit.Context(), candidateID)
	if cause := context.Cause(permit.Context()); cause != nil {
		return CandidateView{}, memoryReadError(cause)
	}
	return value, err
}

func (s *Service) ListCandidates(ctx context.Context, page PageRequest) (CandidatePage, error) {
	permit, err := s.readPermits.Acquire(ctx, privacy.OwnerMemory)
	if err != nil {
		return CandidatePage{}, memoryReadError(err)
	}
	defer permit.Release()
	value, err := s.store.ListCandidates(permit.Context(), page)
	if cause := context.Cause(permit.Context()); cause != nil {
		return CandidatePage{}, memoryReadError(cause)
	}
	return value, err
}

func (s *Service) ListRecords(ctx context.Context, page PageRequest) (RecordPage, error) {
	permit, err := s.readPermits.Acquire(ctx, privacy.OwnerMemory)
	if err != nil {
		return RecordPage{}, memoryReadError(err)
	}
	defer permit.Release()
	value, err := s.store.ListRecords(permit.Context(), page)
	if cause := context.Cause(permit.Context()); cause != nil {
		return RecordPage{}, memoryReadError(cause)
	}
	return value, err
}

func (s *Service) Record(ctx context.Context, logicalMemoryID string) (RecordView, error) {
	if !validUUID(logicalMemoryID) {
		return RecordView{}, invalid("invalid_logical_memory_id")
	}
	permit, err := s.readPermits.Acquire(ctx, privacy.OwnerMemory)
	if err != nil {
		return RecordView{}, memoryReadError(err)
	}
	defer permit.Release()
	value, err := s.store.Record(permit.Context(), logicalMemoryID)
	if cause := context.Cause(permit.Context()); cause != nil {
		return RecordView{}, memoryReadError(cause)
	}
	return value, err
}

func memoryReadError(err error) error {
	switch privacy.ErrorCode(err) {
	case privacy.CodeContentRedacted:
		return &Error{Code: CodeContentRedacted, Reason: "memory_read_gate_closed", Cause: err}
	case privacy.CodeInvalidRequest:
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_memory_read_permit", Cause: err}
	default:
		return err
	}
}

func MemoryOutboxPayload(intent OutboxIntent) (json.RawMessage, error) {
	if !validUUID(intent.DeliveryID) || !validHash(intent.PayloadHash) || intent.RecordRevision < 1 || intent.LearnerGeneration < 1 || intent.RecordGeneration < 1 {
		return nil, invalid("invalid_outbox_intent")
	}
	return json.Marshal(intent)
}
