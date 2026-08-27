package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

var (
	ErrIdempotencyConflict = errors.New("outbox idempotency key conflicts with a different message")
	ErrNotFound            = errors.New("outbox message was not found")
)

func (s *Store) Enqueue(ctx context.Context, message outbox.Message) (bool, error) {
	return EnqueueWith(ctx, s.pool, message)
}

// EnqueueWith writes through the caller-owned transaction.
func EnqueueWith(ctx context.Context, db DBTX, message outbox.Message) (bool, error) {
	if err := message.Validate(); err != nil {
		return false, err
	}
	command, err := db.Exec(ctx, `
		INSERT INTO outbox_messages(
			id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,
			status,available_at,attempts,max_attempts,last_error_category,last_error_at,terminal_disposition,
			lease_expires_at,lease_token,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14,NULLIF($15,''),$16,NULLIF($17,'')::uuid,$18,$19)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		message.ID, message.BusinessType, message.AggregateID, message.IdempotencyKey,
		message.Revision, message.Generation, message.Payload, message.AuditMetadata,
		message.Status, message.AvailableAt, message.Attempts, message.MaxAttempts,
		message.LastErrorCategory, message.LastErrorAt, message.TerminalDisposition,
		message.LeaseExpiresAt, message.LeaseToken, message.CreatedAt, message.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("enqueue outbox message: %w", err)
	}
	if command.RowsAffected() == 1 {
		return true, nil
	}
	var identical bool
	err = db.QueryRow(ctx, `
		SELECT id=$1 AND business_type=$2 AND aggregate_id=$3 AND idempotency_key=$4
			AND revision=$5 AND generation=$6 AND payload=$7 AND audit_metadata=$8
			AND status=$9 AND available_at=$10 AND attempts=$11 AND max_attempts=$12
			AND last_error_category IS NOT DISTINCT FROM NULLIF($13,'')
			AND last_error_at IS NOT DISTINCT FROM $14
			AND terminal_disposition IS NOT DISTINCT FROM NULLIF($15,'')
			AND lease_expires_at IS NOT DISTINCT FROM $16
			AND lease_token IS NOT DISTINCT FROM NULLIF($17,'')::uuid
			AND created_at=$18 AND updated_at=$19
		FROM outbox_messages WHERE idempotency_key=$4`,
		message.ID, message.BusinessType, message.AggregateID, message.IdempotencyKey,
		message.Revision, message.Generation, message.Payload, message.AuditMetadata,
		message.Status, message.AvailableAt, message.Attempts, message.MaxAttempts,
		message.LastErrorCategory, message.LastErrorAt, message.TerminalDisposition,
		message.LeaseExpiresAt, message.LeaseToken, message.CreatedAt, message.UpdatedAt).Scan(&identical)
	if err != nil {
		return false, fmt.Errorf("verify existing outbox message: %w", err)
	}
	if !identical {
		return false, ErrIdempotencyConflict
	}
	return false, nil
}

func (s *Store) ClaimBusinessTypes(ctx context.Context, now time.Time, lease time.Duration, limit int, businessTypes []string) ([]outbox.Message, error) {
	if len(businessTypes) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
			SELECT id FROM outbox_messages
			WHERE business_type = ANY($4::text[])
			  AND ((status='pending' AND available_at <= $1)
			       OR (status='processing' AND lease_expires_at <= $1))
			ORDER BY available_at,created_at,id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE outbox_messages o
		SET status='processing',attempts=o.attempts+1,
		    lease_expires_at=$1 + ($3 * interval '1 microsecond'),lease_token=gen_random_uuid(),updated_at=$1
		FROM candidates c WHERE o.id=c.id
		RETURNING o.id,o.business_type,o.aggregate_id,o.idempotency_key,o.revision,o.generation,
		          o.payload,o.audit_metadata,o.status,o.available_at,o.attempts,o.max_attempts,
		          COALESCE(o.last_error_category,''),o.last_error_at,COALESCE(o.terminal_disposition,''),
		          o.lease_expires_at,o.lease_token::text,o.created_at,o.updated_at`,
		now, limit, lease.Microseconds(), businessTypes)
	if err != nil {
		return nil, fmt.Errorf("claim filtered outbox messages: %w", err)
	}
	return scanClaimedMessages(rows)
}

