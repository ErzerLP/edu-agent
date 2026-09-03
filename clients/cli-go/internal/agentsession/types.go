package agentsession

import (
	"encoding/json"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
)

const (
	profileSecretService         = "edu-agent-agent-sessions-v1"
	profileSecretVersion         = 1
	recordContainerSchemaVersion = 1
	recordPayloadSchemaVersion   = 2
	recordMigrationMaxSteps      = 1
	indexSchemaVersion           = 1
	projectionSchemaVersion      = 1
	dirtySchemaVersion           = 1
	transcriptSchemaVersion      = 1
	envelopeSchemaVersion        = 1
)

type Limits struct {
	Sessions                int
	ProfileCiphertextBytes  int64
	SessionPlaintextBytes   int64
	SessionCiphertextBytes  int64
	DirtyMarkerBytes        int64
	DirectoryEntries        int
	TranscriptEntries       int
	TranscriptBytes         int64
	TranscriptEntryBytes    int
	TranscriptEventBytes    int
	TranscriptEntryLines    int
	TranscriptLineColumns   int
	TranscriptTools         int
	PickerQueryRunes        int
	PickerResults           int
	SearchSummaryRunes      int
	SearchSummaryBytes      int
	ManualTitleBytes        int
	ManualTitleRunes        int
	ManualTitleColumns      int
	AutoTitleInputBytes     int
	AutoTitlePartBytes      int
	AutoTitleResponseBytes  int
	AutoTitleMaxTokens      int
	AutoTitleTurnInterval   uint64
	AutoTitleMinInterval    time.Duration
	AutoTitleRequestTimeout time.Duration
	AutoTitleSaveTimeout    time.Duration
	NoticeCount             int
	ReceiptCount            int
}

func DefaultLimits() Limits {
	return Limits{
		Sessions: 256, ProfileCiphertextBytes: 1 << 30,
		SessionPlaintextBytes: 48 << 20, SessionCiphertextBytes: 64 << 20,
		DirtyMarkerBytes: 16 << 10, DirectoryEntries: 2048,
		TranscriptEntries: 2048, TranscriptBytes: 4 << 20,
		TranscriptEntryBytes: 64 << 10, TranscriptEventBytes: 16 << 10,
		TranscriptEntryLines: 1024, TranscriptLineColumns: 4096, TranscriptTools: 64,
		PickerQueryRunes: 128, PickerResults: 256,
		SearchSummaryRunes: 160, SearchSummaryBytes: 512,
		ManualTitleBytes: 256, ManualTitleRunes: 80, ManualTitleColumns: 60,
		AutoTitleInputBytes: 6000, AutoTitlePartBytes: 1600, AutoTitleResponseBytes: 256,
		AutoTitleMaxTokens: 96, AutoTitleTurnInterval: 3, AutoTitleMinInterval: 10 * time.Minute,
		AutoTitleRequestTimeout: 15 * time.Second, AutoTitleSaveTimeout: 5 * time.Second,
		NoticeCount: 32, ReceiptCount: 32,
	}
}

type SecretBackend interface {
	Available(keybackend.Locator) error
	Load(keybackend.Locator, int) ([]byte, error)
	Store(keybackend.Locator, []byte) error
	Delete(keybackend.Locator) error
}

type Options struct {
	Root               string
	ProfileFingerprint string
	Secrets            SecretBackend
	Limits             Limits
	Now                func() time.Time
	LockTimeout        time.Duration
}

type CreateInput struct {
	Title                     string
	WorkspaceID               string
	WorkspaceRoot             string
	WorkspaceLabel            string
	WorkspacePathHash         string
	WorkspaceRootIdentityHash string
	ProviderName              string
	ProviderEndpoint          string
	ProviderModel             string
	PrivacyLearnerGeneration  int64
	PrivacyMemoryGeneration   int64
	PrivacyVerified           bool
	Checkpoint                json.RawMessage
	Transcript                json.RawMessage
}

const (
	PreferenceStageCreate = "create"
	PreferenceStageAdmit  = "admit"
	PreferenceStageReject = "reject"

	FilePublicationCompletedCode = "workspace_write_completed"
	FilePublicationUnknownCode   = "workspace_write_outcome_unknown"
)

