package notesync

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/google/uuid"
	internaldiff "github.com/rogpeppe/go-internal/diff"
)

const (
	PreviewCategoryInSync                = "in_sync"
	PreviewCategoryRemoteUnchanged       = "remote_unchanged"
	PreviewCategoryLocalChanged          = "local_changed"
	PreviewCategoryRemoteChanged         = "remote_changed"
	PreviewCategoryBothChanged           = "both_changed"
	PreviewCategoryRemoteMissing         = "remote_missing"
	PreviewCategoryRemoteMoved           = "remote_moved"
	PreviewCategoryUnbasedRemote         = "unbased_remote"
	PreviewCategoryPathOccupied          = "path_occupied"
	PreviewCategoryInvalidRemoteMarkdown = "invalid_remote_markdown"

	ReviewReasonLocalRevisionChanged  = "local_revision_changed"
	ReviewReasonBothSidesChanged      = "both_sides_changed"
	ReviewReasonRemoteIdentityMoved   = "remote_identity_moved"
	ReviewReasonUnmanagedRemoteNote   = "unmanaged_remote_note"
	ReviewReasonRemoteMarkdownInvalid = "remote_markdown_invalid"

	ResolutionAcceptRemote  = "accept_remote"
	ResolutionKeepCanonical = "keep_canonical"
	ResolutionMerged        = "merged"

	KnowledgeImportSource = "obsidian-fast-note-sync"

	ReviewStatusOpen     = "open"
	ReviewStatusResolved = "resolved"
	ReviewStatusClosed   = "closed"

	CodeReviewInvalidRequest      = "invalid_request"
	CodeReviewNotFound            = "not_found"
	CodeReviewStale               = "stale_notesync_review"
	CodeReviewIdempotencyConflict = "idempotency_conflict"
	CodeReviewUnavailable         = "notesync_unavailable"
	CodeReviewContentRedacted     = "content_redacted"

	DefaultPreviewPageSize = 25
	MaxPreviewPageSize     = 25
	maxScanPageSize        = 100
	maxScanPages           = 1000
	DefaultReviewPageSize  = 25
	MaxReviewPageSize      = 25
	maxDiffLines           = 10000
	maxDiffInputBytes      = knowledge.MaxDocumentBytes
	maxDiffBytes           = 256 << 10
)

var notesyncImportOperationNamespace = uuid.MustParse("843233f0-c504-5fdc-9693-9609e8d3ba71")

type ReviewError struct {
	Code  string
	Cause error
}

func (e *ReviewError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("notesync review operation failed: code=%s: %v", e.Code, e.Cause)
	}
	return "notesync review operation failed: code=" + e.Code
}

func (e *ReviewError) Unwrap() error { return e.Cause }