func scanClaimedMessages(rows pgx.Rows) ([]outbox.Message, error) {
	defer rows.Close()
	var messages []outbox.Message
	for rows.Next() {
		var message outbox.Message
		if err := rows.Scan(
			&message.ID, &message.BusinessType, &message.AggregateID, &message.IdempotencyKey,
			&message.Revision, &message.Generation, &message.Payload, &message.AuditMetadata,
			&message.Status, &message.AvailableAt, &message.Attempts, &message.MaxAttempts,
			&message.LastErrorCategory, &message.LastErrorAt, &message.TerminalDisposition,
			&message.LeaseExpiresAt, &message.LeaseToken, &message.CreatedAt, &message.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan outbox message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox messages: %w", err)
	}
	return messages, nil
}

func (s *Store) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]outbox.Message, error) {
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
			SELECT id FROM outbox_messages
			WHERE (status='pending' AND available_at <= $1)
			   OR (status='processing' AND lease_expires_at <= $1)
			ORDER BY available_at,created_at,id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE outbox_messages o
		SET status='processing',attempts=o.attempts+1,
		    lease_expires_at=$1 + ($3 * interval '1 microsecond'),lease_token=gen_random_uuid(),updated_at=$1
		FROM candidates c WHERE o.id=c.id
		RETURNING o.id,o.business_type,o.aggregate_id,o.idempotency_key,o.revision,o.generation,
		          o.payload,o.audit_metadata,o.status,o.available_at,o.attempts,o.max_attempts,
		          COALESCE(o.last_error_category,''),o.last_error_at,COALESCE(o.terminal_disposition,''),
		          o.lease_expires_at,o.lease_token::text,o.created_at,o.updated_at`,
		now, limit, lease.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("claim outbox messages: %w", err)
	}
	return scanClaimedMessages(rows)
}

func (s *Store) RequeueDead(ctx context.Context, request outbox.RequeueRequest) error {
	return RequeueDeadWith(ctx, s.pool, request)
}

// RequeueDeadWith resets only a dead message whose immutable delivery tuple still matches.
// The caller may compose it with its own Inbox write in the same transaction.
func RequeueDeadWith(ctx context.Context, db DBTX, request outbox.RequeueRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	var changed, tupleMatches bool
	var status outbox.Status
	err := db.QueryRow(ctx, `
		WITH requeued AS (
			UPDATE outbox_messages
			SET status='pending',available_at=$7,attempts=0,last_error_category=NULL,last_error_at=NULL,
			    terminal_disposition=NULL,lease_expires_at=NULL,lease_token=NULL,updated_at=$7
			WHERE idempotency_key=$1 AND status='dead'
			  AND business_type=$2 AND aggregate_id=$3 AND revision=$4 AND generation=$5 AND payload=$6
			RETURNING TRUE AS changed,status,
			          business_type=$2 AND aggregate_id=$3 AND revision=$4 AND generation=$5 AND payload=$6 AS tuple_matches
		)
		SELECT changed,status,tuple_matches FROM requeued
		UNION ALL
		SELECT FALSE,status,
		       business_type=$2 AND aggregate_id=$3 AND revision=$4 AND generation=$5 AND payload=$6
		FROM outbox_messages
		WHERE idempotency_key=$1 AND NOT EXISTS (SELECT 1 FROM requeued)
		LIMIT 1`, request.IdempotencyKey, request.BusinessType, request.AggregateID,
		request.Revision, request.Generation, request.Payload, request.AvailableAt).Scan(
		&changed, &status, &tupleMatches,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("requeue dead outbox message: %w", err)
	}
	if changed || status == outbox.StatusPending && tupleMatches {
		return nil
	}
	if !tupleMatches {
		return ErrIdempotencyConflict
	}
	return outbox.ErrInvalidTransition
}

func (s *Store) Cancel(ctx context.Context, request outbox.CancelRequest) error {
	return CancelWith(ctx, s.pool, request)
}

// CancelWith fences a pending/dead message or the processing message owned by the supplied lease.
// The state transition and optional payload tombstone are one CAS so retries can verify the same result.
func CancelWith(ctx context.Context, db DBTX, request outbox.CancelRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	var tombstone any
	if len(request.TombstonePayload) > 0 {
		tombstone = []byte(request.TombstonePayload)
	}
	var changed, payloadMatches bool
	var status outbox.Status
	var disposition outbox.TerminalDisposition
	err := db.QueryRow(ctx, `
		WITH canceled AS (
			UPDATE outbox_messages
			SET status='canceled',terminal_disposition=$3,
			    payload=COALESCE($5::jsonb,payload),lease_expires_at=NULL,lease_token=NULL,updated_at=$4
			WHERE idempotency_key=$1
			  AND ((status IN ('pending','dead') AND $2='')
			       OR (status='processing' AND lease_token=NULLIF($2,'')::uuid))
			RETURNING TRUE AS changed,status,COALESCE(terminal_disposition,'') AS terminal_disposition,
			          ($5::jsonb IS NULL OR payload=$5::jsonb) AS payload_matches
		)
		SELECT changed,status,terminal_disposition,payload_matches FROM canceled
		UNION ALL
		SELECT FALSE,status,COALESCE(terminal_disposition,''),($5::jsonb IS NULL OR payload=$5::jsonb)
		FROM outbox_messages
		WHERE idempotency_key=$1 AND NOT EXISTS (SELECT 1 FROM canceled)
		LIMIT 1`, request.IdempotencyKey, request.LeaseToken, request.Disposition, request.CanceledAt, tombstone).Scan(
		&changed, &status, &disposition, &payloadMatches,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("cancel outbox message: %w", err)
	}
	if changed {
		return nil
	}
	if status == outbox.StatusCanceled {
		if disposition != request.Disposition || !payloadMatches {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if status == outbox.StatusProcessing {
		return outbox.ErrLeaseLost
	}
	return outbox.ErrInvalidTransition
}

func (s *Store) MarkApplied(ctx context.Context, id, leaseToken string, now time.Time) error {
	return MarkAppliedWith(ctx, s.pool, id, leaseToken, now)
}

// MarkAppliedWith finalizes only the processing message owned by the supplied lease.
func MarkAppliedWith(ctx context.Context, db DBTX, id, leaseToken string, now time.Time) error {
	command, err := db.Exec(ctx, `
		UPDATE outbox_messages SET status='applied',lease_expires_at=NULL,lease_token=NULL,updated_at=$3
		WHERE id=$1 AND status='processing' AND lease_token=$2`, id, leaseToken, now)
	if err != nil {
		return fmt.Errorf("mark outbox applied: %w", err)
	}
	if command.RowsAffected() != 1 {
		return outbox.ErrLeaseLost
	}
	return nil
}

func (s *Store) MarkDeferred(ctx context.Context, id, leaseToken, category string, deferredAt, availableAt time.Time) error {
	return MarkDeferredWith(ctx, s.pool, id, leaseToken, category, deferredAt, availableAt)
}

// MarkDeferredWith returns caller-owned processing work to pending without consuming the Claim attempt.
func MarkDeferredWith(
	ctx context.Context,
	db DBTX,
	id, leaseToken, category string,
	deferredAt, availableAt time.Time,
) error {
	if strings.TrimSpace(category) == "" || deferredAt.IsZero() || availableAt.IsZero() || !availableAt.After(deferredAt) {
		return fmt.Errorf("mark outbox deferred: non-empty category and ordered timestamps are required")
	}
	command, err := db.Exec(ctx, `
		UPDATE outbox_messages
		SET status='pending',available_at=$4,attempts=GREATEST(attempts-1,0),
		    last_error_category=$3,last_error_at=$5,terminal_disposition=NULL,
		    lease_expires_at=NULL,lease_token=NULL,updated_at=$5
		WHERE id=$1 AND status='processing' AND lease_token=$2`,
		id, leaseToken, category, availableAt, deferredAt)
	if err != nil {
		return fmt.Errorf("mark outbox deferred: %w", err)
	}
	if command.RowsAffected() != 1 {
		return outbox.ErrLeaseLost
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id, leaseToken, category string, failedAt, nextAt time.Time, dead bool) error {
	status := outbox.StatusPending
	if dead {
		status = outbox.StatusDead
	}
	command, err := s.pool.Exec(ctx, `
		UPDATE outbox_messages
		SET status=$3,available_at=$4,last_error_category=$5,last_error_at=$6,
		    lease_expires_at=NULL,lease_token=NULL,updated_at=$6
		WHERE id=$1 AND status='processing' AND lease_token=$2`, id, leaseToken, status, nextAt, category, failedAt)
	if err != nil {
		return fmt.Errorf("mark outbox failed: %w", err)
	}
	if command.RowsAffected() != 1 {
		return outbox.ErrLeaseLost
	}
	return nil
}
