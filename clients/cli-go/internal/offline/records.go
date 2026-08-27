package offline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	keyMigrationStatePrepared            = "prepared"
	keyMigrationStateDestinationVerified = "destination_verified"
	keyMigrationStateAuthoritySwitched   = "authority_switched"
	keyMigrationStateSourceCleaned       = "source_cleaned"
	keyMigrationStateCompleted           = "completed"
	preparePublicationVersion            = 1
)

type prepareDetail struct {
	Purpose              string          `json:"purpose"`
	Request              json.RawMessage `json:"request"`
	TrustState           json.RawMessage `json:"trust_state,omitempty"`
	PublicationVersion   int             `json:"publication_version,omitempty"`
	RequestDigest        string          `json:"request_digest,omitempty"`
	TrustStateDigest     string          `json:"trust_state_digest,omitempty"`
	BaseTrustState       json.RawMessage `json:"base_trust_state,omitempty"`
	BaseTrustStateDigest string          `json:"base_trust_state_digest,omitempty"`
	NextTrustState       json.RawMessage `json:"next_trust_state,omitempty"`
	NextTrustStateDigest string          `json:"next_trust_state_digest,omitempty"`
	PackID               string          `json:"pack_id,omitempty"`
	PackRecord           *PackRecord     `json:"pack_record,omitempty"`
	PackRecordDigest     string          `json:"pack_record_digest,omitempty"`
}

func (d prepareDetail) validate(state string) error {
	emptyPublication := d.PublicationVersion == 0 && d.RequestDigest == "" && d.TrustStateDigest == "" && len(d.BaseTrustState) == 0 && d.BaseTrustStateDigest == "" && len(d.NextTrustState) == 0 && d.NextTrustStateDigest == "" && d.PackID == "" && d.PackRecord == nil && d.PackRecordDigest == ""
	switch d.Purpose {
	case "prepare_intent":
		if state != "prepared" || !emptyPublication {
			return ErrInvalidState
		}
		if _, err := requireCanonicalObject(d.Request); err != nil {
			return err
		}
		if len(d.TrustState) != 0 {
			if _, err := requireCanonicalObject(d.TrustState); err != nil {
				return err
			}
		}
		return nil
	case "prepare_publish":
		if (state != "publishing" && state != "completed") || d.PublicationVersion != preparePublicationVersion {
			return ErrInvalidState
		}
		if err := validateCanonicalDigest(d.Request, d.RequestDigest); err != nil {
			return fmt.Errorf("prepare request digest: %w", err)
		}
		if len(d.TrustState) == 0 {
			if d.TrustStateDigest != "" {
				return errors.New("prepare intent trust digest has no checkpoint")
			}
		} else if err := validateCanonicalDigest(d.TrustState, d.TrustStateDigest); err != nil {
			return fmt.Errorf("prepare intent trust digest: %w", err)
		}
		if err := validateCanonicalDigest(d.BaseTrustState, d.BaseTrustStateDigest); err != nil {
			return fmt.Errorf("prepare base trust digest: %w", err)
		}
		if err := validateCanonicalDigest(d.NextTrustState, d.NextTrustStateDigest); err != nil {
			return fmt.Errorf("prepare next trust digest: %w", err)
		}
		if _, err := parseUUID(d.PackID); err != nil {
			return err
		}
		if d.PackRecord == nil || d.PackRecord.validate() != nil || d.PackRecord.PackID != d.PackID {
			return errors.New("prepare pack record is missing or mismatched")
		}
		digest, err := packRecordDigest(*d.PackRecord)
		if err != nil || digest != d.PackRecordDigest {
			return errors.New("prepare pack record digest is invalid")
		}
		return nil
	case "save_pack":
		if state != "publishing" || !bytes.Equal(d.Request, []byte("null")) || len(d.TrustState) != 0 || !emptyPublication {
			return ErrInvalidState
		}
		return nil
	default:
		return errors.New("unknown prepare journal purpose")
	}
}

