package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const maxNotesyncMergeBytes = 4 << 20

var notesyncUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (a *App) RunKnowledgeNotesync(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return notesyncUsage("knowledge notesync requires status, preview, reviews, review, or resolve")
	}
	switch args[0] {
	case "status":
		return a.runNotesyncStatus(ctx, args[1:])
	case "preview":
		return a.runNotesyncPreview(ctx, args[1:])
	case "reviews":
		return a.runNotesyncReviews(ctx, args[1:])
	case "review":
		return a.runNotesyncReview(ctx, args[1:])
	case "resolve":
		return a.runNotesyncResolve(ctx, args[1:])
	default:
		return notesyncUsage("unknown knowledge notesync command " + args[0])
	}
}

func (a *App) runNotesyncStatus(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge notesync status")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "knowledge notesync status accepts only connection flags", "run edu-agent knowledge notesync status", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	status, err := online.client.NotesyncStatus(ctx)
	if err != nil {
		return mapAPIError(err)
	}
	_, err = fmt.Fprintf(a.Out, "Configured: %t\nCompatible: %t\nReason: %s\nVersion: %s\nVault: %s\nPath prefix: %s\nExternal cleanup required: %t\n",
		status.Configured, status.Compatible, safeText(status.Reason), safeText(status.Version), safeText(status.Vault), safeText(status.PathPrefix), status.ExternalCleanupRequired)
	return err
}