type PreferencePayload struct {
	Content     string    `json:"content"`
	Reason      string    `json:"reason"`
	Category    string    `json:"category"`
	Sensitivity string    `json:"sensitivity"`
	Stability   string    `json:"stability"`
	ValidUntil  time.Time `json:"valid_until"`
}

type PreferenceReceipt struct {
	ToolCallID        string            `json:"tool_call_id"`
	CreateOperationID string            `json:"create_operation_id"`
	AdmitOperationID  string            `json:"admit_operation_id"`
	RejectOperationID string            `json:"reject_operation_id,omitempty"`
	Payload           PreferencePayload `json:"payload"`
	CandidateID       string            `json:"candidate_id,omitempty"`
	CandidateRevision int64             `json:"candidate_revision,omitempty"`
	Stage             string            `json:"stage"`
	StableCode        string            `json:"stable_code"`
	Outcome           string            `json:"outcome"`
}

type FileReceipt struct {
	ToolCallID         string `json:"tool_call_id"`
	Operation          string `json:"operation"`
	Path               string `json:"path"`
	Kind               string `json:"kind"`
	ContentHash        string `json:"content_hash,omitempty"`
	InvalidateObserved bool   `json:"invalidate_observed"`
	StableCode         string `json:"stable_code"`
	Outcome            string `json:"publication_outcome"`
}

func (value FileReceipt) publicationOutcome() string { return value.Outcome }

type PreferenceWriteAhead struct {
	ToolCallID        string            `json:"tool_call_id"`
	CreateOperationID string            `json:"create_operation_id"`
	AdmitOperationID  string            `json:"admit_operation_id"`
	RejectOperationID string            `json:"reject_operation_id,omitempty"`
	Payload           PreferencePayload `json:"payload"`
	CandidateID       string            `json:"candidate_id,omitempty"`
	CandidateRevision int64             `json:"candidate_revision,omitempty"`
	Stage             string            `json:"stage"`
	StableCode        string            `json:"stable_code,omitempty"`
	Outcome           string            `json:"outcome,omitempty"`
}

type FileWriteAhead struct {
	ToolCallID         string `json:"tool_call_id"`
	Operation          string `json:"operation"`
	Path               string `json:"path"`
	Kind               string `json:"kind"`
	ContentHash        string `json:"content_hash,omitempty"`
	InvalidateObserved bool   `json:"invalidate_observed"`
	StableCode         string `json:"stable_code"`
	PublicationOutcome string `json:"publication_outcome"`
}

type SessionRecord struct {
	SchemaVersion     int    `json:"schema_version"`
	SessionID         string `json:"session_id"`
	StorageID         string `json:"storage_id"`
	PrivacyGeneration uint64 `json:"privacy_generation"`
	RecordRevision    uint64 `json:"record_revision"`
	// CommitID distinguishes an exact publication from a stale revision.
	CommitID                  string              `json:"commit_id"`
	CreatedAt                 time.Time           `json:"created_at"`
	UpdatedAt                 time.Time           `json:"updated_at"`
	LastOpenedAt              time.Time           `json:"last_opened_at"`
	CheckpointRevision        uint64              `json:"checkpoint_revision"`
	ServerProfileFingerprint  string              `json:"server_profile_fingerprint"`
	Lifecycle                 string              `json:"lifecycle"`
	Title                     string              `json:"title"`
	TitleSource               string              `json:"title_source,omitempty"`
	FirstUserSummary          string              `json:"first_user_summary,omitempty"`
	RecentUserSummary         string              `json:"recent_user_summary,omitempty"`
	AutoTitleTurns            uint64              `json:"auto_title_turns,omitempty"`
	TitleRevision             uint64              `json:"title_revision,omitempty"`
	LastTitleAt               time.Time           `json:"last_title_at,omitempty"`
	WorkspaceID               string              `json:"workspace_id"`
	WorkspaceRoot             string              `json:"workspace_root,omitempty"`
	WorkspaceLabel            string              `json:"workspace_label,omitempty"`
	WorkspacePathHash         string              `json:"workspace_path_hash,omitempty"`
	WorkspaceRootIdentityHash string              `json:"workspace_root_identity_hash,omitempty"`
	ProviderName              string              `json:"provider_name,omitempty"`
	ProviderEndpoint          string              `json:"provider_endpoint"`
	ProviderModel             string              `json:"provider_model,omitempty"`
	PrivacyLearnerGeneration  int64               `json:"privacy_learner_generation,omitempty"`
	PrivacyMemoryGeneration   int64               `json:"privacy_memory_generation,omitempty"`
	PrivacyVerified           bool                `json:"privacy_verified"`
	CommittedUserTurns        uint64              `json:"committed_user_turns"`
	TranscriptCount           uint64              `json:"transcript_count"`
	PreferenceReceipts        []PreferenceReceipt `json:"preference_receipts,omitempty"`
	FileReceipts              []FileReceipt       `json:"file_receipts,omitempty"`
	Checkpoint                json.RawMessage     `json:"checkpoint"`
	QuarantinedCheckpoint     json.RawMessage     `json:"quarantined_checkpoint,omitempty"`
	Transcript                json.RawMessage     `json:"transcript"`
	LastConsumedDirtyID       string              `json:"last_consumed_dirty_id,omitempty"`
}

