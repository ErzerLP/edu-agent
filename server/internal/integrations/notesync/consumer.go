package notesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

const PublicationBusinessType = "knowledge.notesync.publish"

func CanonicalPublicationIdempotencyKey(documentID, documentRevisionID string, revisionNo, generation int64) string {
	return fmt.Sprintf("notesync.publish:%s:%s:%d:%d", documentID, documentRevisionID, revisionNo, generation)
}

func ReviewPublicationIdempotencyKey(reviewID, documentRevisionID string, generation int64) string {
	return fmt.Sprintf("notesync.review.publish:%s:%s:%d", reviewID, documentRevisionID, generation)
}

const (
	PublicationReasonCanonicalRevision   = "canonical_revision"
	PublicationReasonReviewKeepCanonical = "review_keep_canonical"
	PublicationReasonReviewImport        = "review_import"

	OutcomeApplied  PublicationOutcomeKind = "applied"
	OutcomeDeferred PublicationOutcomeKind = "deferred"
	OutcomeFailed   PublicationOutcomeKind = "failed"
	OutcomeReview   PublicationOutcomeKind = "review_required"

	ReviewKindRemoteChanged = "remote_changed"
	ReviewKindRemoteMissing = "remote_missing"
	ReviewKindPathOccupied  = "path_occupied"

	ReviewReasonRemoteContentChanged = "remote_content_changed"
	ReviewReasonRemoteNoteMissing    = "remote_note_missing"
	ReviewReasonRemotePathOccupied   = "remote_path_occupied"
	ReviewReasonPreflightChanged     = "publication_preflight_changed"
	ReviewReasonReadbackChanged      = "publication_readback_changed"
)

type PublicationIntent struct {
	SchemaVersion       int    `json:"schema_version"`
	DocumentID          string `json:"document_id"`
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
	DocumentRevisionID  string `json:"document_revision_id"`
	PublicationReason   string `json:"publication_reason"`
	ReviewID            string `json:"review_id,omitempty"`
}

type PublicationMapping struct {
	DocumentID          string
	RemoteVault         string
	RemotePath          string
	KnowledgeRevisionID string
	DocumentRevisionID  string
	RevisionNo          int64
	BaseMarkdown        string
	RemoteVersion       int64
	RemoteLastTime      int64
	Generation          int64
}

type PublicationWork struct {
	DocumentID          string
	KnowledgeRevisionID string
	DocumentRevisionID  string
	RevisionNo          int64
	Generation          int64
	CanonicalPath       string
	TargetMarkdown      string
	TargetModifiedAt    time.Time
	RemoteVault         string
	RemotePath          string
	PathOccupied        bool
	PublicationReason   string
	ReviewID            string
	ReviewRemote        *RemoteObservation
	Mapping             *PublicationMapping
}

type RemoteObservation struct {
	Missing  bool
	Markdown string
	Version  int64
	Ctime    int64
	Mtime    int64
	LastTime int64
}

type PublicationOutcomeKind string

type PublicationOutcome struct {
	Kind        PublicationOutcomeKind
	Category    string
	AvailableAt time.Time
	Permanent   bool
	ReviewKind  string
	ReasonCode  string
	Remote      RemoteObservation
}

type PublicationOperation func(context.Context, PublicationWork) (PublicationOutcome, error)

type PublicationStore interface {
	CanApplyNotesyncPublication(context.Context, outbox.Message, PublicationIntent) (outbox.ApplyDecision, error)
	ApplyNotesyncPublication(context.Context, outbox.Message, PublicationIntent, PublicationOperation) error
}

type Remote interface {
	Probe(context.Context, string) Capability
	GetNote(context.Context, string, string) (Note, error)
	CreateOrUpdateNote(context.Context, NoteWrite) (Note, error)
}

type ConsumerOptions struct {
	Store        PublicationStore
	Remote       Remote
	Vault        string
	PathPrefix   string
	RetryBackoff time.Duration
	Now          func() time.Time
}

type Consumer struct {
	store        PublicationStore
	remote       Remote
	vault        string
	pathPrefix   string
	retryBackoff time.Duration
	now          func() time.Time
}

