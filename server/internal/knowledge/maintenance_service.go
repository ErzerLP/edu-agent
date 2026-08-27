package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	internaldiff "github.com/rogpeppe/go-internal/diff"
)

const maintenanceSource = "knowledge_maintenance"

type maintenancePlanningStore struct {
	CatalogStore
	prepared *PreparedCommit
}

func (s *maintenancePlanningStore) LookupImportOperation(context.Context, string) (ImportOperationRecord, bool, error) {
	return ImportOperationRecord{}, false, nil
}

func (s *maintenancePlanningStore) CommitImport(_ context.Context, prepared PreparedCommit) (ImportResult, error) {
	copy := prepared
	s.prepared = &copy
	return ImportResult{Revision: prepared.Revision, Unchanged: prepared.Unchanged}, nil
}

type maintenanceAnalysis struct {
	diff                    []DocumentDiff
	identity                IdentityImpact
	lineage                 LineageImpact
	affectedNodeRevisionIDs []string
}

func (s *Service) Create(ctx context.Context, command CreateProposalCommand) (Proposal, error) {
	if s.maintenanceStore == nil || s.evidenceImpactReader == nil {
		return Proposal{}, fmt.Errorf("knowledge maintenance dependencies are not configured")
	}
	normalized, requestHash, err := s.normalizeCreateCommand(command)
	if err != nil {
		return Proposal{}, err
	}
	if replay, ok, err := s.lookupMaintenanceReplay(ctx, normalized.RequestID, requestHash); err != nil || ok {
		return replay, err
	}
	baseSnapshot, err := s.maintenanceStore.MaintenanceBase(ctx, normalized.BaseRevisionID)
	if err != nil {
		return Proposal{}, err
	}
	if baseSnapshot.HeadRevisionID != normalized.BaseRevisionID {
		return Proposal{}, &Error{
			Code: CodeRevisionConflict, CurrentRevisionID: optionalRevisionPointer(baseSnapshot.HeadRevisionID), CurrentRevisionKnown: true,
		}
	}
	base := baseSnapshot.Revision
	if base.Redacted {
		return Proposal{}, &Error{Code: CodeContentRedacted}
	}
	commit, err := s.planCandidate(ctx, normalized, base)
	if err != nil {
		return Proposal{}, err
	}
	analysis := analyzeMaintenanceRevision(base, commit.Revision)
	for _, documentID := range analysis.identity.AddedDocumentIDs {
		exists, lookupErr := s.store.DocumentIdentityExists(ctx, documentID)
		if lookupErr != nil {
			return Proposal{}, fmt.Errorf("check restored document identity: %w", lookupErr)
		}
		if exists {
			analysis.lineage.Restore = true
		}
	}
	if len(analysis.diff) == 0 {
		return Proposal{}, &Error{Code: CodeInvalidRequest}
	}
	evidence, err := s.readAcceptedEvidenceImpact(ctx, analysis.affectedNodeRevisionIDs)
	if err != nil {
		return Proposal{}, err
	}
	risk := maintenanceRisk(analysis, evidence)
	createdAt := commit.Revision.CreatedAt
	proposalID := s.newUUID()
	if !validUUID(proposalID) {
		return Proposal{}, fmt.Errorf("UUID generator returned invalid proposal ID")
	}
	proposal := Proposal{
		ID: proposalID, RequestID: normalized.RequestID, RequestHash: requestHash,
		Kind: ProposalCandidate, Status: ProposalOpen, BaseRevisionID: normalized.BaseRevisionID,
		Sources: normalized.Sources, CandidateSnapshot: normalized.CandidateSnapshot,
		Diff: analysis.diff, IdentityImpact: analysis.identity, LineageImpact: analysis.lineage,
		EvidenceImpact: evidence, AffectedNodeRevisionIDs: analysis.affectedNodeRevisionIDs, Risk: risk,
		CanonicalizerVersion: CanonicalizerVersion, IdentityPolicyVersion: IdentityPolicyVersion,
		DiffVersion: MaintenanceDiffVersion, RiskVersion: MaintenanceRiskVersion,
		AutoPolicyVersion: MaintenanceAutoPolicy, CreatedByDeviceID: normalized.ActorDeviceID,
		KnowledgeGeneration: baseSnapshot.KnowledgeGeneration,
		CreatedAt:           createdAt, UpdatedAt: createdAt, PlannedRevisionID: commit.Revision.ID,
		PlannedRevisionNo: commit.Revision.RevisionNo, PlannedManifestHash: commit.Revision.ManifestHash,
	}
	if risk.AutoApply {
		decisionID := s.newUUID()
		if !validUUID(decisionID) {
			return Proposal{}, fmt.Errorf("UUID generator returned invalid proposal decision ID")
		}
		proposal.Decision = &ProposalDecision{
			ID: decisionID, RequestedDecision: "auto", Outcome: string(ProposalApplied),
			Reason: MaintenanceAutoPolicy, ActorDeviceID: normalized.ActorDeviceID, CreatedAt: createdAt,
		}
	}
	return s.maintenanceStore.SaveProposal(ctx, PreparedProposal{Proposal: proposal, Commit: commit})
}