func canonicalDigest(value json.RawMessage) (string, error) {
	canonical, err := requireCanonicalObject(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func validateCanonicalDigest(value json.RawMessage, expected string) error {
	if err := validateSHA256Hex(expected); err != nil {
		return err
	}
	actual, err := canonicalDigest(value)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("canonical digest mismatch")
	}
	return nil
}

func prepareRequestOperationID(value json.RawMessage) (string, error) {
	var request struct {
		OperationID string `json:"operation_id"`
	}
	if _, err := requireCanonicalObject(value); err != nil {
		return "", err
	}
	if err := json.Unmarshal(value, &request); err != nil {
		return "", err
	}
	if _, err := parseUUID(request.OperationID); err != nil {
		return "", errors.New("prepare request operation ID is invalid")
	}
	return request.OperationID, nil
}

type keyMigrationDetail struct {
	MigrationVersion     int    `json:"migration_version"`
	ProfileID            string `json:"profile_id"`
	SourceBackend        string `json:"source_backend"`
	DestinationBackend   string `json:"destination_backend"`
	SourceLocator        string `json:"source_locator"`
	DestinationLocator   string `json:"destination_locator"`
	KeyIdentity          string `json:"key_identity"`
	SourceKeyDigest      string `json:"source_key_digest"`
	DestinationKeyDigest string `json:"destination_key_digest"`
}

func (r ProfileRecord) validate(binding Binding, expectedTrust TrustState) error {
	if r.Format != Format || (r.ProfileVersion != 1 && r.ProfileVersion != 2) || r.ProfileID != binding.ProfileUUID() {
		return fmt.Errorf("%w: invalid profile record", ErrCorruptStore)
	}
	trust, err := requireCanonicalObject(r.TrustState)
	if err != nil {
		return fmt.Errorf("%w: invalid profile trust state", ErrCorruptStore)
	}
	root := trust
	if r.ProfileVersion == 2 {
		root, err = requireCanonicalObject(r.TrustRoot)
		if err != nil {
			return fmt.Errorf("%w: invalid profile trust root", ErrCorruptStore)
		}
	} else if len(r.TrustRoot) != 0 {
		return fmt.Errorf("%w: legacy profile carries an unexpected trust root", ErrCorruptStore)
	}
	if expectedTrust.valid() && !bytes.Equal(root, expectedTrust.canonical) {
		return ErrBindingMismatch
	}
	return nil
}

func (r ProfileRecord) trustRoot() json.RawMessage {
	if r.ProfileVersion == 2 {
		return append(json.RawMessage(nil), r.TrustRoot...)
	}
	return append(json.RawMessage(nil), r.TrustState...)
}

func canonicalPackBytes(value json.RawMessage) (json.RawMessage, error) {
	canonical, err := requireCanonicalObject(value)
	if err != nil {
		return nil, errors.New("pack bytes must be one canonical JSON object")
	}
	if len(canonical) > MaxCanonicalPackBytes {
		return nil, fmt.Errorf("pack canonical JSON exceeds %d-byte limit", MaxCanonicalPackBytes)
	}
	return canonical, nil
}

func packToRecord(pack Pack) (PackRecord, error) {
	packID, err := parseUUID(pack.ID)
	if err != nil || packID == ([16]byte{}) {
		return PackRecord{}, errors.New("pack ID must be a canonical UUID")
	}
	eligible, err := normalizeTime(pack.EligibleUntil)
	if err != nil {
		return PackRecord{}, err
	}
	archive, err := normalizeTime(pack.ArchiveUntil)
	if err != nil || archive.Before(eligible) {
		return PackRecord{}, errors.New("pack archive deadline is invalid")
	}
	if pack.ItemCount < 0 || pack.ItemCount > 20 {
		return PackRecord{}, errors.New("pack item count is outside the supported range")
	}
	canonical, err := canonicalPackBytes(pack.Canonical)
	if err != nil {
		return PackRecord{}, err
	}
	return PackRecord{RecordVersion: 1, PackID: pack.ID, EligibleUntil: formatRecordTime(eligible), ArchiveUntil: formatRecordTime(archive), ItemCount: pack.ItemCount, CanonicalBytes: append(json.RawMessage(nil), canonical...)}, nil
}

func (r PackRecord) validate() error {
	if r.RecordVersion != 1 {
		return errors.New("unsupported pack record version")
	}
	if _, err := parseUUID(r.PackID); err != nil {
		return errors.New("invalid pack record identity")
	}
	eligible, err := parseRecordTime(r.EligibleUntil)
	if err != nil {
		return err
	}
	archive, err := parseRecordTime(r.ArchiveUntil)
	if err != nil || archive.Before(eligible) {
		return errors.New("invalid pack record deadline")
	}
	if r.ItemCount < 0 || r.ItemCount > 20 {
		return errors.New("invalid pack record item count")
	}
	_, err = canonicalPackBytes(r.CanonicalBytes)
	return err
}

func (r PackRecord) public() (Pack, error) {
	if err := r.validate(); err != nil {
		return Pack{}, err
	}
	eligible, _ := parseRecordTime(r.EligibleUntil)
	archive, _ := parseRecordTime(r.ArchiveUntil)
	return Pack{ID: r.PackID, EligibleUntil: eligible, ArchiveUntil: archive, ItemCount: r.ItemCount, Canonical: append(json.RawMessage(nil), r.CanonicalBytes...)}, nil
}

func operationToRecord(operation QueuedOperation) (OperationRecord, error) {
	if _, err := parseUUID(operation.ID); err != nil {
		return OperationRecord{}, errors.New("operation ID must be a canonical UUID")
	}
	if _, err := parseUUID(operation.SubmissionID); err != nil {
		return OperationRecord{}, errors.New("submission ID must be a canonical UUID")
	}
	if _, err := parseUUID(operation.PackID); err != nil {
		return OperationRecord{}, errors.New("pack ID must be a canonical UUID")
	}
	if operation.DeviceSequence == 0 {
		return OperationRecord{}, errors.New("device sequence must be positive")
	}
	queuedAt, err := normalizeTime(operation.QueuedAt)
	if err != nil {
		return OperationRecord{}, err
	}
	canonical, err := requireCanonicalObject(operation.Canonical)
	if err != nil {
		return OperationRecord{}, errors.New("operation bytes must be one exact canonical JSON object")
	}
	return OperationRecord{RecordVersion: 1, OperationID: operation.ID, SubmissionID: operation.SubmissionID, PackID: operation.PackID, DeviceSequence: canonicalUint(operation.DeviceSequence), State: StateQueued, QueuedAt: formatRecordTime(queuedAt), CanonicalBytes: append(json.RawMessage(nil), canonical...)}, nil
}

func (r OperationRecord) validate() error {
	if r.RecordVersion != 1 || r.State != StateQueued {
		return errors.New("invalid immutable operation record version or state")
	}
	if _, err := parseUUID(r.OperationID); err != nil {
		return errors.New("invalid operation record identity")
	}
	if _, err := parseUUID(r.SubmissionID); err != nil {
		return errors.New("invalid operation submission identity")
	}
	if _, err := parseUUID(r.PackID); err != nil {
		return errors.New("invalid operation pack identity")
	}
	if _, err := parseCanonicalUint(r.DeviceSequence, true); err != nil {
		return errors.New("invalid operation device sequence")
	}
	if _, err := parseRecordTime(r.QueuedAt); err != nil {
		return err
	}
	_, err := requireCanonicalObject(r.CanonicalBytes)
	return err
}

func (r OperationRecord) public() (QueuedOperation, error) {
	if err := r.validate(); err != nil {
		return QueuedOperation{}, err
	}
	sequence, _ := parseCanonicalUint(r.DeviceSequence, true)
	queuedAt, _ := parseRecordTime(r.QueuedAt)
	return QueuedOperation{ID: r.OperationID, SubmissionID: r.SubmissionID, PackID: r.PackID, DeviceSequence: sequence, QueuedAt: queuedAt, Canonical: append(json.RawMessage(nil), r.CanonicalBytes...)}, nil
}

func receiptFromResult(result SyncResult, sequence uint64) (ReceiptRecord, error) {
	if _, err := parseUUID(result.OperationID); err != nil {
		return ReceiptRecord{}, errors.New("sync result operation ID is invalid")
	}
	if _, err := parseUUID(result.SubmissionID); err != nil {
		return ReceiptRecord{}, errors.New("sync result submission ID is invalid")
	}
	if sequence == 0 || !result.State.valid() || result.State == StateDraft || result.State == StateDiscarded {
		return ReceiptRecord{}, ErrInvalidState
	}
	updated, err := normalizeTime(result.UpdatedAt)
	if err != nil {
		return ReceiptRecord{}, err
	}
	receipt, err := canonicalObjectOrNull(result.Receipt)
	if err != nil {
		return ReceiptRecord{}, errors.New("ingest receipt must be null or a canonical JSON object")
	}
	status, err := canonicalObjectOrNull(result.Status)
	if err != nil {
		return ReceiptRecord{}, errors.New("operation status must be null or a canonical JSON object")
	}
	reasons := append([]string(nil), result.ReasonCodes...)
	if reasons == nil {
		reasons = []string{}
	}
	for _, reason := range reasons {
		if reason == "" || strings.TrimSpace(reason) != reason || len(reason) > 128 {
			return ReceiptRecord{}, errors.New("sync reason code is invalid")
		}
	}
	if (result.State == StateArchivedPendingEvidence || result.State == StateTerminal) && bytes.Equal(receipt, []byte("null")) {
		return ReceiptRecord{}, errors.New("durable archive state requires an ingest receipt")
	}
	return ReceiptRecord{RecordVersion: 1, OperationID: result.OperationID, SubmissionID: result.SubmissionID, DeviceSequence: canonicalUint(sequence), State: result.State, ArchiveStatus: result.ArchiveStatus, AssessmentStatus: result.AssessmentStatus, EvidenceStatus: result.EvidenceStatus, ReasonCodes: reasons, Receipt: receipt, Status: status, UpdatedAt: formatRecordTime(updated)}, nil
}

func (r ReceiptRecord) validate() error {
	if r.RecordVersion != 1 || !r.State.valid() || r.State == StateDraft || r.State == StateDiscarded {
		return ErrInvalidState
	}
	if _, err := parseUUID(r.OperationID); err != nil {
		return errors.New("invalid receipt operation identity")
	}
	if _, err := parseUUID(r.SubmissionID); err != nil {
		return errors.New("invalid receipt submission identity")
	}
	if _, err := parseCanonicalUint(r.DeviceSequence, true); err != nil {
		return errors.New("invalid receipt sequence")
	}
	if _, err := parseRecordTime(r.UpdatedAt); err != nil {
		return err
	}
	if r.ReasonCodes == nil {
		return errors.New("receipt reason codes must be an array")
	}
	if _, err := canonicalObjectOrNull(r.Receipt); err != nil {
		return err
	}
	if _, err := canonicalObjectOrNull(r.Status); err != nil {
		return err
	}
	if (r.State == StateArchivedPendingEvidence || r.State == StateTerminal) && bytes.Equal(r.Receipt, []byte("null")) {
		return errors.New("archive state is missing a durable receipt")
	}
	return nil
}

func (r ReceiptRecord) public() (OperationStatus, error) {
	if err := r.validate(); err != nil {
		return OperationStatus{}, err
	}
	updated, _ := parseRecordTime(r.UpdatedAt)
	return OperationStatus{OperationID: r.OperationID, SubmissionID: r.SubmissionID, State: r.State, ArchiveStatus: r.ArchiveStatus, AssessmentStatus: r.AssessmentStatus, EvidenceStatus: r.EvidenceStatus, ReasonCodes: append([]string(nil), r.ReasonCodes...), UpdatedAt: updated}, nil
}

func (r JournalRecord) validate() error {
	if r.JournalVersion != 1 {
		return errors.New("unsupported journal version")
	}
	if _, err := parseUUID(r.JournalID); err != nil {
		return errors.New("invalid journal identity")
	}
	if _, err := parseUUID(r.SourceID); err != nil {
		return errors.New("invalid journal source identity")
	}
	if _, err := parseUUID(r.TargetID); err != nil {
		return errors.New("invalid journal target identity")
	}
	if _, err := parseCanonicalUint(r.Revision, true); err != nil {
		return errors.New("invalid journal revision")
	}
	if _, err := parseRecordTime(r.CreatedAt); err != nil {
		return err
	}
	if _, err := requireCanonicalObject(r.Detail); err != nil {
		return errors.New("journal detail is not a canonical object")
	}
	switch r.Kind {
	case JournalPreparePublish:
		var detail prepareDetail
		if err := decodeClosed(r.Detail, &detail); err != nil || detail.validate(r.State) != nil {
			return errors.New("invalid prepare publish journal")
		}
		switch detail.Purpose {
		case "prepare_intent":
			requestID, err := prepareRequestOperationID(detail.Request)
			if err != nil || requestID != r.SourceID || r.SourceID != r.JournalID || r.TargetID != r.JournalID || r.Revision != "1" {
				return ErrInvalidState
			}
		case "prepare_publish":
			requestID, err := prepareRequestOperationID(detail.Request)
			expectedRevision := map[string]string{"publishing": "2", "completed": "3"}[r.State]
			if err != nil || requestID != r.SourceID || r.SourceID != r.JournalID || r.TargetID != detail.PackID || expectedRevision == "" || r.Revision != expectedRevision {
				return ErrInvalidState
			}
		case "save_pack":
			if r.SourceID != r.TargetID || r.Revision != "1" {
				return ErrInvalidState
			}
		default:
			return errors.New("unknown prepare publish journal purpose")
		}
	case JournalObjectReplace:
		if r.State != "pending" {
			return ErrInvalidState
		}
	case JournalCounterReservation:
		if r.State != "reserved" {
			return ErrInvalidState
		}
	case JournalCryptoDiscard:
		if r.State != "deleting" {
			return ErrInvalidState
		}
	case JournalKeyMigration:
		var detail keyMigrationDetail
		if err := decodeClosed(r.Detail, &detail); err != nil || detail.validate(r.State) != nil {
			return errors.New("invalid key migration journal")
		}
		if r.SourceID != detail.ProfileID || r.TargetID != detail.ProfileID {
			return errors.New("key migration journal profile mismatch")
		}
		expectedRevision := map[string]string{
			keyMigrationStatePrepared:            "1",
			keyMigrationStateDestinationVerified: "2",
			keyMigrationStateAuthoritySwitched:   "3",
			keyMigrationStateSourceCleaned:       "4",
			keyMigrationStateCompleted:           "5",
		}[r.State]
		if expectedRevision == "" || r.Revision != expectedRevision {
			return ErrInvalidState
		}
	default:
		return errors.New("unknown journal kind")
	}
	return nil
}

func allowedTransition(from, to LocalState) bool {
	if from == to {
		return true
	}
	switch from {
	case StateDraft:
		return to == StateQueued || to == StateDiscarded
	case StateQueued:
		return to == StateUploading || to == StateDiscarded
	case StateUploading:
		return to == StateQueued || to == StateArchivedPendingEvidence || to == StateTerminal || to == StateConflict || to == StateBlocked || to == StateDiscarded
	case StateArchivedPendingEvidence:
		return to == StateTerminal || to == StateBlocked || to == StateDiscarded
	case StateTerminal, StateConflict, StateBlocked:
		return to == StateDiscarded
	default:
		return false
	}
}

func canonicalObjectOrNull(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return json.RawMessage("null"), nil
	}
	canonical, err := requireCanonicalObject(value)
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), canonical...), nil
}

func formatRecordTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseRecordTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || formatRecordTime(parsed) != value {
		return time.Time{}, errors.New("record time is not canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func sortedUniqueUUIDs(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	for _, value := range result {
		if _, err := parseUUID(value); err != nil {
			return nil, err
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, errors.New("duplicate UUID")
		}
	}
	return result, nil
}

func decodeNoncePrefix(value string) ([4]byte, error) {
	var prefix [4]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(prefix) || value != strings.ToLower(value) {
		return prefix, errors.New("invalid nonce prefix")
	}
	copy(prefix[:], decoded)
	if prefix == ([4]byte{}) {
		return [4]byte{}, errors.New("zero nonce prefix")
	}
	return prefix, nil
}