func NewConsumer(options ConsumerOptions) (*Consumer, error) {
	if options.Store == nil || options.Remote == nil || !validRemoteVault(options.Vault) ||
		!validManagedPath(options.PathPrefix) || options.RetryBackoff <= 0 {
		return nil, errors.New("valid NoteSync publication store, remote, vault, path prefix, and retry backoff are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Consumer{
		store: options.Store, remote: options.Remote, vault: options.Vault, pathPrefix: options.PathPrefix,
		retryBackoff: options.RetryBackoff, now: options.Now,
	}, nil
}

func DecodePublicationIntent(message outbox.Message) (PublicationIntent, error) {
	if message.BusinessType != PublicationBusinessType || message.Status != outbox.StatusProcessing ||
		uuid.Validate(message.ID) != nil || uuid.Validate(message.AggregateID) != nil || uuid.Validate(message.LeaseToken) != nil ||
		message.Revision < 1 || message.Generation < 1 {
		return PublicationIntent{}, invalidIntent("invalid message envelope")
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Payload))
	decoder.DisallowUnknownFields()
	var intent PublicationIntent
	if err := decoder.Decode(&intent); err != nil {
		return PublicationIntent{}, invalidIntent("invalid payload")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return PublicationIntent{}, invalidIntent("multiple payload values")
	}
	if intent.SchemaVersion != 1 || uuid.Validate(intent.DocumentID) != nil ||
		uuid.Validate(intent.KnowledgeRevisionID) != nil || uuid.Validate(intent.DocumentRevisionID) != nil {
		return PublicationIntent{}, invalidIntent("unsupported payload identity")
	}
	var expectedKey string
	switch intent.PublicationReason {
	case PublicationReasonCanonicalRevision:
		if intent.ReviewID != "" {
			return PublicationIntent{}, invalidIntent("canonical publication cannot carry a review")
		}
		expectedKey = CanonicalPublicationIdempotencyKey(intent.DocumentID, intent.DocumentRevisionID, message.Revision, message.Generation)
	case PublicationReasonReviewKeepCanonical, PublicationReasonReviewImport:
		if uuid.Validate(intent.ReviewID) != nil {
			return PublicationIntent{}, invalidIntent("review publication requires a review identity")
		}
		expectedKey = ReviewPublicationIdempotencyKey(intent.ReviewID, intent.DocumentRevisionID, message.Generation)
	default:
		return PublicationIntent{}, invalidIntent("unsupported publication reason")
	}
	if message.AggregateID != intent.DocumentID || message.IdempotencyKey != expectedKey {
		return PublicationIntent{}, invalidIntent("message and payload identity differ")
	}
	return intent, nil
}

func (c *Consumer) CanApply(ctx context.Context, message outbox.Message) (outbox.ApplyDecision, error) {
	intent, err := DecodePublicationIntent(message)
	if err != nil {
		return outbox.ApplyDecision{}, err
	}
	return c.store.CanApplyNotesyncPublication(ctx, message, intent)
}

func (c *Consumer) Apply(ctx context.Context, message outbox.Message) error {
	intent, err := DecodePublicationIntent(message)
	if err != nil {
		return err
	}
	if err := c.store.ApplyNotesyncPublication(ctx, message, intent, c.publish); err != nil {
		return err
	}
	return outbox.ErrConsumerFinalized
}

func (c *Consumer) publish(ctx context.Context, work PublicationWork) (PublicationOutcome, error) {
	if work.DocumentID == "" || work.KnowledgeRevisionID == "" || work.DocumentRevisionID == "" ||
		work.RevisionNo < 1 || work.Generation < 1 || work.TargetModifiedAt.IsZero() ||
		work.RemoteVault == "" || !c.managesPath(work.RemotePath) {
		return failed("notesync_target_contract_mismatch", true), nil
	}
	if work.RemoteVault != c.vault {
		return failed("notesync_mapping_contract_mismatch", true), nil
	}
	if work.PathOccupied {
		return PublicationOutcome{
			Kind: OutcomeReview, ReviewKind: ReviewKindPathOccupied,
			ReasonCode: ReviewReasonRemotePathOccupied, Remote: RemoteObservation{Missing: true},
		}, nil
	}
	capability := c.remote.Probe(ctx, work.RemoteVault)
	if !capability.Compatible {
		category := capability.Reason
		if category == "" {
			category = "capability_unavailable"
		}
		return c.deferred("notesync_" + category), nil
	}

	remote, err := c.remote.GetNote(ctx, work.RemoteVault, work.RemotePath)
	if work.ReviewRemote != nil {
		return c.publishReviewed(ctx, work, remote, err)
	}
	if work.Mapping == nil {
		return c.publishInitial(ctx, work, remote, err)
	}
	return c.publishUpdate(ctx, work, remote, err)
}

