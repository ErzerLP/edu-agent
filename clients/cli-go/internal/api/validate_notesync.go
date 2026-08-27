package api

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxNotesyncPathBytes     = 1024
	maxNotesyncCursorBytes   = 4096
	maxNotesyncPageSize      = 25
	maxNotesyncMarkdownBytes = 4 << 20
	maxNotesyncDiffBytes     = 256 << 10
)

func ValidateNotesyncPreviewRequest(value NotesyncPreviewRequest) error {
	if !utf8.ValidString(value.Path) || len(value.Path) > maxNotesyncPathBytes || value.Page < 0 || value.PageSize < 0 || value.PageSize > maxNotesyncPageSize {
		return errors.New("notesync preview request is invalid")
	}
	if strings.TrimSpace(value.Path) != "" && value.Page > 1 {
		return errors.New("path preview only supports the first page")
	}
	return nil
}

func ValidateNotesyncReviewQuery(status, cursor string, limit int) error {
	if status != "" && status != "all" && status != "open" && status != "resolved" && status != "closed" {
		return errors.New("notesync review status is invalid")
	}
	if !utf8.ValidString(cursor) || len(cursor) > maxNotesyncCursorBytes || limit < 0 || limit > maxNotesyncPageSize {
		return errors.New("notesync review pagination is invalid")
	}
	return nil
}

func ValidateNotesyncResolutionRequest(reviewID string, value NotesyncResolutionRequest) error {
	if !validLearningUUID(reviewID) || !validSHA256(value.BasisHash) || !validLearningUUID(value.OperationID) || !validNotesyncActionResolutionKind(value.Kind) {
		return errors.New("notesync resolution identity is invalid")
	}
	if value.Kind == NotesyncResolutionMerged {
		if value.MergedMarkdown == nil || !utf8.ValidString(*value.MergedMarkdown) || len(*value.MergedMarkdown) > maxNotesyncMarkdownBytes {
			return errors.New("merged markdown is invalid")
		}
	} else if value.MergedMarkdown != nil {
		return errors.New("merged markdown is only allowed for merged resolution")
	}
	return nil
}

func ValidateNotesyncStatus(value NotesyncStatus) error {
	if !value.ExternalCleanupRequired || !validNotesyncStatusReason(value.Reason) {
		return errors.New("notesync status is invalid")
	}
	if !value.Configured {
		if value.Compatible || value.Reason != "not_configured" || value.Version != "" || value.Vault != "" || value.PathPrefix != "" {
			return errors.New("unconfigured notesync status is inconsistent")
		}
		return nil
	}
	if value.Reason == "not_configured" || value.Vault == "" || value.PathPrefix == "" || !utf8.ValidString(value.Vault) || !utf8.ValidString(value.PathPrefix) {
		return errors.New("configured notesync status is incomplete")
	}
	if value.Compatible != (value.Reason == "") {
		return errors.New("notesync compatibility status is inconsistent")
	}
	if value.Compatible && value.Version == "" {
		return errors.New("compatible notesync status has no version")
	}
	return nil
}

func ValidateNotesyncPreviewResult(value NotesyncPreviewResult) error {
	if value.Items == nil || value.Page < 1 || value.PageSize < 1 || value.PageSize > maxNotesyncPageSize || value.TotalRows < 0 || len(value.Items) > value.PageSize {
		return errors.New("notesync preview pagination is invalid")
	}
	if value.NextPage != 0 {
		if value.NextPage != value.Page+1 || value.TotalRows <= value.Page*value.PageSize {
			return errors.New("notesync preview next page is inconsistent")
		}
	} else if value.TotalRows > value.Page*value.PageSize {
		return errors.New("notesync preview next page is missing")
	}
	seen := make(map[string]struct{}, len(value.Items))
	for _, item := range value.Items {
		if err := validateNotesyncPreviewItem(item); err != nil {
			return err
		}
		if _, exists := seen[item.RemotePath]; exists {
			return errors.New("notesync preview contains a duplicate path")
		}
		seen[item.RemotePath] = struct{}{}
	}
	return nil
}

func ValidateNotesyncReviewPage(value NotesyncReviewPage) error {
	if value.Items == nil || len(value.Items) > maxNotesyncPageSize || len(value.NextCursor) > maxNotesyncCursorBytes || !utf8.ValidString(value.NextCursor) {
		return errors.New("notesync review page is invalid")
	}
	seen := make(map[string]struct{}, len(value.Items))
	for _, item := range value.Items {
		if err := validateNotesyncReviewSummary(item); err != nil {
			return err
		}
		if _, exists := seen[item.ReviewID]; exists {
			return errors.New("notesync review page contains a duplicate review")
		}
		seen[item.ReviewID] = struct{}{}
	}
	return nil
}

