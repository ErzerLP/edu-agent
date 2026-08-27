package offline

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	Format                = "offline-queue-v1"
	KeyHeaderSize         = 128
	ObjectHeaderSize      = 160
	MaxCanonicalPackBytes = 8 << 20
	MaxSealedObject       = 12 << 20
	DefaultLeaseTimeout   = 5 * time.Second
)

type ObjectKind uint8

const (
	ObjectProfile   ObjectKind = 1
	ObjectPack      ObjectKind = 2
	ObjectOperation ObjectKind = 3
	ObjectReceipt   ObjectKind = 4
	ObjectJournal   ObjectKind = 5
)

func (k ObjectKind) valid() bool { return k >= ObjectProfile && k <= ObjectJournal }

func (k ObjectKind) directory() string {
	switch k {
	case ObjectProfile:
		return "profile"
	case ObjectPack:
		return "pack"
	case ObjectOperation:
		return "operation"
	case ObjectReceipt:
		return "receipt"
	case ObjectJournal:
		return "journal"
	default:
		return ""
	}
}

type LocalState string

const (
	StateDraft                   LocalState = "draft"
	StateQueued                  LocalState = "queued"
	StateUploading               LocalState = "uploading"
	StateArchivedPendingEvidence LocalState = "archived_pending_evidence"
	StateTerminal                LocalState = "terminal"
	StateConflict                LocalState = "conflict"
	StateBlocked                 LocalState = "blocked"
	StateDiscarded               LocalState = "discarded"
)

func (s LocalState) valid() bool {
	switch s {
	case StateDraft, StateQueued, StateUploading, StateArchivedPendingEvidence, StateTerminal, StateConflict, StateBlocked, StateDiscarded:
		return true
	default:
		return false
	}
}

type Binding struct {
	OriginHash        [32]byte
	DeviceID          [16]byte
	LearnerGeneration uint64
	ProfileID         [16]byte
}

func NewBinding(normalizedOrigin, canonicalDeviceID string, learnerGeneration uint64) (Binding, error) {
	if learnerGeneration == 0 {
		return Binding{}, errors.New("learner generation must be positive")
	}
	if err := validateNormalizedOrigin(normalizedOrigin); err != nil {
		return Binding{}, err
	}
	device, err := parseUUID(canonicalDeviceID)
	if err != nil {
		return Binding{}, fmt.Errorf("invalid device UUID: %w", err)
	}
	return Binding{OriginHash: sha256.Sum256([]byte(normalizedOrigin)), DeviceID: device, LearnerGeneration: learnerGeneration}, nil
}

func (b Binding) DeviceUUID() string      { return formatUUID(b.DeviceID) }
func (b Binding) ProfileUUID() string     { return formatUUID(b.ProfileID) }
func (b Binding) OriginDigestHex() string { return hex.EncodeToString(b.OriginHash[:]) }

func (b Binding) validate(expectProfile bool) error {
	if b.OriginHash == ([32]byte{}) || b.DeviceID == ([16]byte{}) || b.LearnerGeneration == 0 {
		return errors.New("offline binding is incomplete")
	}
	if expectProfile && b.ProfileID == ([16]byte{}) {
		return errors.New("offline profile UUID is missing")
	}
	return nil
}

func (b Binding) matches(expected Binding) bool {
	if b.OriginHash != expected.OriginHash || b.DeviceID != expected.DeviceID || b.LearnerGeneration != expected.LearnerGeneration {
		return false
	}
	return expected.ProfileID == ([16]byte{}) || b.ProfileID == expected.ProfileID
}

type TrustState struct {
	canonical json.RawMessage
}

func NewTrustState(canonical json.RawMessage) (TrustState, error) {
	validated, err := requireCanonicalObject(canonical)
	if err != nil {
		return TrustState{}, errors.New("trusted manifest must be a canonical closed JSON object")
	}
	return TrustState{canonical: append(json.RawMessage(nil), validated...)}, nil
}