func (c *Consumer) publishReviewed(ctx context.Context, work PublicationWork, remote Note, readErr error) (PublicationOutcome, error) {
	frozen := work.ReviewRemote
	if frozen == nil ||
		(work.PublicationReason != PublicationReasonReviewKeepCanonical && work.PublicationReason != PublicationReasonReviewImport) ||
		uuid.Validate(work.ReviewID) != nil {
		return failed("notesync_review_publication_contract_mismatch", true), nil
	}
	if frozen.Missing {
		if readErr == nil {
			return reviewWithNote(ReviewKindPathOccupied, ReviewReasonRemotePathOccupied, remote), nil
		}
		if !IsNotFound(readErr) {
			return c.remoteFailure(readErr), nil
		}
		modified := work.TargetModifiedAt.UTC().UnixMilli()
		_, writeErr := c.remote.CreateOrUpdateNote(ctx, NoteWrite{
			Vault: work.RemoteVault, Path: work.RemotePath, Content: work.TargetMarkdown,
			Ctime: modified, Mtime: modified, CreateOnly: true,
		})
		return c.reconcileReviewedWrite(ctx, work, writeErr)
	}
	if readErr != nil {
		if IsNotFound(readErr) {
			return PublicationOutcome{Kind: OutcomeReview, ReviewKind: ReviewKindRemoteMissing, ReasonCode: ReviewReasonRemoteNoteMissing, Remote: RemoteObservation{Missing: true}}, nil
		}
		return c.remoteFailure(readErr), nil
	}
	if remote.Content != frozen.Markdown {
		return reviewWithNote(ReviewKindRemoteChanged, ReviewReasonRemoteContentChanged, remote), nil
	}
	if remote.Content == work.TargetMarkdown {
		return applied(remote), nil
	}
	_, writeErr := c.remote.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: work.RemoteVault, Path: work.RemotePath, Content: work.TargetMarkdown,
		Ctime: remote.Ctime, Mtime: work.TargetModifiedAt.UTC().UnixMilli(), CreateOnly: false,
	})
	return c.reconcileReviewedWrite(ctx, work, writeErr)
}

func (c *Consumer) reconcileReviewedWrite(ctx context.Context, work PublicationWork, writeErr error) (PublicationOutcome, error) {
	remote, readErr := c.remote.GetNote(ctx, work.RemoteVault, work.RemotePath)
	if readErr == nil {
		if remote.Content == work.TargetMarkdown {
			return applied(remote), nil
		}
		if work.ReviewRemote != nil && !work.ReviewRemote.Missing && remote.Content == work.ReviewRemote.Markdown {
			category := "notesync_write_not_observed"
			permanent := false
			if writeErr != nil {
				category = remoteErrorCategory(writeErr)
				permanent = remoteErrorPermanent(writeErr)
			}
			return c.failure(category, permanent), nil
		}
		return reviewWithNote(ReviewKindRemoteChanged, ReviewReasonReadbackChanged, remote), nil
	}
	if IsNotFound(readErr) && work.ReviewRemote != nil && work.ReviewRemote.Missing {
		category := "notesync_publication_outcome_unknown"
		permanent := false
		if writeErr != nil && !ambiguousWriteError(writeErr) {
			category = remoteErrorCategory(writeErr)
			permanent = remoteErrorPermanent(writeErr)
		}
		return c.failure(category, permanent), nil
	}
	if writeErr != nil && Category(writeErr) == CategoryAuth && Category(readErr) == CategoryAuth {
		return c.deferred(remoteErrorCategory(writeErr)), nil
	}
	return failed("notesync_publication_outcome_unknown", false), nil
}