// recordPayloadV1 freezes the complete v1 payload contract. It must not gain
// fields when SessionRecord evolves; migrations decode this DTO before
// producing the current payload.
type recordPayloadV1 struct {
	SchemaVersion             int                 `json:"schema_version"`
	SessionID                 string              `json:"session_id"`
	StorageID                 string              `json:"storage_id"`
	PrivacyGeneration         uint64              `json:"privacy_generation"`
	RecordRevision            uint64              `json:"record_revision"`
	CommitID                  string              `json:"commit_id"`
	CreatedAt                 time.Time           `json:"created_at"`
	UpdatedAt                 time.Time           `json:"updated_at"`
	LastOpenedAt              time.Time           `json:"last_opened_at"`
	CheckpointRevision        uint64              `json:"checkpoint_revision"`
	ServerProfileFingerprint  string              `json:"server_profile_fingerprint"`
	Lifecycle                 string              `json:"lifecycle"`
	Title                     string              `json:"title"`
	TitleSource               string              `json:"title_source,omitempty"`
	FirstUserSummary          string              `json:"first_user_summary,omitempty"`
	RecentUserSummary         string              `json:"recent_user_summary,omitempty"`
	AutoTitleTurns            uint64              `json:"auto_title_turns,omitempty"`
	TitleRevision             uint64              `json:"title_revision,omitempty"`
	LastTitleAt               time.Time           `json:"last_title_at,omitempty"`
	WorkspaceID               string              `json:"workspace_id"`
	WorkspaceRoot             string              `json:"workspace_root,omitempty"`
	WorkspaceLabel            string              `json:"workspace_label,omitempty"`
	WorkspacePathHash         string              `json:"workspace_path_hash,omitempty"`
	WorkspaceRootIdentityHash string              `json:"workspace_root_identity_hash,omitempty"`
	ProviderName              string              `json:"provider_name,omitempty"`
	ProviderEndpoint          string              `json:"provider_endpoint"`
	ProviderModel             string              `json:"provider_model,omitempty"`
	PrivacyLearnerGeneration  int64               `json:"privacy_learner_generation,omitempty"`
	PrivacyMemoryGeneration   int64               `json:"privacy_memory_generation,omitempty"`
	PrivacyVerified           bool                `json:"privacy_verified"`
	CommittedUserTurns        uint64              `json:"committed_user_turns"`
	TranscriptCount           uint64              `json:"transcript_count"`
	PreferenceReceipts        []PreferenceReceipt `json:"preference_receipts,omitempty"`
	FileReceipts              []FileReceipt       `json:"file_receipts,omitempty"`
	Checkpoint                json.RawMessage     `json:"checkpoint"`
	QuarantinedCheckpoint     json.RawMessage     `json:"quarantined_checkpoint,omitempty"`
	Transcript                json.RawMessage     `json:"transcript"`
	LastConsumedDirtyID       string              `json:"last_consumed_dirty_id,omitempty"`
}

