package learning

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	EvidenceCarryoverPolicyVersion = "evidence-carryover-v1"
	MaxCarryoverDecisionRunes      = 4000
)

type EvidenceCarryoverStatus string

const (
	EvidenceCarryoverOpen     EvidenceCarryoverStatus = "open"
	EvidenceCarryoverApproved EvidenceCarryoverStatus = "approved"
	EvidenceCarryoverRejected EvidenceCarryoverStatus = "rejected"
	EvidenceCarryoverStale    EvidenceCarryoverStatus = "stale"
	EvidenceCarryoverRedacted EvidenceCarryoverStatus = "redacted"
)

type EvidenceCarryoverCandidate struct {
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	DocumentRevisionID  string `json:"document_revision_id"`
}

type EvidenceCarryoverLink struct {
	ID                        string    `json:"link_id"`
	ProposalID                string    `json:"proposal_id"`
	SourceEvidenceID          string    `json:"source_evidence_id,omitempty"`
	TargetKnowledgeRevisionID string    `json:"target_knowledge_revision_id,omitempty"`
	TargetNodeID              string    `json:"target_node_id,omitempty"`
	TargetNodeRevisionID      string    `json:"target_node_revision_id,omitempty"`
	TargetDocumentRevisionID  string    `json:"target_document_revision_id,omitempty"`
	DecisionID                string    `json:"decision_id"`
	EventID                   string    `json:"event_id"`
	EventSequence             int64     `json:"event_seq"`
	CreatedAt                 time.Time `json:"created_at"`
}

type EvidenceCarryoverDecision struct {
	ID                string    `json:"decision_id"`
	OperationID       string    `json:"operation_id"`
	RequestedDecision string    `json:"requested_decision"`
	Outcome           string    `json:"outcome"`
	Reason            string    `json:"reason,omitempty"`
	ActorDeviceID     string    `json:"actor_device_id"`
	EventID           string    `json:"event_id"`
	EventSequence     int64     `json:"event_seq"`
	CreatedAt         time.Time `json:"created_at"`
}

type EvidenceCarryoverProposal struct {
	ID                        string                       `json:"proposal_id"`
	KnowledgeProposalID       string                       `json:"knowledge_proposal_id"`
	Status                    EvidenceCarryoverStatus      `json:"status"`
	SourceEvidenceID          string                       `json:"source_evidence_id,omitempty"`
	SourceKnowledgeRevisionID string                       `json:"source_knowledge_revision_id,omitempty"`
	SourceNodeRevisionID      string                       `json:"source_node_revision_id,omitempty"`
	TargetKnowledgeRevisionID string                       `json:"target_knowledge_revision_id,omitempty"`
	Candidates                []EvidenceCarryoverCandidate `json:"candidates,omitempty"`
	KnowledgeBasisHash        string                       `json:"knowledge_basis_hash,omitempty"`
	EvidenceFingerprint       string                       `json:"accepted_evidence_fingerprint,omitempty"`
	CandidateFingerprint      string                       `json:"candidate_fingerprint,omitempty"`
	BasisFingerprint          string                       `json:"basis_fingerprint,omitempty"`
	KnowledgeGeneration       int64                        `json:"knowledge_generation"`
	LearningGeneration        int64                        `json:"learning_generation"`
	PolicyVersion             string                       `json:"policy_version"`
	Decision                  *EvidenceCarryoverDecision   `json:"decision,omitempty"`
	Links                     []EvidenceCarryoverLink      `json:"links,omitempty"`
	CreatedAt                 time.Time                    `json:"created_at"`
	UpdatedAt                 time.Time                    `json:"updated_at"`
	Redacted                  bool                         `json:"redacted"`
	Replayed                  bool                         `json:"replayed,omitempty"`
}

