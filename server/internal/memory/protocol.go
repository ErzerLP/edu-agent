package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
)

const (
	NocturneUpstreamCommit = "54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254"
	NocturneCompatRevision = "edu-agent-maintenance-v1"
)

type DeliveryWork struct {
	Delivery            Delivery
	Policy              DeliveryPolicy
	Content             string
	PreviousContentHash string
	CurrentGeneration   Generation
	ExternalNodeID      string
	ExternalMemoryID    int64
}

func (w DeliveryWork) ValidateIntent(intent OutboxIntent) error {
	if w.Delivery.ID != intent.DeliveryID || w.Delivery.PayloadHash != intent.PayloadHash ||
		w.Delivery.RecordRevision != intent.RecordRevision ||
		w.Delivery.LearnerGeneration != intent.LearnerGeneration || w.Delivery.RecordGeneration != intent.RecordGeneration {
		return &Error{Code: CodeDeliveryConflict, Reason: "outbox_delivery_tuple_mismatch"}
	}
	return nil
}

func DecodeOutboxIntent(payload json.RawMessage) (OutboxIntent, error) {
	var intent OutboxIntent
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return intent, invalid("invalid_outbox_intent")
	}
	if err := ensureDecodeEOF(decoder); err != nil {
		return intent, invalid("invalid_outbox_intent")
	}
	if !validUUID(intent.DeliveryID) || !validHash(intent.PayloadHash) || intent.RecordRevision < 1 ||
		intent.LearnerGeneration < 1 || intent.RecordGeneration < 1 {
		return intent, invalid("invalid_outbox_intent")
	}
	return intent, nil
}

func ensureDecodeEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

type AttemptRetryAuthorization struct {
	AttemptID           string
	AttemptToken        string
	LeaseToken          string
	From                AttemptState
	ObservedBootEpoch   string
	AbsenceObservations int
	EvidenceDigest      string
	At                  time.Time
}

type RemoteNode struct {
	NodeID     string
	Path       string
	URI        string
	Content    string
	Priority   int
	Disclosure string
}

type RemoteMutation struct {
	URI      string
	MemoryID int64
}

type RemoteSearchResult struct {
	Path string
	URI  string
}

type RemoteOrphan struct {
	MemoryID   int64
	NodeID     string
	Deprecated bool
	MigratedTo int64
	Category   string
}

type RemoteDeleteResult struct {
	DeletedMemoryID int64
	ChainRepairedTo int64
}

type RemotePathReference struct {
	Namespace string `json:"namespace"`
	Domain    string `json:"domain"`
	Path      string `json:"path"`
	URI       string `json:"uri"`
	Alias     bool   `json:"alias"`
}

type RemoteBootReference struct {
	Preset    string `json:"preset"`
	Namespace string `json:"namespace"`
	URI       string `json:"uri"`
}

type RemoteReferences struct {
	NodeID            string
	Complete          bool
	ActiveMemoryID    int64
	MemoryIDs         []int64
	Paths             []RemotePathReference
	EdgeIDs           []string
	GlossaryKeywords  []string
	SearchDocumentIDs []string
	AccessLogIDs      []string
	BootURIs          []RemoteBootReference
	ReviewReferences  []string
}

func (r RemoteReferences) HasNonMemoryReferences() bool {
	return len(r.Paths) != 0 || len(r.EdgeIDs) != 0 || len(r.GlossaryKeywords) != 0 ||
		len(r.SearchDocumentIDs) != 0 || len(r.AccessLogIDs) != 0 || len(r.BootURIs) != 0 ||
		len(r.ReviewReferences) != 0
}

type NocturneCapabilities struct {
	UpstreamCommit string
	CompatRevision string
	BootEpoch      string
}

type ManagedBackup struct {
	Path              string
	CreatedAt         time.Time
	Size              int64
	SHA256            string
	LearnerGeneration int64
	WrappedKeyID      string
}

type BackupInventory struct {
	Validated      bool
	ManifestSHA256 string
	Artifacts      []ManagedBackup
}

