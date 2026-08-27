package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var evidenceCarryoverProposalNamespace = uuid.MustParse("3fb62bc1-348d-4ace-9aa6-506262586c9d")

const maintenanceProposalColumns = `
	proposal_id::text,request_id::text,request_hash,kind,status,base_revision_id::text,
	rollback_target_revision_id::text,planned_revision_id::text,planned_revision_no,
	planned_manifest_hash,current_revision_id::text,applied_revision_id::text,basis_hash,
	knowledge_generation,evidence_generation,canonicalizer_version,identity_policy_version,
	diff_version,risk_version,auto_policy_version,created_by_device_id::text,
	created_at,updated_at,redacted_at IS NOT NULL,record`

type storedProposalRecord struct {
	Proposal                knowledge.Proposal `json:"proposal"`
	AffectedNodeRevisionIDs []string           `json:"affected_node_revision_ids"`
}

type storedPreparedCommit struct {
	Commit            knowledge.PreparedCommit `json:"commit"`
	CanonicalMarkdown map[string]string        `json:"canonical_markdown"`
}

func (s *Store) MaintenanceBase(ctx context.Context, revisionID string) (knowledge.MaintenanceBaseSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return knowledge.MaintenanceBaseSnapshot{}, fmt.Errorf("begin knowledge maintenance base read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return knowledge.MaintenanceBaseSnapshot{}, err
	}
	var headID *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1`).Scan(&headID); err != nil {
		return knowledge.MaintenanceBaseSnapshot{}, fmt.Errorf("read knowledge maintenance head: %w", err)
	}
	revision, err := loadRevision(ctx, tx, revisionID)
	if err != nil {
		return knowledge.MaintenanceBaseSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.MaintenanceBaseSnapshot{}, fmt.Errorf("commit knowledge maintenance base read: %w", err)
	}
	return knowledge.MaintenanceBaseSnapshot{
		Revision: revision, HeadRevisionID: optionalString(headID), KnowledgeGeneration: generation,
	}, nil
}

func (s *Store) beginMaintenanceProposalRead(ctx context.Context) (pgx.Tx, int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, fmt.Errorf("begin knowledge maintenance proposal privacy read: %w", err)
	}
	// Proposal payloads include learning-owned evidence impact. Match the
	// carryover knowledge-before-learning owner order.
	knowledgeGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, 0, err
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, 0, err
	}
	return tx, knowledgeGeneration, nil
}

func (s *Store) LookupMaintenanceOperation(ctx context.Context, operationID string) (knowledge.MaintenanceOperationRecord, bool, error) {
	tx, _, err := s.beginMaintenanceProposalRead(ctx)
	if err != nil {
		return knowledge.MaintenanceOperationRecord{}, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	record, exists, err := lookupMaintenanceOperationWith(ctx, tx, operationID, false)
	if err != nil {
		return knowledge.MaintenanceOperationRecord{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.MaintenanceOperationRecord{}, false, fmt.Errorf("commit knowledge maintenance operation read: %w", err)
	}
	return record, exists, nil
}

func (s *Store) SaveProposal(ctx context.Context, prepared knowledge.PreparedProposal) (knowledge.Proposal, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return knowledge.Proposal{}, fmt.Errorf("begin knowledge maintenance proposal: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return knowledge.Proposal{}, err
	}
	if prepared.Proposal.KnowledgeGeneration != generation {
		return knowledge.Proposal{}, &knowledge.Error{Code: knowledge.CodeProposalStale}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('knowledge-maintenance-operation:'||$1,0))`, prepared.Proposal.RequestID); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("lock knowledge maintenance create operation: %w", err)
	}
	if stored, exists, err := lookupMaintenanceOperationWith(ctx, tx, prepared.Proposal.RequestID, true); err != nil {
		return knowledge.Proposal{}, err
	} else if exists {
		if stored.RequestHash != prepared.Proposal.RequestHash {
			return knowledge.Proposal{}, &knowledge.Error{Code: knowledge.CodeIdempotencyConflict}
		}
		proposal, err := readMaintenanceProposal(ctx, tx, stored.ProposalID, false)
		if err != nil {
			return knowledge.Proposal{}, err
		}
		proposal.Replayed = true
		return proposal, nil
	}
	// Lock the catalog before learning tables so an apply cannot deadlock while
	// upgrading its carryover tables behind another same-base proposal.
	var headID *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1 FOR UPDATE`).Scan(&headID); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("lock knowledge head for maintenance proposal: %w", err)
	}
	if headID == nil || *headID != prepared.Proposal.BaseRevisionID || !sameOptional(headID, prepared.Commit.ExpectedParentRevisionID) {
		return knowledge.Proposal{}, &knowledge.Error{Code: knowledge.CodeRevisionConflict, CurrentRevisionID: headID, CurrentRevisionKnown: true}
	}
	currentEvidence, err := lockAcceptedEvidenceImpact(ctx, tx, prepared.Proposal.AffectedNodeRevisionIDs)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	if currentEvidence.Generation != prepared.Proposal.EvidenceImpact.Generation ||
		currentEvidence.Fingerprint != prepared.Proposal.EvidenceImpact.Fingerprint {
		return knowledge.Proposal{}, &knowledge.Error{Code: knowledge.CodeProposalStale}
	}
	proposal := prepared.Proposal
	proposal.PlannedRevisionID = prepared.Commit.Revision.ID
	proposal.PlannedRevisionNo = prepared.Commit.Revision.RevisionNo
	proposal.PlannedManifestHash = prepared.Commit.Revision.ManifestHash
	proposal.BasisHash = knowledge.ComputeProposalBasis(proposal)
	if err := validatePreparedProposal(proposal, prepared.Commit); err != nil {
		return knowledge.Proposal{}, err
	}
	record, err := encodeStoredProposal(proposal)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	plan, err := encodeStoredPreparedCommit(prepared.Commit)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	requestHash, err := decodeHash(proposal.RequestHash)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	basisHash, err := decodeHash(proposal.BasisHash)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	manifestHash, err := decodeHash(proposal.PlannedManifestHash)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_maintenance_proposals(
		  proposal_id,request_id,request_hash,kind,status,base_revision_id,rollback_target_revision_id,
		  planned_revision_id,planned_revision_no,planned_manifest_hash,basis_hash,knowledge_generation,
		  evidence_generation,canonicalizer_version,identity_policy_version,diff_version,risk_version,
		  auto_policy_version,record,prepared_commit,created_by_device_id,created_at,updated_at)
		VALUES($1,$2,$3,$4,'open',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$21)`,
		proposal.ID, proposal.RequestID, requestHash, proposal.Kind, proposal.BaseRevisionID,
		nullableUUID(proposal.RollbackTargetRevisionID), proposal.PlannedRevisionID, proposal.PlannedRevisionNo,
		manifestHash, basisHash, proposal.KnowledgeGeneration, proposal.EvidenceImpact.Generation,
		proposal.CanonicalizerVersion, proposal.IdentityPolicyVersion, proposal.DiffVersion,
		proposal.RiskVersion, proposal.AutoPolicyVersion, record, plan, proposal.CreatedByDeviceID,
		proposal.CreatedAt.UTC()); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("insert knowledge maintenance proposal: %w", err)
	}
	if proposal.Risk.AutoApply {
		if proposal.Decision == nil || proposal.Decision.RequestedDecision != "auto" || proposal.Decision.Outcome != string(knowledge.ProposalApplied) {
			return knowledge.Proposal{}, errors.New("auto-apply proposal lacks its policy decision")
		}
		if err := s.applyMaintenanceRevision(ctx, tx, proposal, prepared.Commit, generation, proposal.Decision.CreatedAt); err != nil {
			return knowledge.Proposal{}, err
		}
		if err := insertMaintenanceDecision(ctx, tx, proposal.ID, *proposal.Decision); err != nil {
			return knowledge.Proposal{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_maintenance_proposals
			SET status='applied',current_revision_id=$2,applied_revision_id=$2,updated_at=$3
			WHERE proposal_id=$1 AND status='open'`, proposal.ID, prepared.Commit.Revision.ID, proposal.Decision.CreatedAt.UTC()); err != nil {
			return knowledge.Proposal{}, fmt.Errorf("mark auto-applied knowledge proposal: %w", err)
		}
		proposal.Status = knowledge.ProposalApplied
		proposal.CurrentRevisionID = prepared.Commit.Revision.ID
		proposal.AppliedRevisionID = prepared.Commit.Revision.ID
		proposal.Origin = maintenanceOrigin(proposal)
		proposal.UpdatedAt = proposal.Decision.CreatedAt.UTC()
	}
	if err := insertMaintenanceOperation(ctx, tx, proposal.RequestID, "create", proposal.RequestHash, proposal.ID, proposal.UpdatedAt); err != nil {
		return knowledge.Proposal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("commit knowledge maintenance proposal: %w", err)
	}
	return proposal, nil
}

func (s *Store) ListProposals(ctx context.Context, command knowledge.ProposalListCommand) (knowledge.ProposalPage, error) {
	tx, generation, err := s.beginMaintenanceProposalRead(ctx)
	if err != nil {
		return knowledge.ProposalPage{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if command.ExpectedGeneration != 0 && command.ExpectedGeneration != generation {
		return knowledge.ProposalPage{}, &knowledge.Error{Code: knowledge.CodeProposalStale}
	}
	var afterAt any
	var afterID any
	if !command.AfterCreatedAt.IsZero() {
		afterAt = command.AfterCreatedAt.UTC()
		afterID = command.AfterProposalID
	}
	rows, err := tx.Query(ctx, `
		SELECT proposal_id::text
		FROM knowledge_maintenance_proposals
		WHERE ($1='all' OR status=$1)
		  AND ($2::timestamptz IS NULL OR (created_at,proposal_id)>($2,$3::uuid))
		ORDER BY created_at,proposal_id LIMIT $4`, command.Status, afterAt, afterID, command.Limit+1)
	if err != nil {
		return knowledge.ProposalPage{}, fmt.Errorf("query knowledge maintenance proposals: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return knowledge.ProposalPage{}, fmt.Errorf("scan knowledge maintenance proposal ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return knowledge.ProposalPage{}, fmt.Errorf("iterate knowledge maintenance proposals: %w", err)
	}
	rows.Close()
	result := knowledge.ProposalPage{Items: make([]knowledge.Proposal, 0, command.Limit)}
	for _, id := range ids {
		proposal, err := readMaintenanceProposal(ctx, tx, id, false)
		if err != nil {
			return knowledge.ProposalPage{}, err
		}
		proposal.CandidateSnapshot = nil
		result.Items = append(result.Items, proposal)
	}
	if len(result.Items) > command.Limit {
		result.Items = result.Items[:command.Limit]
		last := result.Items[len(result.Items)-1]
		result.NextCursor = knowledge.EncodeProposalCursor(generation, last.CreatedAt, last.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.ProposalPage{}, fmt.Errorf("commit knowledge maintenance proposal list: %w", err)
	}
	return result, nil
}

func (s *Store) Proposal(ctx context.Context, proposalID string) (knowledge.Proposal, error) {
	tx, _, err := s.beginMaintenanceProposalRead(ctx)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	proposal, err := readMaintenanceProposal(ctx, tx, proposalID, false)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("commit knowledge maintenance proposal read: %w", err)
	}
	return proposal, nil
}

func (s *Store) DecideProposal(ctx context.Context, command knowledge.PreparedProposalDecision) (knowledge.Proposal, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return knowledge.Proposal{}, fmt.Errorf("begin knowledge maintenance decision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return knowledge.Proposal{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('knowledge-maintenance-operation:'||$1,0))`, command.OperationID); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("lock knowledge maintenance decision operation: %w", err)
	}
	if stored, exists, err := lookupMaintenanceOperationWith(ctx, tx, command.OperationID, true); err != nil {
		return knowledge.Proposal{}, err
	} else if exists {
		if stored.RequestHash != command.RequestHash {
			return knowledge.Proposal{}, &knowledge.Error{Code: knowledge.CodeIdempotencyConflict}
		}
		proposal, err := readMaintenanceProposal(ctx, tx, stored.ProposalID, false)
		if err != nil {
			return knowledge.Proposal{}, err
		}
		proposal.Replayed = true
		return proposal, nil
	}
	proposal, err := readMaintenanceProposal(ctx, tx, command.ProposalID, true)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	if proposal.Status != knowledge.ProposalOpen || proposal.Redacted {
		return knowledge.Proposal{}, &knowledge.Error{Code: knowledge.CodeProposalClosed}
	}
	// Keep the cross-owner lock order aligned with proposal creation and
	// carryover approval: knowledge catalog before learning tables.
	var headID *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1 FOR UPDATE`).Scan(&headID); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("lock knowledge head for proposal decision: %w", err)
	}
	currentEvidence, err := lockAcceptedEvidenceImpact(ctx, tx, proposal.AffectedNodeRevisionIDs)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	stale := headID == nil || *headID != proposal.BaseRevisionID || proposal.KnowledgeGeneration != generation ||
		proposal.EvidenceImpact.Generation != command.EvidenceGeneration || proposal.EvidenceImpact.Fingerprint != command.EvidenceFingerprint ||
		proposal.EvidenceImpact.Generation != currentEvidence.Generation || proposal.EvidenceImpact.Fingerprint != currentEvidence.Fingerprint ||
		proposal.CanonicalizerVersion != command.CanonicalizerVersion || proposal.IdentityPolicyVersion != command.IdentityPolicyVersion ||
		proposal.DiffVersion != command.DiffVersion || proposal.RiskVersion != command.RiskVersion ||
		proposal.AutoPolicyVersion != command.AutoPolicyVersion || proposal.BasisHash != knowledge.ComputeProposalBasis(proposal)
	decision := knowledge.ProposalDecision{
		ID: command.DecisionID, OperationID: command.OperationID, RequestedDecision: command.RequestedDecision,
		Reason: command.Reason, ActorDeviceID: command.ActorDeviceID, CreatedAt: command.DecidedAt.UTC(),
	}
	if stale {
		decision.Outcome = string(knowledge.ProposalStale)
		if err := insertMaintenanceDecision(ctx, tx, proposal.ID, decision); err != nil {
			return knowledge.Proposal{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_maintenance_proposals
			SET status='stale',current_revision_id=$2,updated_at=$3
			WHERE proposal_id=$1 AND status='open'`, proposal.ID, headID, command.DecidedAt.UTC()); err != nil {
			return knowledge.Proposal{}, fmt.Errorf("mark knowledge maintenance proposal stale: %w", err)
		}
		proposal.Status = knowledge.ProposalStale
		if headID != nil {
			proposal.CurrentRevisionID = *headID
		}
	} else if command.RequestedDecision == "reject" {
		decision.Outcome = string(knowledge.ProposalRejected)
		if err := insertMaintenanceDecision(ctx, tx, proposal.ID, decision); err != nil {
			return knowledge.Proposal{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_maintenance_proposals SET status='rejected',updated_at=$2
			WHERE proposal_id=$1 AND status='open'`, proposal.ID, command.DecidedAt.UTC()); err != nil {
			return knowledge.Proposal{}, fmt.Errorf("reject knowledge maintenance proposal: %w", err)
		}
		proposal.Status = knowledge.ProposalRejected
	} else {
		commit, err := readStoredPreparedCommit(ctx, tx, proposal.ID)
		if err != nil {
			return knowledge.Proposal{}, err
		}
		decision.Outcome = string(knowledge.ProposalApplied)
		if err := s.applyMaintenanceRevision(ctx, tx, proposal, commit, generation, command.DecidedAt); err != nil {
			return knowledge.Proposal{}, err
		}
		if err := insertMaintenanceDecision(ctx, tx, proposal.ID, decision); err != nil {
			return knowledge.Proposal{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_maintenance_proposals
			SET status='applied',current_revision_id=$2,applied_revision_id=$2,updated_at=$3
			WHERE proposal_id=$1 AND status='open'`, proposal.ID, commit.Revision.ID, command.DecidedAt.UTC()); err != nil {
			return knowledge.Proposal{}, fmt.Errorf("apply knowledge maintenance proposal: %w", err)
		}
		proposal.Status = knowledge.ProposalApplied
		proposal.CurrentRevisionID = commit.Revision.ID
		proposal.AppliedRevisionID = commit.Revision.ID
		proposal.Origin = maintenanceOrigin(proposal)
	}
	proposal.Decision = &decision
	proposal.UpdatedAt = command.DecidedAt.UTC()
	if err := insertMaintenanceOperation(ctx, tx, command.OperationID, "decide", command.RequestHash, proposal.ID, command.DecidedAt); err != nil {
		return knowledge.Proposal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.Proposal{}, fmt.Errorf("commit knowledge maintenance decision: %w", err)
	}
	return proposal, nil
}