func (c *Consumer) publishInitial(ctx context.Context, work PublicationWork, remote Note, readErr error) (PublicationOutcome, error) {
	if readErr == nil {
		if remote.Content == work.TargetMarkdown {
			return applied(remote), nil
		}
		return reviewWithNote(ReviewKindPathOccupied, ReviewReasonRemotePathOccupied, remote), nil
	}
	if !IsNotFound(readErr) {
		return c.remoteFailure(readErr), nil
	}
	modified := work.TargetModifiedAt.UTC().UnixMilli()
	write := NoteWrite{
		Vault: work.RemoteVault, Path: work.RemotePath, Content: work.TargetMarkdown,
		Ctime: modified, Mtime: modified, CreateOnly: true,
	}
	_, writeErr := c.remote.CreateOrUpdateNote(ctx, write)
	return c.reconcileWrite(ctx, work, nil, writeErr)
}

func (c *Consumer) publishUpdate(ctx context.Context, work PublicationWork, remote Note, readErr error) (PublicationOutcome, error) {
	if readErr != nil {
		if IsNotFound(readErr) {
			return PublicationOutcome{
				Kind: OutcomeReview, ReviewKind: ReviewKindRemoteMissing,
				ReasonCode: ReviewReasonRemoteNoteMissing, Remote: RemoteObservation{Missing: true},
			}, nil
		}
		return c.remoteFailure(readErr), nil
	}
	if remote.Content == work.TargetMarkdown {
		return applied(remote), nil
	}
	if remote.Content != work.Mapping.BaseMarkdown {
		return reviewWithNote(ReviewKindRemoteChanged, ReviewReasonRemoteContentChanged, remote), nil
	}
	write := NoteWrite{
		Vault: work.RemoteVault, Path: work.RemotePath, Content: work.TargetMarkdown,
		Ctime: remote.Ctime, Mtime: work.TargetModifiedAt.UTC().UnixMilli(), CreateOnly: false,
	}
	_, writeErr := c.remote.CreateOrUpdateNote(ctx, write)
	return c.reconcileWrite(ctx, work, work.Mapping, writeErr)
}

func (c *Consumer) reconcileWrite(ctx context.Context, work PublicationWork, base *PublicationMapping, writeErr error) (PublicationOutcome, error) {
	remote, readErr := c.remote.GetNote(ctx, work.RemoteVault, work.RemotePath)
	if readErr != nil {
		if IsNotFound(readErr) {
			if base != nil {
				return PublicationOutcome{
					Kind: OutcomeReview, ReviewKind: ReviewKindRemoteMissing,
					ReasonCode: ReviewReasonRemoteNoteMissing, Remote: RemoteObservation{Missing: true},
				}, nil
			}
			category := "notesync_publication_outcome_unknown"
			permanent := false
			if writeErr != nil && !ambiguousWriteError(writeErr) {
				category = remoteErrorCategory(writeErr)
				permanent = remoteErrorPermanent(writeErr)
			}
			return c.failure(category, permanent), nil
		}
		if writeErr != nil && Category(writeErr) == CategoryAuth && Category(readErr) == CategoryAuth {
			return c.deferred(remoteErrorCategory(writeErr)), nil
		}
		return failed("notesync_publication_outcome_unknown", false), nil
	}
	if remote.Content == work.TargetMarkdown {
		return applied(remote), nil
	}
	if base != nil && remote.Content == base.BaseMarkdown {
		category := "notesync_write_not_observed"
		permanent := false
		if writeErr != nil {
			category = remoteErrorCategory(writeErr)
			permanent = remoteErrorPermanent(writeErr)
		}
		return c.failure(category, permanent), nil
	}
	reason := ReviewReasonReadbackChanged
	if writeErr != nil {
		reason = ReviewReasonPreflightChanged
	}
	kind := ReviewKindRemoteChanged
	if base == nil {
		kind = ReviewKindPathOccupied
	}
	return reviewWithNote(kind, reason, remote), nil
}

func (c *Consumer) remoteFailure(err error) PublicationOutcome {
	return c.failure(remoteErrorCategory(err), remoteErrorPermanent(err))
}

