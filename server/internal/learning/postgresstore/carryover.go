package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var carryoverLinkNamespace = uuid.MustParse("91f9600b-2edf-46d3-8761-01bedfcd21ad")

type carryoverDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type carryoverKnowledgeValidator interface {
	ValidateEvidenceCarryoverTargetLockedWith(
		context.Context,
		pgx.Tx,
		string,
		string,
		string,
		[]learning.EvidenceCarryoverCandidate,
	) (bool, error)
}

func (s *Store) ListEvidenceCarryovers(ctx context.Context, command learning.EvidenceCarryoverListCommand) (learning.EvidenceCarryoverPage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return learning.EvidenceCarryoverPage{}, fmt.Errorf("begin evidence carryover list: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return learning.EvidenceCarryoverPage{}, err
	}
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return learning.EvidenceCarryoverPage{}, err
	}
	if command.ExpectedGeneration != 0 && command.ExpectedGeneration != generation {
		return learning.EvidenceCarryoverPage{}, &learning.Error{Code: learning.CodeStaleCursor}
	}
	var afterAt any
	var afterID any
	if !command.AfterCreatedAt.IsZero() {
		afterAt = command.AfterCreatedAt.UTC()
		afterID = command.AfterProposalID
	}
	rows, err := tx.Query(ctx, `
		SELECT proposal_id::text
		FROM learning_evidence_carryover_proposals
		WHERE ($1='all' OR status=$1)
		  AND ($2::timestamptz IS NULL OR (created_at,proposal_id)>($2,$3::uuid))
		ORDER BY created_at,proposal_id LIMIT $4`, command.Status, afterAt, afterID, command.Limit+1)
	if err != nil {
		return learning.EvidenceCarryoverPage{}, fmt.Errorf("query evidence carryover proposals: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return learning.EvidenceCarryoverPage{}, fmt.Errorf("scan evidence carryover proposal ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return learning.EvidenceCarryoverPage{}, fmt.Errorf("iterate evidence carryover proposals: %w", err)
	}
	rows.Close()
	result := learning.EvidenceCarryoverPage{Items: make([]learning.EvidenceCarryoverProposal, 0, command.Limit)}
	for _, id := range ids {
		proposal, err := readEvidenceCarryover(ctx, tx, id, false)
		if err != nil {
			return learning.EvidenceCarryoverPage{}, err
		}
		result.Items = append(result.Items, proposal)
	}
	if len(result.Items) > command.Limit {
		result.Items = result.Items[:command.Limit]
		last := result.Items[len(result.Items)-1]
		result.NextCursor = learning.EncodeEvidenceCarryoverCursor(generation, last.CreatedAt, last.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.EvidenceCarryoverPage{}, fmt.Errorf("commit evidence carryover list: %w", err)
	}
	return result, nil
}

func (s *Store) EvidenceCarryover(ctx context.Context, proposalID string) (learning.EvidenceCarryoverProposal, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("begin evidence carryover read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	proposal, err := readEvidenceCarryover(ctx, tx, proposalID, false)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("commit evidence carryover read: %w", err)
	}
	return proposal, nil
}

func (s *Store) DecideEvidenceCarryover(ctx context.Context, command learning.PreparedEvidenceCarryoverDecision) (learning.EvidenceCarryoverProposal, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("begin evidence carryover decision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	knowledgeGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	learningGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('learning-carryover-operation:'||$1,0))`, command.OperationID); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("lock evidence carryover operation: %w", err)
	}
	if proposalID, requestHash, exists, err := lookupEvidenceCarryoverOperation(ctx, tx, command.OperationID); err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	} else if exists {
		if requestHash != command.RequestHash {
			return learning.EvidenceCarryoverProposal{}, &learning.Error{Code: learning.CodeOperationConflict}
		}
		proposal, err := readEvidenceCarryover(ctx, tx, proposalID, false)
		if err != nil {
			return learning.EvidenceCarryoverProposal{}, err
		}
		proposal.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return learning.EvidenceCarryoverProposal{}, fmt.Errorf("commit evidence carryover replay: %w", err)
		}
		return proposal, nil
	}
	proposal, err := readEvidenceCarryover(ctx, tx, command.ProposalID, true)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	if proposal.Status != learning.EvidenceCarryoverOpen || proposal.Redacted {
		return learning.EvidenceCarryoverProposal{}, &learning.Error{Code: learning.CodeOperationConflict}
	}
	if command.RequestedDecision == "approve" && len(proposal.Candidates) == 0 {
		return learning.EvidenceCarryoverProposal{}, &learning.Error{Code: learning.CodeEvidenceCarryoverNoCandidates}
	}
	if command.RequestedDecision == "approve" {
		if _, err := tx.Exec(ctx, `
			LOCK TABLE learning_evidence_carryover_proposals,learning_evidence_carryover_links
			IN ROW EXCLUSIVE MODE`); err != nil {
			return learning.EvidenceCarryoverProposal{}, fmt.Errorf("lock evidence carryover approval writes: %w", err)
		}
	}

	stale := false
	if command.RequestedDecision == "approve" {
		stale, err = s.evidenceCarryoverIsStale(ctx, tx, proposal, knowledgeGeneration, learningGeneration)
		if err != nil {
			return learning.EvidenceCarryoverProposal{}, err
		}
	}
	outcome := string(learning.EvidenceCarryoverRejected)
	eventType := learning.EventEvidenceCarryoverRejected
	if command.RequestedDecision == "approve" {
		outcome = string(learning.EvidenceCarryoverApproved)
		eventType = learning.EventEvidenceCarryoverApproved
		if stale {
			outcome = string(learning.EvidenceCarryoverStale)
			eventType = learning.EventEvidenceCarryoverStaled
		}
	}
	proposal, err = appendEvidenceCarryoverDecision(ctx, tx, proposal, command, eventType, outcome)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("commit evidence carryover decision: %w", err)
	}
	return proposal, nil
}

