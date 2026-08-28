package privacy

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const ManagedBackupInventoryFormat = "edu-agent-managed-backup-inventory-v1"

var (
	ErrGenerationKeyDestroyed       = errors.New("managed backup generation key is destroyed")
	ErrGenerationKeyUnavailable     = errors.New("managed backup generation key is unavailable")
	ErrManagedBackupInvalid         = errors.New("managed backup metadata is invalid")
	ErrManagedBackupConflict        = errors.New("managed backup metadata conflicts with an immutable record")
	ErrManagedBackupIntegrity       = errors.New("managed backup integrity verification failed")
	ErrManagedBackupLiveOldKey      = errors.New("managed backup old generation key is still live")
	ErrManagedBackupBarrierUnproven = errors.New("managed backup barrier destruction is not proven")
)

type ManagedBackupVerificationRequest struct {
	ErasureID         string
	LearnerGeneration int64
}

func (r ManagedBackupVerificationRequest) Validate() error {
	if uuid.Validate(r.ErasureID) != nil || r.LearnerGeneration < 2 {
		return ErrManagedBackupInvalid
	}
	return nil
}

// ManagedBackupVerificationResult contains only receipt-safe state. Evidence is
// represented by a digest; key material and restored content never cross this port.
type ManagedBackupVerificationResult struct {
	Status         StepStatus
	StableReason   string
	EvidenceDigest string
	CompletedAt    time.Time
}

type ManagedBackupVerifier interface {
	VerifyManagedBackups(context.Context, ManagedBackupVerificationRequest) (ManagedBackupVerificationResult, error)
}

type ManagedBackupBarrierState struct {
	VerifiedUnrecoverableAt time.Time
	DestroyedOldKeyCount    int64
}

// ManagedBackupBarrierRepository proves the database-side barrier invariant
// without returning wrapped keys, plaintext keys, or backup content.
type ManagedBackupBarrierRepository interface {
	VerifyManagedBackupBarrier(context.Context, string, int64) (ManagedBackupBarrierState, error)
}

// GenerationKeyLease exposes plaintext key material only inside repository-owned
// callbacks. Implementations must invalidate the lease and erase its plaintext
// key after the outer callback returns.
type GenerationKeyLease interface {
	WrappedKeyID() string
	Generation() int64
	Use(func([]byte) error) error
	RecordManagedBackup(context.Context, ManagedBackupArtifact) error
}

// GenerationKeyRepository owns generation-key creation, unwrap, and destruction
// verification. Callers receive no key value that can be stored on a controller.
type GenerationKeyRepository interface {
	WithGenerationKey(context.Context, int64, func(GenerationKeyLease) error) error
	WithExistingGenerationKey(context.Context, int64, string, func(GenerationKeyLease) error) error
	VerifyGenerationKeyDestroyed(context.Context, int64, string) error
}

type ManagedBackupArtifact struct {
	Path              string    `json:"path"`
	CreatedAt         time.Time `json:"created_at"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
	LearnerGeneration int64     `json:"learner_generation"`
	WrappedKeyID      string    `json:"wrapped_key_id"`
}

func (a ManagedBackupArtifact) Validate() error {
	if !validManagedBackupPath(a.Path) || a.CreatedAt.IsZero() || a.CreatedAt.Location() != time.UTC ||
		a.Size < 0 || a.LearnerGeneration < 1 || uuid.Validate(a.WrappedKeyID) != nil {
		return ErrManagedBackupInvalid
	}
	parsedID, err := uuid.Parse(a.WrappedKeyID)
	if err != nil || parsedID.String() != a.WrappedKeyID {
		return ErrManagedBackupInvalid
	}
	if len(a.SHA256) != 64 || strings.ToLower(a.SHA256) != a.SHA256 {
		return ErrManagedBackupInvalid
	}
	digest, err := hex.DecodeString(a.SHA256)
	if err != nil || len(digest) != 32 {
		return ErrManagedBackupInvalid
	}
	return nil
}

func validManagedBackupPath(value string) bool {
	if value == "" || value == "managed-inventory.json" || value == ".edu-agent-backup.lock" ||
		value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// ManagedBackupInventoryRepository persists the same artifact fields published
// in managed-inventory.json and records successful maintenance pruning.
type ManagedBackupInventoryRepository interface {
	RecordManagedBackup(context.Context, ManagedBackupArtifact) error
	DiscardManagedBackupPublication(context.Context, ManagedBackupArtifact, time.Time) error
	ManagedBackupInventory(context.Context) ([]ManagedBackupArtifact, error)
	MarkManagedBackupsPruned(context.Context, []string, time.Time) error
}

type ManagedBackupRepository interface {
	GenerationKeyRepository
	ManagedBackupInventoryRepository
}