func (s *Service) CreateRollback(ctx context.Context, command CreateRollbackCommand) (Proposal, error) {
	if s.maintenanceStore == nil || s.evidenceImpactReader == nil {
		return Proposal{}, fmt.Errorf("knowledge maintenance dependencies are not configured")
	}
	normalized, requestHash, err := normalizeRollbackCommand(command)
	if err != nil {
		return Proposal{}, err
	}
	if replay, ok, err := s.lookupMaintenanceReplay(ctx, normalized.RequestID, requestHash); err != nil || ok {
		return replay, err
	}
	baseSnapshot, err := s.maintenanceStore.MaintenanceBase(ctx, normalized.BaseRevisionID)
	if err != nil {
		return Proposal{}, err
	}
	if baseSnapshot.HeadRevisionID != normalized.BaseRevisionID {
		return Proposal{}, &Error{
			Code: CodeRevisionConflict, CurrentRevisionID: optionalRevisionPointer(baseSnapshot.HeadRevisionID), CurrentRevisionKnown: true,
		}
	}
	head := baseSnapshot.Revision
	target, err := s.store.Revision(ctx, normalized.TargetRevisionID)
	if err != nil {
		return Proposal{}, err
	}
	if target.Redacted || head.Redacted {
		return Proposal{}, &Error{Code: CodeContentRedacted}
	}
	if !s.isAncestor(ctx, head, target.ID) {
		return Proposal{}, &Error{Code: CodeInvalidRequest}
	}
	createdAt := s.now().UTC().Truncate(time.Microsecond)
	revisionID := s.newUUID()
	if !validUUID(revisionID) {
		return Proposal{}, fmt.Errorf("UUID generator returned invalid rollback revision ID")
	}
	parentID := head.ID
	revision := KnowledgeRevision{
		ID: revisionID, RevisionNo: head.RevisionNo + 1, ParentRevisionID: &parentID,
		ManifestHash: target.ManifestHash, Source: maintenanceSource + "_rollback",
		CreatedByDeviceID: normalized.ActorDeviceID, CreatedAt: createdAt,
		CanonicalizerVersion: CanonicalizerVersion, ParserVersion: ParserVersion,
		IndexerVersion: IndexerVersion, IdentityPolicyVersion: IdentityPolicyVersion,
		Documents: cloneSnapshotDocuments(target.Documents),
	}
	commit := PreparedCommit{ExpectedParentRevisionID: &parentID, Revision: revision}
	analysis := analyzeMaintenanceRevision(head, revision)
	analysis.lineage.Rollback = true
	analysis.lineage.Restore = true
	evidence, err := s.readAcceptedEvidenceImpact(ctx, analysis.affectedNodeRevisionIDs)
	if err != nil {
		return Proposal{}, err
	}
	risk := maintenanceRisk(analysis, evidence)
	proposalID := s.newUUID()
	if !validUUID(proposalID) {
		return Proposal{}, fmt.Errorf("UUID generator returned invalid rollback proposal ID")
	}
	candidate := make([]ImportDocument, 0, len(target.Documents))
	for _, document := range target.Documents {
		candidate = append(candidate, ImportDocument{Path: document.Path, Markdown: document.Revision.CanonicalMarkdown})
	}
	proposal := Proposal{
		ID: proposalID, RequestID: normalized.RequestID, RequestHash: requestHash,
		Kind: ProposalRollback, Status: ProposalOpen, BaseRevisionID: head.ID,
		RollbackTargetRevisionID: target.ID, Sources: normalized.Sources, CandidateSnapshot: candidate,
		Diff: analysis.diff, IdentityImpact: analysis.identity, LineageImpact: analysis.lineage,
		EvidenceImpact: evidence, AffectedNodeRevisionIDs: analysis.affectedNodeRevisionIDs, Risk: risk,
		CanonicalizerVersion: CanonicalizerVersion, IdentityPolicyVersion: IdentityPolicyVersion,
		DiffVersion: MaintenanceDiffVersion, RiskVersion: MaintenanceRiskVersion,
		AutoPolicyVersion: MaintenanceAutoPolicy, CreatedByDeviceID: normalized.ActorDeviceID,
		KnowledgeGeneration: baseSnapshot.KnowledgeGeneration,
		CreatedAt:           createdAt, UpdatedAt: createdAt, PlannedRevisionID: revision.ID,
		PlannedRevisionNo: revision.RevisionNo, PlannedManifestHash: revision.ManifestHash,
	}
	return s.maintenanceStore.SaveProposal(ctx, PreparedProposal{Proposal: proposal, Commit: commit})
}

func optionalRevisionPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (s *Service) List(ctx context.Context, command ProposalListCommand) (ProposalPage, error) {
	if s.maintenanceStore == nil {
		return ProposalPage{}, fmt.Errorf("knowledge maintenance store is not configured")
	}
	status := strings.TrimSpace(command.Status)
	if status == "" {
		status = "all"
	}
	switch ProposalStatus(status) {
	case ProposalOpen, ProposalApplied, ProposalRejected, ProposalStale, ProposalRedacted:
	case ProposalStatus("all"):
	default:
		return ProposalPage{}, &Error{Code: CodeInvalidRequest}
	}
	if command.Limit == 0 {
		command.Limit = 50
	}
	if command.Limit < 1 || command.Limit > 100 {
		return ProposalPage{}, &Error{Code: CodeInvalidRequest}
	}
	generation, afterAt, afterID, err := DecodeProposalCursor(command.Cursor)
	if err != nil {
		return ProposalPage{}, err
	}
	command.Status = status
	command.ExpectedGeneration = generation
	command.AfterCreatedAt = afterAt
	command.AfterProposalID = afterID
	return s.maintenanceStore.ListProposals(ctx, command)
}

func (s *Service) Get(ctx context.Context, proposalID string) (Proposal, error) {
	if s.maintenanceStore == nil || !validUUID(strings.ToLower(strings.TrimSpace(proposalID))) {
		return Proposal{}, &Error{Code: CodeInvalidRequest}
	}
	return s.maintenanceStore.Proposal(ctx, strings.ToLower(strings.TrimSpace(proposalID)))
}

