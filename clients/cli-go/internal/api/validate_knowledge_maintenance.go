package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	maxKnowledgeMaintenanceSources       = 20
	maxKnowledgeMaintenanceDocuments     = 1000
	maxKnowledgeMaintenanceMarkdownBytes = 4 << 20
	maxKnowledgeMaintenanceDiffBytes     = 256 << 10
	maxKnowledgeMaintenanceCursorBytes   = 4096
)

func ValidateKnowledgeMaintenanceProposalRequest(value KnowledgeMaintenanceProposalRequest) error {
	if !validLearningUUID(value.RequestID) || !validLearningUUID(value.BaseRevisionID) || value.Sources == nil || value.CandidateSnapshot == nil || len(value.CandidateSnapshot) > maxKnowledgeMaintenanceDocuments {
		return errors.New("knowledge maintenance proposal identity is invalid")
	}
	if err := validateKnowledgeMaintenanceSources(value.Sources); err != nil {
		return err
	}
	for _, document := range value.CandidateSnapshot {
		if strings.TrimSpace(document.Path) == "" || len(document.Path) > 512 || !utf8.ValidString(document.Path) || strings.IndexByte(document.Path, 0) >= 0 || len(document.Markdown) > maxKnowledgeMaintenanceMarkdownBytes || !utf8.ValidString(document.Markdown) {
			return errors.New("knowledge maintenance candidate document is invalid")
		}
	}
	identityValues := []string{value.IdentityReviewBasisHash, value.IdentityReviewOperationID, value.IdentityReviewReceipt}
	identityPresent := identityValues[0] != "" || identityValues[1] != "" || identityValues[2] != "" || len(value.DocumentResolutions) > 0 || len(value.NodeResolutions) > 0
	if identityPresent && (!validSHA256(value.IdentityReviewBasisHash) || !validLearningUUID(value.IdentityReviewOperationID) || !validSHA256(value.IdentityReviewReceipt)) {
		return errors.New("knowledge maintenance identity review is incomplete")
	}
	for _, resolution := range value.DocumentResolutions {
		if !validSHA256(resolution.Locator) || (resolution.Action != "preserve" && resolution.Action != "new") || strings.TrimSpace(resolution.Reason) == "" || !validMaintenanceText(resolution.Reason, 4000) || resolution.Action == "preserve" && !validLearningUUID(resolution.DocumentID) || resolution.Action == "new" && resolution.DocumentID != "" {
			return errors.New("knowledge maintenance document resolution is invalid")
		}
	}
	for _, resolution := range value.NodeResolutions {
		if !validSHA256(resolution.Locator) || !validMaintenanceNodeAction(resolution.Action) || strings.TrimSpace(resolution.Reason) == "" || !validMaintenanceText(resolution.Reason, 4000) {
			return errors.New("knowledge maintenance node resolution is invalid")
		}
		for _, id := range resolution.SourceNodeRevisionIDs {
			if !validLearningUUID(id) {
				return errors.New("knowledge maintenance node resolution source is invalid")
			}
		}
		if resolution.Action == "new" && len(resolution.SourceNodeRevisionIDs) != 0 || (resolution.Action == "preserve" || resolution.Action == "rewrite" || resolution.Action == "split") && len(resolution.SourceNodeRevisionIDs) != 1 || resolution.Action == "merge" && len(resolution.SourceNodeRevisionIDs) < 2 {
			return errors.New("knowledge maintenance node resolution cardinality is invalid")
		}
	}
	return nil
}

func ValidateKnowledgeMaintenanceRollbackRequest(value KnowledgeMaintenanceRollbackRequest) error {
	if !validLearningUUID(value.RequestID) || !validLearningUUID(value.BaseRevisionID) || !validLearningUUID(value.TargetRevisionID) || value.BaseRevisionID == value.TargetRevisionID || value.Sources == nil {
		return errors.New("knowledge maintenance rollback identity is invalid")
	}
	return validateKnowledgeMaintenanceSources(value.Sources)
}

func ValidateKnowledgeMaintenanceDecisionRequest(proposalID string, value KnowledgeMaintenanceDecisionRequest) error {
	if !validLearningUUID(proposalID) || !validLearningUUID(value.OperationID) || strings.TrimSpace(value.Reason) == "" || !validMaintenanceText(value.Reason, 4000) {
		return errors.New("knowledge maintenance decision is invalid")
	}
	return nil
}

