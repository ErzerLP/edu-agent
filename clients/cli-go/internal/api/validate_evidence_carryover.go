package api

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxEvidenceCarryoverCursorBytes = 4096

func ValidateEvidenceCarryoverQuery(status, cursor string, limit int) error {
	switch status {
	case "all", "open", "approved", "rejected", "stale", "redacted":
	default:
		return errors.New("evidence carryover status is invalid")
	}
	if !validCarryoverCursor(cursor) || limit < 1 || limit > 100 {
		return errors.New("evidence carryover pagination is invalid")
	}
	return nil
}

func ValidateEvidenceCarryoverProposalID(value string) error {
	if !validLearningUUID(value) {
		return errors.New("evidence carryover proposal ID is invalid")
	}
	return nil
}

func ValidateEvidenceCarryoverDecisionRequest(proposalID, expectedDecision string, value EvidenceCarryoverDecisionRequest) error {
	if !validLearningUUID(proposalID) || !validLearningUUID(value.OperationID) ||
		(expectedDecision != "approve" && expectedDecision != "reject") || value.Decision != expectedDecision ||
		strings.TrimSpace(value.Reason) == "" || !validMaintenanceText(value.Reason, 4000) {
		return errors.New("evidence carryover decision request is invalid")
	}
	return nil
}

func ValidateEvidenceCarryoverPage(value EvidenceCarryoverPage, limit int) error {
	if value.Items == nil || len(value.Items) > limit || !validCarryoverCursor(value.NextCursor) {
		return errors.New("evidence carryover page is invalid")
	}
	seen := make(map[string]struct{}, len(value.Items))
	for _, proposal := range value.Items {
		if err := ValidateEvidenceCarryoverProposal(proposal); err != nil || proposal.Replayed {
			return errors.New("evidence carryover page item is invalid")
		}
		if _, duplicate := seen[proposal.ProposalID]; duplicate {
			return errors.New("evidence carryover page contains duplicate proposals")
		}
		seen[proposal.ProposalID] = struct{}{}
	}
	return nil
}

func ValidateEvidenceCarryoverProposal(value EvidenceCarryoverProposal) error {
	if !validLearningUUID(value.ProposalID) || !validLearningUUID(value.KnowledgeProposalID) ||
		!validEvidenceCarryoverStatus(value.Status) || value.KnowledgeGeneration < 1 || value.LearningGeneration < 1 ||
		value.PolicyVersion != EvidenceCarryoverPolicyVersion || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() ||
		value.UpdatedAt.Before(value.CreatedAt) {
		return errors.New("evidence carryover proposal core is invalid")
	}
	if value.Redacted {
		if value.Status != "redacted" {
			return errors.New("redacted evidence carryover status is invalid")
		}
		return validateRedactedEvidenceCarryover(value)
	}
	if value.Status == "redacted" || !validLearningUUID(value.SourceEvidenceID) ||
		!validLearningUUID(value.SourceKnowledgeRevisionID) || !validLearningUUID(value.SourceNodeRevisionID) ||
		!validLearningUUID(value.TargetKnowledgeRevisionID) || !validSHA256(value.KnowledgeBasisHash) ||
		!validSHA256(value.AcceptedEvidenceFingerprint) || !validSHA256(value.CandidateFingerprint) ||
		!validSHA256(value.BasisFingerprint) {
		return errors.New("evidence carryover payload is incomplete")
	}
	if err := validateEvidenceCarryoverCandidates(value); err != nil {
		return err
	}
	return validateEvidenceCarryoverState(value, false)
}

func validateRedactedEvidenceCarryover(value EvidenceCarryoverProposal) error {
	if value.SourceEvidenceID != "" || value.SourceKnowledgeRevisionID != "" || value.SourceNodeRevisionID != "" ||
		value.TargetKnowledgeRevisionID != "" || value.Candidates != nil || value.KnowledgeBasisHash != "" ||
		value.AcceptedEvidenceFingerprint != "" || value.CandidateFingerprint != "" || value.BasisFingerprint != "" {
		return errors.New("redacted evidence carryover exposes payload")
	}
	return validateEvidenceCarryoverState(value, true)
}

func validateEvidenceCarryoverCandidates(value EvidenceCarryoverProposal) error {
	seen := make(map[string]struct{}, len(value.Candidates))
	previousNode, previousDocument := "", ""
	for _, candidate := range value.Candidates {
		if candidate.KnowledgeRevisionID != value.TargetKnowledgeRevisionID || !validLearningUUID(candidate.NodeID) ||
			!validLearningUUID(candidate.NodeRevisionID) || !validLearningUUID(candidate.DocumentRevisionID) {
			return errors.New("evidence carryover candidate is invalid")
		}
		if _, duplicate := seen[candidate.NodeRevisionID]; duplicate {
			return errors.New("evidence carryover candidate is duplicated")
		}
		if previousNode > candidate.NodeRevisionID || previousNode == candidate.NodeRevisionID && previousDocument >= candidate.DocumentRevisionID {
			return errors.New("evidence carryover candidates are not deterministic")
		}
		seen[candidate.NodeRevisionID] = struct{}{}
		previousNode, previousDocument = candidate.NodeRevisionID, candidate.DocumentRevisionID
	}
	return nil
}