func (s *Store) applyMaintenanceRevision(
	ctx context.Context,
	tx pgx.Tx,
	proposal knowledge.Proposal,
	commit knowledge.PreparedCommit,
	generation int64,
	appliedAt time.Time,
) error {
	if err := validatePreparedProposal(proposal, commit); err != nil {
		return err
	}
	commit.Revision.CreatedAt = appliedAt.UTC().Truncate(time.Microsecond)
	commit.Lineages = append([]knowledge.Lineage(nil), commit.Lineages...)
	for index := range commit.Lineages {
		commit.Lineages[index].CreatedAt = commit.Revision.CreatedAt
	}
	commit.Revision.Lineages = append([]knowledge.Lineage(nil), commit.Lineages...)
	var parentDocuments map[string]notesyncParentDocument
	var err error
	if s.notesyncPublication {
		parentDocuments, err = loadParentDocumentRevisions(ctx, tx, commit.Revision.ParentRevisionID)
		if err != nil {
			return err
		}
		if err := lockNotesyncOutboxGeneration(ctx, tx, generation); err != nil {
			return err
		}
		if err := lockNotesyncPublicationDocuments(ctx, tx, commit.Revision, parentDocuments); err != nil {
			return err
		}
	}
	if err := insertRevision(ctx, tx, commit.Revision, commit.Lineages); err != nil {
		return err
	}
	basis, err := decodeHash(proposal.BasisHash)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_revision_origins(
		  revision_id,proposal_id,origin_version,origin_kind,base_revision_id,
		  rollback_target_revision_id,basis_hash,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		commit.Revision.ID, proposal.ID, knowledge.MaintenanceOriginVersion, proposal.Kind,
		proposal.BaseRevisionID, nullableUUID(proposal.RollbackTargetRevisionID), basis, commit.Revision.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("insert knowledge revision origin: %w", err)
	}
	if err := insertEvidenceCarryoverProposals(ctx, tx, proposal, commit); err != nil {
		return err
	}
	if s.notesyncPublication {
		if err := enqueueNotesyncPublicationIntents(ctx, tx, commit.Revision, generation, parentDocuments, nil, nil); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE knowledge_catalog SET head_revision_id=$1,updated_at=$2 WHERE singleton_id=1`, commit.Revision.ID, commit.Revision.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("advance knowledge head for maintenance proposal: %w", err)
	}
	return nil
}

func insertEvidenceCarryoverProposals(ctx context.Context, tx pgx.Tx, proposal knowledge.Proposal, commit knowledge.PreparedCommit) error {
	if len(proposal.EvidenceImpact.References) == 0 {
		return nil
	}
	targetsByNodeID := make(map[string]learning.EvidenceCarryoverCandidate)
	targetsByRevisionID := make(map[string]learning.EvidenceCarryoverCandidate)
	for _, document := range commit.Revision.Documents {
		for _, node := range document.Revision.Nodes {
			target := learning.EvidenceCarryoverCandidate{
				KnowledgeRevisionID: commit.Revision.ID, NodeID: node.NodeID,
				NodeRevisionID: node.ID, DocumentRevisionID: node.DocumentRevisionID,
			}
			targetsByNodeID[node.NodeID] = target
			targetsByRevisionID[node.ID] = target
		}
	}
	lineageTargets := make(map[string][]learning.EvidenceCarryoverCandidate)
	for _, lineage := range commit.Lineages {
		var sources []string
		var targets []learning.EvidenceCarryoverCandidate
		for _, member := range lineage.Members {
			switch member.Role {
			case "source":
				sources = append(sources, member.NodeRevisionID)
			case "target":
				if target, ok := targetsByRevisionID[member.NodeRevisionID]; ok {
					targets = append(targets, target)
				}
			}
		}
		for _, source := range sources {
			lineageTargets[source] = append(lineageTargets[source], targets...)
		}
	}
	for _, reference := range proposal.EvidenceImpact.References {
		var sourceNodeID string
		if err := tx.QueryRow(ctx, `SELECT node_id::text FROM knowledge_node_revisions WHERE id=$1`, reference.NodeRevisionID).Scan(&sourceNodeID); err != nil {
			return fmt.Errorf("read evidence carryover source node identity: %w", err)
		}
		candidates, explicitLineage := lineageTargets[reference.NodeRevisionID]
		if !explicitLineage {
			if target, ok := targetsByNodeID[sourceNodeID]; ok {
				candidates = append(candidates, target)
			}
		}
		var err error
		candidates, candidateFingerprint, err := learning.NormalizeEvidenceCarryoverCandidates(candidates)
		if err != nil {
			return err
		}
		carryoverID := uuid.NewSHA1(evidenceCarryoverProposalNamespace, []byte(proposal.ID+"\n"+reference.EvidenceID)).String()
		carryoverKey, err := learning.HashJSON(struct {
			Version, KnowledgeProposalID, EvidenceID string
		}{learning.EvidenceCarryoverPolicyVersion, proposal.ID, reference.EvidenceID})
		if err != nil {
			return err
		}
		carryover := learning.EvidenceCarryoverProposal{
			ID: carryoverID, KnowledgeProposalID: proposal.ID, Status: learning.EvidenceCarryoverOpen,
			SourceEvidenceID: reference.EvidenceID, SourceKnowledgeRevisionID: reference.KnowledgeRevisionID,
			SourceNodeRevisionID: reference.NodeRevisionID, TargetKnowledgeRevisionID: commit.Revision.ID,
			Candidates: candidates, KnowledgeBasisHash: proposal.BasisHash,
			EvidenceFingerprint: proposal.EvidenceImpact.Fingerprint, CandidateFingerprint: candidateFingerprint,
			KnowledgeGeneration: proposal.KnowledgeGeneration, LearningGeneration: proposal.EvidenceImpact.Generation,
			PolicyVersion: learning.EvidenceCarryoverPolicyVersion, CreatedAt: commit.Revision.CreatedAt.UTC(),
			UpdatedAt: commit.Revision.CreatedAt.UTC(),
		}
		carryover.BasisFingerprint = learning.ComputeEvidenceCarryoverBasis(carryover)
		keyBytes, err := decodeHash(carryoverKey)
		if err != nil {
			return err
		}
		knowledgeBasis, err := decodeHash(carryover.KnowledgeBasisHash)
		if err != nil {
			return err
		}
		evidenceFingerprint, err := decodeHash(carryover.EvidenceFingerprint)
		if err != nil {
			return err
		}
		candidateFingerprintBytes, err := decodeHash(carryover.CandidateFingerprint)
		if err != nil {
			return err
		}
		basisFingerprint, err := decodeHash(carryover.BasisFingerprint)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO learning_evidence_carryover_proposals(
			  proposal_id,carryover_key,knowledge_proposal_id,status,source_evidence_id,
			  source_knowledge_revision_id,source_node_revision_id,target_knowledge_revision_id,
			  knowledge_basis_hash,evidence_fingerprint,candidate_fingerprint,basis_fingerprint,
			  knowledge_generation,learning_generation,policy_version,created_at,updated_at)
			VALUES($1,$2,$3,'open',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15)`,
			carryover.ID, keyBytes, carryover.KnowledgeProposalID, carryover.SourceEvidenceID,
			carryover.SourceKnowledgeRevisionID, carryover.SourceNodeRevisionID,
			carryover.TargetKnowledgeRevisionID, knowledgeBasis, evidenceFingerprint,
			candidateFingerprintBytes, basisFingerprint, carryover.KnowledgeGeneration,
			carryover.LearningGeneration, carryover.PolicyVersion, carryover.CreatedAt); err != nil {
			return fmt.Errorf("insert evidence carryover proposal: %w", err)
		}
		for index, candidate := range candidates {
			if _, err := tx.Exec(ctx, `
				INSERT INTO learning_evidence_carryover_candidates(
				  proposal_id,ordinal,target_knowledge_revision_id,target_node_id,
				  target_node_revision_id,target_document_revision_id)
				VALUES($1,$2,$3,$4,$5,$6)`, carryover.ID, index, candidate.KnowledgeRevisionID,
				candidate.NodeID, candidate.NodeRevisionID, candidate.DocumentRevisionID); err != nil {
				return fmt.Errorf("insert evidence carryover candidate: %w", err)
			}
		}
	}
	return nil
}