type ProvisionalEvidenceCarryover struct {
	ProposalID                string                  `json:"proposal_id"`
	KnowledgeProposalID       string                  `json:"knowledge_proposal_id"`
	SourceEvidenceID          string                  `json:"source_evidence_id"`
	SourceKnowledgeRevisionID string                  `json:"source_knowledge_revision_id"`
	SourceNodeRevisionID      string                  `json:"source_node_revision_id"`
	TargetKnowledgeRevisionID string                  `json:"target_knowledge_revision_id"`
	Links                     []EvidenceCarryoverLink `json:"links"`
	BasisFingerprint          string                  `json:"basis_fingerprint"`
	PolicyVersion             string                  `json:"policy_version"`
	ApprovedEventSequence     int64                   `json:"approved_event_seq"`
}

type EvidenceCarryoverEvent struct {
	ProposalID        string                        `json:"proposal_id"`
	DecisionID        string                        `json:"decision_id"`
	RequestedDecision string                        `json:"requested_decision"`
	Outcome           string                        `json:"outcome"`
	Reason            string                        `json:"reason"`
	Provisional       *ProvisionalEvidenceCarryover `json:"provisional,omitempty"`
}

type EvidenceCarryoverDecisionCommand struct {
	OperationID string `json:"operation_id"`
	ProposalID  string `json:"proposal_id"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
}

func (c *EvidenceCarryoverDecisionCommand) UnmarshalJSON(data []byte) error {
	type alias EvidenceCarryoverDecisionCommand
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded alias
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	*c = EvidenceCarryoverDecisionCommand(decoded)
	return nil
}

type EvidenceCarryoverListCommand struct {
	Status             string    `json:"status,omitempty"`
	Limit              int       `json:"limit,omitempty"`
	Cursor             string    `json:"cursor,omitempty"`
	ExpectedGeneration int64     `json:"-"`
	AfterCreatedAt     time.Time `json:"-"`
	AfterProposalID    string    `json:"-"`
}

type EvidenceCarryoverPage struct {
	Items      []EvidenceCarryoverProposal `json:"items"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}

type PreparedEvidenceCarryoverDecision struct {
	OperationID       string
	RequestHash       string
	ProposalID        string
	RequestedDecision string
	Reason            string
	ActorDeviceID     string
	DecisionID        string
	DecidedAt         time.Time
}

type EvidenceCarryoverStore interface {
	ListEvidenceCarryovers(context.Context, EvidenceCarryoverListCommand) (EvidenceCarryoverPage, error)
	EvidenceCarryover(context.Context, string) (EvidenceCarryoverProposal, error)
	DecideEvidenceCarryover(context.Context, PreparedEvidenceCarryoverDecision) (EvidenceCarryoverProposal, error)
}

func NormalizeEvidenceCarryoverCandidates(values []EvidenceCarryoverCandidate) ([]EvidenceCarryoverCandidate, string, error) {
	result := append([]EvidenceCarryoverCandidate(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].NodeRevisionID != result[j].NodeRevisionID {
			return result[i].NodeRevisionID < result[j].NodeRevisionID
		}
		return result[i].DocumentRevisionID < result[j].DocumentRevisionID
	})
	for index, value := range result {
		if value.KnowledgeRevisionID == "" || value.NodeID == "" || value.NodeRevisionID == "" || value.DocumentRevisionID == "" {
			return nil, "", &Error{Code: CodeInvalidRequest, Reason: "carryover_candidate_incomplete"}
		}
		if index > 0 && result[index-1].NodeRevisionID == value.NodeRevisionID {
			return nil, "", &Error{Code: CodeInvalidRequest, Reason: "duplicate_carryover_candidate"}
		}
	}
	fingerprint, err := HashJSON(struct {
		Version    string                       `json:"version"`
		Candidates []EvidenceCarryoverCandidate `json:"candidates"`
	}{Version: EvidenceCarryoverPolicyVersion, Candidates: result})
	if err != nil {
		return nil, "", err
	}
	return result, fingerprint, nil
}