func ReviewErrorCode(err error) string {
	var target *ReviewError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

type ReviewSnapshot struct {
	Missing             bool   `json:"missing"`
	KnowledgeRevisionID string `json:"knowledge_revision_id,omitempty"`
	KnowledgeRevisionNo int64  `json:"knowledge_revision_no,omitempty"`
	DocumentRevisionID  string `json:"document_revision_id,omitempty"`
	SourceRevisionID    string `json:"source_revision_id,omitempty"`
	Path                string `json:"path,omitempty"`
	Markdown            string `json:"markdown"`
	SHA256              string `json:"sha256,omitempty"`
	RemoteVersion       int64  `json:"remote_version"`
	RemoteLastTime      int64  `json:"remote_last_time"`
}

type ReviewSnapshotSummary struct {
	Missing             bool   `json:"missing"`
	KnowledgeRevisionID string `json:"knowledge_revision_id,omitempty"`
	KnowledgeRevisionNo int64  `json:"knowledge_revision_no,omitempty"`
	DocumentRevisionID  string `json:"document_revision_id,omitempty"`
	SourceRevisionID    string `json:"source_revision_id,omitempty"`
	Path                string `json:"path,omitempty"`
	SHA256              string `json:"sha256,omitempty"`
	RemoteVersion       int64  `json:"remote_version"`
	RemoteLastTime      int64  `json:"remote_last_time"`
}

type ThreeWayDiff struct {
	BaseToLocal     string `json:"base_to_local"`
	BaseToRemote    string `json:"base_to_remote"`
	LocalTruncated  bool   `json:"local_truncated"`
	RemoteTruncated bool   `json:"remote_truncated"`
}

type Review struct {
	ReviewID                    string         `json:"review_id"`
	Category                    string         `json:"category"`
	ReasonCode                  string         `json:"reason_code"`
	Status                      string         `json:"status"`
	BasisHash                   string         `json:"basis_hash"`
	Generation                  int64          `json:"generation"`
	HeadRevisionID              string         `json:"head_revision_id"`
	HeadRevisionNo              int64          `json:"head_revision_no"`
	DocumentID                  string         `json:"document_id,omitempty"`
	RemoteDocumentID            string         `json:"remote_document_id,omitempty"`
	CanonicalPath               string         `json:"canonical_path"`
	RemoteVault                 string         `json:"remote_vault"`
	RemotePath                  string         `json:"remote_path"`
	Base                        ReviewSnapshot `json:"base"`
	Local                       ReviewSnapshot `json:"local"`
	Remote                      ReviewSnapshot `json:"remote"`
	Diff                        ThreeWayDiff   `json:"diff"`
	ResolutionKind              string         `json:"resolution_kind,omitempty"`
	ResolutionOperationID       string         `json:"resolution_operation_id,omitempty"`
	ResolvedByDeviceID          string         `json:"resolved_by_device_id,omitempty"`
	ResolvedKnowledgeRevisionID string         `json:"resolved_knowledge_revision_id,omitempty"`
	ResolvedDocumentID          string         `json:"resolved_document_id,omitempty"`
	ResolvedDocumentRevisionID  string         `json:"resolved_document_revision_id,omitempty"`
	CreatedAt                   time.Time      `json:"created_at"`
	UpdatedAt                   time.Time      `json:"updated_at"`
	ResolvedAt                  *time.Time     `json:"resolved_at,omitempty"`
}

type ReviewSummary struct {
	ReviewID                    string                `json:"review_id"`
	Category                    string                `json:"category"`
	ReasonCode                  string                `json:"reason_code"`
	Status                      string                `json:"status"`
	BasisHash                   string                `json:"basis_hash"`
	Generation                  int64                 `json:"generation"`
	HeadRevisionID              string                `json:"head_revision_id"`
	HeadRevisionNo              int64                 `json:"head_revision_no"`
	DocumentID                  string                `json:"document_id,omitempty"`
	RemoteDocumentID            string                `json:"remote_document_id,omitempty"`
	CanonicalPath               string                `json:"canonical_path"`
	RemoteVault                 string                `json:"remote_vault"`
	RemotePath                  string                `json:"remote_path"`
	Base                        ReviewSnapshotSummary `json:"base"`
	Local                       ReviewSnapshotSummary `json:"local"`
	Remote                      ReviewSnapshotSummary `json:"remote"`
	ResolutionKind              string                `json:"resolution_kind,omitempty"`
	ResolutionOperationID       string                `json:"resolution_operation_id,omitempty"`
	ResolvedByDeviceID          string                `json:"resolved_by_device_id,omitempty"`
	ResolvedKnowledgeRevisionID string                `json:"resolved_knowledge_revision_id,omitempty"`
	ResolvedDocumentID          string                `json:"resolved_document_id,omitempty"`
	ResolvedDocumentRevisionID  string                `json:"resolved_document_revision_id,omitempty"`
	CreatedAt                   time.Time             `json:"created_at"`
	UpdatedAt                   time.Time             `json:"updated_at"`
	ResolvedAt                  *time.Time            `json:"resolved_at,omitempty"`
}

type ReviewStatus struct {
	Configured              bool   `json:"configured"`
	Compatible              bool   `json:"compatible"`
	Reason                  string `json:"reason"`
	Version                 string `json:"version,omitempty"`
	Vault                   string `json:"vault,omitempty"`
	PathPrefix              string `json:"path_prefix,omitempty"`
	ExternalCleanupRequired bool   `json:"external_cleanup_required"`
}

type PreviewState struct {
	Generation     int64
	HeadRevisionID string
	HeadRevisionNo int64
	DocumentID     string
	CanonicalPath  string
	Mapping        *PublicationMapping
	IdentityMoved  bool
	PathOccupied   bool
	Local          ReviewSnapshot
}

type PreviewCommand struct {
	Path     string
	Page     int
	PageSize int
}

type PreviewItem struct {
	Category   string                `json:"category"`
	ReasonCode string                `json:"reason_code"`
	ReviewID   string                `json:"review_id,omitempty"`
	BasisHash  string                `json:"basis_hash"`
	DocumentID string                `json:"document_id,omitempty"`
	RemotePath string                `json:"remote_path"`
	Base       ReviewSnapshotSummary `json:"base"`
	Local      ReviewSnapshotSummary `json:"local"`
	Remote     ReviewSnapshotSummary `json:"remote"`
	Diff       ThreeWayDiff          `json:"diff"`
}

type PreviewResult struct {
	Items     []PreviewItem `json:"items"`
	Page      int           `json:"page"`
	PageSize  int           `json:"page_size"`
	NextPage  int           `json:"next_page,omitempty"`
	TotalRows int           `json:"total_rows"`
}

type ReviewListCommand struct {
	Status string
	Cursor string
	Limit  int
}

type ReviewPage struct {
	Items      []ReviewSummary `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type ResolutionCommand struct {
	ReviewID       string
	BasisHash      string
	OperationID    string
	DeviceID       string
	Kind           string
	MergedMarkdown string

	IdentityReviewBasisHash   string
	IdentityReviewOperationID string
	IdentityReviewReceipt     string
	DocumentResolutions       []knowledge.DocumentResolution
	NodeResolutions           []knowledge.NodeResolution
}

type ResolutionResult struct {
	ReviewID            string `json:"review_id"`
	ResolutionKind      string `json:"resolution_kind"`
	KnowledgeRevisionID string `json:"knowledge_revision_id,omitempty"`
	DocumentID          string `json:"document_id,omitempty"`
	DocumentRevisionID  string `json:"document_revision_id,omitempty"`
	Unchanged           bool   `json:"unchanged"`
	Replayed            bool   `json:"replayed,omitempty"`
}

type ResolutionOperationRecord struct {
	RequestHash string
	Result      ResolutionResult
}

type KeepResolutionRequest struct {
	ReviewID       string
	BasisHash      string
	OperationID    string
	DeviceID       string
	RequestHash    string
	ObservedRemote ReviewSnapshot
	ResolvedAt     time.Time
}

type ReviewStore interface {
	LoadNotesyncPreviewState(context.Context, string, string, string, string) (PreviewState, error)
	SaveNotesyncReview(context.Context, Review) (Review, error)
	ListNotesyncReviews(context.Context, ReviewListCommand) (ReviewPage, error)
	NotesyncReview(context.Context, string) (Review, error)
	LookupNotesyncResolution(context.Context, string, string) (ResolutionOperationRecord, bool, error)
	ResolveNotesyncKeep(context.Context, KeepResolutionRequest) (ResolutionResult, error)
}

type ReviewRemote interface {
	Probe(context.Context, string) Capability
	GetNote(context.Context, string, string) (Note, error)
	ListNotes(context.Context, string, int, int) (NotePage, error)
}

type KnowledgeImporter interface {
	Import(context.Context, knowledge.ImportCommand) (knowledge.ImportResult, error)
}

type ReviewServiceOptions struct {
	Store         ReviewStore
	Remote        ReviewRemote
	Importer      KnowledgeImporter
	Canonicalizer *knowledge.Canonicalizer
	Vault         string
	PathPrefix    string
	ScanPageSize  int
	ScanMaxPages  int
	NewUUID       func() string
	Now           func() time.Time
}

type ReviewService struct {
	store         ReviewStore
	remote        ReviewRemote
	importer      KnowledgeImporter
	canonicalizer *knowledge.Canonicalizer
	vault         string
	pathPrefix    string
	scanPageSize  int
	scanMaxPages  int
	newUUID       func() string
	now           func() time.Time
}

func NewReviewService(options ReviewServiceOptions) (*ReviewService, error) {
	if options.Store == nil || options.Remote == nil || options.Importer == nil || options.Canonicalizer == nil ||
		!validRemoteVault(options.Vault) || !validManagedPath(options.PathPrefix) ||
		options.ScanPageSize < 1 || options.ScanPageSize > maxScanPageSize ||
		options.ScanMaxPages < 1 || options.ScanMaxPages > maxScanPages {
		return nil, errors.New("valid NoteSync review store, remote, importer, canonicalizer, vault, path prefix, and scan bounds are required")
	}
	if options.NewUUID == nil {
		options.NewUUID = uuid.NewString
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &ReviewService{
		store: options.Store, remote: options.Remote, importer: options.Importer,
		canonicalizer: options.Canonicalizer, vault: options.Vault, pathPrefix: options.PathPrefix,
		scanPageSize: options.ScanPageSize, scanMaxPages: options.ScanMaxPages,
		newUUID: options.NewUUID, now: options.Now,
	}, nil
}

type previewRemoteNote struct {
	note    Note
	missing bool
}

func (s *ReviewService) Status(ctx context.Context) ReviewStatus {
	capability := s.remote.Probe(ctx, s.vault)
	return ReviewStatus{
		Configured: true, Compatible: capability.Compatible, Reason: capability.Reason,
		Version: capability.Version, Vault: s.vault, PathPrefix: s.pathPrefix,
		ExternalCleanupRequired: true,
	}
}

func (s *ReviewService) Preview(ctx context.Context, command PreviewCommand) (PreviewResult, error) {
	page, pageSize := command.Page, command.PageSize
	if page == 0 {
		page = 1
	}
	maximumPageSize := MaxPreviewPageSize
	if s.scanPageSize < maximumPageSize {
		maximumPageSize = s.scanPageSize
	}
	if pageSize == 0 {
		pageSize = DefaultPreviewPageSize
		if pageSize > maximumPageSize {
			pageSize = maximumPageSize
		}
	}
	if page < 1 || page > s.scanMaxPages || pageSize < 1 || pageSize > maximumPageSize {
		return PreviewResult{}, &ReviewError{Code: CodeReviewInvalidRequest}
	}
	capability := s.remote.Probe(ctx, s.vault)
	if !capability.Compatible {
		return PreviewResult{}, &ReviewError{Code: CodeReviewUnavailable}
	}

	var notes []previewRemoteNote
	totalRows := 0
	nextPage := 0
	requestedPath := strings.TrimSpace(command.Path)
	if requestedPath != "" {
		if page != 1 || !s.managesPath(requestedPath) {
			return PreviewResult{}, &ReviewError{Code: CodeReviewInvalidRequest}
		}
		candidate, err := s.exactRemoteNote(ctx, requestedPath)
		if err != nil {
			return PreviewResult{}, err
		}
		notes = []previewRemoteNote{candidate}
		totalRows = 1
	} else {
		remotePage, err := s.remote.ListNotes(ctx, s.vault, page, pageSize)
		if err != nil {
			return PreviewResult{}, &ReviewError{Code: CodeReviewUnavailable, Cause: err}
		}
		if remotePage.Page != page || remotePage.PageSize != pageSize || remotePage.TotalRows < 0 || len(remotePage.Notes) > pageSize {
			return PreviewResult{}, &ReviewError{Code: CodeReviewUnavailable, Cause: errors.New("remote pagination mismatch")}
		}
		totalRows = remotePage.TotalRows
		seen := make(map[string]struct{}, len(remotePage.Notes))
		for _, listed := range remotePage.Notes {
			if !s.managesPath(listed.Path) {
				continue
			}
			if _, duplicate := seen[listed.Path]; duplicate {
				return PreviewResult{}, &ReviewError{Code: CodeReviewUnavailable, Cause: errors.New("duplicate remote path")}
			}
			seen[listed.Path] = struct{}{}
			candidate, exactErr := s.exactRemoteNote(ctx, listed.Path)
			if exactErr != nil {
				return PreviewResult{}, exactErr
			}
			notes = append(notes, candidate)
		}
		if page < s.scanMaxPages && remotePage.TotalRows > 0 && page <= (remotePage.TotalRows-1)/pageSize {
			nextPage = page + 1
		}
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].note.Path < notes[j].note.Path })
	result := PreviewResult{Items: make([]PreviewItem, 0, len(notes)), Page: page, PageSize: pageSize, NextPage: nextPage, TotalRows: totalRows}
	for _, candidate := range notes {
		item, err := s.previewNote(ctx, candidate.note, candidate.missing)
		if err != nil {
			return PreviewResult{}, err
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func (s *ReviewService) exactRemoteNote(ctx context.Context, remotePath string) (previewRemoteNote, error) {
	note, err := s.remote.GetNote(ctx, s.vault, remotePath)
	if err != nil {
		if IsNotFound(err) {
			return previewRemoteNote{note: Note{Vault: s.vault, Path: remotePath}, missing: true}, nil
		}
		return previewRemoteNote{}, &ReviewError{Code: CodeReviewUnavailable, Cause: err}
	}
	if note.Vault != s.vault || note.Path != remotePath || !utf8.ValidString(note.Content) || len(note.Content) > knowledge.MaxDocumentBytes {
		return previewRemoteNote{}, &ReviewError{Code: CodeReviewUnavailable, Cause: errors.New("exact remote note response is invalid")}
	}
	return previewRemoteNote{note: note}, nil
}

func (s *ReviewService) previewNote(ctx context.Context, note Note, missing bool) (PreviewItem, error) {
	canonicalPath, err := s.canonicalPath(note.Path)
	if err != nil {
		return PreviewItem{}, err
	}
	remote := ReviewSnapshot{Missing: missing, Path: note.Path}
	remoteDocumentID := ""
	invalidRemote := false
	if !remote.Missing {
		remote.Markdown = note.Content
		remote.SHA256 = markdownSHA(note.Content)
		remote.RemoteVersion = note.Version
		remote.RemoteLastTime = note.LastTime
		inspected, inspectErr := s.canonicalizer.Inspect(note.Content)
		if inspectErr != nil || inspected.ExplicitDocumentID == "" || inspected.ExplicitRootNodeID == "" || inspected.ExplicitSourceRevisionID == "" {
			invalidRemote = true
		} else {
			remoteDocumentID = inspected.ExplicitDocumentID
			remote.SourceRevisionID = inspected.ExplicitSourceRevisionID
		}
	}
	state, err := s.store.LoadNotesyncPreviewState(ctx, s.vault, note.Path, canonicalPath, remoteDocumentID)
	if err != nil {
		return PreviewItem{}, err
	}
	if state.CanonicalPath == "" {
		state.CanonicalPath = canonicalPath
	}
	base := ReviewSnapshot{Missing: true}
	if state.Mapping != nil {
		base = ReviewSnapshot{
			KnowledgeRevisionID: state.Mapping.KnowledgeRevisionID, KnowledgeRevisionNo: state.Mapping.RevisionNo,
			DocumentRevisionID: state.Mapping.DocumentRevisionID, SourceRevisionID: state.Mapping.KnowledgeRevisionID,
			Path: state.Mapping.RemotePath, Markdown: state.Mapping.BaseMarkdown, SHA256: markdownSHA(state.Mapping.BaseMarkdown),
			RemoteVersion: state.Mapping.RemoteVersion, RemoteLastTime: state.Mapping.RemoteLastTime,
		}
	}
	category := classifyPreview(invalidRemote, state.IdentityMoved, state.PathOccupied, state.Mapping == nil && !remote.Missing, base, state.Local, remote)
	reason := previewReason(category)
	diff := BuildThreeWayDiff(base, state.Local, remote)
	now := s.now().UTC().Truncate(time.Microsecond)
	review := Review{
		Category: category, ReasonCode: reason, Status: ReviewStatusOpen,
		Generation: state.Generation, HeadRevisionID: state.HeadRevisionID, HeadRevisionNo: state.HeadRevisionNo,
		DocumentID: state.DocumentID, RemoteDocumentID: remoteDocumentID, CanonicalPath: state.CanonicalPath,
		RemoteVault: s.vault, RemotePath: note.Path, Base: base, Local: state.Local, Remote: remote, Diff: diff,
		CreatedAt: now, UpdatedAt: now,
	}
	review.BasisHash = ReviewBasisHash(review)
	if actionablePreview(category) {
		review.ReviewID = s.newUUID()
		stored, saveErr := s.store.SaveNotesyncReview(ctx, review)
		if saveErr != nil {
			return PreviewItem{}, saveErr
		}
		return previewItem(stored), nil
	}
	return previewItem(review), nil
}

func (s *ReviewService) ListReviews(ctx context.Context, command ReviewListCommand) (ReviewPage, error) {
	if command.Limit == 0 {
		command.Limit = DefaultReviewPageSize
	}
	if command.Limit < 1 || command.Limit > MaxReviewPageSize {
		return ReviewPage{}, &ReviewError{Code: CodeReviewInvalidRequest}
	}
	command.Status = strings.TrimSpace(command.Status)
	if command.Status == "" {
		command.Status = ReviewStatusOpen
	}
	if command.Status != "all" && command.Status != ReviewStatusOpen && command.Status != ReviewStatusResolved && command.Status != ReviewStatusClosed {
		return ReviewPage{}, &ReviewError{Code: CodeReviewInvalidRequest}
	}
	return s.store.ListNotesyncReviews(ctx, command)
}

func (s *ReviewService) Review(ctx context.Context, reviewID string) (Review, error) {
	reviewID = strings.ToLower(strings.TrimSpace(reviewID))
	if uuid.Validate(reviewID) != nil {
		return Review{}, &ReviewError{Code: CodeReviewInvalidRequest}
	}
	return s.store.NotesyncReview(ctx, reviewID)
}

func (s *ReviewService) Resolve(ctx context.Context, command ResolutionCommand) (ResolutionResult, error) {
	command.ReviewID = strings.ToLower(strings.TrimSpace(command.ReviewID))
	command.OperationID = strings.ToLower(strings.TrimSpace(command.OperationID))
	command.DeviceID = strings.ToLower(strings.TrimSpace(command.DeviceID))
	command.BasisHash = strings.ToLower(strings.TrimSpace(command.BasisHash))
	command.Kind = strings.TrimSpace(command.Kind)
	if uuid.Validate(command.ReviewID) != nil || uuid.Validate(command.OperationID) != nil || uuid.Validate(command.DeviceID) != nil ||
		!validSHA256(command.BasisHash) || !validResolutionKind(command.Kind) {
		return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest}
	}
	var mergedIdentity knowledge.InspectedDocument
	if command.Kind == ResolutionMerged {
		if !utf8.ValidString(command.MergedMarkdown) || len(command.MergedMarkdown) > knowledge.MaxDocumentBytes {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest}
		}
		var inspectErr error
		mergedIdentity, inspectErr = s.canonicalizer.Inspect(command.MergedMarkdown)
		if inspectErr != nil || mergedIdentity.ExplicitDocumentID == "" || mergedIdentity.ExplicitRootNodeID == "" || mergedIdentity.ExplicitSourceRevisionID == "" {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest, Cause: inspectErr}
		}
	} else if command.MergedMarkdown != "" {
		return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest}
	}
	requestHash := resolutionRequestHash(command)
	if stored, exists, err := s.store.LookupNotesyncResolution(ctx, command.DeviceID, command.OperationID); err != nil {
		return ResolutionResult{}, err
	} else if exists {
		if stored.RequestHash != requestHash {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewIdempotencyConflict}
		}
		stored.Result.Replayed = true
		return stored.Result, nil
	}
	review, err := s.store.NotesyncReview(ctx, command.ReviewID)
	if err != nil {
		return ResolutionResult{}, err
	}
	if review.Status != ReviewStatusOpen || review.BasisHash != command.BasisHash {
		return ResolutionResult{}, &ReviewError{Code: CodeReviewStale}
	}
	observed, err := s.recheckRemote(ctx, review)
	if err != nil {
		return ResolutionResult{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if command.Kind == ResolutionKeepCanonical {
		if review.Local.Missing || review.DocumentID == "" {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest}
		}
		return s.store.ResolveNotesyncKeep(ctx, KeepResolutionRequest{
			ReviewID: command.ReviewID, BasisHash: command.BasisHash, OperationID: command.OperationID,
			DeviceID: command.DeviceID, RequestHash: requestHash, ObservedRemote: observed, ResolvedAt: now,
		})
	}

	markdown := review.Remote.Markdown
	expectedDocumentID := expectedResolutionDocumentID(review, command.Kind)
	if command.Kind == ResolutionAcceptRemote {
		if review.Remote.Missing || review.Category == PreviewCategoryInvalidRemoteMarkdown {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest}
		}
		inspected, inspectErr := s.canonicalizer.Inspect(markdown)
		if inspectErr != nil || inspected.ExplicitDocumentID == "" || inspected.ExplicitRootNodeID == "" || inspected.ExplicitSourceRevisionID == "" ||
			expectedDocumentID == "" || inspected.ExplicitDocumentID != expectedDocumentID {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest, Cause: inspectErr}
		}
	} else {
		markdown = command.MergedMarkdown
		if !review.Local.Missing && markdown == review.Local.Markdown {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest, Cause: errors.New("merged Markdown equals canonical content; use keep_canonical")}
		}
		if expectedDocumentID == "" || mergedIdentity.ExplicitDocumentID != expectedDocumentID {
			return ResolutionResult{}, &ReviewError{Code: CodeReviewInvalidRequest, Cause: errors.New("merged Markdown identity does not match the review authority")}
		}
	}
	var expectedParent *string
	if review.HeadRevisionID != "" {
		value := review.HeadRevisionID
		expectedParent = &value
	}
	knowledgeOperationID := knowledgeImportOperationID(command)
	importResult, err := s.importer.Import(ctx, knowledge.ImportCommand{
		OperationID: knowledgeOperationID, ExpectedParentRevisionID: expectedParent, ExpectedParentProvided: true,
		Source: KnowledgeImportSource, Documents: []knowledge.ImportDocument{{Path: review.CanonicalPath, Markdown: markdown}},
		IdentityReviewBasisHash: command.IdentityReviewBasisHash, IdentityReviewOperationID: command.IdentityReviewOperationID,
		IdentityReviewReceipt: command.IdentityReviewReceipt, DocumentResolutions: command.DocumentResolutions,
		NodeResolutions: command.NodeResolutions, ActorDeviceID: command.DeviceID,
		NotesyncResolution: &knowledge.NotesyncImportResolution{
			ReviewID: command.ReviewID, BasisHash: command.BasisHash, DeviceID: command.DeviceID,
			OperationID: command.OperationID, RequestHash: requestHash, Kind: command.Kind,
			ObservedRemoteMissing: observed.Missing, ObservedRemoteMarkdown: observed.Markdown,
			ObservedRemoteSHA256: observed.SHA256, ObservedRemoteVersion: observed.RemoteVersion,
			ObservedRemoteLastTime: observed.RemoteLastTime,
			CanonicalPath:          review.CanonicalPath, ExpectedDocumentID: expectedDocumentID, ResolvedAt: now,
		},
	})
	if err != nil {
		return ResolutionResult{}, err
	}
	result := ResolutionResult{
		ReviewID: command.ReviewID, ResolutionKind: command.Kind, KnowledgeRevisionID: importResult.Revision.ID,
		Unchanged: importResult.Unchanged, Replayed: importResult.Replayed,
	}
	for _, document := range importResult.Revision.Documents {
		if document.Path == review.CanonicalPath {
			result.DocumentID = document.Revision.DocumentID
			result.DocumentRevisionID = document.Revision.ID
			break
		}
	}
	if result.DocumentRevisionID == "" {
		return ResolutionResult{}, errors.New("notesync resolution result lacks canonical document")
	}
	return result, nil
}

func (s *ReviewService) recheckRemote(ctx context.Context, review Review) (ReviewSnapshot, error) {
	note, err := s.remote.GetNote(ctx, review.RemoteVault, review.RemotePath)
	if err != nil {
		if IsNotFound(err) && review.Remote.Missing {
			return ReviewSnapshot{Missing: true, Path: review.RemotePath}, nil
		}
		if IsNotFound(err) {
			return ReviewSnapshot{}, &ReviewError{Code: CodeReviewStale}
		}
		return ReviewSnapshot{}, &ReviewError{Code: CodeReviewUnavailable, Cause: err}
	}
	observed := ReviewSnapshot{
		Path: note.Path, Markdown: note.Content, SHA256: markdownSHA(note.Content),
		RemoteVersion: note.Version, RemoteLastTime: note.LastTime,
	}
	if review.Remote.Missing || observed.SHA256 != review.Remote.SHA256 || observed.Markdown != review.Remote.Markdown ||
		observed.RemoteVersion != review.Remote.RemoteVersion || observed.RemoteLastTime != review.Remote.RemoteLastTime {
		return ReviewSnapshot{}, &ReviewError{Code: CodeReviewStale}
	}
	return observed, nil
}

func (s *ReviewService) managesPath(value string) bool {
	return validManagedPath(value) && strings.HasPrefix(value, s.pathPrefix+"/") && value != s.pathPrefix
}

func (s *ReviewService) canonicalPath(remotePath string) (string, error) {
	if !s.managesPath(remotePath) {
		return "", &ReviewError{Code: CodeReviewInvalidRequest}
	}
	value := strings.TrimPrefix(remotePath, s.pathPrefix+"/")
	normalized, err := knowledge.NormalizePath(value)
	if err != nil {
		return "", &ReviewError{Code: CodeReviewInvalidRequest, Cause: err}
	}
	return normalized, nil
}

func classifyPreview(invalidRemote, moved, occupied, unbased bool, base, local, remote ReviewSnapshot) string {
	switch {
	case invalidRemote:
		return PreviewCategoryInvalidRemoteMarkdown
	case moved:
		return PreviewCategoryRemoteMoved
	case occupied:
		return PreviewCategoryPathOccupied
	case unbased:
		return PreviewCategoryUnbasedRemote
	case remote.Missing:
		return PreviewCategoryRemoteMissing
	case !local.Missing && local.SHA256 == remote.SHA256:
		return PreviewCategoryInSync
	case remote.SHA256 == base.SHA256 && local.Missing:
		return PreviewCategoryRemoteUnchanged
	case remote.SHA256 == base.SHA256:
		return PreviewCategoryLocalChanged
	case !local.Missing && local.SHA256 == base.SHA256:
		return PreviewCategoryRemoteChanged
	default:
		return PreviewCategoryBothChanged
	}
}

func previewReason(category string) string {
	switch category {
	case PreviewCategoryInvalidRemoteMarkdown:
		return ReviewReasonRemoteMarkdownInvalid
	case PreviewCategoryRemoteMoved:
		return ReviewReasonRemoteIdentityMoved
	case PreviewCategoryPathOccupied:
		return ReviewReasonRemotePathOccupied
	case PreviewCategoryUnbasedRemote:
		return ReviewReasonUnmanagedRemoteNote
	case PreviewCategoryRemoteMissing:
		return ReviewReasonRemoteNoteMissing
	case PreviewCategoryRemoteChanged:
		return ReviewReasonRemoteContentChanged
	case PreviewCategoryBothChanged:
		return ReviewReasonBothSidesChanged
	case PreviewCategoryLocalChanged, PreviewCategoryRemoteUnchanged:
		return ReviewReasonLocalRevisionChanged
	default:
		return category
	}
}

func actionablePreview(category string) bool {
	switch category {
	case PreviewCategoryInvalidRemoteMarkdown, PreviewCategoryRemoteMoved, PreviewCategoryPathOccupied,
		PreviewCategoryUnbasedRemote, PreviewCategoryRemoteMissing, PreviewCategoryRemoteChanged, PreviewCategoryBothChanged:
		return true
	default:
		return false
	}
}

func ReviewBasisHash(review Review) string {
	value := struct {
		Category, Reason, HeadRevisionID, DocumentID, RemoteDocumentID, CanonicalPath string
		RemoteVault, RemotePath                                                       string
		Generation, HeadRevisionNo, BaseKnowledgeRevisionNo, LocalKnowledgeRevisionNo int64
		BaseMissing, LocalMissing, RemoteMissing                                      bool
		BaseKnowledgeRevisionID, BaseDocumentRevisionID, BasePath, BaseSHA            string
		BaseRemoteVersion, BaseRemoteLastTime                                         int64
		LocalKnowledgeRevisionID, LocalDocumentRevisionID, LocalPath, LocalSHA        string
		RemoteSourceRevisionID, RemoteSHA                                             string
		RemoteVersion, RemoteLastTime                                                 int64
	}{
		Category: review.Category, Reason: review.ReasonCode, HeadRevisionID: review.HeadRevisionID,
		DocumentID: review.DocumentID, RemoteDocumentID: review.RemoteDocumentID, CanonicalPath: review.CanonicalPath,
		RemoteVault: review.RemoteVault, RemotePath: review.RemotePath, Generation: review.Generation,
		HeadRevisionNo: review.HeadRevisionNo, BaseKnowledgeRevisionNo: review.Base.KnowledgeRevisionNo,
		LocalKnowledgeRevisionNo: review.Local.KnowledgeRevisionNo,
		BaseMissing:              review.Base.Missing, LocalMissing: review.Local.Missing,
		RemoteMissing: review.Remote.Missing, BaseKnowledgeRevisionID: review.Base.KnowledgeRevisionID,
		BaseDocumentRevisionID: review.Base.DocumentRevisionID, BasePath: review.Base.Path,
		BaseSHA: review.Base.SHA256, BaseRemoteVersion: review.Base.RemoteVersion,
		BaseRemoteLastTime:       review.Base.RemoteLastTime,
		LocalKnowledgeRevisionID: review.Local.KnowledgeRevisionID, LocalDocumentRevisionID: review.Local.DocumentRevisionID,
		LocalPath: review.Local.Path, LocalSHA: review.Local.SHA256, RemoteSourceRevisionID: review.Remote.SourceRevisionID,
		RemoteSHA: review.Remote.SHA256, RemoteVersion: review.Remote.RemoteVersion, RemoteLastTime: review.Remote.RemoteLastTime,
	}
	encoded, _ := json.Marshal(value)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func previewItem(review Review) PreviewItem {
	return PreviewItem{
		Category: review.Category, ReasonCode: review.ReasonCode, ReviewID: review.ReviewID,
		BasisHash: review.BasisHash, DocumentID: review.DocumentID, RemotePath: review.RemotePath,
		Base: summarizeSnapshot(review.Base), Local: summarizeSnapshot(review.Local),
		Remote: summarizeSnapshot(review.Remote), Diff: review.Diff,
	}
}

func SummarizeReview(review Review) ReviewSummary {
	return ReviewSummary{
		ReviewID: review.ReviewID, Category: review.Category, ReasonCode: review.ReasonCode,
		Status: review.Status, BasisHash: review.BasisHash, Generation: review.Generation,
		HeadRevisionID: review.HeadRevisionID, HeadRevisionNo: review.HeadRevisionNo,
		DocumentID: review.DocumentID, RemoteDocumentID: review.RemoteDocumentID,
		CanonicalPath: review.CanonicalPath, RemoteVault: review.RemoteVault, RemotePath: review.RemotePath,
		Base: summarizeSnapshot(review.Base), Local: summarizeSnapshot(review.Local), Remote: summarizeSnapshot(review.Remote),
		ResolutionKind: review.ResolutionKind, ResolutionOperationID: review.ResolutionOperationID,
		ResolvedByDeviceID: review.ResolvedByDeviceID, ResolvedKnowledgeRevisionID: review.ResolvedKnowledgeRevisionID,
		ResolvedDocumentID: review.ResolvedDocumentID, ResolvedDocumentRevisionID: review.ResolvedDocumentRevisionID,
		CreatedAt: review.CreatedAt, UpdatedAt: review.UpdatedAt, ResolvedAt: review.ResolvedAt,
	}
}

func summarizeSnapshot(snapshot ReviewSnapshot) ReviewSnapshotSummary {
	return ReviewSnapshotSummary{
		Missing: snapshot.Missing, KnowledgeRevisionID: snapshot.KnowledgeRevisionID,
		KnowledgeRevisionNo: snapshot.KnowledgeRevisionNo, DocumentRevisionID: snapshot.DocumentRevisionID,
		SourceRevisionID: snapshot.SourceRevisionID, Path: snapshot.Path, SHA256: snapshot.SHA256,
		RemoteVersion: snapshot.RemoteVersion, RemoteLastTime: snapshot.RemoteLastTime,
	}
}

func BuildThreeWayDiff(base, local, remote ReviewSnapshot) ThreeWayDiff {
	baseMarkdown := ""
	if !base.Missing {
		baseMarkdown = base.Markdown
	}
	localMarkdown := ""
	if !local.Missing {
		localMarkdown = local.Markdown
	}
	remoteMarkdown := ""
	if !remote.Missing {
		remoteMarkdown = remote.Markdown
	}
	localDiff, localTruncated := unifiedDiff("base", baseMarkdown, "local", localMarkdown)
	remoteDiff, remoteTruncated := unifiedDiff("base", baseMarkdown, "remote", remoteMarkdown)
	return ThreeWayDiff{BaseToLocal: localDiff, BaseToRemote: remoteDiff, LocalTruncated: localTruncated, RemoteTruncated: remoteTruncated}
}

func unifiedDiff(fromName, from, toName, to string) (string, bool) {
	if from == to {
		return "", false
	}
	fromLines := strings.Count(from, "\n") + 1
	toLines := strings.Count(to, "\n") + 1
	if len(from) > maxDiffInputBytes || len(to) > maxDiffInputBytes || fromLines > maxDiffLines || toLines > maxDiffLines {
		return fmt.Sprintf("--- %s\n+++ %s\n@@ diff omitted: input limit exceeded @@\n", fromName, toName), true
	}
	value := string(internaldiff.Diff(fromName, []byte(from), toName, []byte(to)))
	return truncateDiff(value)
}

func truncateDiff(value string) (string, bool) {
	if len(value) <= maxDiffBytes {
		return value, false
	}
	const suffix = "\n@@ diff truncated @@\n"
	limit := maxDiffBytes - len(suffix)
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + suffix, true
}

func resolutionRequestHash(command ResolutionCommand) string {
	value := struct {
		ReviewID, BasisHash, DeviceID, Kind, MergedSHA    string
		IdentityBasis, IdentityOperation, IdentityReceipt string
		Document                                          []knowledge.DocumentResolution
		Node                                              []knowledge.NodeResolution
	}{
		ReviewID: command.ReviewID, BasisHash: command.BasisHash, DeviceID: command.DeviceID, Kind: command.Kind,
		MergedSHA: markdownSHA(command.MergedMarkdown), IdentityBasis: command.IdentityReviewBasisHash,
		IdentityOperation: command.IdentityReviewOperationID, IdentityReceipt: command.IdentityReviewReceipt,
		Document: append([]knowledge.DocumentResolution(nil), command.DocumentResolutions...),
		Node:     append([]knowledge.NodeResolution(nil), command.NodeResolutions...),
	}
	encoded, _ := json.Marshal(value)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func knowledgeImportOperationID(command ResolutionCommand) string {
	identity := command.DeviceID + "\n" + command.OperationID + "\n" + command.ReviewID
	if command.IdentityReviewOperationID != "" {
		identity += "\nidentity-review\n" + command.IdentityReviewOperationID + "\n" + command.IdentityReviewReceipt
	}
	return uuid.NewSHA1(notesyncImportOperationNamespace, []byte(identity)).String()
}

func expectedResolutionDocumentID(review Review, kind string) string {
	if kind == ResolutionMerged && !review.Local.Missing && review.DocumentID != "" {
		return review.DocumentID
	}
	if review.RemoteDocumentID != "" {
		return review.RemoteDocumentID
	}
	return review.DocumentID
}

func validResolutionKind(value string) bool {
	return value == ResolutionAcceptRemote || value == ResolutionKeepCanonical || value == ResolutionMerged
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func markdownSHA(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

type reviewCursorWire struct {
	Version    int    `json:"v"`
	Generation int64  `json:"generation"`
	CreatedAt  string `json:"created_at"`
	ReviewID   string `json:"review_id"`
}

func EncodeReviewCursor(generation int64, createdAt time.Time, reviewID string) string {
	wire, _ := json.Marshal(reviewCursorWire{Version: 1, Generation: generation, CreatedAt: createdAt.UTC().Format(time.RFC3339Nano), ReviewID: reviewID})
	return base64.RawURLEncoding.EncodeToString(wire)
}

func DecodeReviewCursor(value string, generation int64) (time.Time, string, error) {
	if value == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, "", &ReviewError{Code: CodeReviewStale}
	}
	var wire reviewCursorWire
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Version != 1 || wire.Generation != generation || uuid.Validate(wire.ReviewID) != nil {
		return time.Time{}, "", &ReviewError{Code: CodeReviewStale}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return time.Time{}, "", &ReviewError{Code: CodeReviewStale}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, wire.CreatedAt)
	if err != nil || createdAt.Location() != time.UTC {
		return time.Time{}, "", &ReviewError{Code: CodeReviewStale}
	}
	return createdAt, wire.ReviewID, nil
}