func (t TrustState) Bytes() json.RawMessage {
	return append(json.RawMessage(nil), t.canonical...)
}

func (t TrustState) valid() bool {
	validated, err := requireCanonicalObject(t.canonical)
	return err == nil && len(validated) != 0
}

type CreateOptions struct {
	Binding      Binding
	TrustState   TrustState
	LeaseTimeout time.Duration
}

type JournalKind string

const (
	JournalPreparePublish     JournalKind = "prepare_publish"
	JournalObjectReplace      JournalKind = "object_replace"
	JournalCounterReservation JournalKind = "counter_reservation"
	JournalKeyMigration       JournalKind = "key_migration"
	JournalCryptoDiscard      JournalKind = "crypto_discard"
)

type KeyMetadata struct {
	Backend uint8
	Binding Binding
}

type SystemKeyProvider interface {
	Generate() ([]byte, error)
	Load(string) ([]byte, error)
	Store(string, []byte) error
	Delete(string) error
}

type KeyMigrationFailpoint string

const (
	KeyMigrationAfterJournalDurable    KeyMigrationFailpoint = "after_journal_durable"
	KeyMigrationAfterDestinationStore  KeyMigrationFailpoint = "after_destination_store"
	KeyMigrationAfterDestinationWrite  KeyMigrationFailpoint = "after_destination_write"
	KeyMigrationAfterDestinationVerify KeyMigrationFailpoint = "after_destination_verify"
	KeyMigrationAfterAuthoritySwitch   KeyMigrationFailpoint = "after_authority_switch"
	KeyMigrationBeforeSourceCleanup    KeyMigrationFailpoint = "before_source_cleanup"
	KeyMigrationAfterSourceCleanup     KeyMigrationFailpoint = "after_source_cleanup"
	KeyMigrationAfterJournalCompletion KeyMigrationFailpoint = "after_journal_completion"
)

type KeyMigrationOptions struct {
	DestinationBackend    uint8
	SystemLocator         string
	DestinationPassphrase []byte
	SystemKeys            SystemKeyProvider
	Failpoint             func(KeyMigrationFailpoint) error
}

type KeyMigrationResult struct {
	SourceBackend      uint8
	DestinationBackend uint8
	Changed            bool
	Resumed            bool
}

type ProfileRecord struct {
	Format         string          `json:"format"`
	ProfileVersion int             `json:"profile_version"`
	ProfileID      string          `json:"profile_id"`
	TrustRoot      json.RawMessage `json:"trust_root,omitempty"`
	TrustState     json.RawMessage `json:"trust_state"`
}

type PackRecord struct {
	RecordVersion  int             `json:"record_version"`
	PackID         string          `json:"pack_id"`
	EligibleUntil  string          `json:"eligible_until"`
	ArchiveUntil   string          `json:"archive_until"`
	ItemCount      int             `json:"item_count"`
	CanonicalBytes json.RawMessage `json:"canonical_bytes"`
}

type OperationRecord struct {
	RecordVersion  int             `json:"record_version"`
	OperationID    string          `json:"operation_id"`
	SubmissionID   string          `json:"submission_id"`
	PackID         string          `json:"pack_id"`
	DeviceSequence string          `json:"device_sequence"`
	State          LocalState      `json:"state"`
	QueuedAt       string          `json:"queued_at"`
	CanonicalBytes json.RawMessage `json:"canonical_bytes"`
}

type ReceiptRecord struct {
	RecordVersion    int             `json:"record_version"`
	OperationID      string          `json:"operation_id"`
	SubmissionID     string          `json:"submission_id"`
	DeviceSequence   string          `json:"device_sequence"`
	State            LocalState      `json:"state"`
	ArchiveStatus    string          `json:"archive_status"`
	AssessmentStatus string          `json:"assessment_status"`
	EvidenceStatus   string          `json:"evidence_status"`
	ReasonCodes      []string        `json:"reason_codes"`
	Receipt          json.RawMessage `json:"receipt"`
	Status           json.RawMessage `json:"status"`
	UpdatedAt        string          `json:"updated_at"`
}