func lockAcceptedEvidenceImpact(ctx context.Context, tx pgx.Tx, nodeRevisionIDs []string) (knowledge.AcceptedEvidenceImpact, error) {
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return knowledge.AcceptedEvidenceImpact{}, err
	}
	impact := knowledge.AcceptedEvidenceImpact{
		Generation: generation, References: []knowledge.AcceptedEvidenceReference{},
	}
	if len(nodeRevisionIDs) == 0 {
		return knowledge.NormalizeAcceptedEvidenceImpact(impact)
	}
	if _, err := tx.Exec(ctx, `
		LOCK TABLE learning_evidence,learning_evidence_invalidations,
		  learning_evidence_carryover_proposals,learning_evidence_carryover_links
		IN SHARE MODE`); err != nil {
		return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("lock accepted evidence impact basis: %w", err)
	}
	ids := append([]string(nil), nodeRevisionIDs...)
	sort.Strings(ids)
	rows, err := tx.Query(ctx, `
		WITH valid_source_evidence AS (
		  SELECT evidence.id,evidence.node_revision_id,evidence.knowledge_revision_id
		  FROM learning_evidence evidence
		  WHERE evidence.accepted_event_seq IS NOT NULL
		    AND NOT EXISTS (
		      SELECT 1 FROM learning_evidence_invalidations invalidation
		      WHERE invalidation.evidence_id=evidence.id
		    )
		), impact_references AS (
		  SELECT evidence.id AS evidence_id,evidence.node_revision_id,evidence.knowledge_revision_id
		  FROM valid_source_evidence evidence
		  WHERE evidence.node_revision_id=ANY($1::uuid[])
		  UNION
		  SELECT evidence.id,link.target_node_revision_id,link.target_knowledge_revision_id
		  FROM valid_source_evidence evidence
		  JOIN learning_evidence_carryover_links link ON link.source_evidence_id=evidence.id
		  JOIN learning_evidence_carryover_proposals proposal
		    ON proposal.proposal_id=link.proposal_id AND proposal.status='approved'
		  WHERE link.target_node_revision_id=ANY($1::uuid[])
		)
		SELECT evidence_id::text,node_revision_id::text,knowledge_revision_id::text
		FROM impact_references
		ORDER BY evidence_id,node_revision_id,knowledge_revision_id`, ids)
	if err != nil {
		return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("query locked accepted evidence impact: %w", err)
	}
	for rows.Next() {
		var reference knowledge.AcceptedEvidenceReference
		if err := rows.Scan(&reference.EvidenceID, &reference.NodeRevisionID, &reference.KnowledgeRevisionID); err != nil {
			rows.Close()
			return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("scan locked accepted evidence impact: %w", err)
		}
		impact.References = append(impact.References, reference)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("iterate locked accepted evidence impact: %w", err)
	}
	rows.Close()
	return knowledge.NormalizeAcceptedEvidenceImpact(impact)
}

