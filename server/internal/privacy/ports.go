package privacy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type RemoteEraseRequest struct {
	ErasureID         string
	LearnerGeneration int64
	Receipt           StepReceipt
}
type RemoteEraseResult struct {
	Status         StepStatus
	StableReason   string
	EvidenceDigest string
	CompletedAt    time.Time
}

// DBTX is the caller-owned transaction passed to owner generation-gate ports.
type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type LocalRedactionRequest struct {
	ErasureID            string
	Store                StoreKind
	ReceiptID            string
	LearnerGeneration    int64
	RedactedThroughEvent int64
}

func (r LocalRedactionRequest) Validate(owner OwnerKind) error {
	actual, ok := OwnerForStore(r.Store)
	if r.ErasureID == "" || r.ReceiptID == "" || r.LearnerGeneration < 2 || r.RedactedThroughEvent < 0 || !ok || actual != owner {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_local_redaction_request"}
	}
	return nil
}

type GenerationTransition struct {
	ErasureID        string
	FromGeneration   int64
	TargetGeneration int64
	ReceiptID        string
	At               time.Time
}

func (t GenerationTransition) Validate(opening bool) error {
	if t.ErasureID == "" || t.TargetGeneration < 2 || t.At.IsZero() {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_generation_transition"}
	}
	if opening {
		if t.ReceiptID == "" || t.FromGeneration != t.TargetGeneration {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_generation_open"}
		}
		return nil
	}
	if t.ReceiptID != "" || t.FromGeneration < 1 || t.TargetGeneration != t.FromGeneration+1 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_generation_close"}
	}
	return nil
}

type RedactionEventAppendRequest struct {
	ErasureID         string
	LearnerGeneration int64
	ReasonCode        string
	ActorDeviceID     string
	OperationID       string
	At                time.Time
}

func (r RedactionEventAppendRequest) Validate() error {
	if uuid.Validate(r.ErasureID) != nil || r.LearnerGeneration < 2 || !ValidReasonCode(r.ReasonCode) || uuid.Validate(r.ActorDeviceID) != nil || uuid.Validate(r.OperationID) != nil || r.At.IsZero() {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_redaction_event_append"}
	}
	return nil
}

type RedactionEventAppendResult struct {
	EventID              string
	RedactedThroughEvent int64
}

type RedactionEventAppender interface {
	AppendEventRedactedTx(context.Context, DBTX, RedactionEventAppendRequest) (RedactionEventAppendResult, error)
}

// LocalOwnerPort keeps owner-private redaction SQL in the owning adapter. The
// privacy orchestrator only sequences receipts and passes caller-owned gate
// transactions.
type LocalOwnerPort interface {
	Owner() OwnerKind
	CloseGenerationTx(context.Context, DBTX, GenerationTransition) error
	OpenGenerationTx(context.Context, DBTX, GenerationTransition) error
	RedactTx(context.Context, LocalRedactionRequest) error
	VerifyRedacted(context.Context, LocalRedactionRequest) (int64, error)
}

func ValidateOwnerPort(port LocalOwnerPort) error {
	if port == nil || !port.Owner().Valid() {
		return fmt.Errorf("invalid privacy owner port")
	}
	return nil
}

func LockOwnerRead(ctx context.Context, db DBTX, owner OwnerKind) (int64, error) {
	if db == nil || !owner.Valid() {
		return 0, &Error{Code: CodeInvalidRequest, Reason: "invalid_owner_read_gate"}
	}
	var generation int64
	if err := db.QueryRow(ctx, `SELECT privacy_lock_owner_gate($1,'read',NULL)`, owner).Scan(&generation); err != nil {
		if databaseErrorCode(err) == "content_redacted" {
			return 0, &Error{Code: CodeContentRedacted, Reason: string(owner) + "_read_gate_closed", Cause: err}
		}
		return 0, fmt.Errorf("lock %s privacy read gate: %w", owner, err)
	}
	return generation, nil
}

func databaseErrorCode(err error) string {
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		return ""
	}
	return databaseError.Message
}

type RemoteEraser interface {
	Erase(context.Context, RemoteEraseRequest) (RemoteEraseResult, error)
}

type BackupKeyDestroyRequest struct {
	ErasureID         string
	LearnerGeneration int64
	RequestedAt       time.Time
	Deadline          time.Time
	At                time.Time
}

func (r BackupKeyDestroyRequest) Validate() error {
	if uuid.Validate(r.ErasureID) != nil || r.LearnerGeneration < 2 || r.RequestedAt.IsZero() || r.Deadline.IsZero() || r.At.IsZero() || r.Deadline.Before(r.RequestedAt) || r.Deadline.After(r.RequestedAt.Add(30*24*time.Hour)) || r.At.After(r.Deadline) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_backup_key_destroy_request"}
	}
	return nil
}

type BackupKeyDestroyResult struct {
	DestroyedKeys  int64
	EvidenceDigest string
	DestroyedAt    time.Time
}

type BackupKeyDestroyer interface {
	DestroyGenerationKeysTx(context.Context, DBTX, BackupKeyDestroyRequest) (BackupKeyDestroyResult, error)
}
