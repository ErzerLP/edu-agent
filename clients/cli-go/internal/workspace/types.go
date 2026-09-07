package workspace

import (
	"context"
	"sync"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	ToolStat           = "stat"
	ToolFind           = "find"
	ToolList           = "list"
	ToolRead           = "read"
	ToolSearch         = "search"
	ToolWrite          = "write"
	ToolEdit           = "edit"
	ToolMove           = "move"
	ToolCopy           = "copy"
	ToolMkdir          = "mkdir"
	ToolArchive        = "archive"
	ToolPatch          = "apply_patch"
	ToolRestoreArchive = "restore_archive"
	ToolPurgeArchive   = "purge_archive"
)

const (
	DefaultReadFileBytes int64 = 64 << 20
	DefaultEditFileBytes int64 = 64 << 20
	DefaultDiffBytes     int64 = 128 << 20
	DefaultPatchBytes    int64 = 64 << 20
	DefaultCopyBytes     int64 = 1 << 30
	DefaultCopyPlanBytes int64 = 64 << 20
	DefaultCopyEntries         = 100000
	MaxPatchFiles              = 16
)

type Limits struct {
	ListEntries          int
	DirectoryScanEntries int
	ResultBytes          int
	ReadLines            int
	FileBytes            int64
	ReadFileBytes        int64
	EditFileBytes        int64
	DiffBytes            int64
	PatchBytes           int64
	CopyBytes            int64
	CopyPlanBytes        int64
	CopyEntries          int
	QueryMemoryBytes     int64
	QueryEntries         int
	QueryRecords         int
	SearchMatches        int
	SearchFiles          int
	SearchBytes          int64
	SearchDepth          int
	SearchPreviewBytes   int
	SearchEntries        int
	MutationPreviewBytes int
	EditReplacements     int
}

func DefaultLimits() Limits {
	return Limits{
		ListEntries: 200, DirectoryScanEntries: 2000, ResultBytes: 6 << 10,
		ReadLines: 200, FileBytes: 1 << 20, ReadFileBytes: DefaultReadFileBytes, EditFileBytes: DefaultEditFileBytes,
		QueryMemoryBytes: DefaultQueryMemoryBytes, QueryEntries: DefaultQueryEntries, QueryRecords: DefaultQueryRecords,
		CopyBytes: DefaultCopyBytes, CopyPlanBytes: DefaultCopyPlanBytes, CopyEntries: DefaultCopyEntries,
		SearchMatches: 100, SearchFiles: 2000, SearchBytes: 16 << 20,
		SearchDepth: 64, SearchPreviewBytes: 512, SearchEntries: 10000,
		MutationPreviewBytes: 6 << 10, DiffBytes: DefaultDiffBytes, PatchBytes: DefaultPatchBytes, EditReplacements: 32,
	}
}

type Status struct {
	Available bool
	Label     string
	Code      string
}

type Reference struct {
	Path               string `json:"path"`
	ContentHash        string `json:"content_hash,omitempty"`
	Kind               string `json:"kind"`
	InvalidateObserved bool   `json:"invalidate_observed,omitempty"`
}

func (r *Reference) Identity() string {
	if r == nil {
		return ""
	}
	return r.Kind + "\x00" + r.Path
}

func (r *Reference) Supersedes(previous *Reference) bool {
	if previous.IsOperation() {
		return false
	}
	if metadataReferenceSupersedes(r, previous) {
		return true
	}
	if r != nil && previous != nil && r.InvalidateObserved && r.IsArchive() {
		return archiveAffectsReference(r.Path, r.Kind == "archive_directory", previous)
	}
	if r == nil || previous == nil || r.Identity() == "" || r.Identity() != previous.Identity() || previous.ContentHash == "" {
		return false
	}
	if r.InvalidateObserved {
		return r.ContentHash == "" || r.ContentHash == previous.ContentHash
	}
	return r.ContentHash != "" && r.ContentHash != previous.ContentHash
}

type PublicationOutcome string

const (
	PublicationUnchanged PublicationOutcome = "unchanged"
	PublicationCompleted PublicationOutcome = "completed"
	PublicationUnknown   PublicationOutcome = "unknown"
)

type MutationPresentation struct {
	Tool            string
	Operation       string
	Path            string
	PreviewKind     string
	Preview         string
	Truncated       bool
	BaseVersion     string
	DestinationPath string
	ArchivePath     string
	EntryKind       string
}

type PreparedMutation struct {
	Presentation MutationPresentation

	path                 string
	candidate            []byte
	candidateHash        string
	baseVersion          string
	basePermission       uint32
	fileBytes            int64 // processing budget frozen when write/edit is prepared
	create               bool
	previewHash          string
	fullDiff             string // immutable complete raw-byte diff, never a result projection
	firstChangeLine      int
	replacements         int
	archivePath          string
	movePlan             *securefile.MovePlan
	restorePlan          *securefile.RestorePlan
	purgePlan            *securefile.PurgePlan
	purgePresentation    MutationPresentation
	purgeManifest        string
	copyPlan             *securefile.CopyPlan
	copyTreePlan         *securefile.CopyTreePlan
	copyTreePresentation MutationPresentation
	copyManifest         string
	mkdirPlan            *securefile.MkdirPlan
	archiveEntry         *securefile.ArchiveEntry
	archiveContentHash   string // patch-only raw content check; baseVersion remains entry-v1
	patchItems           []*PreparedMutation
	patchPresentation    MutationPresentation
	commitMu             sync.Mutex
	committed            bool
}

// FullDiff returns the complete diff frozen at prepare time without copying it.
// Non-text operations have no diff. The text is data, not a replayable candidate.
func (p *PreparedMutation) FullDiff() string {
	if p == nil {
		return ""
	}
	return p.fullDiff
}

type Result struct {
	Effect      *fileeffects.Effect
	Value       any
	Summary     string
	Reference   *Reference
	Publication PublicationOutcome
}

type Executor interface {
	Definitions() []modelclient.Tool
	Execute(context.Context, string, string) Result
	PrepareMutation(context.Context, string, string) (*PreparedMutation, Result)
	CommitMutation(context.Context, *PreparedMutation) Result
	Status() Status
	Close() error
}

type Workspace struct {
	root       *securefile.Root
	limits     Limits
	status     Status
	queues     mutationQueues // serializes commits per normalized target path
	queriesMu  sync.Mutex
	queries    map[string]*workspaceQuery
	queryBytes int64
}