type JournalRecord struct {
	JournalVersion int             `json:"journal_version"`
	JournalID      string          `json:"journal_id"`
	Kind           JournalKind     `json:"kind"`
	State          string          `json:"state"`
	SourceID       string          `json:"source_id"`
	TargetID       string          `json:"target_id"`
	Revision       string          `json:"revision"`
	CreatedAt      string          `json:"created_at"`
	Detail         json.RawMessage `json:"detail"`
}

type Pack struct {
	ID            string
	EligibleUntil time.Time
	ArchiveUntil  time.Time
	ItemCount     int
	Canonical     json.RawMessage
}

type PackInfo struct {
	ID            string
	EligibleUntil time.Time
	ArchiveUntil  time.Time
	ItemCount     int
	Available     bool
}

type QueuedOperation struct {
	ID             string
	SubmissionID   string
	PackID         string
	DeviceSequence uint64
	QueuedAt       time.Time
	Canonical      json.RawMessage
}

type SyncResult struct {
	OperationID      string
	SubmissionID     string
	State            LocalState
	ArchiveStatus    string
	AssessmentStatus string
	EvidenceStatus   string
	ReasonCodes      []string
	Receipt          json.RawMessage
	Status           json.RawMessage
	UpdatedAt        time.Time
}

type OperationStatus struct {
	OperationID      string
	SubmissionID     string
	State            LocalState
	ArchiveStatus    string
	AssessmentStatus string
	EvidenceStatus   string
	ReasonCodes      []string
	UpdatedAt        time.Time
}

type PrepareIntent struct {
	RequestID  string
	CreatedAt  time.Time
	Canonical  json.RawMessage
	TrustState TrustState
}

type SyncBatch struct {
	JournalID  string
	Operations []QueuedOperation
}

type Summary struct {
	PackCount            int
	AvailablePackCount   int
	AvailableItemCount   int
	QueuedCount          int
	UploadingCount       int
	ArchivedPendingCount int
	TerminalCount        int
	ConflictCount        int
	BlockedCount         int
	EarliestExpiry       *time.Time
	LastSuccessfulSync   *time.Time
	PendingJournalCount  int
}

type LogoutPreflight struct {
	Nonterminal      bool
	PendingJournals  bool
	NonterminalCount int
	JournalCount     int
}

func DefaultRoot() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user configuration directory: %w", err)
	}
	return filepath.Join(root, "edu-agent", "offline"), nil
}

func validateNormalizedOrigin(value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return errors.New("normalized origin is empty or padded")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("normalized origin must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("normalized origin contains forbidden components")
	}
	if parsed.Scheme != strings.ToLower(parsed.Scheme) || parsed.Host != strings.ToLower(parsed.Host) {
		return errors.New("origin is not normalized")
	}
	return nil
}

func parseUUID(value string) ([16]byte, error) {
	var out [16]byte
	if len(value) != 36 || value != strings.ToLower(value) || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return out, errors.New("UUID is not canonical lowercase text")
	}
	raw := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != len(out) {
		return out, errors.New("UUID has invalid hexadecimal bytes")
	}
	copy(out[:], decoded)
	if out == ([16]byte{}) || out[8]&0xc0 != 0x80 {
		return [16]byte{}, errors.New("UUID is not RFC 4122")
	}
	return out, nil
}

func formatUUID(value [16]byte) string {
	raw := hex.EncodeToString(value[:])
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32]
}

func randomUUID() ([16]byte, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return value, err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return value, nil
}

func canonicalUint(value uint64) string { return strconv.FormatUint(value, 10) }

func parseCanonicalUint(value string, positive bool) (uint64, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("integer is not canonical unsigned decimal")
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || (positive && parsed == 0) {
		return 0, errors.New("integer is outside the supported range")
	}
	return parsed, nil
}

func normalizeTime(value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, errors.New("time is required")
	}
	return value.UTC().Truncate(time.Microsecond), nil
}