func (a *App) runNotesyncPreview(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge notesync preview")
	var flags onlineFlags
	var path string
	var page, pageSize int
	addOnlineFlags(set, &flags)
	set.StringVar(&path, "path", "", "managed remote path")
	set.IntVar(&page, "page", 0, "remote page number")
	set.IntVar(&pageSize, "page-size", 0, "remote page size")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "knowledge notesync preview accepts path, page, page-size, and connection flags", "run edu-agent knowledge notesync preview [--path PATH] [--page N] [--page-size N]", ExitInput)
	}
	path = strings.TrimSpace(path)
	if page < 0 || pageSize < 0 || pageSize > 25 || !utf8.ValidString(path) || len(path) > 1024 || path != "" && page > 1 {
		return commandError("invalid_notesync_preview", "preview path or pagination is invalid", "use a path up to 1024 bytes, page 1 for an exact path, and page-size 1 through 25", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	result, err := online.client.NotesyncPreview(ctx, api.NotesyncPreviewRequest{Path: path, Page: page, PageSize: pageSize})
	if err != nil {
		return mapAPIError(err)
	}
	for _, item := range result.Items {
		_, _ = fmt.Fprintf(a.Out, "Preview: path=%s category=%s reason=%s review_id=%s basis_hash=%s document_id=%s\n",
			safeText(item.RemotePath), safeText(item.Category), safeText(item.ReasonCode), safeText(item.ReviewID), safeText(item.BasisHash), safeText(item.DocumentID))
		printNotesyncDiff(a.Out, item.Diff)
	}
	_, err = fmt.Fprintf(a.Out, "Page: %d\nPage size: %d\nTotal rows: %d\nNext page: %d\n", result.Page, result.PageSize, result.TotalRows, result.NextPage)
	return err
}

func (a *App) runNotesyncReviews(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge notesync reviews")
	var flags onlineFlags
	var status, cursor string
	var limit int
	addOnlineFlags(set, &flags)
	set.StringVar(&status, "status", "", "all, open, resolved, or closed")
	set.StringVar(&cursor, "cursor", "", "opaque review cursor")
	set.IntVar(&limit, "limit", 0, "review page size")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "knowledge notesync reviews accepts status, cursor, limit, and connection flags", "run edu-agent knowledge notesync reviews [--status STATUS] [--cursor CURSOR] [--limit N]", ExitInput)
	}
	if !validNotesyncListInput(status, cursor, limit) {
		return commandError("invalid_notesync_reviews", "review status or pagination is invalid", "use status all, open, resolved, or closed; cursor up to 4096 bytes; and limit 1 through 25", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	result, err := online.client.NotesyncReviews(ctx, status, cursor, limit)
	if err != nil {
		return mapAPIError(err)
	}
	for _, review := range result.Items {
		_, _ = fmt.Fprintf(a.Out, "Review: id=%s status=%s category=%s reason=%s path=%s basis_hash=%s generation=%d\n",
			safeText(review.ReviewID), safeText(review.Status), safeText(review.Category), safeText(review.ReasonCode), safeText(review.RemotePath), safeText(review.BasisHash), review.Generation)
	}
	_, err = fmt.Fprintf(a.Out, "Next cursor: %s\n", safeText(result.NextCursor))
	return err
}

func (a *App) runNotesyncReview(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "knowledge notesync review requires one review ID", "run edu-agent knowledge notesync review <review-id>", ExitInput)
	}
	reviewID := strings.ToLower(strings.TrimSpace(args[0]))
	set := newFlagSet("knowledge notesync review")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if !validNotesyncUUID(reviewID) || set.Parse(args[1:]) != nil || len(set.Args()) != 0 {
		return commandError("usage", "knowledge notesync review requires one valid review ID and optional connection flags", "run edu-agent knowledge notesync review <review-id>", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	review, err := online.client.NotesyncReview(ctx, reviewID)
	if err != nil {
		return mapAPIError(err)
	}
	_, _ = fmt.Fprintf(a.Out, "Review: id=%s status=%s category=%s reason=%s basis_hash=%s generation=%d\n",
		safeText(review.ReviewID), safeText(review.Status), safeText(review.Category), safeText(review.ReasonCode), safeText(review.BasisHash), review.Generation)
	_, _ = fmt.Fprintf(a.Out, "Canonical path: %s\nRemote vault: %s\nRemote path: %s\nHead revision: %s\nHead revision number: %d\n",
		safeText(review.CanonicalPath), safeText(review.RemoteVault), safeText(review.RemotePath), safeText(review.HeadRevisionID), review.HeadRevisionNo)
	printNotesyncSnapshot(a.Out, "Base", review.Base)
	printNotesyncSnapshot(a.Out, "Local", review.Local)
	printNotesyncSnapshot(a.Out, "Remote", review.Remote)
	printNotesyncDiff(a.Out, review.Diff)
	return nil
}

func (a *App) runNotesyncResolve(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "knowledge notesync resolve requires one review ID", "run edu-agent knowledge notesync resolve <review-id> --kind KIND --basis-hash HASH --operation-id ID", ExitInput)
	}
	reviewID := strings.ToLower(strings.TrimSpace(args[0]))
	set := newFlagSet("knowledge notesync resolve")
	var flags onlineFlags
	var kind, basisHash, operationID, contentFile string
	addOnlineFlags(set, &flags)
	set.StringVar(&kind, "kind", "", "accept-local, accept-remote, or merge")
	set.StringVar(&basisHash, "basis-hash", "", "review basis SHA-256")
	set.StringVar(&operationID, "operation-id", "", "idempotent operation UUID")
	set.StringVar(&contentFile, "content-file", "", "merged Markdown file")
	if !validNotesyncUUID(reviewID) || set.Parse(args[1:]) != nil || len(set.Args()) != 0 {
		return commandError("usage", "knowledge notesync resolve requires one valid review ID and flags", "run edu-agent knowledge notesync resolve <review-id> --kind KIND --basis-hash HASH --operation-id ID", ExitInput)
	}
	basisHash = strings.ToLower(strings.TrimSpace(basisHash))
	operationID = strings.ToLower(strings.TrimSpace(operationID))
	serverKind, ok := notesyncServerResolutionKind(strings.TrimSpace(kind))
	if !ok || !validNotesyncSHA256(basisHash) || !validNotesyncUUID(operationID) {
		return commandError("invalid_notesync_resolution", "kind, basis hash, or operation ID is invalid", "use kind accept-local, accept-remote, or merge with a lowercase SHA-256 and UUID", ExitInput)
	}
	var merged *string
	if serverKind == api.NotesyncResolutionMerged {
		if strings.TrimSpace(contentFile) == "" {
			return commandError("invalid_notesync_resolution", "merge requires --content-file", "provide one regular UTF-8 Markdown file up to 4 MiB", ExitInput)
		}
		content, err := readNotesyncMergeFile(contentFile)
		if err != nil {
			return err
		}
		merged = &content
	} else if contentFile != "" {
		return commandError("invalid_notesync_resolution", "--content-file is only allowed with kind merge", "remove --content-file or select --kind merge", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	result, err := online.client.ResolveNotesyncReview(ctx, reviewID, api.NotesyncResolutionRequest{
		BasisHash: basisHash, OperationID: operationID, Kind: serverKind, MergedMarkdown: merged,
	})
	if err != nil {
		return mapAPIError(err)
	}
	state := "resolved"
	if result.Replayed {
		state = "replayed"
	} else if result.Unchanged {
		state = "unchanged"
	}
	_, err = fmt.Fprintf(a.Out, "Review: %s\nResolution: %s\nResult: %s\nKnowledge revision: %s\nDocument: %s\nDocument revision: %s\n",
		safeText(result.ReviewID), safeText(kind), state, safeText(result.KnowledgeRevisionID), safeText(result.DocumentID), safeText(result.DocumentRevisionID))
	return err
}

func notesyncUsage(detail string) error {
	return commandError("usage", detail, "run edu-agent knowledge notesync status, preview, reviews, review, or resolve", ExitInput)
}

func validNotesyncListInput(status, cursor string, limit int) bool {
	if status != "" && status != "all" && status != "open" && status != "resolved" && status != "closed" {
		return false
	}
	return utf8.ValidString(cursor) && len(cursor) <= 4096 && limit >= 0 && limit <= 25
}

func validNotesyncUUID(value string) bool { return notesyncUUIDPattern.MatchString(value) }

func validNotesyncSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func notesyncServerResolutionKind(value string) (string, bool) {
	switch value {
	case "accept-local":
		return api.NotesyncResolutionKeepCanonical, true
	case "accept-remote":
		return api.NotesyncResolutionAcceptRemote, true
	case "merge":
		return api.NotesyncResolutionMerged, true
	default:
		return "", false
	}
}

func readNotesyncMergeFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", commandError("invalid_notesync_content_file", "merged Markdown must be a readable regular file", "remove symlinks and select a regular file", ExitInput)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", commandError("invalid_notesync_content_file", "merged Markdown could not be opened", "check file permissions", ExitInput)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", commandError("invalid_notesync_content_file", "merged Markdown changed while being opened", "retry with a stable regular file", ExitInput)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxNotesyncMergeBytes+1))
	if err != nil {
		return "", commandError("invalid_notesync_content_file", "merged Markdown could not be read", "check file permissions and retry", ExitInput)
	}
	if len(data) > maxNotesyncMergeBytes || !utf8.Valid(data) {
		return "", commandError("invalid_notesync_content_file", "merged Markdown must be UTF-8 and no larger than 4 MiB", "reduce or re-encode the file", ExitInput)
	}
	return string(data), nil
}

func printNotesyncSnapshot(out interface{ Write([]byte) (int, error) }, label string, value api.NotesyncReviewSnapshot) {
	_, _ = fmt.Fprintf(out, "%s: missing=%t path=%s sha256=%s knowledge_revision=%s document_revision=%s source_revision=%s remote_version=%d remote_last_time=%d\n",
		label, value.Missing, safeText(value.Path), safeText(value.SHA256), safeText(value.KnowledgeRevisionID), safeText(value.DocumentRevisionID), safeText(value.SourceRevisionID), value.RemoteVersion, value.RemoteLastTime)
}

func printNotesyncDiff(out interface{ Write([]byte) (int, error) }, value api.NotesyncThreeWayDiff) {
	_, _ = fmt.Fprintf(out, "Base to local diff (truncated=%t):\n%s\n", value.LocalTruncated, safeText(value.BaseToLocal))
	_, _ = fmt.Fprintf(out, "Base to remote diff (truncated=%t):\n%s\n", value.RemoteTruncated, safeText(value.BaseToRemote))
}
