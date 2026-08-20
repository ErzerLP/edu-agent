package postgresstore

import (
	"context"
	"errors"
	"fmt"
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

var ErrIdempotencyConflict = errors.New("outbox idempotency key conflicts with a different message")

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
			status,available_at,attempts,max_attempts,last_error_category,last_error_at,
			lease_expires_at,lease_token,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14,$15,NULLIF($16,'')::uuid,$17,$18)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		message.ID, message.BusinessType, message.AggregateID, message.IdempotencyKey,
		message.Revision, message.Generation, message.Payload, message.AuditMetadata,
		message.Status, message.AvailableAt, message.Attempts, message.MaxAttempts,
		message.LastErrorCategory, message.LastErrorAt, message.LeaseExpiresAt, message.LeaseToken,
		message.CreatedAt, message.UpdatedAt)
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
			AND lease_expires_at IS NOT DISTINCT FROM $15
			AND lease_token IS NOT DISTINCT FROM NULLIF($16,'')::uuid
			AND created_at=$17 AND updated_at=$18
		FROM outbox_messages WHERE idempotency_key=$4`,
		message.ID, message.BusinessType, message.AggregateID, message.IdempotencyKey,
		message.Revision, message.Generation, message.Payload, message.AuditMetadata,
		message.Status, message.AvailableAt, message.Attempts, message.MaxAttempts,
		message.LastErrorCategory, message.LastErrorAt, message.LeaseExpiresAt, message.LeaseToken,
		message.CreatedAt, message.UpdatedAt).Scan(&identical)
	if err != nil {
		return false, fmt.Errorf("verify existing outbox message: %w", err)
	}
	if !identical {
		return false, ErrIdempotencyConflict
	}
	return false, nil
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
		          o.last_error_category,o.last_error_at,o.lease_expires_at,o.lease_token::text,o.created_at,o.updated_at`,
		now, limit, lease.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("claim outbox messages: %w", err)
	}
	defer rows.Close()
	var messages []outbox.Message
	for rows.Next() {
		var message outbox.Message
		if err := rows.Scan(
			&message.ID, &message.BusinessType, &message.AggregateID, &message.IdempotencyKey,
			&message.Revision, &message.Generation, &message.Payload, &message.AuditMetadata,
			&message.Status, &message.AvailableAt, &message.Attempts, &message.MaxAttempts,
			&message.LastErrorCategory, &message.LastErrorAt, &message.LeaseExpiresAt, &message.LeaseToken,
			&message.CreatedAt, &message.UpdatedAt,
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

func (s *Store) MarkApplied(ctx context.Context, id, leaseToken string, now time.Time) error {
	command, err := s.pool.Exec(ctx, `
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