type DirtyMarker struct {
	SchemaVersion     int                   `json:"schema_version"`
	DirtyID           string                `json:"dirty_id"`
	SessionID         string                `json:"session_id"`
	StorageID         string                `json:"storage_id"`
	BaseRevision      uint64                `json:"base_revision"`
	TurnSequence      uint64                `json:"turn_sequence"`
	OperationClass    string                `json:"operation_class"`
	MayHaveSideEffect bool                  `json:"may_have_side_effect"`
	StartedAt         time.Time             `json:"started_at"`
	Preference        *PreferenceWriteAhead `json:"preference,omitempty"`
	File              *FileWriteAhead       `json:"file,omitempty"`
}

type Loaded struct {
	Record      SessionRecord
	Interrupted *DirtyMarker
}

type DeleteTarget struct {
	SessionID              string
	StorageID              string
	ExpectedRecordRevision uint64
}

type Summary struct {
	SessionID                string
	StorageID                string
	RecordRevision           uint64
	CheckpointRevision       uint64
	CreatedAt                time.Time
	UpdatedAt                time.Time
	LastOpenedAt             time.Time
	Title                    string
	TitleSource              string
	FirstUserSummary         string
	RecentUserSummary        string
	TitleRevision            uint64
	CommittedUserTurns       uint64
	TranscriptCount          uint64
	ServerProfileFingerprint string
	WorkspaceID              string
	WorkspaceLabel           string
	ProviderName             string
	ProviderEndpoint         string
	ProviderModel            string
	Lifecycle                string
	Corrupt                  bool
	Locked                   bool
	Unavailable              bool
	LocatorOnly              bool
	VersionUnsupported       bool
}

type indexFile struct {
	SchemaVersion     int            `json:"schema_version"`
	IndexRevision     uint64         `json:"index_revision"`
	PrivacyGeneration uint64         `json:"privacy_generation"`
	Entries           []indexLocator `json:"entries"`
}

type indexLocator struct {
	SessionID string `json:"session_id"`
	StorageID string `json:"storage_id"`
}

type indexProjection struct {
	SchemaVersion            int       `json:"schema_version"`
	SessionID                string    `json:"session_id"`
	StorageID                string    `json:"storage_id"`
	PrivacyGeneration        uint64    `json:"privacy_generation"`
	RecordRevision           uint64    `json:"record_revision"`
	RecordCommitID           string    `json:"record_commit_id"`
	CheckpointRevision       uint64    `json:"checkpoint_revision"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
	LastOpenedAt             time.Time `json:"last_opened_at"`
	Title                    string    `json:"title"`
	TitleSource              string    `json:"title_source"`
	FirstUserSummary         string    `json:"first_user_summary,omitempty"`
	RecentUserSummary        string    `json:"recent_user_summary,omitempty"`
	TitleRevision            uint64    `json:"title_revision"`
	CommittedUserTurns       uint64    `json:"committed_user_turns"`
	TranscriptCount          uint64    `json:"transcript_count"`
	ServerProfileFingerprint string    `json:"server_profile_fingerprint"`
	WorkspaceID              string    `json:"workspace_id"`
	WorkspaceLabel           string    `json:"workspace_label,omitempty"`
	ProviderName             string    `json:"provider_name,omitempty"`
	ProviderEndpoint         string    `json:"provider_endpoint"`
	ProviderModel            string    `json:"provider_model,omitempty"`
	Lifecycle                string    `json:"lifecycle"`

	Corrupt            bool `json:"-"`
	LocatorOnly        bool `json:"-"`
	VersionUnsupported bool `json:"-"`
	EnvelopeValid      bool `json:"-"`
	RecordValid        bool `json:"-"`
	ProjectionValid    bool `json:"-"`
}

// keyEnvelope is the only persisted path from a profile wrapping key to a
// per-session data key.
type keyEnvelope struct {
	SchemaVersion     int    `json:"schema_version"`
	SessionID         string `json:"session_id"`
	StorageID         string `json:"storage_id"`
	PrivacyGeneration uint64 `json:"privacy_generation"`
	DataKey           string `json:"data_key"`
}