func (s *Store) ValidateEvidenceCarryoverTargetLockedWith(
	ctx context.Context,
	tx pgx.Tx,
	targetRevisionID string,
	knowledgeProposalID string,
	knowledgeBasisHash string,
	candidates []learning.EvidenceCarryoverCandidate,
) (bool, error) {
	var headID *string
	var redacted bool
	err := tx.QueryRow(ctx, `
		SELECT catalog.head_revision_id::text,revision.redacted_at IS NOT NULL
		FROM knowledge_catalog catalog
		JOIN knowledge_revisions revision ON revision.id=$1
		WHERE catalog.singleton_id=1
		FOR UPDATE OF catalog`, targetRevisionID).Scan(&headID, &redacted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock knowledge target for evidence carryover: %w", err)
	}
	if redacted || headID == nil || *headID != targetRevisionID {
		return false, nil
	}
	var originProposal string
	var originBasis []byte
	err = tx.QueryRow(ctx, `
		SELECT proposal_id::text,basis_hash
		FROM knowledge_revision_origins
		WHERE revision_id=$1`, targetRevisionID).Scan(&originProposal, &originBasis)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read evidence carryover knowledge origin: %w", err)
	}
	if originProposal != knowledgeProposalID || hex.EncodeToString(originBasis) != knowledgeBasisHash {
		return false, nil
	}
	for _, candidate := range candidates {
		if candidate.KnowledgeRevisionID != targetRevisionID {
			return false, nil
		}
		var valid bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1
			  FROM knowledge_node_revisions node
			  JOIN knowledge_snapshot_documents snapshot
			    ON snapshot.knowledge_revision_id=$4
			   AND snapshot.document_revision_id=node.document_revision_id
			  WHERE node.id=$1 AND node.node_id=$2 AND node.document_revision_id=$3
			)`, candidate.NodeRevisionID, candidate.NodeID, candidate.DocumentRevisionID,
			targetRevisionID).Scan(&valid); err != nil {
			return false, fmt.Errorf("validate evidence carryover knowledge candidate: %w", err)
		}
		if !valid {
			return false, nil
		}
	}
	return true, nil
}

func validatePreparedProposal(proposal knowledge.Proposal, commit knowledge.PreparedCommit) error {
	if proposal.Status != knowledge.ProposalOpen || commit.Unchanged || commit.Revision.ID != proposal.PlannedRevisionID ||
		commit.Revision.RevisionNo != proposal.PlannedRevisionNo || commit.Revision.ManifestHash != proposal.PlannedManifestHash ||
		commit.Revision.ParentRevisionID == nil || *commit.Revision.ParentRevisionID != proposal.BaseRevisionID ||
		commit.ExpectedParentRevisionID == nil || *commit.ExpectedParentRevisionID != proposal.BaseRevisionID ||
		commit.Revision.CanonicalizerVersion != proposal.CanonicalizerVersion ||
		commit.Revision.IdentityPolicyVersion != proposal.IdentityPolicyVersion {
		return &knowledge.Error{Code: knowledge.CodeProposalStale}
	}
	return nil
}

func maintenanceOrigin(proposal knowledge.Proposal) *knowledge.RevisionOrigin {
	origin := &knowledge.RevisionOrigin{
		Version: knowledge.MaintenanceOriginVersion, Kind: string(proposal.Kind), ProposalID: proposal.ID,
		BaseRevisionID: proposal.BaseRevisionID, BasisHash: proposal.BasisHash,
	}
	if proposal.RollbackTargetRevisionID != "" {
		target := proposal.RollbackTargetRevisionID
		origin.RollbackTargetRevisionID = &target
	}
	return origin
}

func encodeStoredProposal(proposal knowledge.Proposal) ([]byte, error) {
	value, err := json.Marshal(storedProposalRecord{
		Proposal: proposal, AffectedNodeRevisionIDs: append([]string(nil), proposal.AffectedNodeRevisionIDs...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode knowledge maintenance proposal: %w", err)
	}
	return value, nil
}

func encodeStoredPreparedCommit(commit knowledge.PreparedCommit) ([]byte, error) {
	markdown := make(map[string]string, len(commit.Revision.Documents))
	for index := range commit.Revision.Documents {
		document := &commit.Revision.Documents[index].Revision
		markdown[document.ID] = document.CanonicalMarkdown
	}
	value, err := json.Marshal(storedPreparedCommit{Commit: commit, CanonicalMarkdown: markdown})
	if err != nil {
		return nil, fmt.Errorf("encode prepared knowledge maintenance commit: %w", err)
	}
	return value, nil
}

func readStoredPreparedCommit(ctx context.Context, db queryer, proposalID string) (knowledge.PreparedCommit, error) {
	var encoded []byte
	if err := db.QueryRow(ctx, `SELECT prepared_commit FROM knowledge_maintenance_proposals WHERE proposal_id=$1`, proposalID).Scan(&encoded); err != nil {
		return knowledge.PreparedCommit{}, fmt.Errorf("read prepared knowledge maintenance commit: %w", err)
	}
	var stored storedPreparedCommit
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return knowledge.PreparedCommit{}, fmt.Errorf("decode prepared knowledge maintenance commit: %w", err)
	}
	for index := range stored.Commit.Revision.Documents {
		document := &stored.Commit.Revision.Documents[index].Revision
		document.CanonicalMarkdown = stored.CanonicalMarkdown[document.ID]
	}
	return stored.Commit, nil
}

func readMaintenanceProposal(ctx context.Context, db queryer, proposalID string, forUpdate bool) (knowledge.Proposal, error) {
	query := `SELECT ` + maintenanceProposalColumns + ` FROM knowledge_maintenance_proposals WHERE proposal_id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var proposal knowledge.Proposal
	var requestHash, manifestHash, basisHash, record []byte
	var rollbackTarget, currentRevision, appliedRevision *string
	var redacted bool
	err := db.QueryRow(ctx, query, proposalID).Scan(
		&proposal.ID, &proposal.RequestID, &requestHash, &proposal.Kind, &proposal.Status,
		&proposal.BaseRevisionID, &rollbackTarget, &proposal.PlannedRevisionID, &proposal.PlannedRevisionNo,
		&manifestHash, &currentRevision, &appliedRevision, &basisHash, &proposal.KnowledgeGeneration,
		&proposal.EvidenceImpact.Generation, &proposal.CanonicalizerVersion, &proposal.IdentityPolicyVersion,
		&proposal.DiffVersion, &proposal.RiskVersion, &proposal.AutoPolicyVersion, &proposal.CreatedByDeviceID,
		&proposal.CreatedAt, &proposal.UpdatedAt, &redacted, &record,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.Proposal{}, &knowledge.Error{Code: knowledge.CodeNotFound}
	}
	if err != nil {
		return knowledge.Proposal{}, fmt.Errorf("read knowledge maintenance proposal: %w", err)
	}
	proposal.RequestHash = hex.EncodeToString(requestHash)
	proposal.PlannedManifestHash = hex.EncodeToString(manifestHash)
	proposal.BasisHash = hex.EncodeToString(basisHash)
	proposal.RollbackTargetRevisionID = optionalString(rollbackTarget)
	proposal.CurrentRevisionID = optionalString(currentRevision)
	proposal.AppliedRevisionID = optionalString(appliedRevision)
	proposal.CreatedAt = proposal.CreatedAt.UTC()
	proposal.UpdatedAt = proposal.UpdatedAt.UTC()
	proposal.Redacted = redacted
	if !redacted {
		var stored storedProposalRecord
		if err := json.Unmarshal(record, &stored); err != nil {
			return knowledge.Proposal{}, fmt.Errorf("decode knowledge maintenance proposal: %w", err)
		}
		frozen := stored.Proposal
		frozen.ID, frozen.RequestID, frozen.RequestHash = proposal.ID, proposal.RequestID, proposal.RequestHash
		frozen.Kind, frozen.Status, frozen.BaseRevisionID = proposal.Kind, proposal.Status, proposal.BaseRevisionID
		frozen.RollbackTargetRevisionID, frozen.CurrentRevisionID = proposal.RollbackTargetRevisionID, proposal.CurrentRevisionID
		frozen.AppliedRevisionID, frozen.PlannedRevisionID = proposal.AppliedRevisionID, proposal.PlannedRevisionID
		frozen.PlannedRevisionNo, frozen.PlannedManifestHash = proposal.PlannedRevisionNo, proposal.PlannedManifestHash
		frozen.BasisHash, frozen.KnowledgeGeneration = proposal.BasisHash, proposal.KnowledgeGeneration
		frozen.EvidenceImpact.Generation = proposal.EvidenceImpact.Generation
		frozen.CanonicalizerVersion, frozen.IdentityPolicyVersion = proposal.CanonicalizerVersion, proposal.IdentityPolicyVersion
		frozen.DiffVersion, frozen.RiskVersion, frozen.AutoPolicyVersion = proposal.DiffVersion, proposal.RiskVersion, proposal.AutoPolicyVersion
		frozen.CreatedByDeviceID, frozen.CreatedAt, frozen.UpdatedAt = proposal.CreatedByDeviceID, proposal.CreatedAt, proposal.UpdatedAt
		frozen.AffectedNodeRevisionIDs = append([]string(nil), stored.AffectedNodeRevisionIDs...)
		proposal = frozen
		if proposal.Status == knowledge.ProposalApplied {
			proposal.Origin = maintenanceOrigin(proposal)
		}
	} else {
		proposal.RequestHash = ""
		proposal.BasisHash = ""
		proposal.PlannedManifestHash = ""
		if proposal.Status == knowledge.ProposalApplied {
			proposal.Origin = maintenanceOrigin(proposal)
		}
	}
	decision, exists, err := readMaintenanceDecision(ctx, db, proposal.ID)
	if err != nil {
		return knowledge.Proposal{}, err
	}
	if exists {
		if proposal.Redacted {
			decision.Reason = ""
		}
		proposal.Decision = &decision
	}
	return proposal, nil
}