func ValidateKnowledgeMaintenanceQuery(status, cursor string, limit int) error {
	switch status {
	case "", "all", "open", "applied", "rejected", "stale", "redacted":
	default:
		return errors.New("knowledge maintenance status is invalid")
	}
	if !utf8.ValidString(cursor) || len(cursor) > maxKnowledgeMaintenanceCursorBytes || limit < 0 || limit > 100 {
		return errors.New("knowledge maintenance pagination is invalid")
	}
	return nil
}

func ValidateKnowledgeMaintenanceProposalPage(value KnowledgeMaintenanceProposalPage) error {
	if value.Items == nil || len(value.Items) > 100 || !utf8.ValidString(value.NextCursor) || len(value.NextCursor) > maxKnowledgeMaintenanceCursorBytes {
		return errors.New("knowledge maintenance proposal page is invalid")
	}
	seen := make(map[string]struct{}, len(value.Items))
	for _, item := range value.Items {
		if err := ValidateKnowledgeMaintenanceProposal(item); err != nil {
			return err
		}
		if _, duplicate := seen[item.ProposalID]; duplicate {
			return errors.New("knowledge maintenance proposal page contains duplicates")
		}
		seen[item.ProposalID] = struct{}{}
	}
	return nil
}

func ValidateKnowledgeMaintenanceProposal(value KnowledgeMaintenanceProposal) error {
	if !validLearningUUID(value.ProposalID) || !validLearningUUID(value.RequestID) || !validLearningUUID(value.BaseRevisionID) || !validLearningUUID(value.CreatedByDeviceID) || !validMaintenanceKind(value.Kind) || !validMaintenanceStatus(value.Status) || value.KnowledgeGeneration < 1 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) || value.CanonicalizerVersion != "edu-markdown-v1" || value.IdentityPolicyVersion != "identity-policy-v1" || value.DiffVersion != "knowledge-diff-v1" || value.RiskVersion != "knowledge-risk-v1" || value.AutoApplyPolicyVersion != "knowledge-auto-apply-v1" {
		return errors.New("knowledge maintenance proposal core is invalid")
	}
	for _, id := range []string{value.CurrentRevisionID, value.RollbackTargetRevisionID, value.AppliedRevisionID} {
		if id != "" && !validLearningUUID(id) {
			return errors.New("knowledge maintenance proposal identity is invalid")
		}
	}
	if value.Kind == KnowledgeMaintenanceKindRollback && value.RollbackTargetRevisionID == "" || value.Kind == KnowledgeMaintenanceKindCandidate && value.RollbackTargetRevisionID != "" {
		return errors.New("knowledge maintenance rollback target is inconsistent")
	}
	if value.Redacted {
		if value.Status == "open" || value.Sources != nil || value.CandidateSnapshot != nil || value.Diff != nil || value.BasisHash != "" {
			return errors.New("redacted knowledge maintenance proposal exposes payload")
		}
	} else {
		if value.Status == "redacted" || value.Sources == nil || !validSHA256(value.BasisHash) {
			return errors.New("knowledge maintenance proposal payload is incomplete")
		}
		if err := validateKnowledgeMaintenanceSources(value.Sources); err != nil {
			return err
		}
		for _, document := range value.CandidateSnapshot {
			if strings.TrimSpace(document.Path) == "" || len(document.Path) > 512 || !validMaintenanceText(document.Path, 512) || len(document.Markdown) > maxKnowledgeMaintenanceMarkdownBytes || !utf8.ValidString(document.Markdown) {
				return errors.New("knowledge maintenance candidate snapshot is invalid")
			}
		}
	}
	if err := validateMaintenanceImpacts(value, value.Redacted); err != nil {
		return err
	}
	return validateMaintenanceState(value)
}