type BackupPruneRequest struct {
	OperationID            string
	Cutoff                 time.Time
	ExpectedManifestSHA256 string
	Paths                  []string
}

type BackupPruneResult struct {
	OperationID    string
	DeletedPaths   []string
	ManifestSHA256 string
}

// NocturneRemote never accepts namespace or domain from a call site. The adapter owns both.
type NocturneRemote interface {
	Health(context.Context) error
	EnsureParent(context.Context) error
	Capabilities(context.Context) (NocturneCapabilities, error)
	GetNode(context.Context, string) (RemoteNode, error)
	CreateNode(context.Context, string, string) (RemoteMutation, error)
	UpdateNode(context.Context, string, string) (RemoteMutation, error)
	DeletePath(context.Context, string) error
	Search(context.Context, string) ([]RemoteSearchResult, error)
	ListOrphans(context.Context) ([]RemoteOrphan, error)
	OrphanDetail(context.Context, int64) (RemoteOrphan, error)
	PermanentDelete(context.Context, int64) (RemoteDeleteResult, error)
	References(context.Context, string) (RemoteReferences, error)
	ClearReviewReferences(context.Context, string) error
	Backups(context.Context) (BackupInventory, error)
	PruneBackups(context.Context, BackupPruneRequest) (BackupPruneResult, error)
}

type RemoteDeletePlan struct {
	ID                  string
	DeliveryID          string
	NodeID              string
	ExternalURI         string
	ActiveMemoryID      int64
	MemoryIDs           []int64
	Paths               []RemotePathReference
	ReviewCleanupNeeded bool
	SnapshotDigest      string
	CreatedAt           time.Time
}

func (p RemoteDeletePlan) Validate() error {
	if !validUUID(p.ID) || !validUUID(p.DeliveryID) || !validUUID(p.NodeID) || p.ActiveMemoryID <= 0 ||
		p.ExternalURI == "" || !validHash(p.SnapshotDigest) || p.CreatedAt.IsZero() || !isUTC(p.CreatedAt) ||
		len(p.MemoryIDs) == 0 || len(p.Paths) == 0 {
		return invalid("invalid_remote_delete_plan")
	}
	activeFound := false
	seenMemory := make(map[int64]struct{}, len(p.MemoryIDs))
	for _, id := range p.MemoryIDs {
		if id <= 0 {
			return invalid("invalid_remote_delete_memory_id")
		}
		if _, exists := seenMemory[id]; exists {
			return invalid("duplicate_remote_delete_memory_id")
		}
		seenMemory[id] = struct{}{}
		activeFound = activeFound || id == p.ActiveMemoryID
	}
	if !activeFound {
		return invalid("active_remote_memory_not_enumerated")
	}
	seenPath := make(map[string]struct{}, len(p.Paths))
	for _, ref := range p.Paths {
		if strings.TrimSpace(ref.Namespace) == "" || strings.TrimSpace(ref.Domain) == "" ||
			strings.TrimSpace(ref.Path) == "" || strings.TrimSpace(ref.URI) == "" {
			return invalid("invalid_remote_delete_path")
		}
		key := ref.Namespace + "\x00" + ref.Domain + "\x00" + ref.Path
		if _, exists := seenPath[key]; exists {
			return invalid("duplicate_remote_delete_path")
		}
		seenPath[key] = struct{}{}
	}
	return nil
}

