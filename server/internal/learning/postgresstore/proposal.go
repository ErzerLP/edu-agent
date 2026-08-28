package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ClaimProposal(ctx context.Context, deviceID string, request learning.ProposalRequest, requestHash string, now time.Time) (learning.ProposalClaim, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return learning.ProposalClaim{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "tutoring-proposal:"+deviceID+":"+request.RequestID); err != nil {
		return learning.ProposalClaim{}, err
	}
	hash, err := decodeHash(requestHash)
	if err != nil {
		return learning.ProposalClaim{}, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	var storedHash []byte
	var status string
	var proposalID, category *string
	var leaseExpires *time.Time
	err = tx.QueryRow(ctx, `SELECT request_hash,status,result_proposal_id,error_category,lease_expires_at FROM tutoring_proposal_requests WHERE device_id=$1 AND request_id=$2 FOR UPDATE`, deviceID, request.RequestID).Scan(&storedHash, &status, &proposalID, &category, &leaseExpires)
	if err == nil {
		if hex.EncodeToString(storedHash) != requestHash {
			return learning.ProposalClaim{}, &learning.Error{Code: learning.CodeIdempotencyConflict}
		}
		switch status {
		case "ready":
			artifact, err := loadProposalTx(ctx, tx, *proposalID)
			if err != nil {
				return learning.ProposalClaim{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return learning.ProposalClaim{}, err
			}
			return learning.ProposalClaim{State: "ready", Artifact: &artifact}, nil
		case "failed":
			if err := tx.Commit(ctx); err != nil {
				return learning.ProposalClaim{}, err
			}
			return learning.ProposalClaim{State: "failed", Category: deref(category)}, nil
		case "processing":
			if leaseExpires != nil && leaseExpires.After(now) {
				if err := tx.Commit(ctx); err != nil {
					return learning.ProposalClaim{}, err
				}
				return learning.ProposalClaim{State: "busy"}, nil
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return learning.ProposalClaim{}, fmt.Errorf("read proposal request: %w", err)
	}
	lease := uuid.NewString()
	expires := now.Add(2 * time.Minute)
	input, _ := json.Marshal(request)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `INSERT INTO tutoring_proposal_requests(device_id,request_id,request_hash,proposal_type,aggregate_type,aggregate_id,aggregate_version,input,status,lease_token,lease_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'processing',$9,$10,$11,$11)`, deviceID, request.RequestID, hash, request.Type, request.AggregateType, request.AggregateID, request.AggregateVersion, input, lease, expires, now); err != nil {
			return learning.ProposalClaim{}, fmt.Errorf("insert proposal request: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE tutoring_proposal_requests SET status='processing',lease_token=$3,lease_expires_at=$4,error_category=NULL,updated_at=$5 WHERE device_id=$1 AND request_id=$2`, deviceID, request.RequestID, lease, expires, now); err != nil {
			return learning.ProposalClaim{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.ProposalClaim{}, err
	}
	return learning.ProposalClaim{State: "claimed", LeaseToken: lease}, nil
}
func (s *Store) CompleteProposal(ctx context.Context, deviceID, lease string, artifact learning.ProposalArtifact, now time.Time) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	inputHash, err := decodeHash(artifact.InputHash)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(artifact)
	params, _ := json.Marshal(artifact.ModelParameters)
	var requestID string
	err = tx.QueryRow(ctx, `SELECT request_id FROM tutoring_proposal_requests WHERE device_id=$1 AND lease_token=$2 AND status='processing' AND lease_expires_at>clock_timestamp() FOR UPDATE`, deviceID, lease).Scan(&requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &learning.Error{Code: learning.CodeStaleProposal}
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tutoring_proposal_artifacts(id,device_id,request_id,schema_version,input_hash,proposal_type,aggregate_type,aggregate_id,aggregate_version,goal_revision_id,route_revision_id,activity_id,attempt_id,knowledge_revision_id,artifact,trusted_model_id,model_parameters,prompt_revision,attempt_categories,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`, artifact.ID, deviceID, requestID, artifact.SchemaVersion, inputHash, artifact.Type, artifact.AggregateType, artifact.AggregateID, artifact.AggregateVersion, nullable(artifact.GoalRevisionID), nullable(artifact.RouteRevisionID), nullable(artifact.ActivityID), nullable(artifact.AttemptID), artifact.KnowledgeRevisionID, raw, artifact.ModelID, params, artifact.PromptRevision, artifact.AttemptCategories, artifact.CreatedAt); err != nil {
		return fmt.Errorf("insert proposal artifact: %w", err)
	}
	command, err := tx.Exec(ctx, `UPDATE tutoring_proposal_requests SET status='ready',result_proposal_id=$3,attempt_categories=$4,lease_token=NULL,lease_expires_at=NULL,updated_at=$5 WHERE device_id=$1 AND request_id=$2 AND lease_token=$6 AND status='processing' AND lease_expires_at>clock_timestamp()`, deviceID, requestID, artifact.ID, artifact.AttemptCategories, now, lease)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeStaleProposal}
	}
	return tx.Commit(ctx)
}
func (s *Store) FailProposal(ctx context.Context, deviceID, lease string, categories []string, category string, now time.Time) error {
	normalizedCategories := append([]string{}, categories...)
	command, err := s.pool.Exec(ctx, `UPDATE tutoring_proposal_requests SET status='failed',attempt_categories=$3,error_category=$4,lease_token=NULL,lease_expires_at=NULL,updated_at=$5 WHERE device_id=$1 AND lease_token=$2 AND status='processing' AND lease_expires_at>clock_timestamp()`, deviceID, lease, normalizedCategories, category, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeStaleProposal}
	}
	return nil
}
func loadProposalTx(ctx context.Context, tx pgx.Tx, id string) (learning.ProposalArtifact, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT artifact FROM tutoring_proposal_artifacts WHERE id=$1`, id).Scan(&raw); err != nil {
		return learning.ProposalArtifact{}, err
	}
	var value learning.ProposalArtifact
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	return value, nil
}