func validateMaintenanceImpacts(value KnowledgeMaintenanceProposal, redacted bool) error {
	identity := value.IdentityImpact
	lineage := value.LineageImpact
	evidence := value.AcceptedLearningEvidenceImpact
	if !redacted && (identity.PreservedDocumentIDs == nil || identity.AddedDocumentIDs == nil || identity.RemovedDocumentIDs == nil || identity.MovedDocumentIDs == nil || identity.PreservedNodeIDs == nil || identity.AddedNodeIDs == nil || identity.RemovedNodeIDs == nil || lineage.Lineages == nil || evidence.References == nil || value.Risk.Reasons == nil) {
		return errors.New("knowledge maintenance impact arrays are null")
	}
	for _, ids := range [][]string{identity.PreservedDocumentIDs, identity.AddedDocumentIDs, identity.RemovedDocumentIDs, identity.MovedDocumentIDs, identity.PreservedNodeIDs, identity.AddedNodeIDs, identity.RemovedNodeIDs} {
		if !validMaintenanceUUIDs(ids) {
			return errors.New("knowledge maintenance impact identity is invalid")
		}
	}
	for _, diff := range value.Diff {
		if !validLearningUUID(diff.DocumentID) || !validMaintenanceDiffKind(diff.Kind) || diff.ChangedBodyBytes < 0 || !utf8.ValidString(diff.UnifiedDiff) || len(diff.UnifiedDiff) > maxKnowledgeMaintenanceDiffBytes || !validMaintenanceUUIDs(diff.AddedNodeIDs) || !validMaintenanceUUIDs(diff.RemovedNodeIDs) || !validMaintenanceUUIDs(diff.EditedNodeIDs) || !validMaintenanceUUIDs(diff.TitleNodeIDs) || !validMaintenanceUUIDs(diff.StructureNodeIDs) {
			return errors.New("knowledge maintenance diff is invalid")
		}
	}
	for _, item := range lineage.Lineages {
		if !validLearningUUID(item.LineageID) || !validLearningUUID(item.KnowledgeRevisionID) || !validLearningUUID(item.ActorDeviceID) || item.CreatedAt.IsZero() || !validMaintenanceLineageAction(item.Action) || item.PolicyVersion == "" || item.Members == nil {
			return errors.New("knowledge maintenance lineage is invalid")
		}
		sources, targets := 0, 0
		seen := make(map[string]struct{}, len(item.Members))
		for _, member := range item.Members {
			if (member.Role != "source" && member.Role != "target") || !validLearningUUID(member.NodeRevisionID) {
				return errors.New("knowledge maintenance lineage member is invalid")
			}
			key := member.Role + "\x00" + member.NodeRevisionID
			if _, duplicate := seen[key]; duplicate {
				return errors.New("knowledge maintenance lineage member is duplicated")
			}
			seen[key] = struct{}{}
			if member.Role == "source" {
				sources++
			} else {
				targets++
			}
		}
		if item.Action == "rewrite" && (sources != 1 || targets != 1) || item.Action == "split" && (sources != 1 || targets < 2) || item.Action == "merge" && (sources < 2 || targets != 1) {
			return errors.New("knowledge maintenance lineage cardinality is invalid")
		}
	}
	if evidence.Count < 0 || evidence.Generation < 1 || evidence.Count != len(evidence.References) || redacted && evidence.Fingerprint != "" || !redacted && !validSHA256(evidence.Fingerprint) {
		return errors.New("knowledge maintenance evidence impact is invalid")
	}
	for _, reference := range evidence.References {
		if !validLearningUUID(reference.EvidenceID) || !validLearningUUID(reference.NodeRevisionID) || !validLearningUUID(reference.KnowledgeRevisionID) {
			return errors.New("knowledge maintenance evidence reference is invalid")
		}
	}
	if !validMaintenanceRisk(value.Risk, redacted) {
		return errors.New("knowledge maintenance risk is invalid")
	}
	return nil
}

func validateMaintenanceState(value KnowledgeMaintenanceProposal) error {
	if value.Redacted {
		switch value.Status {
		case "redacted", "rejected", "stale":
			return requireMaintenanceState(value.AppliedRevisionID == "" && value.Origin == nil)
		case "applied":
			if value.Origin == nil {
				return errors.New("redacted applied proposal lacks origin")
			}
			return validateMaintenanceOrigin(*value.Origin, value)
		default:
			return errors.New("redacted proposal state is invalid")
		}
	}
	if value.Status == "open" {
		return requireMaintenanceState(value.Decision == nil && value.AppliedRevisionID == "" && value.Origin == nil && value.CurrentRevisionID == "" && !value.Risk.AutoApply && (value.Risk.Level == "medium" || value.Risk.Level == "high"))
	}
	if value.Decision == nil || !validMaintenanceDecision(*value.Decision) {
		return errors.New("knowledge maintenance terminal proposal lacks decision")
	}
	switch value.Status {
	case "applied":
		if value.Decision.Outcome != "applied" || value.AppliedRevisionID == "" || value.CurrentRevisionID != value.AppliedRevisionID || value.Origin == nil || value.Decision.RequestedDecision == "auto" != value.Risk.AutoApply || value.Decision.RequestedDecision == "auto" && value.Risk.Level != "low" || value.Decision.RequestedDecision == "approve" && value.Risk.Level == "low" {
			return errors.New("applied knowledge maintenance proposal is inconsistent")
		}
		return validateMaintenanceOrigin(*value.Origin, value)
	case "rejected":
		return requireMaintenanceState(value.Decision.RequestedDecision == "reject" && value.Decision.Outcome == "rejected" && value.AppliedRevisionID == "" && value.Origin == nil && value.CurrentRevisionID == "" && !value.Risk.AutoApply && value.Risk.Level != "low")
	case "stale":
		return requireMaintenanceState(value.Decision.RequestedDecision == "approve" && value.Decision.Outcome == "stale" && value.CurrentRevisionID != "" && value.AppliedRevisionID == "" && value.Origin == nil && !value.Risk.AutoApply)
	case "redacted":
		return requireMaintenanceState(value.AppliedRevisionID == "" || value.Origin != nil)
	default:
		return errors.New("knowledge maintenance state is invalid")
	}
}