func ComputeEvidenceCarryoverBasis(proposal EvidenceCarryoverProposal) string {
	fingerprint, err := HashJSON(struct {
		Version                   string                       `json:"version"`
		ProposalID                string                       `json:"proposal_id"`
		KnowledgeProposalID       string                       `json:"knowledge_proposal_id"`
		SourceEvidenceID          string                       `json:"source_evidence_id"`
		SourceKnowledgeRevisionID string                       `json:"source_knowledge_revision_id"`
		SourceNodeRevisionID      string                       `json:"source_node_revision_id"`
		TargetKnowledgeRevisionID string                       `json:"target_knowledge_revision_id"`
		Candidates                []EvidenceCarryoverCandidate `json:"candidates"`
		KnowledgeBasisHash        string                       `json:"knowledge_basis_hash"`
		EvidenceFingerprint       string                       `json:"accepted_evidence_fingerprint"`
		CandidateFingerprint      string                       `json:"candidate_fingerprint"`
		KnowledgeGeneration       int64                        `json:"knowledge_generation"`
		LearningGeneration        int64                        `json:"learning_generation"`
		PolicyVersion             string                       `json:"policy_version"`
	}{
		Version: EvidenceCarryoverPolicyVersion, ProposalID: proposal.ID,
		KnowledgeProposalID: proposal.KnowledgeProposalID, SourceEvidenceID: proposal.SourceEvidenceID,
		SourceKnowledgeRevisionID: proposal.SourceKnowledgeRevisionID, SourceNodeRevisionID: proposal.SourceNodeRevisionID,
		TargetKnowledgeRevisionID: proposal.TargetKnowledgeRevisionID, Candidates: proposal.Candidates,
		KnowledgeBasisHash: proposal.KnowledgeBasisHash, EvidenceFingerprint: proposal.EvidenceFingerprint,
		CandidateFingerprint: proposal.CandidateFingerprint, KnowledgeGeneration: proposal.KnowledgeGeneration,
		LearningGeneration: proposal.LearningGeneration, PolicyVersion: proposal.PolicyVersion,
	})
	if err != nil {
		return ""
	}
	return fingerprint
}

func EncodeEvidenceCarryoverCursor(generation int64, createdAt time.Time, proposalID string) string {
	value, _ := json.Marshal(struct {
		Version    int    `json:"v"`
		Generation int64  `json:"generation"`
		CreatedAt  string `json:"created_at"`
		ProposalID string `json:"proposal_id"`
	}{Version: 1, Generation: generation, CreatedAt: createdAt.UTC().Format(time.RFC3339Nano), ProposalID: proposalID})
	return base64.RawURLEncoding.EncodeToString(value)
}