func readMaintenanceDecision(ctx context.Context, db queryer, proposalID string) (knowledge.ProposalDecision, bool, error) {
	var decision knowledge.ProposalDecision
	var operationID *string
	err := db.QueryRow(ctx, `
		SELECT decision_id::text,operation_id::text,requested_decision,outcome,reason,actor_device_id::text,created_at
		FROM knowledge_maintenance_decisions WHERE proposal_id=$1`, proposalID).Scan(
		&decision.ID, &operationID, &decision.RequestedDecision, &decision.Outcome,
		&decision.Reason, &decision.ActorDeviceID, &decision.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.ProposalDecision{}, false, nil
	}
	if err != nil {
		return knowledge.ProposalDecision{}, false, fmt.Errorf("read knowledge maintenance decision: %w", err)
	}
	decision.OperationID = optionalString(operationID)
	decision.CreatedAt = decision.CreatedAt.UTC()
	return decision, true, nil
}

func insertMaintenanceDecision(ctx context.Context, tx pgx.Tx, proposalID string, decision knowledge.ProposalDecision) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_maintenance_decisions(
		  decision_id,proposal_id,operation_id,requested_decision,outcome,reason,actor_device_id,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		decision.ID, proposalID, nullableUUID(decision.OperationID), decision.RequestedDecision,
		decision.Outcome, decision.Reason, decision.ActorDeviceID, decision.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("insert knowledge maintenance decision: %w", err)
	}
	return nil
}