func (s *Store) evidenceCarryoverIsStale(ctx context.Context, tx pgx.Tx, proposal learning.EvidenceCarryoverProposal, knowledgeGeneration, learningGeneration int64) (bool, error) {
	if proposal.KnowledgeGeneration != knowledgeGeneration || proposal.LearningGeneration != learningGeneration ||
		proposal.PolicyVersion != learning.EvidenceCarryoverPolicyVersion ||
		proposal.BasisFingerprint != learning.ComputeEvidenceCarryoverBasis(proposal) {
		return true, nil
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE learning_evidence,learning_evidence_invalidations IN SHARE MODE`); err != nil {
		return false, fmt.Errorf("lock evidence carryover source validity: %w", err)
	}
	var sourceReferenceValid bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1
		  FROM learning_evidence evidence
		  WHERE evidence.id=$1
		    AND evidence.accepted_event_seq IS NOT NULL
		    AND NOT EXISTS (
		      SELECT 1 FROM learning_evidence_invalidations invalidation
		      WHERE invalidation.evidence_id=evidence.id
		    )
		    AND (
		      (evidence.knowledge_revision_id=$2 AND evidence.node_revision_id=$3)
		      OR EXISTS (
		        SELECT 1
		        FROM learning_evidence_carryover_links link
		        JOIN learning_evidence_carryover_proposals linked_proposal
		          ON linked_proposal.proposal_id=link.proposal_id
		         AND linked_proposal.status='approved'
		        WHERE link.source_evidence_id=evidence.id
		          AND link.target_knowledge_revision_id=$2
		          AND link.target_node_revision_id=$3
		      )
		    )
		)`, proposal.SourceEvidenceID, proposal.SourceKnowledgeRevisionID, proposal.SourceNodeRevisionID).Scan(&sourceReferenceValid)
	if err != nil {
		return false, fmt.Errorf("lock evidence carryover source: %w", err)
	}
	if !sourceReferenceValid {
		return true, nil
	}
	candidates, fingerprint, err := validatedEvidenceCarryoverCandidates(ctx, tx, proposal.ID, proposal.TargetKnowledgeRevisionID)
	if err != nil {
		return false, err
	}
	if fingerprint != proposal.CandidateFingerprint || len(candidates) != len(proposal.Candidates) {
		return true, nil
	}
	for index := range candidates {
		if candidates[index] != proposal.Candidates[index] {
			return true, nil
		}
	}
	validator, ok := s.knowledge.(carryoverKnowledgeValidator)
	if !ok {
		return false, fmt.Errorf("knowledge evidence carryover validator is not configured")
	}
	valid, err := validator.ValidateEvidenceCarryoverTargetLockedWith(
		ctx, tx, proposal.TargetKnowledgeRevisionID, proposal.KnowledgeProposalID,
		proposal.KnowledgeBasisHash, candidates,
	)
	if err != nil {
		return false, err
	}
	return !valid, nil
}