func validateMaintenanceOrigin(origin KnowledgeMaintenanceRevisionOrigin, proposal KnowledgeMaintenanceProposal) error {
	return requireMaintenanceState(origin.Version == "knowledge-revision-origin-v1" && origin.Kind == proposal.Kind && origin.ProposalID == proposal.ProposalID && origin.BaseRevisionID == proposal.BaseRevisionID && (origin.BasisHash == "" || validSHA256(origin.BasisHash)) && origin.RollbackTargetRevisionID == proposal.RollbackTargetRevisionID)
}

func validMaintenanceDecision(value KnowledgeMaintenanceDecision) bool {
	return validLearningUUID(value.DecisionID) && (value.OperationID == "" || validLearningUUID(value.OperationID)) && (value.RequestedDecision == "auto" || value.RequestedDecision == "approve" || value.RequestedDecision == "reject") && (value.Outcome == "applied" || value.Outcome == "rejected" || value.Outcome == "stale") && validLearningUUID(value.ActorDeviceID) && !value.CreatedAt.IsZero() && strings.TrimSpace(value.Reason) != "" && validMaintenanceText(value.Reason, 4000)
}

func validateKnowledgeMaintenanceSources(values []KnowledgeMaintenanceSource) error {
	if len(values) < 1 || len(values) > maxKnowledgeMaintenanceSources {
		return errors.New("knowledge maintenance sources are invalid")
	}
	for _, source := range values {
		digest := sha256.Sum256([]byte(source.Excerpt))
		if !validMaintenanceSourceKind(source.Kind) || strings.TrimSpace(source.Locator) == "" || !validMaintenanceText(source.Locator, 2048) || !validMaintenanceText(source.Title, 500) || !validMaintenanceText(source.Excerpt, 4000) || !validSHA256(source.SHA256) || source.SHA256 != hex.EncodeToString(digest[:]) {
			return errors.New("knowledge maintenance source is invalid")
		}
	}
	return nil
}

func requireMaintenanceState(ok bool) error {
	if !ok {
		return errors.New("knowledge maintenance state is inconsistent")
	}
	return nil
}

func validMaintenanceUUIDs(values []string) bool {
	for _, value := range values {
		if !validLearningUUID(value) {
			return false
		}
	}
	return true
}

func validMaintenanceText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && strings.IndexByte(value, 0) < 0 && len([]rune(value)) <= maxRunes
}

func validMaintenanceNodeAction(value string) bool {
	return value == "preserve" || value == "new" || value == "rewrite" || value == "split" || value == "merge"
}
func validMaintenanceSourceKind(value string) bool {
	return value == "url" || value == "file" || value == "note" || value == "repository" || value == "other"
}
func validMaintenanceKind(value string) bool { return value == "candidate" || value == "rollback" }
func validMaintenanceStatus(value string) bool {
	return value == "open" || value == "applied" || value == "rejected" || value == "stale" || value == "redacted"
}
func validMaintenanceDiffKind(value string) bool {
	return value == "add" || value == "delete" || value == "edit" || value == "unchanged" || value == "move" || value == "move_edit"
}
func validMaintenanceLineageAction(value string) bool {
	return value == "rewrite" || value == "split" || value == "merge"
}

func validMaintenanceRisk(value KnowledgeMaintenanceRisk, redacted bool) bool {
	if redacted {
		return value.Level == "" && value.PolicyVersion == "" && !value.AutoApply
	}
	if value.PolicyVersion != "knowledge-auto-apply-v1" || value.Level != "low" && value.Level != "medium" && value.Level != "high" {
		return false
	}
	return !value.AutoApply || value.Level == "low"
}