func (s *Service) Decide(ctx context.Context, command ProposalDecisionCommand) (Proposal, error) {
	if s.maintenanceStore == nil || s.evidenceImpactReader == nil {
		return Proposal{}, fmt.Errorf("knowledge maintenance dependencies are not configured")
	}
	normalized, requestHash, err := normalizeDecisionCommand(command)
	if err != nil {
		return Proposal{}, err
	}
	if replay, ok, err := s.lookupMaintenanceReplay(ctx, normalized.OperationID, requestHash); err != nil || ok {
		return replay, err
	}
	proposal, err := s.maintenanceStore.Proposal(ctx, normalized.ProposalID)
	if err != nil {
		return Proposal{}, err
	}
	evidence, err := s.readAcceptedEvidenceImpact(ctx, proposal.AffectedNodeRevisionIDs)
	if err != nil {
		return Proposal{}, err
	}
	decisionID := s.newUUID()
	if !validUUID(decisionID) {
		return Proposal{}, fmt.Errorf("UUID generator returned invalid proposal decision ID")
	}
	return s.maintenanceStore.DecideProposal(ctx, PreparedProposalDecision{
		OperationID: normalized.OperationID, RequestHash: requestHash, ProposalID: normalized.ProposalID,
		RequestedDecision: normalized.Decision, Reason: normalized.Reason, ActorDeviceID: normalized.ActorDeviceID,
		DecisionID: decisionID, DecidedAt: s.now().UTC().Truncate(time.Microsecond),
		EvidenceFingerprint: evidence.Fingerprint, EvidenceGeneration: evidence.Generation,
		CanonicalizerVersion: CanonicalizerVersion, IdentityPolicyVersion: IdentityPolicyVersion,
		DiffVersion: MaintenanceDiffVersion, RiskVersion: MaintenanceRiskVersion,
		AutoPolicyVersion: MaintenanceAutoPolicy,
	})
}

func (s *Service) lookupMaintenanceReplay(ctx context.Context, operationID, requestHash string) (Proposal, bool, error) {
	record, exists, err := s.maintenanceStore.LookupMaintenanceOperation(ctx, operationID)
	if err != nil {
		return Proposal{}, false, fmt.Errorf("lookup knowledge maintenance operation: %w", err)
	}
	if !exists {
		return Proposal{}, false, nil
	}
	if record.RequestHash != requestHash {
		return Proposal{}, false, &Error{Code: CodeIdempotencyConflict}
	}
	proposal, err := s.maintenanceStore.Proposal(ctx, record.ProposalID)
	if err != nil {
		return Proposal{}, false, err
	}
	proposal.Replayed = true
	return proposal, true, nil
}

func (s *Service) planCandidate(ctx context.Context, command CreateProposalCommand, base KnowledgeRevision) (PreparedCommit, error) {
	if len(command.CandidateSnapshot) == 0 {
		createdAt := s.now().UTC().Truncate(time.Microsecond)
		revisionID := s.newUUID()
		if !validUUID(revisionID) {
			return PreparedCommit{}, fmt.Errorf("UUID generator returned invalid knowledge revision ID")
		}
		parentID := base.ID
		return PreparedCommit{
			ExpectedParentRevisionID: &parentID,
			Revision: KnowledgeRevision{
				ID: revisionID, RevisionNo: base.RevisionNo + 1, ParentRevisionID: &parentID,
				ManifestHash: hashManifest(nil), Source: maintenanceSource,
				CreatedByDeviceID: command.ActorDeviceID, CreatedAt: createdAt,
				CanonicalizerVersion: CanonicalizerVersion, ParserVersion: ParserVersion,
				IndexerVersion: IndexerVersion, IdentityPolicyVersion: IdentityPolicyVersion,
				Documents: []SnapshotDocument{},
			},
		}, nil
	}
	planningStore := &maintenancePlanningStore{CatalogStore: s.store}
	planner := &Service{
		store: planningStore, canonicalizer: s.canonicalizer, selector: s.selector,
		newUUID: s.newUUID, now: s.now, reviews: s.copyIdentityReviews(),
		maintenanceReplaceSnapshot: true, maintenanceAllowHistorical: true,
	}
	parentID := base.ID
	_, err := planner.Import(ctx, ImportCommand{
		OperationID: command.RequestID, ExpectedParentRevisionID: &parentID, ExpectedParentProvided: true,
		Source: maintenanceSource, Documents: command.CandidateSnapshot,
		IdentityReviewBasisHash:   command.IdentityReviewBasisHash,
		IdentityReviewOperationID: command.IdentityReviewOperationID,
		IdentityReviewReceipt:     command.IdentityReviewReceipt,
		DocumentResolutions:       command.DocumentResolutions, NodeResolutions: command.NodeResolutions,
		ActorDeviceID: command.ActorDeviceID,
	})
	s.mergeIdentityReviews(planner.copyIdentityReviews())
	if err != nil {
		return PreparedCommit{}, err
	}
	if planningStore.prepared == nil {
		return PreparedCommit{}, errors.New("knowledge maintenance planner produced no commit")
	}
	prepared := *planningStore.prepared
	prepared.OperationID = ""
	prepared.RequestHash = ""
	return prepared, nil
}

func (s *Service) copyIdentityReviews() map[string]identityReviewRecord {
	s.reviewMu.RLock()
	defer s.reviewMu.RUnlock()
	result := make(map[string]identityReviewRecord, len(s.reviews))
	for key, value := range s.reviews {
		result[key] = value
	}
	return result
}

func (s *Service) mergeIdentityReviews(values map[string]identityReviewRecord) {
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	if s.reviews == nil {
		s.reviews = make(map[string]identityReviewRecord)
	}
	for key, value := range values {
		s.reviews[key] = value
	}
}