func (c *Consumer) failure(category string, permanent bool) PublicationOutcome {
	if category == "notesync_auth_unavailable" {
		return c.deferred(category)
	}
	return failed(category, permanent)
}

func failed(category string, permanent bool) PublicationOutcome {
	if strings.TrimSpace(category) == "" {
		category = "notesync_remote_failure"
	}
	return PublicationOutcome{Kind: OutcomeFailed, Category: category, Permanent: permanent}
}

func (c *Consumer) deferred(category string) PublicationOutcome {
	if strings.TrimSpace(category) == "" {
		category = "notesync_dependency_unavailable"
	}
	now := c.now().UTC()
	return PublicationOutcome{Kind: OutcomeDeferred, Category: category, AvailableAt: now.Add(c.retryBackoff)}
}

func (c *Consumer) managesPath(value string) bool {
	return validManagedPath(value) && strings.HasPrefix(value, c.pathPrefix+"/") && value != c.pathPrefix
}

func ManagedPath(prefix, canonicalPath string) (string, error) {
	if !validManagedPath(prefix) || !validManagedPath(canonicalPath) {
		return "", errors.New("NoteSync managed path components must be canonical relative paths")
	}
	value := prefix + "/" + canonicalPath
	if !validManagedPath(value) || !strings.HasPrefix(value, prefix+"/") {
		return "", errors.New("NoteSync managed path escapes its configured prefix")
	}
	return value, nil
}

func validRemoteVault(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) ||
		utf8.RuneCountInString(value) > 255 || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validManagedPath(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) || !norm.NFC.IsNormalString(value) ||
		strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		path.Clean(value) != value || value == "." || utf8.RuneCountInString(value) > 512 || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func applied(note Note) PublicationOutcome {
	return PublicationOutcome{Kind: OutcomeApplied, Remote: observe(note)}
}

func reviewWithNote(kind, reason string, note Note) PublicationOutcome {
	return PublicationOutcome{Kind: OutcomeReview, ReviewKind: kind, ReasonCode: reason, Remote: observe(note)}
}

func observe(note Note) RemoteObservation {
	return RemoteObservation{
		Markdown: note.Content, Version: note.Version, Ctime: note.Ctime,
		Mtime: note.Mtime, LastTime: note.LastTime,
	}
}

func remoteErrorCategory(err error) string {
	switch Category(err) {
	case CategoryAuth:
		return "notesync_auth_unavailable"
	case CategoryValidation:
		return "notesync_validation_unavailable"
	case CategoryRateLimited:
		return "notesync_rate_limited"
	case CategoryUpstream:
		return "notesync_upstream_unavailable"
	case CategoryTimeout:
		return "notesync_timeout"
	case CategoryContractMismatch:
		return "notesync_contract_mismatch"
	case CategoryConflict:
		return "notesync_write_conflict"
	default:
		return "notesync_transport_unavailable"
	}
}

func remoteErrorPermanent(err error) bool {
	switch Category(err) {
	case CategoryValidation, CategoryContractMismatch:
		return true
	default:
		return false
	}
}

func ambiguousWriteError(err error) bool {
	switch Category(err) {
	case CategoryTimeout, CategoryTransport, CategoryUpstream:
		return true
	default:
		return false
	}
}

type publicationFailure struct {
	category  string
	permanent bool
}

func (e *publicationFailure) Error() string    { return "NoteSync publication failed: " + e.category }
func (e *publicationFailure) Category() string { return e.category }
func (e *publicationFailure) Permanent() bool  { return e.permanent }

// FailedPublication returns an Outbox-classified failure after the knowledge transaction commits its attempt audit.
func FailedPublication(category string, permanent bool) error {
	if strings.TrimSpace(category) == "" {
		category = "notesync_remote_failure"
	}
	return &publicationFailure{category: category, permanent: permanent}
}

type classifiedIntentError struct {
	reason string
}

func (e *classifiedIntentError) Error() string {
	return "invalid NoteSync publication intent: " + e.reason
}
func (e *classifiedIntentError) Category() string { return "notesync_invalid_intent" }
func (e *classifiedIntentError) Permanent() bool  { return true }

func invalidIntent(reason string) error { return &classifiedIntentError{reason: reason} }