func ValidateNotesyncReview(value NotesyncReview) error {
	if err := validateNotesyncReviewCore(value.ReviewID, value.Category, value.ReasonCode, value.Status, value.BasisHash,
		value.Generation, value.HeadRevisionID, value.HeadRevisionNo, value.DocumentID, value.RemoteDocumentID,
		value.CanonicalPath, value.RemoteVault, value.RemotePath, value.ResolutionKind, value.ResolutionOperationID,
		value.ResolvedByDeviceID, value.ResolvedKnowledgeRevisionID, value.ResolvedDocumentID, value.ResolvedDocumentRevisionID,
		value.CreatedAt.IsZero(), value.UpdatedAt.IsZero(), value.ResolvedAt); err != nil {
		return err
	}
	if err := validateNotesyncSnapshot(value.Base); err != nil {
		return err
	}
	if err := validateNotesyncSnapshot(value.Local); err != nil {
		return err
	}
	if err := validateNotesyncSnapshot(value.Remote); err != nil {
		return err
	}
	return validateNotesyncDiff(value.Diff)
}

func validateNotesyncReviewSummary(value NotesyncReviewSummary) error {
	if err := validateNotesyncReviewCore(value.ReviewID, value.Category, value.ReasonCode, value.Status, value.BasisHash,
		value.Generation, value.HeadRevisionID, value.HeadRevisionNo, value.DocumentID, value.RemoteDocumentID,
		value.CanonicalPath, value.RemoteVault, value.RemotePath, value.ResolutionKind, value.ResolutionOperationID,
		value.ResolvedByDeviceID, value.ResolvedKnowledgeRevisionID, value.ResolvedDocumentID, value.ResolvedDocumentRevisionID,
		value.CreatedAt.IsZero(), value.UpdatedAt.IsZero(), value.ResolvedAt); err != nil {
		return err
	}
	if err := validateNotesyncSnapshotSummary(value.Base); err != nil {
		return err
	}
	if err := validateNotesyncSnapshotSummary(value.Local); err != nil {
		return err
	}
	return validateNotesyncSnapshotSummary(value.Remote)
}

func validateNotesyncReviewCore(reviewID, category, reason, status, basisHash string, generation int64,
	headRevisionID string, headRevisionNo int64, documentID, remoteDocumentID, canonicalPath, remoteVault, remotePath,
	resolutionKind, resolutionOperationID, resolvedByDeviceID, resolvedKnowledgeRevisionID, resolvedDocumentID,
	resolvedDocumentRevisionID string, createdAtZero, updatedAtZero bool, resolvedAt *time.Time) error {
	if !validLearningUUID(reviewID) || !validNotesyncCategory(category) || !validNotesyncReason(reason) || !validNotesyncReviewStatus(status) ||
		!validSHA256(basisHash) || generation < 1 || headRevisionNo < 0 || canonicalPath == "" || remoteVault == "" || remotePath == "" ||
		createdAtZero || updatedAtZero || len(canonicalPath) > maxNotesyncPathBytes || len(remoteVault) > maxNotesyncPathBytes || len(remotePath) > maxNotesyncPathBytes {
		return errors.New("notesync review is incomplete")
	}
	for _, value := range []string{headRevisionID, documentID, remoteDocumentID, resolutionOperationID, resolvedByDeviceID, resolvedKnowledgeRevisionID, resolvedDocumentID, resolvedDocumentRevisionID} {
		if value != "" && !validLearningUUID(value) {
			return errors.New("notesync review contains an invalid identity")
		}
	}
	if resolutionKind != "" && !validNotesyncReviewResolutionKind(resolutionKind) {
		return errors.New("notesync review resolution kind is invalid")
	}
	switch status {
	case "open":
		if resolutionKind != "" || resolutionOperationID != "" || resolvedByDeviceID != "" || resolvedKnowledgeRevisionID != "" ||
			resolvedDocumentID != "" || resolvedDocumentRevisionID != "" || resolvedAt != nil {
			return errors.New("open notesync review contains resolution state")
		}
	case "resolved":
		if !validNotesyncActionResolutionKind(resolutionKind) || resolutionOperationID == "" || resolvedByDeviceID == "" ||
			resolvedKnowledgeRevisionID == "" || resolvedDocumentID == "" || resolvedDocumentRevisionID == "" ||
			resolvedAt == nil || resolvedAt.IsZero() {
			return errors.New("resolved notesync review lacks resolution state")
		}
	case "closed":
		if !validNotesyncTerminalResolutionKind(resolutionKind) || resolutionOperationID != "" || resolvedByDeviceID != "" ||
			resolvedKnowledgeRevisionID != "" || resolvedDocumentID != "" || resolvedDocumentRevisionID != "" ||
			resolvedAt == nil || resolvedAt.IsZero() {
			return errors.New("closed notesync review contains invalid terminal state")
		}
	}
	return nil
}

func validateNotesyncSnapshot(value NotesyncReviewSnapshot) error {
	if !utf8.ValidString(value.Markdown) || len(value.Markdown) > maxNotesyncMarkdownBytes {
		return errors.New("notesync snapshot markdown is invalid")
	}
	return validateNotesyncSnapshotFields(value.Missing, value.KnowledgeRevisionID, value.KnowledgeRevisionNo, value.DocumentRevisionID,
		value.SourceRevisionID, value.Path, value.SHA256, value.RemoteVersion, value.RemoteLastTime)
}

