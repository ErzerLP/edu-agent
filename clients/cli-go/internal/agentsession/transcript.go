package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

const (
	TranscriptKindUser             = "user"
	TranscriptKindAssistant        = "assistant"
	TranscriptKindTool             = "tool"
	TranscriptKindError            = "error"
	TranscriptKindContext          = "context"
	TranscriptKindFileNotice       = "file_notice"
	TranscriptKindPreferenceNotice = "preference_notice"
	TranscriptKindSessionNotice    = "session_notice"

	AssistantStateFinal   = "final"
	AssistantStateStopped = "stopped"
	AssistantStateFailed  = "failed"

	ToolStateCompleted = "completed"
	ToolStateStopped   = "stopped"
	ToolStateFailed    = "failed"
	ToolStateUnknown   = "unknown"

	ContextEventCompacted           = "compacted"
	ContextEventDegraded            = "degraded"
	ContextEventSourceUnavailable   = "source_unavailable"
	ContextEventPrivacyRevalidation = "privacy_revalidation_pending"

	NoticeOutcomeCompleted     = "completed"
	NoticeOutcomeFailed        = "failed"
	NoticeOutcomeUnknown       = "unknown"
	NoticeOutcomeRejected      = "rejected"
	NoticeOutcomeInterrupted   = "interrupted"
	NoticeOutcomeUnavailable   = "unavailable"
	NoticeOutcomeRequired      = "required"
	NoticeOutcomeInformational = "informational"

	transcriptCompactedCode = "transcript_compacted"
)

// TranscriptV1 is a durable presentation projection. It intentionally has no
// fields for running activity, raw tool payloads, previews, provider bodies, or
// hidden reasoning; strict decoding rejects attempts to add them.
type TranscriptV1 struct {
	SchemaVersion int                 `json:"schema_version"`
	Entries       []TranscriptEntryV1 `json:"entries"`
}

type TranscriptEntryV1 struct {
	Sequence         uint64                   `json:"sequence"`
	PresentationTurn uint64                   `json:"presentation_turn"`
	Kind             string                   `json:"kind"`
	CreatedAt        time.Time                `json:"created_at"`
	Text             string                   `json:"text,omitempty"`
	AssistantState   string                   `json:"assistant_state,omitempty"`
	ModelCommitted   bool                     `json:"model_committed"`
	PresentationOnly bool                     `json:"presentation_only"`
	Tools            []TerminalToolActivityV1 `json:"tools,omitempty"`
	Error            *StableErrorV1           `json:"error,omitempty"`
	Context          *StableContextEventV1    `json:"context,omitempty"`
	Notice           *TypedNoticeV1           `json:"notice,omitempty"`
}

type TerminalToolActivityV1 struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Summary string `json:"summary,omitempty"`
}

type StableErrorV1 struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

type StableContextEventV1 struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

type TypedNoticeV1 struct {
	Code    string `json:"code"`
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
	Count   uint64 `json:"count,omitempty"`
}

func canonicalTranscriptBlob(data []byte, limits Limits) (json.RawMessage, error) {
	if len(data) == 0 {
		return EncodeTranscript(TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{}}, limits)
	}
	value, err := DecodeTranscript(data, limits)
	if err != nil {
		return nil, err
	}
	return EncodeTranscript(value, limits)
}