func (s *Service) isAncestor(ctx context.Context, base KnowledgeRevision, targetID string) bool {
	if base.ID == targetID {
		return false
	}
	current := base
	for current.ParentRevisionID != nil {
		if *current.ParentRevisionID == targetID {
			return true
		}
		loaded, err := s.store.Revision(ctx, *current.ParentRevisionID)
		if err != nil || loaded.Redacted {
			return false
		}
		current = loaded
	}
	return false
}

func (s *Service) readAcceptedEvidenceImpact(ctx context.Context, nodeRevisionIDs []string) (AcceptedEvidenceImpact, error) {
	impact, err := s.evidenceImpactReader.AcceptedEvidenceImpact(ctx, append([]string(nil), nodeRevisionIDs...))
	if err != nil {
		return AcceptedEvidenceImpact{}, fmt.Errorf("read accepted learning evidence impact: %w", err)
	}
	return NormalizeAcceptedEvidenceImpact(impact)
}

func NormalizeAcceptedEvidenceImpact(impact AcceptedEvidenceImpact) (AcceptedEvidenceImpact, error) {
	if impact.Generation < 1 {
		return AcceptedEvidenceImpact{}, errors.New("accepted learning evidence impact lacks a privacy generation")
	}
	sort.Slice(impact.References, func(i, j int) bool {
		if impact.References[i].EvidenceID != impact.References[j].EvidenceID {
			return impact.References[i].EvidenceID < impact.References[j].EvidenceID
		}
		return impact.References[i].NodeRevisionID < impact.References[j].NodeRevisionID
	})
	impact.Count = len(impact.References)
	var builder strings.Builder
	builder.WriteString("accepted-evidence-impact-v1\n")
	builder.WriteString(fmt.Sprintf("%d\n", impact.Generation))
	for _, reference := range impact.References {
		if !validUUID(reference.EvidenceID) || !validUUID(reference.NodeRevisionID) || !validUUID(reference.KnowledgeRevisionID) {
			return AcceptedEvidenceImpact{}, errors.New("accepted learning evidence impact contains an invalid identity")
		}
		builder.WriteString(reference.EvidenceID)
		builder.WriteByte('|')
		builder.WriteString(reference.NodeRevisionID)
		builder.WriteByte('|')
		builder.WriteString(reference.KnowledgeRevisionID)
		builder.WriteByte('\n')
	}
	impact.Fingerprint = sha256Hex([]byte(builder.String()))
	return impact, nil
}

func (s *Service) normalizeCreateCommand(command CreateProposalCommand) (CreateProposalCommand, string, error) {
	command.RequestID = strings.ToLower(strings.TrimSpace(command.RequestID))
	command.BaseRevisionID = strings.ToLower(strings.TrimSpace(command.BaseRevisionID))
	command.ActorDeviceID = strings.ToLower(strings.TrimSpace(command.ActorDeviceID))
	command.IdentityReviewOperationID = strings.ToLower(strings.TrimSpace(command.IdentityReviewOperationID))
	command.IdentityReviewReceipt = strings.ToLower(strings.TrimSpace(command.IdentityReviewReceipt))
	if !validUUID(command.RequestID) || !validUUID(command.BaseRevisionID) || !validUUID(command.ActorDeviceID) ||
		len(command.CandidateSnapshot) > MaxImportDocuments ||
		(len(command.CandidateSnapshot) == 0 && (command.IdentityReviewBasisHash != "" ||
			command.IdentityReviewOperationID != "" || command.IdentityReviewReceipt != "" ||
			len(command.DocumentResolutions) != 0 || len(command.NodeResolutions) != 0)) {
		return CreateProposalCommand{}, "", &Error{Code: CodeInvalidRequest}
	}
	sources, err := normalizeProposalSources(command.Sources)
	if err != nil {
		return CreateProposalCommand{}, "", err
	}
	command.Sources = sources
	seenPaths := make(map[string]struct{}, len(command.CandidateSnapshot))
	totalBodyBytes := 0
	for index := range command.CandidateSnapshot {
		path, err := NormalizePath(command.CandidateSnapshot[index].Path)
		if err != nil {
			return CreateProposalCommand{}, "", err
		}
		if _, duplicate := seenPaths[foldedPath(path)]; duplicate {
			return CreateProposalCommand{}, "", &Error{Code: CodeInvalidPath}
		}
		seenPaths[foldedPath(path)] = struct{}{}
		inspected, err := s.canonicalizer.Inspect(command.CandidateSnapshot[index].Markdown)
		if err != nil {
			return CreateProposalCommand{}, "", err
		}
		command.CandidateSnapshot[index] = ImportDocument{Path: path, Markdown: inspected.Normalized}
		totalBodyBytes += len(inspected.Normalized)
		if totalBodyBytes > MaxImportBodyBytes {
			return CreateProposalCommand{}, "", &Error{Code: CodePayloadTooLarge}
		}
	}
	sort.Slice(command.CandidateSnapshot, func(i, j int) bool { return command.CandidateSnapshot[i].Path < command.CandidateSnapshot[j].Path })
	for index := range command.DocumentResolutions {
		command.DocumentResolutions[index].DocumentID = strings.ToLower(strings.TrimSpace(command.DocumentResolutions[index].DocumentID))
	}
	for index := range command.NodeResolutions {
		for sourceIndex := range command.NodeResolutions[index].SourceNodeRevisionIDs {
			command.NodeResolutions[index].SourceNodeRevisionIDs[sourceIndex] = strings.ToLower(strings.TrimSpace(command.NodeResolutions[index].SourceNodeRevisionIDs[sourceIndex]))
		}
		sort.Strings(command.NodeResolutions[index].SourceNodeRevisionIDs)
	}
	sort.Slice(command.DocumentResolutions, func(i, j int) bool {
		return command.DocumentResolutions[i].Locator < command.DocumentResolutions[j].Locator
	})
	sort.Slice(command.NodeResolutions, func(i, j int) bool { return command.NodeResolutions[i].Locator < command.NodeResolutions[j].Locator })
	value := struct {
		RequestID, BaseRevisionID, ActorDeviceID          string
		Sources                                           []ProposalSource
		CandidateSnapshot                                 []ImportDocument
		IdentityBasis, IdentityOperation, IdentityReceipt string
		DocumentResolutions                               []DocumentResolution
		NodeResolutions                                   []NodeResolution
	}{
		RequestID: command.RequestID, BaseRevisionID: command.BaseRevisionID, ActorDeviceID: command.ActorDeviceID,
		Sources: command.Sources, CandidateSnapshot: command.CandidateSnapshot,
		IdentityBasis: command.IdentityReviewBasisHash, IdentityOperation: command.IdentityReviewOperationID,
		IdentityReceipt: command.IdentityReviewReceipt, DocumentResolutions: command.DocumentResolutions,
		NodeResolutions: command.NodeResolutions,
	}
	return command, hashMaintenanceRequest(value), nil
}

