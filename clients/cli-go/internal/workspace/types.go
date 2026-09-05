package workspace

import (
	"context"
	"sync"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	ToolList    = "list"
	ToolRead    = "read"
	ToolSearch  = "search"
	ToolWrite   = "write"
	ToolEdit    = "edit"
	ToolArchive = "archive"
)

type Limits struct {
	ListEntries          int
	DirectoryScanEntries int
	ResultBytes          int
	ReadLines            int
	FileBytes            int64
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
		ReadLines: 200, FileBytes: 1 << 20,
		SearchMatches: 100, SearchFiles: 2000, SearchBytes: 16 << 20,
		SearchDepth: 64, SearchPreviewBytes: 512, SearchEntries: 10000,
		MutationPreviewBytes: 6 << 10, EditReplacements: 32,
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
	Tool        string
	Operation   string
	Path        string
	PreviewKind string
	Preview     string
	Truncated   bool
	BaseVersion string
	ArchivePath string
	EntryKind   string
}

type PreparedMutation struct {
	Presentation MutationPresentation

	path            string
	candidate       []byte
	candidateHash   string
	baseVersion     string
	basePermission  uint32
	create          bool
	previewHash     string
	firstChangeLine int
	replacements    int
	archivePath     string
	archiveEntry    *securefile.ArchiveEntry
	commitMu        sync.Mutex
	committed       bool
}

type Result struct {
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
	root   *securefile.Root
	limits Limits
	status Status
	queues mutationQueues // serializes commits per normalized target path
}
