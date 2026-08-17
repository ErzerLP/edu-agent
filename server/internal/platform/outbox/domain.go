package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusApplied    Status = "applied"
	StatusDead       Status = "dead"
)

var ErrInvalidTransition = errors.New("invalid outbox state transition")

type Message struct {
	ID                string          `json:"id"`
	BusinessType      string          `json:"business_type"`
	AggregateID       string          `json:"aggregate_id"`
	IdempotencyKey    string          `json:"idempotency_key"`
	Revision          int64           `json:"revision"`
	Generation        int64           `json:"generation"`
	Payload           json.RawMessage `json:"payload"`
	AuditMetadata     json.RawMessage `json:"audit_metadata"`
	Status            Status          `json:"status"`
	AvailableAt       time.Time       `json:"available_at"`
	Attempts          int             `json:"attempts"`
	MaxAttempts       int             `json:"max_attempts"`
	LastErrorCategory string          `json:"last_error_category,omitempty"`
	LastErrorAt       *time.Time      `json:"last_error_at,omitempty"`
	LeaseExpiresAt    *time.Time      `json:"-"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type NewMessageInput struct {
	BusinessType   string
	AggregateID    string
	IdempotencyKey string
	Revision       int64
	Generation     int64
	Payload        json.RawMessage
	AuditMetadata  json.RawMessage
	MaxAttempts    int
}

func NewMessage(input NewMessageInput, now time.Time) (Message, error) {
	id, err := newUUID()
	if err != nil {
		return Message{}, fmt.Errorf("generate outbox id: %w", err)
	}
	if len(input.AuditMetadata) == 0 {
		input.AuditMetadata = json.RawMessage(`{}`)
	}
	message := Message{
		ID: id, BusinessType: strings.TrimSpace(input.BusinessType), AggregateID: strings.TrimSpace(input.AggregateID),
		IdempotencyKey: strings.TrimSpace(input.IdempotencyKey), Revision: input.Revision, Generation: input.Generation,
		Payload: append(json.RawMessage(nil), input.Payload...), AuditMetadata: append(json.RawMessage(nil), input.AuditMetadata...),
		Status: StatusPending, AvailableAt: now.UTC(), MaxAttempts: input.MaxAttempts,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	return message, nil
}

func (m Message) Validate() error {
	if m.ID == "" || m.BusinessType == "" || m.AggregateID == "" || m.IdempotencyKey == "" {
		return errors.New("outbox identifiers and business type are required")
	}
	if m.Revision < 0 || m.Generation < 0 || m.MaxAttempts <= 0 || m.Attempts < 0 {
		return errors.New("outbox counters are invalid")
	}
	if !json.Valid(m.Payload) || !json.Valid(m.AuditMetadata) {
		return errors.New("outbox payload and audit metadata must be valid JSON")
	}
	if m.Status != StatusPending && m.Status != StatusProcessing && m.Status != StatusApplied && m.Status != StatusDead {
		return errors.New("outbox status is invalid")
	}
	return nil
}

func CanTransition(from, to Status) bool {
	switch from {
	case StatusPending:
		return to == StatusProcessing
	case StatusProcessing:
		return to == StatusPending || to == StatusApplied || to == StatusDead
	default:
		return false
	}
}

type Store interface {
	Enqueue(context.Context, Message) (bool, error)
	Claim(context.Context, time.Time, time.Duration, int) ([]Message, error)
	MarkApplied(context.Context, string, time.Time) error
	MarkFailed(context.Context, string, string, time.Time, time.Time, bool) error
}

// Consumer owns target idempotency and the revision/generation fence.
type Consumer interface {
	CanApply(context.Context, Message) (bool, error)
	Apply(context.Context, Message) error
}

func newUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
