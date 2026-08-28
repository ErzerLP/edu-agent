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
	StatusCanceled   Status = "canceled"
)

type TerminalDisposition string

const (
	DispositionFenced              TerminalDisposition = "fenced"
	DispositionSuperseded          TerminalDisposition = "superseded"
	DispositionPrivacyErasure      TerminalDisposition = "privacy_erasure"
	DispositionExpired             TerminalDisposition = "expired"
	DispositionPermanentlyRejected TerminalDisposition = "permanently_rejected"
	DispositionDeleted             TerminalDisposition = "deleted"
	DispositionReviewRequired      TerminalDisposition = "review_required"
)

var (
	ErrInvalidTransition = errors.New("invalid outbox state transition")
	ErrLeaseLost         = errors.New("outbox lease ownership was lost")
	ErrConsumerFinalized = errors.New("consumer atomically finalized the outbox disposition")
)

type Message struct {
	ID                  string              `json:"id"`
	BusinessType        string              `json:"business_type"`
	AggregateID         string              `json:"aggregate_id"`
	IdempotencyKey      string              `json:"idempotency_key"`
	Revision            int64               `json:"revision"`
	Generation          int64               `json:"generation"`
	Payload             json.RawMessage     `json:"payload"`
	AuditMetadata       json.RawMessage     `json:"audit_metadata"`
	Status              Status              `json:"status"`
	AvailableAt         time.Time           `json:"available_at"`
	Attempts            int                 `json:"attempts"`
	MaxAttempts         int                 `json:"max_attempts"`
	LastErrorCategory   string              `json:"last_error_category,omitempty"`
	LastErrorAt         *time.Time          `json:"last_error_at,omitempty"`
	TerminalDisposition TerminalDisposition `json:"terminal_disposition,omitempty"`
	LeaseExpiresAt      *time.Time          `json:"-"`
	LeaseToken          string              `json:"-"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
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
	if m.Status != StatusPending && m.Status != StatusProcessing && m.Status != StatusApplied && m.Status != StatusDead && m.Status != StatusCanceled {
		return errors.New("outbox status is invalid")
	}
	if (m.Status == StatusCanceled) != validTerminalDisposition(m.TerminalDisposition) {
		return errors.New("outbox terminal disposition is invalid")
	}
	return nil
}

func CanTransition(from, to Status) bool {
	switch from {
	case StatusPending:
		return to == StatusProcessing || to == StatusCanceled
	case StatusProcessing:
		return to == StatusPending || to == StatusApplied || to == StatusDead || to == StatusCanceled
	case StatusDead:
		return to == StatusPending || to == StatusCanceled
	default:
		return false
	}
}

type RequeueRequest struct {
	BusinessType   string
	AggregateID    string
	IdempotencyKey string
	Revision       int64
	Generation     int64
	Payload        json.RawMessage
	AvailableAt    time.Time
}

func (r RequeueRequest) Validate() error {
	if strings.TrimSpace(r.BusinessType) == "" || strings.TrimSpace(r.AggregateID) == "" ||
		strings.TrimSpace(r.IdempotencyKey) == "" || r.Revision < 0 || r.Generation < 0 ||
		r.AvailableAt.IsZero() || !json.Valid(r.Payload) {
		return errors.New("valid dead outbox requeue identity, payload, and time are required")
	}
	return nil
}

type CancelRequest struct {
	IdempotencyKey   string
	LeaseToken       string
	Disposition      TerminalDisposition
	TombstonePayload json.RawMessage
	CanceledAt       time.Time
}

func (r CancelRequest) Validate() error {
	if strings.TrimSpace(r.IdempotencyKey) == "" || !validTerminalDisposition(r.Disposition) || r.CanceledAt.IsZero() {
		return errors.New("valid outbox cancellation key, disposition, and time are required")
	}
	if len(r.TombstonePayload) > 0 && !json.Valid(r.TombstonePayload) {
		return errors.New("outbox cancellation tombstone must be valid JSON")
	}
	return nil
}

type Store interface {
	Enqueue(context.Context, Message) (bool, error)
	Claim(context.Context, time.Time, time.Duration, int) ([]Message, error)
	RequeueDead(context.Context, RequeueRequest) error
	Cancel(context.Context, CancelRequest) error
	MarkApplied(context.Context, string, string, time.Time) error
	MarkDeferred(context.Context, string, string, string, time.Time, time.Time) error
	MarkFailed(context.Context, string, string, string, time.Time, time.Time, bool) error
}

func validTerminalDisposition(value TerminalDisposition) bool {
	return value == DispositionFenced || value == DispositionSuperseded || value == DispositionPrivacyErasure ||
		value == DispositionExpired || value == DispositionPermanentlyRejected || value == DispositionDeleted ||
		value == DispositionReviewRequired
}

type ApplyDecision struct {
	Apply               bool
	TerminalDisposition TerminalDisposition
}

func (d ApplyDecision) Validate() error {
	if d.Apply {
		if d.TerminalDisposition != "" {
			return errors.New("applicable outbox decision cannot be terminal")
		}
		return nil
	}
	if !validTerminalDisposition(d.TerminalDisposition) {
		return errors.New("non-applicable outbox decision requires a terminal disposition")
	}
	return nil
}

// Consumer owns target idempotency and the revision/generation fence.
type Consumer interface {
	CanApply(context.Context, Message) (ApplyDecision, error)
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