func normalizeRollbackCommand(command CreateRollbackCommand) (CreateRollbackCommand, string, error) {
	command.RequestID = strings.ToLower(strings.TrimSpace(command.RequestID))
	command.BaseRevisionID = strings.ToLower(strings.TrimSpace(command.BaseRevisionID))
	command.TargetRevisionID = strings.ToLower(strings.TrimSpace(command.TargetRevisionID))
	command.ActorDeviceID = strings.ToLower(strings.TrimSpace(command.ActorDeviceID))
	if !validUUID(command.RequestID) || !validUUID(command.BaseRevisionID) || !validUUID(command.TargetRevisionID) ||
		!validUUID(command.ActorDeviceID) || command.BaseRevisionID == command.TargetRevisionID {
		return CreateRollbackCommand{}, "", &Error{Code: CodeInvalidRequest}
	}
	sources, err := normalizeProposalSources(command.Sources)
	if err != nil {
		return CreateRollbackCommand{}, "", err
	}
	command.Sources = sources
	return command, hashMaintenanceRequest(struct {
		RequestID, BaseRevisionID, TargetRevisionID, ActorDeviceID string
		Sources                                                    []ProposalSource
	}{command.RequestID, command.BaseRevisionID, command.TargetRevisionID, command.ActorDeviceID, command.Sources}), nil
}

func normalizeDecisionCommand(command ProposalDecisionCommand) (ProposalDecisionCommand, string, error) {
	command.OperationID = strings.ToLower(strings.TrimSpace(command.OperationID))
	command.ProposalID = strings.ToLower(strings.TrimSpace(command.ProposalID))
	command.ActorDeviceID = strings.ToLower(strings.TrimSpace(command.ActorDeviceID))
	command.Decision = strings.TrimSpace(command.Decision)
	command.Reason = strings.TrimSpace(command.Reason)
	if !validUUID(command.OperationID) || !validUUID(command.ProposalID) || !validUUID(command.ActorDeviceID) ||
		(command.Decision != "approve" && command.Decision != "reject") || command.Reason == "" ||
		!utf8.ValidString(command.Reason) || strings.IndexByte(command.Reason, 0) >= 0 ||
		utf8.RuneCountInString(command.Reason) > MaxMaintenanceSourceRunes {
		return ProposalDecisionCommand{}, "", &Error{Code: CodeInvalidRequest}
	}
	return command, hashMaintenanceRequest(struct {
		OperationID, ProposalID, Decision, Reason, ActorDeviceID string
	}{command.OperationID, command.ProposalID, command.Decision, command.Reason, command.ActorDeviceID}), nil
}

func normalizeProposalSources(sources []ProposalSource) ([]ProposalSource, error) {
	if len(sources) == 0 || len(sources) > MaxMaintenanceSources {
		return nil, &Error{Code: CodeInvalidRequest}
	}
	result := append([]ProposalSource(nil), sources...)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		result[index].Kind = strings.TrimSpace(result[index].Kind)
		result[index].Locator = strings.TrimSpace(result[index].Locator)
		result[index].Title = strings.TrimSpace(result[index].Title)
		result[index].SHA256 = strings.ToLower(strings.TrimSpace(result[index].SHA256))
		switch result[index].Kind {
		case "url", "file", "note", "repository", "other":
		default:
			return nil, &Error{Code: CodeInvalidRequest}
		}
		if result[index].Locator == "" || !utf8.ValidString(result[index].Locator) ||
			utf8.RuneCountInString(result[index].Locator) > 2048 || strings.IndexByte(result[index].Locator, 0) >= 0 ||
			!utf8.ValidString(result[index].Title) || utf8.RuneCountInString(result[index].Title) > MaxSourceRunes ||
			strings.IndexByte(result[index].Title, 0) >= 0 || !utf8.ValidString(result[index].Excerpt) ||
			utf8.RuneCountInString(result[index].Excerpt) > MaxMaintenanceSourceRunes || strings.IndexByte(result[index].Excerpt, 0) >= 0 ||
			!validSHA256Hex(result[index].SHA256) {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		excerptHash := sha256Hex([]byte(result[index].Excerpt))
		if result[index].SHA256 != excerptHash {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		key := result[index].Kind + "\x1f" + result[index].Locator + "\x1f" + result[index].SHA256
		if _, duplicate := seen[key]; duplicate {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		seen[key] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].Locator != result[j].Locator {
			return result[i].Locator < result[j].Locator
		}
		return result[i].SHA256 < result[j].SHA256
	})
	return result, nil
}

func hashMaintenanceRequest(value any) string {
	encoded, _ := json.Marshal(value)
	return sha256Hex(encoded)
}