func RemoteDeleteSnapshotDigest(references RemoteReferences) (string, error) {
	copyValue := references
	copyValue.MemoryIDs = append([]int64(nil), references.MemoryIDs...)
	copyValue.Paths = append([]RemotePathReference(nil), references.Paths...)
	copyValue.EdgeIDs = append([]string(nil), references.EdgeIDs...)
	copyValue.GlossaryKeywords = append([]string(nil), references.GlossaryKeywords...)
	copyValue.SearchDocumentIDs = append([]string(nil), references.SearchDocumentIDs...)
	copyValue.AccessLogIDs = append([]string(nil), references.AccessLogIDs...)
	copyValue.BootURIs = append([]RemoteBootReference(nil), references.BootURIs...)
	copyValue.ReviewReferences = append([]string(nil), references.ReviewReferences...)
	sort.Slice(copyValue.MemoryIDs, func(i, j int) bool { return copyValue.MemoryIDs[i] < copyValue.MemoryIDs[j] })
	sort.Slice(copyValue.Paths, func(i, j int) bool {
		left := copyValue.Paths[i].Namespace + "\x00" + copyValue.Paths[i].Domain + "\x00" + copyValue.Paths[i].Path
		right := copyValue.Paths[j].Namespace + "\x00" + copyValue.Paths[j].Domain + "\x00" + copyValue.Paths[j].Path
		return left < right
	})
	sort.Strings(copyValue.EdgeIDs)
	sort.Strings(copyValue.GlossaryKeywords)
	sort.Strings(copyValue.SearchDocumentIDs)
	sort.Strings(copyValue.AccessLogIDs)
	sort.Slice(copyValue.BootURIs, func(i, j int) bool {
		left := copyValue.BootURIs[i].Preset + "\x00" + copyValue.BootURIs[i].Namespace + "\x00" + copyValue.BootURIs[i].URI
		right := copyValue.BootURIs[j].Preset + "\x00" + copyValue.BootURIs[j].Namespace + "\x00" + copyValue.BootURIs[j].URI
		return left < right
	})
	sort.Strings(copyValue.ReviewReferences)
	encoded, err := json.Marshal(copyValue)
	if err != nil {
		return "", fmt.Errorf("encode remote delete snapshot: %w", err)
	}
	return SHA256String(string(encoded)), nil
}

type DeliveryProtocolPersistence interface {
	LoadDeliveryWork(context.Context, OutboxIntent) (DeliveryWork, outbox.ApplyDecision, error)
	ClaimAttempt(context.Context, string, time.Time, time.Duration) (Attempt, error)
	ClaimUnknownAttempt(context.Context, time.Time, time.Duration) (Attempt, error)
	TransitionAttempt(context.Context, AttemptTransition) (Attempt, error)
	AuthorizeAttemptRetry(context.Context, AttemptRetryAuthorization) (Attempt, error)
	PermanentlyRejectDelivery(context.Context, PolicyRejection) error
	FinalizeAttempt(context.Context, AttemptOutcome) (Attempt, error)
	FenceDelivery(context.Context, string, string, time.Time) error
	ClaimExpiryReconciliation(context.Context, time.Time, time.Duration) (ExpiryReconciliation, error)
	TransitionExpiryReconciliation(context.Context, ReconciliationTransition) (ExpiryReconciliation, error)
	FinalizeExpiryReconciliation(context.Context, ReconciliationFinalization) (ExpiryReconciliation, error)
	SaveRemoteDeletePlan(context.Context, RemoteDeletePlan) (RemoteDeletePlan, error)
	LoadRemoteDeletePlan(context.Context, string) (RemoteDeletePlan, error)
}

type MaintenanceReconciliationSummary struct {
	Pending   int64
	Conflicts int64
}

// MaintenanceReconciliationPersistence is the only port allowed to advance old-generation
// remote cleanup while the business write gate is closed. Every call is bound to the active
// erasure, its current receipt, and the target learner generation.
type MaintenanceReconciliationPersistence interface {
	ClaimMaintenanceExpiryReconciliation(context.Context, MaintenanceAuthorization, time.Time, time.Duration) (ExpiryReconciliation, error)
	TransitionMaintenanceExpiryReconciliation(context.Context, MaintenanceAuthorization, ReconciliationTransition) (ExpiryReconciliation, error)
	FinalizeMaintenanceExpiryReconciliation(context.Context, MaintenanceAuthorization, ReconciliationFinalization) (ExpiryReconciliation, error)
	SaveMaintenanceRemoteDeletePlan(context.Context, MaintenanceAuthorization, RemoteDeletePlan) (RemoteDeletePlan, error)
	MaintenanceReconciliationSummary(context.Context, MaintenanceAuthorization) (MaintenanceReconciliationSummary, error)
}