// EncodeTranscript validates, deterministically compacts if necessary, and
// returns a canonical versioned JSON blob suitable for SessionRecord.
func EncodeTranscript(value TranscriptV1, limits Limits) (json.RawMessage, error) {
	compacted, err := CompactTranscript(value, limits)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(compacted)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

// DecodeTranscript strictly decodes a persisted transcript. Unknown fields,
// trailing JSON, malformed enums, unsafe terminal text, and bound violations
// fail closed.
func DecodeTranscript(data []byte, limits Limits) (TranscriptV1, error) {
	limits = normalizedLimits(limits)
	if len(data) == 0 || int64(len(data)) > limits.TranscriptBytes || !utf8.Valid(data) {
		return TranscriptV1{}, ErrCorrupt
	}
	var wire struct {
		SchemaVersion int             `json:"schema_version"`
		Entries       json.RawMessage `json:"entries"`
	}
	if err := decodeTranscriptStrict(data, &wire); err != nil || wire.SchemaVersion != transcriptSchemaVersion {
		return TranscriptV1{}, ErrCorrupt
	}
	trimmedEntries := bytes.TrimSpace(wire.Entries)
	if len(trimmedEntries) < 2 || trimmedEntries[0] != '[' || trimmedEntries[len(trimmedEntries)-1] != ']' {
		return TranscriptV1{}, ErrCorrupt
	}
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(trimmedEntries, &rawEntries); err != nil || rawEntries == nil || len(rawEntries) > limits.TranscriptEntries {
		return TranscriptV1{}, ErrCorrupt
	}
	value := TranscriptV1{SchemaVersion: wire.SchemaVersion, Entries: make([]TranscriptEntryV1, len(rawEntries))}
	for index, raw := range rawEntries {
		if err := decodeTranscriptStrict(raw, &value.Entries[index]); err != nil {
			return TranscriptV1{}, ErrCorrupt
		}
	}
	if err := validateTranscript(value, limits, true); err != nil {
		return TranscriptV1{}, ErrCorrupt
	}
	return cloneTranscript(value), nil
}

// CompactTranscript folds only entries explicitly marked presentation-only.
// Critical entries, including every unknown outcome notice, are never removed.
func CompactTranscript(value TranscriptV1, limits Limits) (TranscriptV1, error) {
	limits = normalizedLimits(limits)
	candidate := cloneTranscript(value)
	if candidate.SchemaVersion == 0 {
		candidate.SchemaVersion = transcriptSchemaVersion
	}
	if candidate.Entries == nil {
		candidate.Entries = []TranscriptEntryV1{}
	}
	if len(candidate.Entries) > limits.TranscriptEntries*2 {
		return TranscriptV1{}, ErrStoreFull
	}
	if err := validateTranscript(candidate, limits, false); err != nil {
		return TranscriptV1{}, err
	}
	for {
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return TranscriptV1{}, err
		}
		countOver := len(candidate.Entries) > limits.TranscriptEntries
		bytesOver := int64(len(encoded)) > limits.TranscriptBytes
		if !countOver && !bytesOver {
			if err := validateTranscript(candidate, limits, true); err != nil {
				return TranscriptV1{}, err
			}
			return cloneTranscript(candidate), nil
		}

		placeholder := transcriptCompactionPlaceholderIndex(candidate.Entries)
		if placeholder >= 0 {
			remove := nextPresentationOnly(candidate.Entries, placeholder+1)
			if remove < 0 {
				return TranscriptV1{}, ErrStoreFull
			}
			candidate.Entries[placeholder].Notice.Count++
			candidate.Entries[placeholder].Notice.Message = compactedTranscriptMessage(candidate.Entries[placeholder].Notice.Count)
			candidate.Entries = append(candidate.Entries[:remove], candidate.Entries[remove+1:]...)
			continue
		}

		first := nextPresentationOnly(candidate.Entries, 0)
		if first < 0 {
			return TranscriptV1{}, ErrStoreFull
		}
		removeCount := 1
		second := -1
		if countOver {
			second = nextPresentationOnly(candidate.Entries, first+1)
			if second < 0 {
				return TranscriptV1{}, ErrStoreFull
			}
			removeCount = 2
		}
		removed := candidate.Entries[first]
		placeholderEntry := TranscriptEntryV1{
			Sequence: removed.Sequence, PresentationTurn: removed.PresentationTurn,
			Kind: TranscriptKindSessionNotice, CreatedAt: removed.CreatedAt,
			Notice: &TypedNoticeV1{Code: transcriptCompactedCode, Outcome: NoticeOutcomeInformational, Count: uint64(removeCount), Message: compactedTranscriptMessage(uint64(removeCount))},
		}
		if second < 0 {
			candidate.Entries[first] = placeholderEntry
		} else {
			candidate.Entries[first] = placeholderEntry
			candidate.Entries = append(candidate.Entries[:second], candidate.Entries[second+1:]...)
		}
	}
}