func DecodeEvidenceCarryoverCursor(value string) (int64, time.Time, string, error) {
	if value == "" {
		return 0, time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, time.Time{}, "", &Error{Code: CodeStaleCursor}
	}
	var wire struct {
		Version    int    `json:"v"`
		Generation int64  `json:"generation"`
		CreatedAt  string `json:"created_at"`
		ProposalID string `json:"proposal_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Version != 1 || wire.Generation < 1 || !validUUID(wire.ProposalID) {
		return 0, time.Time{}, "", &Error{Code: CodeStaleCursor}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, wire.CreatedAt)
	if err != nil {
		return 0, time.Time{}, "", &Error{Code: CodeStaleCursor}
	}
	return wire.Generation, createdAt.UTC(), wire.ProposalID, nil
}

func validateEvidenceCarryoverDecision(actorDeviceID string, command EvidenceCarryoverDecisionCommand) error {
	if !validUUID(actorDeviceID) || !validUUID(command.OperationID) || !validUUID(command.ProposalID) ||
		(command.Decision != "approve" && command.Decision != "reject") || !utf8.ValidString(command.Reason) ||
		strings.TrimSpace(command.Reason) == "" || utf8.RuneCountInString(command.Reason) > MaxCarryoverDecisionRunes {
		return &Error{Code: CodeInvalidRequest}
	}
	return nil
}

func normalizeEvidenceCarryoverStatus(value string) (string, error) {
	if value == "" {
		return "all", nil
	}
	switch EvidenceCarryoverStatus(value) {
	case EvidenceCarryoverOpen, EvidenceCarryoverApproved, EvidenceCarryoverRejected,
		EvidenceCarryoverStale, EvidenceCarryoverRedacted:
		return value, nil
	default:
		return "", &Error{Code: CodeInvalidRequest}
	}
}

func (s *Service) ListEvidenceCarryovers(ctx context.Context, command EvidenceCarryoverListCommand) (EvidenceCarryoverPage, error) {
	if s.carryovers == nil {
		return EvidenceCarryoverPage{}, fmt.Errorf("evidence carryover store is not configured")
	}
	status, err := normalizeEvidenceCarryoverStatus(strings.TrimSpace(command.Status))
	if err != nil {
		return EvidenceCarryoverPage{}, err
	}
	if command.Limit == 0 {
		command.Limit = 50
	}
	if command.Limit < 1 || command.Limit > 100 {
		return EvidenceCarryoverPage{}, &Error{Code: CodeInvalidRequest}
	}
	generation, afterAt, afterID, err := DecodeEvidenceCarryoverCursor(command.Cursor)
	if err != nil {
		return EvidenceCarryoverPage{}, err
	}
	command.Status = status
	command.ExpectedGeneration = generation
	command.AfterCreatedAt = afterAt
	command.AfterProposalID = afterID
	return s.carryovers.ListEvidenceCarryovers(ctx, command)
}

func (s *Service) GetEvidenceCarryover(ctx context.Context, proposalID string) (EvidenceCarryoverProposal, error) {
	if s.carryovers == nil {
		return EvidenceCarryoverProposal{}, fmt.Errorf("evidence carryover store is not configured")
	}
	proposalID = strings.ToLower(strings.TrimSpace(proposalID))
	if !validUUID(proposalID) {
		return EvidenceCarryoverProposal{}, &Error{Code: CodeInvalidRequest}
	}
	return s.carryovers.EvidenceCarryover(ctx, proposalID)
}

func (s *Service) DecideEvidenceCarryover(ctx context.Context, actorDeviceID string, command EvidenceCarryoverDecisionCommand) (EvidenceCarryoverProposal, error) {
	if s.carryovers == nil {
		return EvidenceCarryoverProposal{}, fmt.Errorf("evidence carryover store is not configured")
	}
	actorDeviceID = strings.ToLower(strings.TrimSpace(actorDeviceID))
	command.OperationID = strings.ToLower(strings.TrimSpace(command.OperationID))
	command.ProposalID = strings.ToLower(strings.TrimSpace(command.ProposalID))
	command.Decision = strings.TrimSpace(command.Decision)
	command.Reason = strings.TrimSpace(command.Reason)
	if err := validateEvidenceCarryoverDecision(actorDeviceID, command); err != nil {
		return EvidenceCarryoverProposal{}, err
	}
	requestHash, err := HashJSON(struct {
		OperationID, ProposalID, Decision, Reason, ActorDeviceID string
	}{command.OperationID, command.ProposalID, command.Decision, command.Reason, actorDeviceID})
	if err != nil {
		return EvidenceCarryoverProposal{}, err
	}
	decisionID := s.newUUID()
	if !validUUID(decisionID) {
		return EvidenceCarryoverProposal{}, fmt.Errorf("UUID generator returned invalid evidence carryover decision ID")
	}
	return s.carryovers.DecideEvidenceCarryover(ctx, PreparedEvidenceCarryoverDecision{
		OperationID: command.OperationID, RequestHash: requestHash, ProposalID: command.ProposalID,
		RequestedDecision: command.Decision, Reason: command.Reason, ActorDeviceID: actorDeviceID,
		DecisionID: decisionID, DecidedAt: s.now().UTC().Truncate(time.Microsecond),
	})
}