func validSHA256Hex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func analyzeMaintenanceRevision(base, candidate KnowledgeRevision) maintenanceAnalysis {
	oldDocuments := make(map[string]SnapshotDocument, len(base.Documents))
	newDocuments := make(map[string]SnapshotDocument, len(candidate.Documents))
	allIDs := make(map[string]struct{}, len(base.Documents)+len(candidate.Documents))
	for _, document := range base.Documents {
		oldDocuments[document.Revision.DocumentID] = document
		allIDs[document.Revision.DocumentID] = struct{}{}
	}
	for _, document := range candidate.Documents {
		newDocuments[document.Revision.DocumentID] = document
		allIDs[document.Revision.DocumentID] = struct{}{}
	}
	ids := make([]string, 0, len(allIDs))
	for id := range allIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	analysis := maintenanceAnalysis{lineage: LineageImpact{Lineages: append([]Lineage(nil), candidate.Lineages...)}}
	affected := make(map[string]struct{})
	for _, documentID := range ids {
		oldDocument, hadOld := oldDocuments[documentID]
		newDocument, hasNew := newDocuments[documentID]
		switch {
		case !hadOld:
			analysis.identity.AddedDocumentIDs = append(analysis.identity.AddedDocumentIDs, documentID)
			for _, node := range newDocument.Revision.Nodes {
				analysis.identity.AddedNodeIDs = append(analysis.identity.AddedNodeIDs, node.NodeID)
			}
			diff, truncated := boundedMaintenanceDiff("/dev/null", "", newDocument.Path, newDocument.Revision.CanonicalMarkdown)
			analysis.diff = append(analysis.diff, DocumentDiff{
				DocumentID: documentID, AfterPath: newDocument.Path, Kind: "add", Unified: diff, Truncated: truncated,
				AddedNodeIDs: stableNodeIDs(newDocument.Revision.Nodes), ChangedBodyBytes: len(newDocument.Revision.CanonicalMarkdown),
			})
		case !hasNew:
			analysis.identity.RemovedDocumentIDs = append(analysis.identity.RemovedDocumentIDs, documentID)
			analysis.lineage.Delete = true
			for _, node := range oldDocument.Revision.Nodes {
				analysis.identity.RemovedNodeIDs = append(analysis.identity.RemovedNodeIDs, node.NodeID)
				affected[node.ID] = struct{}{}
			}
			diff, truncated := boundedMaintenanceDiff(oldDocument.Path, oldDocument.Revision.CanonicalMarkdown, "/dev/null", "")
			analysis.diff = append(analysis.diff, DocumentDiff{
				DocumentID: documentID, BeforePath: oldDocument.Path, Kind: "delete", Unified: diff, Truncated: truncated,
				RemovedNodeIDs: stableNodeIDs(oldDocument.Revision.Nodes), ChangedBodyBytes: len(oldDocument.Revision.CanonicalMarkdown),
			})
		default:
			analysis.identity.PreservedDocumentIDs = append(analysis.identity.PreservedDocumentIDs, documentID)
			moved := oldDocument.Path != newDocument.Path
			if moved {
				analysis.identity.MovedDocumentIDs = append(analysis.identity.MovedDocumentIDs, documentID)
				analysis.lineage.Move = true
			}
			if oldDocument.Revision.ID == newDocument.Revision.ID && !moved {
				for _, node := range newDocument.Revision.Nodes {
					analysis.identity.PreservedNodeIDs = append(analysis.identity.PreservedNodeIDs, node.NodeID)
				}
				continue
			}
			documentDiff := compareMaintenanceDocument(oldDocument, newDocument)
			if moved && documentDiff.Kind == "edit" {
				documentDiff.Kind = "move_edit"
			} else if moved && documentDiff.Kind == "unchanged" {
				documentDiff.Kind = "move"
			}
			for _, node := range oldDocument.Revision.Nodes {
				affected[node.ID] = struct{}{}
			}
			analysis.diff = append(analysis.diff, documentDiff)
			analysis.identity.PreservedNodeIDs = append(analysis.identity.PreservedNodeIDs, intersectNodeIDs(oldDocument.Revision.Nodes, newDocument.Revision.Nodes)...)
			analysis.identity.AddedNodeIDs = append(analysis.identity.AddedNodeIDs, documentDiff.AddedNodeIDs...)
			analysis.identity.RemovedNodeIDs = append(analysis.identity.RemovedNodeIDs, documentDiff.RemovedNodeIDs...)
		}
	}
	for id := range affected {
		analysis.affectedNodeRevisionIDs = append(analysis.affectedNodeRevisionIDs, id)
	}
	sort.Strings(analysis.affectedNodeRevisionIDs)
	sortMaintenanceImpact(&analysis)
	boundAggregateMaintenanceDiff(analysis.diff)
	return analysis
}

func boundAggregateMaintenanceDiff(diffs []DocumentDiff) {
	remaining := MaxMaintenanceDiffBytes
	const suffix = "\n@@ proposal diff truncated @@\n"
	for index := range diffs {
		if len(diffs[index].Unified) <= remaining {
			remaining -= len(diffs[index].Unified)
			continue
		}
		marker := suffix
		if len(marker) > remaining {
			marker = marker[:remaining]
		}
		limit := remaining - len(marker)
		for limit > 0 && !utf8.ValidString(diffs[index].Unified[:limit]) {
			limit--
		}
		diffs[index].Unified = diffs[index].Unified[:limit] + marker
		diffs[index].Truncated = true
		for later := index + 1; later < len(diffs); later++ {
			if diffs[later].Unified != "" {
				diffs[later].Unified = ""
				diffs[later].Truncated = true
			}
		}
		return
	}
}