func validateTranscript(value TranscriptV1, limits Limits, enforceCollectionBounds bool) error {
	if value.SchemaVersion != transcriptSchemaVersion || value.Entries == nil {
		return ErrInvalid
	}
	if enforceCollectionBounds && len(value.Entries) > limits.TranscriptEntries {
		return ErrStoreFull
	}
	var previous uint64
	placeholderSeen := false
	presentationBeforePlaceholder := false
	for index := range value.Entries {
		entry := value.Entries[index]
		if entry.Sequence == 0 || entry.Sequence <= previous || entry.CreatedAt.IsZero() {
			return ErrInvalid
		}
		previous = entry.Sequence
		if entry.PresentationOnly {
			presentationBeforePlaceholder = true
		}
		isPlaceholder := isTranscriptCompactionPlaceholder(entry)
		if isPlaceholder {
			if placeholderSeen || presentationBeforePlaceholder {
				return ErrInvalid
			}
			placeholderSeen = true
		}
		if err := validateTranscriptEntry(entry, limits); err != nil {
			return err
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		entryLimit := limits.TranscriptEventBytes
		if entry.Kind == TranscriptKindUser {
			entryLimit = limits.TranscriptEntryBytes
		} else if entry.Kind == TranscriptKindAssistant {
			entryLimit = limits.TranscriptAssistantJSONBytes
		}
		if len(encoded) > entryLimit {
			return ErrStoreFull
		}
	}
	if enforceCollectionBounds {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if int64(len(encoded)) > limits.TranscriptBytes {
			return ErrStoreFull
		}
	}
	return nil
}

func validateTranscriptEntry(entry TranscriptEntryV1, limits Limits) error {
	textLimit := limits.TranscriptEntryBytes
	textLimits := limits
	if entry.Kind == TranscriptKindAssistant {
		textLimit = limits.TranscriptAssistantBytes
		// A byte-bounded answer may consist entirely of short lines or one
		// long line. Display wraps it; neither layout may reject valid text.
		textLimits.TranscriptEntryLines = textLimit + 1
		textLimits.TranscriptLineColumns = textLimit
	}
	if len(entry.Text) > textLimit {
		return ErrStoreFull
	}
	for _, tool := range entry.Tools {
		if len(tool.Summary) > limits.TranscriptEventBytes {
			return ErrStoreFull
		}
	}
	if entry.Context != nil && len(entry.Context.Message) > limits.TranscriptEventBytes ||
		entry.Notice != nil && len(entry.Notice.Message) > limits.TranscriptEventBytes {
		return ErrStoreFull
	}
	if !timeIsCanonical(entry.CreatedAt) {
		return ErrInvalid
	}
	if entry.Kind != TranscriptKindSessionNotice && entry.PresentationTurn == 0 {
		return ErrInvalid
	}
	if entry.Kind != TranscriptKindAssistant && (entry.AssistantState != "" || entry.ModelCommitted) {
		return ErrInvalid
	}
	if (entry.Kind != TranscriptKindTool && len(entry.Tools) != 0) ||
		(entry.Kind != TranscriptKindError && entry.Error != nil) ||
		(entry.Kind != TranscriptKindContext && entry.Context != nil) ||
		(!isNoticeKind(entry.Kind) && entry.Notice != nil) {
		return ErrInvalid
	}

	switch entry.Kind {
	case TranscriptKindUser:
		if !validTranscriptText(entry.Text, true, limits.TranscriptEntryBytes, limits, false) || hasAuxiliaryPayload(entry) {
			return ErrInvalid
		}
	case TranscriptKindAssistant:
		if !validTranscriptText(entry.Text, true, textLimit, textLimits, false) || len(entry.Tools) != 0 || entry.Error != nil || entry.Context != nil || entry.Notice != nil {
			return ErrInvalid
		}
		switch entry.AssistantState {
		case AssistantStateFinal:
			if !entry.ModelCommitted {
				return ErrInvalid
			}
		case AssistantStateStopped, AssistantStateFailed:
			if entry.ModelCommitted {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	case TranscriptKindTool:
		if entry.Text != "" || len(entry.Tools) == 0 || len(entry.Tools) > limits.TranscriptTools || entry.Error != nil || entry.Context != nil || entry.Notice != nil {
			return ErrInvalid
		}
		for _, tool := range entry.Tools {
			if !validTranscriptName(tool.Name, 128, false) || !validToolState(tool.State) || !validTranscriptText(tool.Summary, false, limits.TranscriptEventBytes, limits, true) {
				return ErrInvalid
			}
		}
	case TranscriptKindError:
		if entry.Text != "" || entry.Error == nil || len(entry.Tools) != 0 || entry.Context != nil || entry.Notice != nil || !validTranscriptName(entry.Error.Code, 128, true) {
			return ErrInvalid
		}
	case TranscriptKindContext:
		if entry.Text != "" || entry.Context == nil || len(entry.Tools) != 0 || entry.Error != nil || entry.Notice != nil || !validContextEvent(entry.Context.Type) || !validTranscriptText(entry.Context.Message, false, limits.TranscriptEventBytes, limits, true) {
			return ErrInvalid
		}
	case TranscriptKindFileNotice, TranscriptKindPreferenceNotice, TranscriptKindSessionNotice:
		if entry.Text != "" || entry.Notice == nil || len(entry.Tools) != 0 || entry.Error != nil || entry.Context != nil || !validTranscriptName(entry.Notice.Code, 128, true) || !validNoticeOutcome(entry.Notice.Outcome) || !validTranscriptText(entry.Notice.Message, false, limits.TranscriptEventBytes, limits, true) {
			return ErrInvalid
		}
		if entry.Notice.Code == transcriptCompactedCode {
			if !isTranscriptCompactionPlaceholder(entry) {
				return ErrInvalid
			}
		} else if entry.Notice.Count != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if entry.PresentationOnly && !presentationCompactionAllowed(entry) {
		return ErrInvalid
	}
	return nil
}

func hasAuxiliaryPayload(entry TranscriptEntryV1) bool {
	return entry.AssistantState != "" || entry.ModelCommitted || len(entry.Tools) != 0 || entry.Error != nil || entry.Context != nil || entry.Notice != nil
}

func presentationCompactionAllowed(entry TranscriptEntryV1) bool {
	switch entry.Kind {
	case TranscriptKindUser:
		return true
	case TranscriptKindAssistant:
		return entry.AssistantState == AssistantStateFinal || entry.AssistantState == AssistantStateStopped
	case TranscriptKindTool:
		for _, tool := range entry.Tools {
			if tool.State == ToolStateFailed || tool.State == ToolStateUnknown {
				return false
			}
		}
		return true
	case TranscriptKindFileNotice, TranscriptKindPreferenceNotice, TranscriptKindSessionNotice:
		return entry.Notice != nil && entry.Notice.Code != transcriptCompactedCode && entry.Notice.Outcome != NoticeOutcomeUnknown && entry.Notice.Outcome != NoticeOutcomeInterrupted
	default:
		return false
	}
}

func nextPresentationOnly(entries []TranscriptEntryV1, start int) int {
	for index := start; index < len(entries); index++ {
		if entries[index].PresentationOnly {
			return index
		}
	}
	return -1
}

func transcriptCompactionPlaceholderIndex(entries []TranscriptEntryV1) int {
	for index, entry := range entries {
		if isTranscriptCompactionPlaceholder(entry) {
			return index
		}
	}
	return -1
}

func isTranscriptCompactionPlaceholder(entry TranscriptEntryV1) bool {
	return entry.Kind == TranscriptKindSessionNotice && !entry.PresentationOnly && !entry.ModelCommitted && entry.Notice != nil &&
		entry.Notice.Code == transcriptCompactedCode && entry.Notice.Outcome == NoticeOutcomeInformational && entry.Notice.Count > 0 &&
		entry.Notice.Message == compactedTranscriptMessage(entry.Notice.Count)
}

func compactedTranscriptMessage(count uint64) string {
	return fmt.Sprintf("较早的 %d 条展示记录已收起", count)
}

func validTranscriptText(value string, required bool, maxBytes int, limits Limits, rejectStructuredBody bool) bool {
	if !utf8.ValidString(value) || len(value) > maxBytes || required && strings.TrimSpace(value) == "" {
		return false
	}
	if rejectStructuredBody {
		trimmed := strings.TrimSpace(value)
		if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
			return false
		}
	}
	lines := strings.Split(value, "\n")
	if len(lines) > limits.TranscriptEntryLines {
		return false
	}
	for _, line := range lines {
		if runewidth.StringWidth(line) > limits.TranscriptLineColumns {
			return false
		}
	}
	for _, current := range value {
		if unicode.IsControl(current) || isBidiControl(current) {
			if current != '\n' {
				return false
			}
		}
	}
	return !containsAbsolutePath(value)
}

func containsAbsolutePath(value string) bool {
	if strings.Contains(strings.ToLower(value), "file:///") {
		return true
	}
	parts := strings.FieldsFunc(value, func(current rune) bool {
		return unicode.IsSpace(current) || strings.ContainsRune("\"'`()[]{}<>|,;", current)
	})
	for _, part := range parts {
		part = strings.Trim(part, ".:!?")
		if len(part) >= 3 && ((part[0] >= 'a' && part[0] <= 'z') || (part[0] >= 'A' && part[0] <= 'Z')) && part[1] == ':' && (part[2] == '/' || part[2] == '\\') {
			return true
		}
		if strings.HasPrefix(part, `\\`) || strings.HasPrefix(part, "//") {
			return true
		}
		if strings.HasPrefix(part, "/") && len(part) > 1 {
			return true
		}
	}
	return false
}

func validTranscriptName(value string, maxBytes int, lowercase bool) bool {
	if value == "" || len(value) > maxBytes || lowercase && strings.ToLower(value) != value {
		return false
	}
	for _, current := range value {
		if current >= 'a' && current <= 'z' || !lowercase && current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '-' || current == '_' || current == '.' {
			continue
		}
		return false
	}
	return true
}

func validToolState(value string) bool {
	return value == ToolStateCompleted || value == ToolStateStopped || value == ToolStateFailed || value == ToolStateUnknown
}

func validContextEvent(value string) bool {
	return value == ContextEventCompacted || value == ContextEventDegraded || value == ContextEventSourceUnavailable || value == ContextEventPrivacyRevalidation
}

func validNoticeOutcome(value string) bool {
	switch value {
	case NoticeOutcomeCompleted, NoticeOutcomeFailed, NoticeOutcomeUnknown, NoticeOutcomeRejected, NoticeOutcomeInterrupted, NoticeOutcomeUnavailable, NoticeOutcomeRequired, NoticeOutcomeInformational:
		return true
	default:
		return false
	}
}

func isNoticeKind(value string) bool {
	return value == TranscriptKindFileNotice || value == TranscriptKindPreferenceNotice || value == TranscriptKindSessionNotice
}

func timeIsCanonical(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0
}

func decodeTranscriptStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	return nil
}

func cloneTranscript(value TranscriptV1) TranscriptV1 {
	result := TranscriptV1{SchemaVersion: value.SchemaVersion, Entries: make([]TranscriptEntryV1, len(value.Entries))}
	for index, entry := range value.Entries {
		result.Entries[index] = entry
		result.Entries[index].Tools = append([]TerminalToolActivityV1(nil), entry.Tools...)
		if entry.Error != nil {
			copyValue := *entry.Error
			result.Entries[index].Error = &copyValue
		}
		if entry.Context != nil {
			copyValue := *entry.Context
			result.Entries[index].Context = &copyValue
		}
		if entry.Notice != nil {
			copyValue := *entry.Notice
			result.Entries[index].Notice = &copyValue
		}
	}
	return result
}
