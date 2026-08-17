package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Enqueue(ctx context.Context, message outbox.Message) (bool, error) {
	if err := message.Validate(); err != nil {
		return false, err
	}
	command, err := s.pool.Exec(ctx, `
		INSERT INTO outbox_messages(
			id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,
			status,available_at,attempts,max_attempts,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		message.ID, message.BusinessType, message.AggregateID, message.IdempotencyKey,
		message.Revision, message.Generation, message.Payload, message.AuditMetadata,
		message.Status, message.AvailableAt, message.Attempts, message.MaxAttempts,
		message.CreatedAt, message.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("enqueue outbox message: %w", err)
	}
	return command.RowsAffected() == 1, nil
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
		SET status='processing',attempts=o.attempts+1,lease_expires_at=$1 + ($3 * interval '1 microsecond'),updated_at=$1
		FROM candidates c WHERE o.id=c.id
		RETURNING o.id,o.business_type,o.aggregate_id,o.idempotency_key,o.revision,o.generation,
		          o.payload,o.audit_metadata,o.status,o.available_at,o.attempts,o.max_attempts,
		          o.last_error_category,o.last_error_at,o.lease_expires_at,o.created_at,o.updated_at`,
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
			&message.LastErrorCategory, &message.LastErrorAt, &message.LeaseExpiresAt,
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

func (s *Store) MarkApplied(ctx context.Context, id string, now time.Time) error {
	command, err := s.pool.Exec(ctx, `
		UPDATE outbox_messages SET status='applied',lease_expires_at=NULL,updated_at=$2
		WHERE id=$1 AND status='processing'`, id, now)
	if err != nil {
		return fmt.Errorf("mark outbox applied: %w", err)
	}
	if command.RowsAffected() != 1 {
		return outbox.ErrInvalidTransition
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id, category string, failedAt, nextAt time.Time, dead bool) error {
	status := outbox.StatusPending
	if dead {
		status = outbox.StatusDead
	}
	command, err := s.pool.Exec(ctx, `
		UPDATE outbox_messages
		SET status=$2,available_at=$3,last_error_category=$4,last_error_at=$5,
		    lease_expires_at=NULL,updated_at=$5
		WHERE id=$1 AND status='processing'`, id, status, nextAt, category, failedAt)
	if err != nil {
		return fmt.Errorf("mark outbox failed: %w", err)
	}
	if command.RowsAffected() != 1 {
		return outbox.ErrInvalidTransition
	}
	return nil
}

func IsSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}