func compareMaintenanceDocument(oldDocument, newDocument SnapshotDocument) DocumentDiff {
	result := DocumentDiff{DocumentID: oldDocument.Revision.DocumentID, BeforePath: oldDocument.Path, AfterPath: newDocument.Path, Kind: "edit", LocalBodyOnly: true}
	result.Unified, result.Truncated = boundedMaintenanceDiff(oldDocument.Path, oldDocument.Revision.CanonicalMarkdown, newDocument.Path, newDocument.Revision.CanonicalMarkdown)
	oldByID := make(map[string]NodeRevision, len(oldDocument.Revision.Nodes))
	newByID := make(map[string]NodeRevision, len(newDocument.Revision.Nodes))
	oldRevisionToStable := make(map[string]string, len(oldDocument.Revision.Nodes))
	newRevisionToStable := make(map[string]string, len(newDocument.Revision.Nodes))
	all := make(map[string]struct{}, len(oldDocument.Revision.Nodes)+len(newDocument.Revision.Nodes))
	for _, node := range oldDocument.Revision.Nodes {
		oldByID[node.NodeID] = node
		oldRevisionToStable[node.ID] = node.NodeID
		all[node.NodeID] = struct{}{}
	}
	for _, node := range newDocument.Revision.Nodes {
		newByID[node.NodeID] = node
		newRevisionToStable[node.ID] = node.NodeID
		all[node.NodeID] = struct{}{}
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		oldNode, hadOld := oldByID[id]
		newNode, hasNew := newByID[id]
		switch {
		case !hadOld:
			result.AddedNodeIDs = append(result.AddedNodeIDs, id)
			result.LocalBodyOnly = false
		case !hasNew:
			result.RemovedNodeIDs = append(result.RemovedNodeIDs, id)
			result.LocalBodyOnly = false
		default:
			oldParent := stableParentID(oldNode, oldRevisionToStable)
			newParent := stableParentID(newNode, newRevisionToStable)
			if oldNode.Title != newNode.Title || strings.Join(oldNode.AncestorTitles, "\x1f") != strings.Join(newNode.AncestorTitles, "\x1f") {
				result.TitleNodeIDs = append(result.TitleNodeIDs, id)
				result.LocalBodyOnly = false
			}
			if oldParent != newParent || oldNode.SiblingIndex != newNode.SiblingIndex || oldNode.HeadingLevel != newNode.HeadingLevel {
				result.StructureNodeIDs = append(result.StructureNodeIDs, id)
				result.LocalBodyOnly = false
			}
			if oldNode.SemanticLocalBodyHash != newNode.SemanticLocalBodyHash {
				result.EditedNodeIDs = append(result.EditedNodeIDs, id)
				result.ChangedBodyBytes += localBodyLength(newDocument.Revision, newNode)
			}
		}
	}
	if oldDocument.Revision.CanonicalHash != newDocument.Revision.CanonicalHash && len(result.EditedNodeIDs) == 0 && len(result.TitleNodeIDs) == 0 && len(result.StructureNodeIDs) == 0 && len(result.AddedNodeIDs) == 0 && len(result.RemovedNodeIDs) == 0 {
		result.LocalBodyOnly = false
		result.StructureNodeIDs = append(result.StructureNodeIDs, newDocument.Revision.RootNodeID)
	}
	if oldDocument.Revision.ID == newDocument.Revision.ID {
		result.Kind = "unchanged"
		result.LocalBodyOnly = false
	}
	return result
}

func boundedMaintenanceDiff(fromName, from, toName, to string) (string, bool) {
	if from == to {
		return "", false
	}
	if len(from) > MaxMaintenanceDiffInput || len(to) > MaxMaintenanceDiffInput ||
		strings.Count(from, "\n")+1 > MaxMaintenanceDiffLines || strings.Count(to, "\n")+1 > MaxMaintenanceDiffLines {
		return fmt.Sprintf("--- %s\n+++ %s\n@@ diff omitted: input limit exceeded @@\n", fromName, toName), true
	}
	value := string(internaldiff.Diff(fromName, []byte(from), toName, []byte(to)))
	if len(value) <= MaxMaintenanceDiffBytes {
		return value, false
	}
	const suffix = "\n@@ diff truncated @@\n"
	limit := MaxMaintenanceDiffBytes - len(suffix)
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + suffix, true
}

func maintenanceRisk(analysis maintenanceAnalysis, evidence AcceptedEvidenceImpact) ProposalRisk {
	reasons := make([]string, 0, 8)
	auto := true
	changedDocuments, changedNodes, changedBytes := len(analysis.diff), 0, 0
	for _, item := range analysis.diff {
		changedNodes += len(item.AddedNodeIDs) + len(item.RemovedNodeIDs) + len(item.EditedNodeIDs) + len(item.TitleNodeIDs) + len(item.StructureNodeIDs)
		changedBytes += item.ChangedBodyBytes
		if item.Truncated {
			auto = false
			reasons = append(reasons, "diff_truncated")
		}
		switch item.Kind {
		case "add":
		case "edit":
			if !item.LocalBodyOnly || len(item.EditedNodeIDs) == 0 {
				auto = false
				reasons = append(reasons, "non_local_edit")
			}
		default:
			auto = false
			reasons = append(reasons, item.Kind)
		}
	}
	if analysis.identity.Uncertain {
		auto = false
		reasons = append(reasons, "identity_uncertain")
	}
	if len(analysis.lineage.Lineages) != 0 {
		auto = false
		reasons = append(reasons, "lineage_change")
	}
	if analysis.lineage.Move || analysis.lineage.Delete || analysis.lineage.Restore || analysis.lineage.Rollback {
		auto = false
	}
	if analysis.lineage.Move {
		reasons = append(reasons, "move")
	}
	if analysis.lineage.Delete {
		reasons = append(reasons, "delete")
	}
	if analysis.lineage.Restore {
		reasons = append(reasons, "restore")
	}
	if analysis.lineage.Rollback {
		reasons = append(reasons, "rollback")
	}
	if evidence.Count != 0 {
		auto = false
		reasons = append(reasons, "accepted_evidence_affected")
	}
	if changedDocuments > MaxAutoApplyDocuments || changedNodes > MaxAutoApplyNodes || changedBytes > MaxAutoApplyChangedBodyByte {
		auto = false
		reasons = append(reasons, "auto_apply_bounds_exceeded")
	}
	level := "low"
	if !auto {
		level = "medium"
	}
	if analysis.lineage.Delete || analysis.lineage.Restore || analysis.lineage.Rollback || len(analysis.lineage.Lineages) != 0 {
		level = "high"
	}
	reasons = uniqueSorted(reasons)
	if auto {
		reasons = []string{"bounded_add_or_local_body_edit"}
	}
	return ProposalRisk{Level: level, Reasons: reasons, AutoApply: auto, PolicyVersion: MaintenanceAutoPolicy}
}