func insertMaintenanceOperation(ctx context.Context, tx pgx.Tx, operationID, operationType, requestHash, proposalID string, completedAt time.Time) error {
	hash, err := decodeHash(requestHash)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_maintenance_operations(operation_id,operation_type,request_hash,proposal_id,completed_at)
		VALUES($1,$2,$3,$4,$5)`, operationID, operationType, hash, proposalID, completedAt.UTC()); err != nil {
		return fmt.Errorf("insert knowledge maintenance operation: %w", err)
	}
	return nil
}

func lookupMaintenanceOperationWith(ctx context.Context, tx pgx.Tx, operationID string, forUpdate bool) (knowledge.MaintenanceOperationRecord, bool, error) {
	query := `SELECT request_hash,proposal_id::text FROM knowledge_maintenance_operations WHERE operation_id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var hash []byte
	var record knowledge.MaintenanceOperationRecord
	err := tx.QueryRow(ctx, query, operationID).Scan(&hash, &record.ProposalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.MaintenanceOperationRecord{}, false, nil
	}
	if err != nil {
		return knowledge.MaintenanceOperationRecord{}, false, fmt.Errorf("read knowledge maintenance operation: %w", err)
	}
	record.RequestHash = hex.EncodeToString(hash)
	return record, true, nil
}