func validatedEvidenceCarryoverCandidates(ctx context.Context, db carryoverDB, proposalID, targetRevisionID string) ([]learning.EvidenceCarryoverCandidate, string, error) {
	rows, err := db.Query(ctx, `
		SELECT target_knowledge_revision_id::text,target_node_id::text,
		       target_node_revision_id::text,target_document_revision_id::text
		FROM learning_evidence_carryover_candidates
		WHERE proposal_id=$1 AND target_knowledge_revision_id=$2
		ORDER BY ordinal`, proposalID, targetRevisionID)
	if err != nil {
		return nil, "", fmt.Errorf("query valid evidence carryover candidates: %w", err)
	}
	defer rows.Close()
	var values []learning.EvidenceCarryoverCandidate
	for rows.Next() {
		var value learning.EvidenceCarryoverCandidate
		if err := rows.Scan(&value.KnowledgeRevisionID, &value.NodeID, &value.NodeRevisionID, &value.DocumentRevisionID); err != nil {
			return nil, "", fmt.Errorf("scan valid evidence carryover candidate: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate valid evidence carryover candidates: %w", err)
	}
	return learning.NormalizeEvidenceCarryoverCandidates(values)
}

func appendEvidenceCarryoverDecision(ctx context.Context, tx pgx.Tx, proposal learning.EvidenceCarryoverProposal, command learning.PreparedEvidenceCarryoverDecision, eventType learning.EventType, outcome string) (learning.EvidenceCarryoverProposal, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:evidence_carryover:"+proposal.ID); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("lock evidence carryover aggregate: %w", err)
	}
	var existingVersion int64
	err := tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='evidence_carryover' AND aggregate_id=$1 FOR UPDATE`, proposal.ID).Scan(&existingVersion)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return learning.EvidenceCarryoverProposal{}, fmt.Errorf("read evidence carryover aggregate: %w", err)
		}
		return learning.EvidenceCarryoverProposal{}, &learning.Error{Code: learning.CodeOperationConflict}
	}
	var clock int64
	if err := tx.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&clock); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("lock learning event clock for evidence carryover: %w", err)
	}
	sequence := clock + 1
	eventID := uuid.NewSHA1(eventNamespace, []byte(command.ActorDeviceID+"\n"+command.OperationID+"\n0")).String()
	payloadID := uuid.NewSHA1(eventNamespace, []byte("payload\n"+eventID)).String()
	createdAt := command.DecidedAt.UTC().Truncate(time.Microsecond)
	decision := learning.EvidenceCarryoverDecision{
		ID: command.DecisionID, OperationID: command.OperationID, RequestedDecision: command.RequestedDecision,
		Outcome: outcome, Reason: command.Reason, ActorDeviceID: command.ActorDeviceID,
		EventID: eventID, EventSequence: sequence, CreatedAt: createdAt,
	}
	var links []learning.EvidenceCarryoverLink
	var provisional *learning.ProvisionalEvidenceCarryover
	if outcome == string(learning.EvidenceCarryoverApproved) {
		links = make([]learning.EvidenceCarryoverLink, len(proposal.Candidates))
		for index, candidate := range proposal.Candidates {
			links[index] = learning.EvidenceCarryoverLink{
				ID:         uuid.NewSHA1(carryoverLinkNamespace, []byte(proposal.ID+"\n"+candidate.NodeRevisionID)).String(),
				ProposalID: proposal.ID, SourceEvidenceID: proposal.SourceEvidenceID,
				TargetKnowledgeRevisionID: candidate.KnowledgeRevisionID, TargetNodeID: candidate.NodeID,
				TargetNodeRevisionID: candidate.NodeRevisionID, TargetDocumentRevisionID: candidate.DocumentRevisionID,
				DecisionID: decision.ID, EventID: eventID, EventSequence: sequence, CreatedAt: createdAt,
			}
		}
		provisional = &learning.ProvisionalEvidenceCarryover{
			ProposalID: proposal.ID, KnowledgeProposalID: proposal.KnowledgeProposalID,
			SourceEvidenceID: proposal.SourceEvidenceID, SourceKnowledgeRevisionID: proposal.SourceKnowledgeRevisionID,
			SourceNodeRevisionID: proposal.SourceNodeRevisionID, TargetKnowledgeRevisionID: proposal.TargetKnowledgeRevisionID,
			Links: append([]learning.EvidenceCarryoverLink(nil), links...), BasisFingerprint: proposal.BasisFingerprint,
			PolicyVersion: proposal.PolicyVersion, ApprovedEventSequence: sequence,
		}
	}
	eventPayload, err := json.Marshal(learning.EvidenceCarryoverEvent{
		ProposalID: proposal.ID, DecisionID: decision.ID, RequestedDecision: command.RequestedDecision,
		Outcome: outcome, Reason: command.Reason, Provisional: provisional,
	})
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("encode evidence carryover event: %w", err)
	}
	eventPayload, err = canonicalJSON(eventPayload)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("canonicalize evidence carryover event: %w", err)
	}
	payloadHashHex := learning.SHA256(eventPayload)
	payloadHash, _ := hex.DecodeString(payloadHashHex)
	if _, err := tx.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('evidence_carryover',$1,1,$2,$3)`, proposal.ID, sequence, createdAt); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("insert evidence carryover aggregate head: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,$4)`, payloadID, eventPayload, payloadHash, createdAt); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("insert evidence carryover event payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_events(
		  event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,
		  device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash,event_source)
		VALUES($1,$2,$3,$4,'evidence_carryover',$5,1,$6,$7,0,$8,$9,$10,'online')`,
		sequence, eventID, eventType, learning.EventSchemaVersion, proposal.ID, command.ActorDeviceID,
		command.OperationID, createdAt, payloadID, payloadHash); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("insert evidence carryover event: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=$2 WHERE singleton_id=1`, sequence, createdAt); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("advance learning event clock for evidence carryover: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_evidence_carryover_decisions(
		  decision_id,proposal_id,operation_id,requested_decision,outcome,reason,
		  actor_device_id,event_id,event_seq,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, decision.ID, proposal.ID, command.OperationID,
		command.RequestedDecision, outcome, command.Reason, command.ActorDeviceID, eventID, sequence, createdAt); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("insert evidence carryover decision: %w", err)
	}
	for _, link := range links {
		if _, err := tx.Exec(ctx, `
			INSERT INTO learning_evidence_carryover_links(
			  link_id,proposal_id,decision_id,source_evidence_id,target_knowledge_revision_id,
			  target_node_id,target_node_revision_id,target_document_revision_id,event_id,event_seq,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, link.ID, link.ProposalID, link.DecisionID,
			link.SourceEvidenceID, link.TargetKnowledgeRevisionID, link.TargetNodeID, link.TargetNodeRevisionID,
			link.TargetDocumentRevisionID, link.EventID, link.EventSequence, link.CreatedAt); err != nil {
			return learning.EvidenceCarryoverProposal{}, fmt.Errorf("insert evidence carryover link: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_evidence_carryover_proposals SET status=$2,updated_at=$3 WHERE proposal_id=$1 AND status='open'`, proposal.ID, outcome, createdAt); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("close evidence carryover proposal: %w", err)
	}
	requestHash, err := decodeHash(command.RequestHash)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_request_hash", Cause: err}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_evidence_carryover_operations(operation_id,request_hash,proposal_id,requested_decision,completed_at) VALUES($1,$2,$3,$4,$5)`, command.OperationID, requestHash, proposal.ID, command.RequestedDecision, createdAt); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("insert evidence carryover operation: %w", err)
	}
	allEvents, err := loadEvents(ctx, tx, 0, sequence)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	var generationID string
	if err := tx.QueryRow(ctx, `SELECT active_generation_id::text FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&generationID); err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("lock active learning projection for evidence carryover: %w", err)
	}
	projection, err := learning.Replay(allEvents, learning.NewEventRegistry(), generationID)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	if err := replaceProjection(ctx, tx, generationID, projection, sequence, createdAt); err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	proposal.Status = learning.EvidenceCarryoverStatus(outcome)
	proposal.Decision = &decision
	proposal.Links = links
	proposal.UpdatedAt = createdAt
	return proposal, nil
}

func lookupEvidenceCarryoverOperation(ctx context.Context, db carryoverDB, operationID string) (string, string, bool, error) {
	var proposalID string
	var requestHash []byte
	err := db.QueryRow(ctx, `SELECT proposal_id::text,request_hash FROM learning_evidence_carryover_operations WHERE operation_id=$1`, operationID).Scan(&proposalID, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("read evidence carryover operation: %w", err)
	}
	return proposalID, hex.EncodeToString(requestHash), true, nil
}

func readEvidenceCarryover(ctx context.Context, db carryoverDB, proposalID string, forUpdate bool) (learning.EvidenceCarryoverProposal, error) {
	query := `
		SELECT proposal_id::text,knowledge_proposal_id::text,status,source_evidence_id::text,
		       source_knowledge_revision_id::text,source_node_revision_id::text,target_knowledge_revision_id::text,
		       knowledge_basis_hash,evidence_fingerprint,candidate_fingerprint,basis_fingerprint,
		       knowledge_generation,learning_generation,policy_version,created_at,updated_at,redacted_at IS NOT NULL
		FROM learning_evidence_carryover_proposals WHERE proposal_id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var proposal learning.EvidenceCarryoverProposal
	var sourceEvidence, sourceKnowledge, sourceNode, targetKnowledge *string
	var knowledgeBasis, evidenceFingerprint, candidateFingerprint, basisFingerprint []byte
	err := db.QueryRow(ctx, query, proposalID).Scan(
		&proposal.ID, &proposal.KnowledgeProposalID, &proposal.Status, &sourceEvidence, &sourceKnowledge,
		&sourceNode, &targetKnowledge, &knowledgeBasis, &evidenceFingerprint, &candidateFingerprint,
		&basisFingerprint, &proposal.KnowledgeGeneration, &proposal.LearningGeneration, &proposal.PolicyVersion,
		&proposal.CreatedAt, &proposal.UpdatedAt, &proposal.Redacted,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.EvidenceCarryoverProposal{}, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, fmt.Errorf("read evidence carryover proposal: %w", err)
	}
	proposal.CreatedAt = proposal.CreatedAt.UTC()
	proposal.UpdatedAt = proposal.UpdatedAt.UTC()
	if !proposal.Redacted {
		proposal.SourceEvidenceID = optionalStringValue(sourceEvidence)
		proposal.SourceKnowledgeRevisionID = optionalStringValue(sourceKnowledge)
		proposal.SourceNodeRevisionID = optionalStringValue(sourceNode)
		proposal.TargetKnowledgeRevisionID = optionalStringValue(targetKnowledge)
		proposal.KnowledgeBasisHash = hex.EncodeToString(knowledgeBasis)
		proposal.EvidenceFingerprint = hex.EncodeToString(evidenceFingerprint)
		proposal.CandidateFingerprint = hex.EncodeToString(candidateFingerprint)
		proposal.BasisFingerprint = hex.EncodeToString(basisFingerprint)
		proposal.Candidates, _, err = validatedEvidenceCarryoverCandidates(ctx, db, proposal.ID, proposal.TargetKnowledgeRevisionID)
		if err != nil {
			return learning.EvidenceCarryoverProposal{}, err
		}
	}
	decision, exists, err := readEvidenceCarryoverDecision(ctx, db, proposal.ID, proposal.Redacted)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	if exists {
		proposal.Decision = &decision
	}
	proposal.Links, err = readEvidenceCarryoverLinks(ctx, db, proposal.ID, proposal.Redacted)
	if err != nil {
		return learning.EvidenceCarryoverProposal{}, err
	}
	return proposal, nil
}