func ComputeProposalBasis(proposal Proposal) string {
	value := struct {
		Version, ProposalID, Kind, BaseRevisionID, RollbackTargetRevisionID string
		PlannedRevisionID, PlannedManifestHash                              string
		PlannedRevisionNo, KnowledgeGeneration                              int64
		Sources                                                             []ProposalSource
		CandidateHash                                                       string
		Diff                                                                []DocumentDiff
		Identity                                                            IdentityImpact
		Lineage                                                             LineageImpact
		Evidence                                                            AcceptedEvidenceImpact
		Risk                                                                ProposalRisk
		Canonicalizer, IdentityPolicy, DiffVersion, RiskVersion, AutoPolicy string
	}{
		Version: MaintenanceBasisVersion, ProposalID: proposal.ID, Kind: string(proposal.Kind),
		BaseRevisionID: proposal.BaseRevisionID, RollbackTargetRevisionID: proposal.RollbackTargetRevisionID,
		PlannedRevisionID: proposal.PlannedRevisionID, PlannedManifestHash: proposal.PlannedManifestHash,
		PlannedRevisionNo: proposal.PlannedRevisionNo, KnowledgeGeneration: proposal.KnowledgeGeneration,
		Sources: proposal.Sources, CandidateHash: hashMaintenanceRequest(proposal.CandidateSnapshot), Diff: proposal.Diff,
		Identity: proposal.IdentityImpact, Lineage: proposal.LineageImpact, Evidence: proposal.EvidenceImpact,
		Risk: proposal.Risk, Canonicalizer: proposal.CanonicalizerVersion,
		IdentityPolicy: proposal.IdentityPolicyVersion, DiffVersion: proposal.DiffVersion,
		RiskVersion: proposal.RiskVersion, AutoPolicy: proposal.AutoPolicyVersion,
	}
	return hashMaintenanceRequest(value)
}

func cloneSnapshotDocuments(input []SnapshotDocument) []SnapshotDocument {
	result := make([]SnapshotDocument, len(input))
	copy(result, input)
	for index := range result {
		result[index].Revision.Nodes = append([]NodeRevision(nil), input[index].Revision.Nodes...)
		for nodeIndex := range result[index].Revision.Nodes {
			result[index].Revision.Nodes[nodeIndex].AncestorTitles = append([]string(nil), input[index].Revision.Nodes[nodeIndex].AncestorTitles...)
			result[index].Revision.Nodes[nodeIndex].Children = append([]string(nil), input[index].Revision.Nodes[nodeIndex].Children...)
		}
	}
	return result
}

func stableNodeIDs(nodes []NodeRevision) []string {
	result := make([]string, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node.NodeID)
	}
	sort.Strings(result)
	return result
}

func intersectNodeIDs(oldNodes, newNodes []NodeRevision) []string {
	old := make(map[string]struct{}, len(oldNodes))
	for _, node := range oldNodes {
		old[node.NodeID] = struct{}{}
	}
	var result []string
	for _, node := range newNodes {
		if _, exists := old[node.NodeID]; exists {
			result = append(result, node.NodeID)
		}
	}
	sort.Strings(result)
	return result
}

func stableParentID(node NodeRevision, revisionToStable map[string]string) string {
	if node.ParentNodeRevisionID == nil {
		return ""
	}
	return revisionToStable[*node.ParentNodeRevisionID]
}

func localBodyLength(document DocumentRevision, node NodeRevision) int {
	if node.LocalBodyRange.Start < 0 || node.LocalBodyRange.End < node.LocalBodyRange.Start || node.LocalBodyRange.End > len(document.CanonicalMarkdown) {
		return 0
	}
	return node.LocalBodyRange.End - node.LocalBodyRange.Start
}

func sortMaintenanceImpact(analysis *maintenanceAnalysis) {
	for _, values := range [][]string{
		analysis.identity.PreservedDocumentIDs, analysis.identity.AddedDocumentIDs,
		analysis.identity.RemovedDocumentIDs, analysis.identity.MovedDocumentIDs,
		analysis.identity.PreservedNodeIDs, analysis.identity.AddedNodeIDs, analysis.identity.RemovedNodeIDs,
	} {
		sort.Strings(values)
	}
	sort.Slice(analysis.diff, func(i, j int) bool {
		left := analysis.diff[i].AfterPath
		if left == "" {
			left = analysis.diff[i].BeforePath
		}
		right := analysis.diff[j].AfterPath
		if right == "" {
			right = analysis.diff[j].BeforePath
		}
		if left != right {
			return left < right
		}
		return analysis.diff[i].DocumentID < analysis.diff[j].DocumentID
	})
}