func validateEvidenceCarryoverState(value EvidenceCarryoverProposal, redacted bool) error {
	if redacted {
		if value.Decision == nil {
			if value.Links != nil || value.Replayed {
				return errors.New("redacted evidence carryover state is inconsistent")
			}
			return nil
		}
		if !validEvidenceCarryoverDecision(*value.Decision, true) {
			return errors.New("redacted evidence carryover decision is invalid")
		}
		decision := *value.Decision
		if value.UpdatedAt.Before(decision.CreatedAt) || decision.CreatedAt.Before(value.CreatedAt) {
			return errors.New("redacted evidence carryover decision time is inconsistent")
		}
		switch decision.Outcome {
		case "approved":
			if decision.RequestedDecision != "approve" || len(value.Links) == 0 {
				return errors.New("redacted approved evidence carryover is inconsistent")
			}
			return validateEvidenceCarryoverLinks(value, true)
		case "rejected":
			if decision.RequestedDecision != "reject" || value.Links != nil {
				return errors.New("redacted rejected evidence carryover is inconsistent")
			}
		case "stale":
			if decision.RequestedDecision != "approve" || value.Links != nil {
				return errors.New("redacted stale evidence carryover is inconsistent")
			}
		default:
			return errors.New("redacted evidence carryover outcome is invalid")
		}
		return nil
	}
	if value.Status == "open" {
		if value.Decision != nil || value.Links != nil || value.Replayed {
			return errors.New("evidence carryover open state is inconsistent")
		}
		return nil
	}
	if value.Decision == nil || !validEvidenceCarryoverDecision(*value.Decision, false) {
		return errors.New("evidence carryover terminal decision is invalid")
	}
	decision := *value.Decision
	if value.UpdatedAt.Before(decision.CreatedAt) || decision.CreatedAt.Before(value.CreatedAt) {
		return errors.New("evidence carryover decision time is inconsistent")
	}
	switch value.Status {
	case "approved":
		if decision.RequestedDecision != "approve" || decision.Outcome != "approved" || len(value.Links) == 0 || !redacted && len(value.Links) != len(value.Candidates) {
			return errors.New("approved evidence carryover is inconsistent")
		}
		return validateEvidenceCarryoverLinks(value, redacted)
	case "rejected":
		if decision.RequestedDecision != "reject" || decision.Outcome != "rejected" || value.Links != nil {
			return errors.New("rejected evidence carryover is inconsistent")
		}
	case "stale":
		if decision.RequestedDecision != "approve" || decision.Outcome != "stale" || value.Links != nil {
			return errors.New("stale evidence carryover is inconsistent")
		}
	default:
		return errors.New("evidence carryover status is inconsistent")
	}
	return nil
}

func validEvidenceCarryoverDecision(value EvidenceCarryoverDecision, redacted bool) bool {
	if !validLearningUUID(value.DecisionID) || !validLearningUUID(value.OperationID) ||
		!validLearningUUID(value.ActorDeviceID) || !validLearningUUID(value.EventID) || value.EventSequence < 1 ||
		value.CreatedAt.IsZero() || (value.RequestedDecision != "approve" && value.RequestedDecision != "reject") ||
		(value.Outcome != "approved" && value.Outcome != "rejected" && value.Outcome != "stale") {
		return false
	}
	if redacted {
		return value.Reason == ""
	}
	return strings.TrimSpace(value.Reason) != "" && validMaintenanceText(value.Reason, 4000)
}

func validateEvidenceCarryoverLinks(value EvidenceCarryoverProposal, redacted bool) error {
	seenLinks := make(map[string]struct{}, len(value.Links))
	seenTargets := make(map[string]struct{}, len(value.Links))
	for _, link := range value.Links {
		if !validLearningUUID(link.LinkID) || link.ProposalID != value.ProposalID || link.DecisionID != value.Decision.DecisionID ||
			link.EventID != value.Decision.EventID || link.EventSequence != value.Decision.EventSequence || !link.CreatedAt.Equal(value.Decision.CreatedAt) {
			return errors.New("evidence carryover link audit identity is invalid")
		}
		if _, duplicate := seenLinks[link.LinkID]; duplicate {
			return errors.New("evidence carryover link is duplicated")
		}
		seenLinks[link.LinkID] = struct{}{}
		if redacted {
			if link.SourceEvidenceID != "" || link.TargetKnowledgeRevisionID != "" || link.TargetNodeID != "" ||
				link.TargetNodeRevisionID != "" || link.TargetDocumentRevisionID != "" {
				return errors.New("redacted evidence carryover link exposes payload")
			}
			continue
		}
		if link.SourceEvidenceID != value.SourceEvidenceID || link.TargetKnowledgeRevisionID != value.TargetKnowledgeRevisionID ||
			!validLearningUUID(link.TargetNodeID) || !validLearningUUID(link.TargetNodeRevisionID) || !validLearningUUID(link.TargetDocumentRevisionID) {
			return errors.New("evidence carryover link payload is invalid")
		}
		key := link.TargetNodeRevisionID + "\x00" + link.TargetDocumentRevisionID
		if _, duplicate := seenTargets[key]; duplicate || !carryoverCandidateContains(value.Candidates, link) {
			return errors.New("evidence carryover link target is inconsistent")
		}
		seenTargets[key] = struct{}{}
	}
	return nil
}

func carryoverCandidateContains(values []EvidenceCarryoverCandidate, link EvidenceCarryoverLink) bool {
	for _, candidate := range values {
		if candidate.KnowledgeRevisionID == link.TargetKnowledgeRevisionID && candidate.NodeID == link.TargetNodeID &&
			candidate.NodeRevisionID == link.TargetNodeRevisionID && candidate.DocumentRevisionID == link.TargetDocumentRevisionID {
			return true
		}
	}
	return false
}

func validEvidenceCarryoverStatus(value string) bool {
	return value == "open" || value == "approved" || value == "rejected" || value == "stale" || value == "redacted"
}

func validCarryoverCursor(value string) bool {
	if !utf8.ValidString(value) || len(value) > maxEvidenceCarryoverCursorBytes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
