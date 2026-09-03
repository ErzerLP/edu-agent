// Package agentsession provides encrypted local Agent Session persistence.
package agentsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func (s *Store) hasEncryptedData() (bool, error) {
	entries, _, complete, err := s.root.ReadDir(".", s.limits.DirectoryEntries)
	if err != nil {
		return false, err
	}
	if !complete {
		return false, ErrStoreFull
	}
	for _, entry := range entries {
		if entry.Type == securefile.EntryFile && isEncryptedDataName(entry.Name) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) sessionCiphertextBytesLocked(storageID string) (int64, error) {
	var total int64
	for _, name := range []string{keyName(storageID), recordName(storageID), indexProjectionName(storageID), dirtyName(storageID)} {
		snapshot, err := s.root.ReadSnapshot(name, s.limits.SessionCiphertextBytes, true)
		if errors.Is(err, securefile.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if snapshot.Size > s.limits.SessionCiphertextBytes-total {
			return 0, ErrStoreFull
		}
		total += snapshot.Size
	}
	return total, nil
}

func (s *Store) profileCiphertextBytesLocked() (int64, error) {
	entries, _, complete, err := s.root.ReadDir(".", s.limits.DirectoryEntries)
	if err != nil {
		return 0, err
	}
	if !complete {
		return 0, ErrStoreFull
	}
	var total int64
	for _, entry := range entries {
		if entry.Type != securefile.EntryFile || !isEncryptedDataName(entry.Name) {
			continue
		}
		limit := max(s.limits.SessionCiphertextBytes, s.limits.SessionPlaintextBytes+containerHeaderSize+32)
		snapshot, err := s.root.ReadSnapshot(entry.Name, limit, true)
		if err != nil {
			return 0, err
		}
		if snapshot.Size > s.limits.ProfileCiphertextBytes-total {
			return 0, ErrStoreFull
		}
		total += snapshot.Size
	}
	return total, nil
}

func (s *Store) publishCreate(ctx context.Context, name string, data []byte) error {
	result, err := s.publish(ctx, name, data, securefile.PublishOptions{Mode: securefile.PublishCreate, Permission: 0o600, Private: true})
	return mapPublishError(result, err)
}

func (s *Store) publishReplace(ctx context.Context, name string, data, expected []byte) error {
	result, err := s.publish(ctx, name, data, securefile.PublishOptions{
		Mode: securefile.PublishReplace, Permission: 0o600, ExpectedHash: contentHash(expected), ExpectedLimit: int64(len(expected)), Private: true,
	})
	return mapPublishError(result, err)
}

func mapPublishError(result securefile.PublishResult, err error) error {
	if result.Outcome == securefile.PublishUnknown || errors.Is(err, securefile.ErrOutcomeUnknown) {
		return ErrOutcomeUnknown
	}
	if errors.Is(err, securefile.ErrChanged) {
		return ErrCheckpointConflict
	}
	if errors.Is(err, securefile.ErrAlreadyExists) {
		return ErrCheckpointConflict
	}
	return err
}

func storeSecretVerified(backend SecretBackend, locator keybackend.Locator, value []byte) error {
	storeErr := backend.Store(locator, value)
	if storeErr == nil {
		return nil
	}
	observed, loadErr := backend.Load(locator, maxProfileSecretBytes)
	defer zero(observed)
	if loadErr == nil && bytes.Equal(observed, value) {
		return nil
	}
	if errors.Is(loadErr, keybackend.ErrNotFound) {
		return storeErr
	}
	return ErrOutcomeUnknown
}

func replaceSecretVerified(backend SecretBackend, locator keybackend.Locator, previous, replacement []byte) error {
	storeErr := backend.Store(locator, replacement)
	if storeErr == nil {
		return nil
	}
	observed, loadErr := backend.Load(locator, maxProfileSecretBytes)
	defer zero(observed)
	if loadErr == nil && bytes.Equal(observed, replacement) {
		return nil
	}
	if (loadErr == nil && bytes.Equal(observed, previous)) || errors.Is(loadErr, keybackend.ErrNotFound) {
		return storeErr
	}
	return ErrOutcomeUnknown
}

const profileSecretSize = 2 + 8 + 32 + 16

func newEncodedProfileSecret(generation uint64) ([]byte, error) {
	if generation == 0 {
		return nil, ErrInvalid
	}
	value := make([]byte, profileSecretSize)
	binary.BigEndian.PutUint16(value[:2], profileSecretVersion)
	binary.BigEndian.PutUint64(value[2:10], generation)
	if _, err := io.ReadFull(rand.Reader, value[10:]); err != nil {
		zero(value)
		return nil, err
	}
	return value, nil
}

func decodeProfileSecret(value []byte) (uint64, []byte, error) {
	if len(value) != profileSecretSize || binary.BigEndian.Uint16(value[:2]) != profileSecretVersion {
		return 0, nil, ErrCorrupt
	}
	generation := binary.BigEndian.Uint64(value[2:10])
	if generation == 0 {
		return 0, nil, ErrCorrupt
	}
	key := append([]byte(nil), value[10:42]...)
	return generation, key, nil
}

func validateCreateInput(input CreateInput, limits Limits) error {
	transcript, err := DecodeTranscript(input.Transcript, limits)
	if err != nil {
		return err
	}
	record := SessionRecord{
		SchemaVersion: recordPayloadSchemaVersion, SessionID: "00000000-0000-4000-8000-000000000000", StorageID: strings.Repeat("0", 32),
		PrivacyGeneration: 1, RecordRevision: 1, CommitID: "00000000-0000-4000-8000-000000000001",
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(), LastOpenedAt: time.Unix(1, 0).UTC(),
		CheckpointRevision: 1, ServerProfileFingerprint: strings.Repeat("0", 64), Lifecycle: "active",
		Title: input.Title, TitleSource: "auto", TitleRevision: 1,
		WorkspaceID: input.WorkspaceID, WorkspaceRoot: input.WorkspaceRoot, WorkspaceLabel: input.WorkspaceLabel,
		WorkspacePathHash: input.WorkspacePathHash, WorkspaceRootIdentityHash: input.WorkspaceRootIdentityHash,
		ProviderName: input.ProviderName, ProviderEndpoint: input.ProviderEndpoint, ProviderModel: input.ProviderModel,
		PrivacyLearnerGeneration: input.PrivacyLearnerGeneration, PrivacyMemoryGeneration: input.PrivacyMemoryGeneration,
		PrivacyVerified: input.PrivacyVerified, TranscriptCount: uint64(len(transcript.Entries)), Checkpoint: input.Checkpoint, Transcript: input.Transcript,
	}
	return validateRecord(record, limits)
}

func validatePreferenceReceipt(receipt PreferenceReceipt) error {
	if err := validatePreferenceState(receipt.ToolCallID, receipt.CreateOperationID, receipt.AdmitOperationID, receipt.RejectOperationID,
		receipt.Payload, receipt.CandidateID, receipt.CandidateRevision, receipt.Stage, receipt.StableCode); err != nil {
		return err
	}
	if receipt.Outcome != NoticeOutcomeUnknown && receipt.Outcome != NoticeOutcomeCompleted && receipt.Outcome != NoticeOutcomeRejected {
		return ErrInvalid
	}
	if strings.TrimSpace(receipt.StableCode) == "" ||
		receipt.Outcome == NoticeOutcomeCompleted && (receipt.CandidateID == "" || receipt.StableCode != "preference_saved") {
		return ErrInvalid
	}
	return nil
}

func validatePreferenceWriteAhead(value PreferenceWriteAhead) error {
	if value.Outcome != "" && value.Outcome != NoticeOutcomeUnknown && value.Outcome != NoticeOutcomeCompleted && value.Outcome != NoticeOutcomeRejected {
		return ErrInvalid
	}
	if value.Outcome == NoticeOutcomeCompleted && (value.CandidateID == "" || value.StableCode != "preference_saved") {
		return ErrInvalid
	}
	if value.Outcome == NoticeOutcomeRejected && strings.TrimSpace(value.StableCode) == "" {
		return ErrInvalid
	}
	return validatePreferenceState(value.ToolCallID, value.CreateOperationID, value.AdmitOperationID, value.RejectOperationID,
		value.Payload, value.CandidateID, value.CandidateRevision, value.Stage, value.StableCode)
}

func validatePreferenceState(toolCallID, createID, admitID, rejectID string, payload PreferencePayload, candidateID string, candidateRevision int64, stage, stableCode string) error {
	if err := validatePreferenceOperationIDs(createID, admitID, rejectID); err != nil {
		return err
	}
	if strings.TrimSpace(toolCallID) == "" || !safeText(toolCallID, 256) ||
		!safeText(payload.Content, 8<<10) || strings.TrimSpace(payload.Content) == "" || utf8.RuneCountInString(payload.Content) > 2000 ||
		!safeText(payload.Reason, 4<<10) || strings.TrimSpace(payload.Reason) == "" || utf8.RuneCountInString(payload.Reason) > 500 ||
		(payload.Category != "interaction_preference" && payload.Category != "time_constraint" && payload.Category != "personal_context") ||
		(payload.Sensitivity != "non_sensitive" && payload.Sensitivity != "sensitive") ||
		(payload.Stability != "stable" && payload.Stability != "transient") || payload.ValidUntil.IsZero() || !timeIsCanonical(payload.ValidUntil) ||
		(stage != "create" && stage != "admit" && stage != "reject") ||
		!safeText(stableCode, 128) || candidateRevision < 0 || (candidateID == "") != (candidateRevision == 0) ||
		(candidateID != "" && !safeText(candidateID, 256)) || ((stage == "admit" || stage == "reject") && candidateID == "") ||
		(stage == "reject" && rejectID == "") {
		return ErrInvalid
	}
	return nil
}

// Preference operation IDs use their own canonical-UUID namespace. Each
// receipt carries three distinct, non-empty IDs so create, admit, and reject
// retries cannot be confused with one another.
func validatePreferenceOperationIDs(createID, admitID, rejectID string) error {
	if !canonicalUUID(createID) || !canonicalUUID(admitID) || !canonicalUUID(rejectID) ||
		createID == admitID || rejectID == createID || rejectID == admitID {
		return ErrInvalid
	}
	return nil
}

func validateFileReceipt(value FileReceipt) error {
	if err := validateFileReference(value.ToolCallID, value.Operation, value.Path, value.Kind, value.ContentHash); err != nil {
		return err
	}
	if !validStableToken(value.StableCode, 128) {
		return ErrInvalid
	}
	switch value.publicationOutcome() {
	case NoticeOutcomeCompleted:
		if value.StableCode != FilePublicationCompletedCode || value.InvalidateObserved {
			return ErrInvalid
		}
	case NoticeOutcomeUnknown:
		if value.StableCode != FilePublicationUnknownCode || !value.InvalidateObserved || value.ContentHash != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validateFileWriteAhead(value FileWriteAhead) error {
	if err := validateFileReference(value.ToolCallID, value.Operation, value.Path, value.Kind, value.ContentHash); err != nil {
		return err
	}
	if value.PublicationOutcome != NoticeOutcomeUnknown || value.StableCode != FilePublicationUnknownCode ||
		!value.InvalidateObserved || value.ContentHash != "" {
		return ErrInvalid
	}
	return nil
}

func validateFileReference(toolCallID, operation, valuePath, kind, contentHash string) error {
	if strings.TrimSpace(toolCallID) == "" || !safeText(toolCallID, 256) ||
		strings.TrimSpace(operation) == "" || !safeText(operation, 128) || operation != strings.TrimSpace(operation) ||
		kind != "file" || !validRelativeWorkspacePath(valuePath) ||
		contentHash != "" && !validSHA256Tag(contentHash) {
		return ErrInvalid
	}
	return nil
}

func validRelativeWorkspacePath(value string) bool {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || !safeText(value, 4096) ||
		filepath.IsAbs(value) || strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) || strings.Contains(value, `\`) ||
		len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' {
		return false
	}
	components := strings.Split(value, "/")
	if len(components) > 64 {
		return false
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func validStableToken(value string, limit int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > limit {
		return false
	}
	for _, current := range value {
		if current != '_' && current != '-' && (current < 'a' || current > 'z') && (current < '0' || current > '9') {
			return false
		}
	}
	return true
}

func validateDirtyMarker(marker DirtyMarker) error {
	if err := persistedSchemaError(marker.SchemaVersion, dirtySchemaVersion); err != nil {
		return err
	}
	if _, err := parseUUID(marker.DirtyID); err != nil {
		return ErrInvalid
	}
	if _, err := parseUUID(marker.SessionID); err != nil {
		return ErrInvalid
	}
	if _, err := parseStorageID(marker.StorageID); err != nil {
		return ErrInvalid
	}
	if marker.BaseRevision == 0 || marker.TurnSequence == 0 || !validStableToken(marker.OperationClass, 128) ||
		marker.StartedAt.IsZero() || !timeIsCanonical(marker.StartedAt) || marker.Preference != nil && marker.File != nil ||
		marker.MayHaveSideEffect != (marker.Preference != nil || marker.File != nil) {
		return ErrInvalid
	}
	if marker.Preference != nil && validatePreferenceWriteAhead(*marker.Preference) != nil {
		return ErrInvalid
	}
	if marker.File != nil && validateFileWriteAhead(*marker.File) != nil {
		return ErrInvalid
	}
	return nil
}

// validateRecordReceiptIdentities keeps the stable identity namespaces
// separate. ToolCallID correlates model/tool events and is shared by both
// receipt lists, so it must be unique across PreferenceReceipts and
// FileReceipts. Preference create/admit/reject operation IDs form a separate
// namespace and must be unique among preference receipts only. FileReceipt's
// Operation is a mutation kind, not an operation identity, so repeated values
// such as "write_replace" are valid when their ToolCallIDs differ.
func validateRecordReceiptIdentities(preferenceReceipts []PreferenceReceipt, fileReceipts []FileReceipt) error {
	seenToolCallIDs := make(map[string]struct{}, len(preferenceReceipts)+len(fileReceipts))
	seenPreferenceOperationIDs := make(map[string]struct{}, len(preferenceReceipts)*3)
	for _, receipt := range preferenceReceipts {
		if _, duplicate := seenToolCallIDs[receipt.ToolCallID]; duplicate {
			return ErrInvalid
		}
		seenToolCallIDs[receipt.ToolCallID] = struct{}{}
		for _, operationID := range []string{receipt.CreateOperationID, receipt.AdmitOperationID, receipt.RejectOperationID} {
			if _, duplicate := seenPreferenceOperationIDs[operationID]; duplicate {
				return ErrInvalid
			}
			seenPreferenceOperationIDs[operationID] = struct{}{}
		}
	}
	for _, receipt := range fileReceipts {
		if _, duplicate := seenToolCallIDs[receipt.ToolCallID]; duplicate {
			return ErrInvalid
		}
		seenToolCallIDs[receipt.ToolCallID] = struct{}{}
	}
	return nil
}

func validWorkspaceMetadata(record SessionRecord) bool {
	hasAny := record.WorkspaceID != "" || record.WorkspaceRoot != "" || record.WorkspaceLabel != "" || record.WorkspacePathHash != "" || record.WorkspaceRootIdentityHash != ""
	if !hasAny {
		return true
	}
	if record.WorkspaceID == "" || record.WorkspaceRoot == "" || record.WorkspaceLabel == "" || !filepath.IsAbs(record.WorkspaceRoot) || filepath.Clean(record.WorkspaceRoot) != record.WorkspaceRoot ||
		!validSHA256Tag(record.WorkspacePathHash) || record.WorkspaceRootIdentityHash != "" && !validSHA256Tag(record.WorkspaceRootIdentityHash) {
		return false
	}
	expected := record.WorkspacePathHash
	if record.WorkspaceRootIdentityHash != "" {
		expected = record.WorkspaceRootIdentityHash
	}
	return record.WorkspaceID == expected
}

func validProviderMetadata(record SessionRecord) bool {
	hasAny := record.ProviderName != "" || record.ProviderEndpoint != "" || record.ProviderModel != ""
	if !hasAny {
		return true
	}
	if strings.TrimSpace(record.ProviderName) == "" || strings.TrimSpace(record.ProviderModel) == "" || record.ProviderEndpoint != strings.TrimRight(record.ProviderEndpoint, "/") {
		return false
	}
	parsed, err := url.Parse(record.ProviderEndpoint)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func validSHA256Tag(value string) bool {
	return strings.HasPrefix(value, "sha256:") && canonicalLowerHex(strings.TrimPrefix(value, "sha256:"), 64)
}

func canonicalUUID(value string) bool {
	_, err := parseUUID(value)
	return err == nil
}

func validSearchProjection(first, recent string) bool {
	limits := DefaultLimits()
	return validSearchProjectionWithin(first, recent, limits)
}

func validSearchProjectionWithin(first, recent string, limits Limits) bool {
	return safeText(first, limits.SearchSummaryBytes) && safeText(recent, limits.SearchSummaryBytes) && utf8.RuneCountInString(first) <= limits.SearchSummaryRunes && utf8.RuneCountInString(recent) <= limits.SearchSummaryRunes &&
		!strings.ContainsAny(first, "\n\t") && !strings.ContainsAny(recent, "\n\t")
}

func validateRecord(record SessionRecord, limits Limits) error {
	if err := persistedSchemaError(record.SchemaVersion, recordPayloadSchemaVersion); err != nil {
		return err
	}
	if record.PrivacyGeneration == 0 || record.RecordRevision == 0 || record.CheckpointRevision == 0 ||
		record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.LastOpenedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) ||
		record.LastOpenedAt.Before(record.CreatedAt) || record.LastOpenedAt.After(record.UpdatedAt) ||
		!timeIsCanonical(record.CreatedAt) || !timeIsCanonical(record.UpdatedAt) || !timeIsCanonical(record.LastOpenedAt) {
		return ErrInvalid
	}
	if _, err := parseUUID(record.SessionID); err != nil {
		return ErrInvalid
	}
	if _, err := parseStorageID(record.StorageID); err != nil {
		return ErrInvalid
	}
	commitID := record.CommitID
	if _, err := parseUUID(commitID); err != nil {
		return ErrInvalid
	}
	if !validWorkspaceMetadata(record) || !validProviderMetadata(record) {
		return ErrInvalid
	}
	if !safeText(record.Title, limits.ManualTitleBytes) || strings.ContainsAny(record.Title, "\n\t") || record.TitleRevision == 0 ||
		!validSearchProjectionWithin(record.FirstUserSummary, record.RecentUserSummary, limits) ||
		(record.TitleSource != "auto" && record.TitleSource != "manual") ||
		(record.Lifecycle != "active" && record.Lifecycle != "closed") || !canonicalLowerHex(record.ServerProfileFingerprint, 64) ||
		(!record.LastTitleAt.IsZero() && !timeIsCanonical(record.LastTitleAt)) ||
		!safeText(record.WorkspaceID, 4096) || !safeText(record.WorkspaceRoot, 4096) || !safeText(record.WorkspaceLabel, 256) ||
		!safeText(record.WorkspacePathHash, 128) || !safeText(record.WorkspaceRootIdentityHash, 128) ||
		!safeText(record.ProviderName, 128) || !safeText(record.ProviderEndpoint, 4096) || !safeText(record.ProviderModel, 512) ||
		record.PrivacyLearnerGeneration < 0 || record.PrivacyMemoryGeneration < 0 || len(record.PreferenceReceipts) > limits.ReceiptCount || len(record.FileReceipts) > limits.ReceiptCount {
		return ErrInvalid
	}
	for _, receipt := range record.PreferenceReceipts {
		if validatePreferenceReceipt(receipt) != nil {
			return ErrInvalid
		}
	}
	for _, receipt := range record.FileReceipts {
		if validateFileReceipt(receipt) != nil {
			return ErrInvalid
		}
	}
	if err := validateRecordReceiptIdentities(record.PreferenceReceipts, record.FileReceipts); err != nil {
		return err
	}
	if len(record.Checkpoint) == 0 || int64(len(record.Checkpoint)) > limits.SessionPlaintextBytes || !utf8.Valid(record.Checkpoint) || !json.Valid(record.Checkpoint) {
		return ErrInvalid
	}
	if len(record.QuarantinedCheckpoint) > 0 && (int64(len(record.QuarantinedCheckpoint)) > limits.SessionPlaintextBytes || !utf8.Valid(record.QuarantinedCheckpoint) || !json.Valid(record.QuarantinedCheckpoint)) {
		return ErrInvalid
	}
	if len(record.Transcript) == 0 {
		return ErrCorrupt
	}
	if int64(len(record.Transcript)) > limits.TranscriptBytes {
		return ErrStoreFull
	}
	transcript, err := DecodeTranscript(record.Transcript, limits)
	if err != nil {
		return err
	}
	if record.TranscriptCount != uint64(len(transcript.Entries)) {
		return ErrInvalid
	}
	if record.LastConsumedDirtyID != "" {
		if _, err := parseUUID(record.LastConsumedDirtyID); err != nil {
			return ErrInvalid
		}
	}
	encoded, err := encodeStrict(record)
	if err != nil || int64(len(encoded)) > limits.SessionPlaintextBytes {
		return ErrStoreFull
	}
	return nil
}

func validateIndex(index indexFile, generation uint64) error {
	if err := persistedSchemaError(index.SchemaVersion, indexSchemaVersion); err != nil {
		return err
	}
	if index.IndexRevision == 0 || index.PrivacyGeneration != generation {
		return ErrCorrupt
	}
	seenSession, seenStorage := map[string]struct{}{}, map[string]struct{}{}
	for _, entry := range index.Entries {
		if _, err := parseUUID(entry.SessionID); err != nil {
			return ErrCorrupt
		}
		if _, err := parseStorageID(entry.StorageID); err != nil {
			return ErrCorrupt
		}
		if _, exists := seenSession[entry.SessionID]; exists {
			return ErrCorrupt
		}
		if _, exists := seenStorage[entry.StorageID]; exists {
			return ErrCorrupt
		}
		seenSession[entry.SessionID], seenStorage[entry.StorageID] = struct{}{}, struct{}{}
	}
	return nil
}

func validateProjection(value indexProjection, generation uint64) error {
	if err := persistedSchemaError(value.SchemaVersion, projectionSchemaVersion); err != nil {
		return err
	}
	if _, err := parseUUID(value.SessionID); err != nil {
		return ErrCorrupt
	}
	if _, err := parseStorageID(value.StorageID); err != nil {
		return ErrCorrupt
	}
	if _, err := parseUUID(value.RecordCommitID); err != nil {
		return ErrCorrupt
	}
	if value.PrivacyGeneration != generation || value.RecordRevision == 0 || value.CheckpointRevision == 0 ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || value.LastOpenedAt.IsZero() ||
		value.UpdatedAt.Before(value.CreatedAt) || value.LastOpenedAt.Before(value.CreatedAt) || value.LastOpenedAt.After(value.UpdatedAt) ||
		!timeIsCanonical(value.CreatedAt) || !timeIsCanonical(value.UpdatedAt) || !timeIsCanonical(value.LastOpenedAt) ||
		!safeText(value.Title, 256) || strings.ContainsAny(value.Title, "\n\t") || value.TitleRevision == 0 ||
		!validSearchProjection(value.FirstUserSummary, value.RecentUserSummary) ||
		(value.TitleSource != "auto" && value.TitleSource != "manual") || !canonicalLowerHex(value.ServerProfileFingerprint, 64) ||
		!validProjectionWorkspaceMetadata(value) || !validProjectionProviderMetadata(value) ||
		(value.Lifecycle != "active" && value.Lifecycle != "closed") {
		return ErrCorrupt
	}
	return nil
}

func validProjectionWorkspaceMetadata(value indexProjection) bool {
	if value.WorkspaceID == "" && value.WorkspaceLabel == "" {
		return true
	}
	return validSHA256Tag(value.WorkspaceID) && strings.TrimSpace(value.WorkspaceLabel) != "" && safeText(value.WorkspaceLabel, 256)
}

func validProjectionProviderMetadata(value indexProjection) bool {
	return validProviderMetadata(SessionRecord{ProviderName: value.ProviderName, ProviderEndpoint: value.ProviderEndpoint, ProviderModel: value.ProviderModel})
}

func decodeRecordPayload(data []byte, limit int64) (SessionRecord, int, error) {
	var record SessionRecord
	version, err := probeRecordPayloadSchema(data, limit)
	if err != nil {
		return record, 0, err
	}
	if version > recordPayloadSchemaVersion {
		return record, version, ErrVersionUnsupported
	}

	steps := 0
	switch version {
	case 1:
		if err := validateRecordPayloadV1Shape(data); err != nil {
			return record, version, err
		}
		var payload recordPayloadV1
		if err := decodeStrict(data, &payload, limit); err != nil {
			return record, version, err
		}
		record = SessionRecord(payload)
		for record.SchemaVersion < recordPayloadSchemaVersion {
			if steps >= recordMigrationMaxSteps {
				return SessionRecord{}, version, ErrVersionUnsupported
			}
			switch record.SchemaVersion {
			case 1:
				record.SchemaVersion = 2
			default:
				return SessionRecord{}, version, ErrVersionUnsupported
			}
			steps++
		}
	case recordPayloadSchemaVersion:
		if err := decodeStrict(data, &record, limit); err != nil {
			return record, version, err
		}
	default:
		return record, version, ErrCorrupt
	}
	return record, version, nil
}

func validateRecordPayloadV1Shape(data []byte) error {
	required := map[string]bool{
		"schema_version": false, "session_id": false, "storage_id": false, "privacy_generation": false,
		"record_revision": false, "commit_id": false, "created_at": false, "updated_at": false,
		"last_opened_at": false, "checkpoint_revision": false, "server_profile_fingerprint": false,
		"lifecycle": false, "title": false, "workspace_id": false, "provider_endpoint": false,
		"privacy_verified": false, "committed_user_turns": false, "transcript_count": false,
		"checkpoint": false, "transcript": false,
	}
	seen := make(map[string]struct{})
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrCorrupt
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return ErrCorrupt
		}
		key, ok := keyToken.(string)
		if !ok {
			return ErrCorrupt
		}
		if _, duplicate := seen[key]; duplicate {
			return ErrCorrupt
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return ErrCorrupt
		}
		if _, requiredField := required[key]; requiredField {
			required[key] = true
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrCorrupt
	}
	for _, present := range required {
		if !present {
			return ErrCorrupt
		}
	}
	return nil
}

func probeRecordPayloadSchema(data []byte, limit int64) (int, error) {
	if int64(len(data)) > limit {
		return 0, ErrStoreFull
	}
	if !utf8.Valid(data) {
		return 0, ErrCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return 0, ErrCorrupt
	}
	version := 0
	found := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return 0, ErrCorrupt
		}
		key, ok := keyToken.(string)
		if !ok {
			return 0, ErrCorrupt
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return 0, ErrCorrupt
		}
		if key != "schema_version" {
			continue
		}
		if found || json.Unmarshal(raw, &version) != nil || version <= 0 {
			return 0, ErrCorrupt
		}
		found = true
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !found {
		return 0, ErrCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return 0, ErrCorrupt
	}
	return version, nil
}

func encodeStrict(value any) ([]byte, error) { return json.Marshal(value) }

func decodeStrict(data []byte, target any, limit int64) error {
	if int64(len(data)) > limit {
		return ErrStoreFull
	}
	if !utf8.Valid(data) {
		return ErrCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	return nil
}

func persistedSchemaError(actual, current int) error {
	switch {
	case actual == current:
		return nil
	case actual > 0:
		return ErrVersionUnsupported
	default:
		return ErrCorrupt
	}
}

func ensurePrivateRoot(path string) error {
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path, volume)
	current := volume + string(filepath.Separator)
	components := strings.FieldsFunc(remainder, func(value rune) bool { return value == '/' || value == '\\' })
	if len(components) == 0 {
		return securefile.ErrPermission
	}
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || sessionPathIsReparse(current, info) {
			return securefile.ErrLink
		}
		if index == len(components)-1 {
			if err := enforcePrivateSessionDirectory(current, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizedLimits(value Limits) Limits {
	defaults := DefaultLimits()
	if value.Sessions <= 0 {
		value.Sessions = defaults.Sessions
	}
	if value.ProfileCiphertextBytes <= 0 {
		value.ProfileCiphertextBytes = defaults.ProfileCiphertextBytes
	}
	if value.SessionPlaintextBytes <= 0 {
		value.SessionPlaintextBytes = defaults.SessionPlaintextBytes
	}
	if value.SessionCiphertextBytes <= 0 {
		value.SessionCiphertextBytes = defaults.SessionCiphertextBytes
	}
	if value.DirtyMarkerBytes <= 0 {
		value.DirtyMarkerBytes = defaults.DirtyMarkerBytes
	}
	if value.DirectoryEntries <= 0 {
		value.DirectoryEntries = defaults.DirectoryEntries
	}
	if value.TranscriptEntries <= 0 {
		value.TranscriptEntries = defaults.TranscriptEntries
	}
	if value.TranscriptBytes <= 0 {
		value.TranscriptBytes = defaults.TranscriptBytes
	}
	if value.TranscriptEntryBytes <= 0 {
		value.TranscriptEntryBytes = defaults.TranscriptEntryBytes
	}
	if value.TranscriptEventBytes <= 0 {
		value.TranscriptEventBytes = defaults.TranscriptEventBytes
	}
	if value.TranscriptEntryLines <= 0 {
		value.TranscriptEntryLines = defaults.TranscriptEntryLines
	}
	if value.TranscriptLineColumns <= 0 {
		value.TranscriptLineColumns = defaults.TranscriptLineColumns
	}
	if value.TranscriptTools <= 0 {
		value.TranscriptTools = defaults.TranscriptTools
	}
	if value.PickerQueryRunes <= 0 {
		value.PickerQueryRunes = defaults.PickerQueryRunes
	}
	if value.PickerResults <= 0 {
		value.PickerResults = defaults.PickerResults
	}
	if value.SearchSummaryRunes <= 0 {
		value.SearchSummaryRunes = defaults.SearchSummaryRunes
	}
	if value.SearchSummaryBytes <= 0 {
		value.SearchSummaryBytes = defaults.SearchSummaryBytes
	}
	if value.ManualTitleBytes <= 0 {
		value.ManualTitleBytes = defaults.ManualTitleBytes
	}
	if value.ManualTitleRunes <= 0 {
		value.ManualTitleRunes = defaults.ManualTitleRunes
	}
	if value.ManualTitleColumns <= 0 {
		value.ManualTitleColumns = defaults.ManualTitleColumns
	}
	if value.AutoTitleInputBytes <= 0 {
		value.AutoTitleInputBytes = defaults.AutoTitleInputBytes
	}
	if value.AutoTitlePartBytes <= 0 {
		value.AutoTitlePartBytes = defaults.AutoTitlePartBytes
	}
	if value.AutoTitleResponseBytes <= 0 {
		value.AutoTitleResponseBytes = defaults.AutoTitleResponseBytes
	}
	if value.AutoTitleMaxTokens <= 0 {
		value.AutoTitleMaxTokens = defaults.AutoTitleMaxTokens
	}
	if value.AutoTitleTurnInterval == 0 {
		value.AutoTitleTurnInterval = defaults.AutoTitleTurnInterval
	}
	if value.AutoTitleMinInterval <= 0 {
		value.AutoTitleMinInterval = defaults.AutoTitleMinInterval
	}
	if value.AutoTitleRequestTimeout <= 0 {
		value.AutoTitleRequestTimeout = defaults.AutoTitleRequestTimeout
	}
	if value.AutoTitleSaveTimeout <= 0 {
		value.AutoTitleSaveTimeout = defaults.AutoTitleSaveTimeout
	}
	if value.NoticeCount <= 0 {
		value.NoticeCount = defaults.NoticeCount
	}
	if value.ReceiptCount <= 0 {
		value.ReceiptCount = defaults.ReceiptCount
	}
	return value
}

func recordsEqual(left, right SessionRecord) bool {
	leftData, leftErr := encodeStrict(left)
	rightData, rightErr := encodeStrict(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func projectionFromRecord(record SessionRecord) indexProjection {
	return indexProjection{
		SchemaVersion: projectionSchemaVersion,
		SessionID:     record.SessionID, StorageID: record.StorageID, PrivacyGeneration: record.PrivacyGeneration,
		RecordRevision: record.RecordRevision, RecordCommitID: record.CommitID,
		CheckpointRevision: record.CheckpointRevision, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, LastOpenedAt: record.LastOpenedAt,
		Title: record.Title, TitleSource: record.TitleSource, FirstUserSummary: record.FirstUserSummary, RecentUserSummary: record.RecentUserSummary, TitleRevision: record.TitleRevision,
		CommittedUserTurns: record.CommittedUserTurns, TranscriptCount: record.TranscriptCount,
		ServerProfileFingerprint: record.ServerProfileFingerprint, WorkspaceID: record.WorkspaceID, WorkspaceLabel: record.WorkspaceLabel,
		ProviderName: record.ProviderName, ProviderEndpoint: record.ProviderEndpoint, ProviderModel: record.ProviderModel, Lifecycle: record.Lifecycle,
	}
}
func projectionPayloadEqual(left, right indexProjection) bool {
	leftData, leftErr := encodeStrict(left)
	rightData, rightErr := encodeStrict(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func sortIndexLocators(entries []indexLocator) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].SessionID == entries[j].SessionID {
			return entries[i].StorageID < entries[j].StorageID
		}
		return entries[i].SessionID < entries[j].SessionID
	})
}

func removeIndexLocator(entries []indexLocator, storageID string) []indexLocator {
	result := entries[:0]
	for _, entry := range entries {
		if entry.StorageID != storageID {
			result = append(result, entry)
		}
	}
	return result
}

func findDeleteTarget(entries []indexProjection, target DeleteTarget) (indexProjection, bool) {
	for _, entry := range entries {
		if target.StorageID != "" && entry.StorageID != target.StorageID {
			continue
		}
		if target.SessionID != "" && entry.SessionID != target.SessionID {
			continue
		}
		return entry, true
	}
	return indexProjection{}, false
}

func deleteExpectationMatches(entry indexProjection, expected uint64) bool {
	if entry.VersionUnsupported {
		return false
	}
	if entry.RecordValid || entry.ProjectionValid {
		return expected != 0 && entry.RecordRevision == expected
	}
	return entry.Corrupt && expected == 0
}

func sortIndexEntries(entries []indexProjection) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].UpdatedAt.Equal(entries[j].UpdatedAt) {
			if entries[i].SessionID == entries[j].SessionID {
				return entries[i].StorageID < entries[j].StorageID
			}
			return entries[i].SessionID < entries[j].SessionID
		}
		return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
	})
}
func findIndexEntry(entries []indexProjection, id string) (indexProjection, bool) {
	for _, entry := range entries {
		if entry.SessionID == id {
			return entry, true
		}
	}
	return indexProjection{}, false
}
func cloneRecord(value SessionRecord) SessionRecord {
	value.Checkpoint = append(json.RawMessage(nil), value.Checkpoint...)
	value.QuarantinedCheckpoint = append(json.RawMessage(nil), value.QuarantinedCheckpoint...)
	value.Transcript = append(json.RawMessage(nil), value.Transcript...)
	value.PreferenceReceipts = append([]PreferenceReceipt(nil), value.PreferenceReceipts...)
	value.FileReceipts = append([]FileReceipt(nil), value.FileReceipts...)
	return value
}

func keyName(storageID string) string             { return "key-" + storageID + ".enc" }
func recordName(storageID string) string          { return "record-" + storageID + ".enc" }
func indexProjectionName(storageID string) string { return "index-" + storageID + ".enc" }
func dirtyName(storageID string) string           { return "dirty-" + storageID + ".enc" }
func sessionLockName(storageID string) string     { return "session-" + storageID + ".lock" }
func isEncryptedDataName(name string) bool {
	return name == indexName || strings.HasSuffix(name, ".enc") && (strings.HasPrefix(name, "key-") || strings.HasPrefix(name, "record-") || strings.HasPrefix(name, "index-") || strings.HasPrefix(name, "dirty-"))
}
func isSessionCleanupName(name string) bool {
	if isEncryptedDataName(name) {
		return true
	}
	if len(name) != len(".edu-agent-")+32 || !strings.HasPrefix(name, ".edu-agent-") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, ".edu-agent-"))
	return err == nil
}
func storageFromKeyName(name string) (string, bool) {
	if len(name) != len("key-")+32+len(".enc") || !strings.HasPrefix(name, "key-") || !strings.HasSuffix(name, ".enc") {
		return "", false
	}
	value := name[4 : len(name)-4]
	_, err := parseStorageID(value)
	return value, err == nil
}

func randomUUID() (string, [16]byte, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", raw, err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return formatUUID(raw), raw, nil
}
func formatUUID(raw [16]byte) string {
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
func parseUUID(value string) ([16]byte, error) {
	var result [16]byte
	if len(value) != 36 || strings.ToLower(value) != value || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return result, ErrInvalid
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(raw) != 16 {
		return result, ErrInvalid
	}
	copy(result[:], raw)
	if result[6]>>4 != 4 || result[8]&0xc0 != 0x80 {
		return [16]byte{}, ErrInvalid
	}
	return result, nil
}

func randomStorageID() (string, [16]byte, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", raw, err
	}
	return hex.EncodeToString(raw[:]), raw, nil
}
func parseStorageID(value string) ([16]byte, error) {
	var result [16]byte
	if len(value) != 32 || strings.ToLower(value) != value {
		return result, ErrInvalid
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 16 {
		return result, ErrInvalid
	}
	copy(result[:], raw)
	return result, nil
}
func randomRevision() (uint64, error) {
	var raw [8]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return 0, err
	}
	value := uint64(0)
	for _, current := range raw {
		value = value<<8 | uint64(current)
	}
	if value == 0 {
		value = 1
	}
	return value, nil
}
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
func canonicalLowerHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func safeText(value string, maxBytes int) bool {
	if !utf8.ValidString(value) || len(value) > maxBytes {
		return false
	}
	for _, current := range value {
		if current != '\n' && current != '\t' && (unicode.IsControl(current) || isBidiControl(current)) {
			return false
		}
	}
	return true
}

func isBidiControl(value rune) bool {
	return value == '\u061c' || value == '\u200e' || value == '\u200f' ||
		value >= '\u202a' && value <= '\u202e' || value >= '\u2066' && value <= '\u2069'
}