func readEvidenceCarryoverDecision(ctx context.Context, db carryoverDB, proposalID string, redacted bool) (learning.EvidenceCarryoverDecision, bool, error) {
	var decision learning.EvidenceCarryoverDecision
	err := db.QueryRow(ctx, `
		SELECT decision_id::text,operation_id::text,requested_decision,outcome,reason,
		       actor_device_id::text,event_id::text,event_seq,created_at
		FROM learning_evidence_carryover_decisions WHERE proposal_id=$1`, proposalID).Scan(
		&decision.ID, &decision.OperationID, &decision.RequestedDecision, &decision.Outcome, &decision.Reason,
		&decision.ActorDeviceID, &decision.EventID, &decision.EventSequence, &decision.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.EvidenceCarryoverDecision{}, false, nil
	}
	if err != nil {
		return learning.EvidenceCarryoverDecision{}, false, fmt.Errorf("read evidence carryover decision: %w", err)
	}
	if redacted {
		decision.Reason = ""
	}
	decision.CreatedAt = decision.CreatedAt.UTC()
	return decision, true, nil
}

func readEvidenceCarryoverLinks(ctx context.Context, db carryoverDB, proposalID string, redacted bool) ([]learning.EvidenceCarryoverLink, error) {
	rows, err := db.Query(ctx, `
		SELECT link_id::text,proposal_id::text,source_evidence_id::text,target_knowledge_revision_id::text,
		       target_node_id::text,target_node_revision_id::text,target_document_revision_id::text,
		       decision_id::text,event_id::text,event_seq,created_at
		FROM learning_evidence_carryover_links WHERE proposal_id=$1 ORDER BY link_id`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("query evidence carryover links: %w", err)
	}
	defer rows.Close()
	var result []learning.EvidenceCarryoverLink
	for rows.Next() {
		var link learning.EvidenceCarryoverLink
		var sourceEvidence, targetKnowledge, targetNodeID, targetNodeRevision, targetDocument *string
		if err := rows.Scan(&link.ID, &link.ProposalID, &sourceEvidence, &targetKnowledge, &targetNodeID,
			&targetNodeRevision, &targetDocument, &link.DecisionID, &link.EventID, &link.EventSequence,
			&link.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan evidence carryover link: %w", err)
		}
		if !redacted {
			link.SourceEvidenceID = optionalStringValue(sourceEvidence)
			link.TargetKnowledgeRevisionID = optionalStringValue(targetKnowledge)
			link.TargetNodeID = optionalStringValue(targetNodeID)
			link.TargetNodeRevisionID = optionalStringValue(targetNodeRevision)
			link.TargetDocumentRevisionID = optionalStringValue(targetDocument)
		}
		link.CreatedAt = link.CreatedAt.UTC()
		result = append(result, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evidence carryover links: %w", err)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