func validateNotesyncSnapshotSummary(value NotesyncReviewSnapshotSummary) error {
	return validateNotesyncSnapshotFields(value.Missing, value.KnowledgeRevisionID, value.KnowledgeRevisionNo, value.DocumentRevisionID,
		value.SourceRevisionID, value.Path, value.SHA256, value.RemoteVersion, value.RemoteLastTime)
}

func validateNotesyncSnapshotFields(missing bool, knowledgeRevisionID string, knowledgeRevisionNo int64, documentRevisionID,
	sourceRevisionID, path, sha string, remoteVersion, remoteLastTime int64) error {
	if knowledgeRevisionNo < 0 || remoteVersion < 0 || remoteLastTime < 0 || len(path) > maxNotesyncPathBytes || !utf8.ValidString(path) {
		return errors.New("notesync snapshot is invalid")
	}
	for _, value := range []string{knowledgeRevisionID, documentRevisionID, sourceRevisionID} {
		if value != "" && !validLearningUUID(value) {
			return errors.New("notesync snapshot identity is invalid")
		}
	}
	if sha != "" && !validSHA256(sha) {
		return errors.New("notesync snapshot hash is invalid")
	}
	if !missing && sha == "" {
		return errors.New("present notesync snapshot lacks a hash")
	}
	return nil
}

func validateNotesyncPreviewItem(value NotesyncPreviewItem) error {
	if !validNotesyncCategory(value.Category) || !validNotesyncPreviewReason(value.ReasonCode) || !validSHA256(value.BasisHash) ||
		value.RemotePath == "" || len(value.RemotePath) > maxNotesyncPathBytes || !utf8.ValidString(value.RemotePath) {
		return errors.New("notesync preview item is incomplete")
	}
	for _, identity := range []string{value.ReviewID, value.DocumentID} {
		if identity != "" && !validLearningUUID(identity) {
			return errors.New("notesync preview item identity is invalid")
		}
	}
	if err := validateNotesyncSnapshotSummary(value.Base); err != nil {
		return err
	}
	if err := validateNotesyncSnapshotSummary(value.Local); err != nil {
		return err
	}
	if err := validateNotesyncSnapshotSummary(value.Remote); err != nil {
		return err
	}
	return validateNotesyncDiff(value.Diff)
}

func validateNotesyncDiff(value NotesyncThreeWayDiff) error {
	if !utf8.ValidString(value.BaseToLocal) || !utf8.ValidString(value.BaseToRemote) || len(value.BaseToLocal) > maxNotesyncDiffBytes || len(value.BaseToRemote) > maxNotesyncDiffBytes {
		return errors.New("notesync diff is invalid")
	}
	return nil
}

func ValidateNotesyncResolutionResult(value NotesyncResolutionResult) error {
	if !validLearningUUID(value.ReviewID) || !validNotesyncActionResolutionKind(value.ResolutionKind) {
		return errors.New("notesync resolution result is incomplete")
	}
	for _, identity := range []string{value.KnowledgeRevisionID, value.DocumentID, value.DocumentRevisionID} {
		if identity != "" && !validLearningUUID(identity) {
			return errors.New("notesync resolution result identity is invalid")
		}
	}
	if (value.DocumentID == "") != (value.DocumentRevisionID == "") || (value.KnowledgeRevisionID == "") != (value.DocumentID == "") {
		return errors.New("notesync resolution canonical identity is inconsistent")
	}
	return nil
}

func validNotesyncStatusReason(value string) bool {
	switch value {
	case "", "not_configured", "version_unavailable", "version_unsupported", "version_untested", "capability_unavailable":
		return true
	default:
		return false
	}
}

func validNotesyncCategory(value string) bool {
	switch value {
	case "in_sync", "remote_unchanged", "local_changed", "remote_changed", "both_changed", "remote_missing", "remote_moved", "unbased_remote", "path_occupied", "invalid_remote_markdown":
		return true
	default:
		return false
	}
}

func validNotesyncReason(value string) bool {
	switch value {
	case "local_revision_changed", "both_sides_changed", "remote_identity_moved", "unmanaged_remote_note", "remote_markdown_invalid", "remote_content_changed", "remote_note_missing", "remote_path_occupied", "publication_preflight_changed", "publication_readback_changed":
		return true
	default:
		return false
	}
}

func validNotesyncPreviewReason(value string) bool {
	return value == "in_sync" || validNotesyncReason(value)
}

func validNotesyncReviewStatus(value string) bool {
	return value == "open" || value == "resolved" || value == "closed"
}

func validNotesyncActionResolutionKind(value string) bool {
	return value == NotesyncResolutionAcceptRemote || value == NotesyncResolutionKeepCanonical || value == NotesyncResolutionMerged
}

func validNotesyncTerminalResolutionKind(value string) bool {
	return value == NotesyncResolutionSuperseded || value == NotesyncResolutionPrivacy
}

func validNotesyncReviewResolutionKind(value string) bool {
	return validNotesyncActionResolutionKind(value) || validNotesyncTerminalResolutionKind(value)
}
