package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTranscriptCodecRoundTripAllDurableKinds(t *testing.T) {
	value := TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{
		transcriptEntry(1, TranscriptKindUser, "用户约束", true),
		{Sequence: 2, PresentationTurn: 1, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(2), Text: "最终回答", AssistantState: AssistantStateFinal, ModelCommitted: true, PresentationOnly: true},
		{Sequence: 3, PresentationTurn: 2, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(3), Text: "已停止的草稿", AssistantState: AssistantStateStopped, PresentationOnly: true},
		{Sequence: 4, PresentationTurn: 3, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(4), Text: "失败的草稿", AssistantState: AssistantStateFailed},
		{Sequence: 5, PresentationTurn: 3, Kind: TranscriptKindTool, CreatedAt: transcriptTimestamp(5), Tools: []TerminalToolActivityV1{{Name: "read_file", State: ToolStateCompleted, Summary: "读取完成"}, {Name: "write_file", State: ToolStateUnknown, Summary: "结果未知"}}},
		{Sequence: 6, PresentationTurn: 3, Kind: TranscriptKindError, CreatedAt: transcriptTimestamp(6), Error: &StableErrorV1{Code: "session_save_failed", Retryable: true}},
		{Sequence: 7, PresentationTurn: 3, Kind: TranscriptKindContext, CreatedAt: transcriptTimestamp(7), Context: &StableContextEventV1{Type: ContextEventDegraded, Message: "上下文已降级"}},
		{Sequence: 8, PresentationTurn: 3, Kind: TranscriptKindFileNotice, CreatedAt: transcriptTimestamp(8), Notice: &TypedNoticeV1{Code: "file_outcome_unknown", Outcome: NoticeOutcomeUnknown, Message: "文件结果未知"}},
		{Sequence: 9, PresentationTurn: 3, Kind: TranscriptKindPreferenceNotice, CreatedAt: transcriptTimestamp(9), PresentationOnly: true, Notice: &TypedNoticeV1{Code: "preference_saved", Outcome: NoticeOutcomeCompleted, Message: "偏好已保存"}},
		{Sequence: 10, Kind: TranscriptKindSessionNotice, CreatedAt: transcriptTimestamp(10), Notice: &TypedNoticeV1{Code: "session_interrupted", Outcome: NoticeOutcomeInterrupted, Message: "上次轮次中断"}},
	}}

	encoded, err := EncodeTranscript(value, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTranscript(encoded, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, value) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", decoded, value)
	}

	decoded.Entries[4].Tools[0].Summary = "changed"
	decoded.Entries[7].Notice.Message = "changed"
	if value.Entries[4].Tools[0].Summary != "读取完成" || value.Entries[7].Notice.Message != "文件结果未知" {
		t.Fatal("decoded transcript shares nested backing state")
	}
}

func TestTranscriptCodecRejectsUnsafeOrNonCanonicalData(t *testing.T) {
	stamp := "2026-09-02T00:00:00Z"
	basePrefix := `{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"user","created_at":"` + stamp + `","text":"safe","model_committed":false,"presentation_only":false`
	baseSuffix := `}]}`
	tests := map[string][]byte{
		"unknown top level":     []byte(`{"schema_version":1,"entries":[],"future":true}`),
		"unknown entry field":   []byte(basePrefix + `,"tool_arguments":{}` + baseSuffix),
		"raw tool result":       []byte(basePrefix + `,"raw_tool_result":"secret"` + baseSuffix),
		"preview":               []byte(basePrefix + `,"preview":"candidate"` + baseSuffix),
		"base version":          []byte(basePrefix + `,"base_version":"sha256:x"` + baseSuffix),
		"provider raw body":     []byte(basePrefix + `,"provider_raw_body":"secret"` + baseSuffix),
		"raw stable error body": []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"error","created_at":"` + stamp + `","model_committed":false,"presentation_only":false,"error":{"code":"provider_failed","retryable":true,"message":"raw provider response"}}]}`),
		"hidden reasoning":      []byte(basePrefix + `,"reasoning":"secret"` + baseSuffix),
		"trailing json":         []byte(`{"schema_version":1,"entries":[]} {}`),
		"running activity":      []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"running","created_at":"` + stamp + `","model_committed":false,"presentation_only":false}]}`),
		"tick":                  []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"tick","created_at":"` + stamp + `","model_committed":false,"presentation_only":false}]}`),
		"terminal escape":       transcriptUserJSON("terminal\u001b[31m", stamp),
		"bidi control":          transcriptUserJSON("bidi\u202eevil", stamp),
		"unix absolute path":    transcriptUserJSON("secret at /home/alice/file.txt", stamp),
		"windows absolute path": transcriptUserJSON(
			`secret at C:\\Users\\alice\\file.txt`, stamp),
		"duplicate sequence":          []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"user","created_at":"` + stamp + `","text":"one","model_committed":false,"presentation_only":false},{"sequence":1,"presentation_turn":2,"kind":"user","created_at":"` + stamp + `","text":"two","model_committed":false,"presentation_only":false}]}`),
		"decreasing sequence":         []byte(`{"schema_version":1,"entries":[{"sequence":2,"presentation_turn":1,"kind":"user","created_at":"` + stamp + `","text":"one","model_committed":false,"presentation_only":false},{"sequence":1,"presentation_turn":2,"kind":"user","created_at":"` + stamp + `","text":"two","model_committed":false,"presentation_only":false}]}`),
		"assistant running":           []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"assistant","created_at":"` + stamp + `","text":"draft","assistant_state":"running","model_committed":false,"presentation_only":false}]}`),
		"stopped committed":           []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"assistant","created_at":"` + stamp + `","text":"draft","assistant_state":"stopped","model_committed":true,"presentation_only":false}]}`),
		"running tool":                []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"tool","created_at":"` + stamp + `","model_committed":false,"presentation_only":false,"tools":[{"name":"read_file","state":"running","summary":"working"}]}]}`),
		"raw structured tool summary": []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"tool","created_at":"` + stamp + `","model_committed":false,"presentation_only":false,"tools":[{"name":"read_file","state":"completed","summary":"{\"secret\":true}"}]}]}`),
		"unknown marked presentation": []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"file_notice","created_at":"` + stamp + `","model_committed":false,"presentation_only":true,"notice":{"code":"file_outcome_unknown","outcome":"unknown","message":"unknown"}}]}`),
	}
	invalidUTF8 := []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"user","created_at":"` + stamp + `","text":"`)
	invalidUTF8 = append(invalidUTF8, 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`","model_committed":false,"presentation_only":false}]}`)...)
	tests["invalid utf8"] = invalidUTF8

	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeTranscript(data, Limits{}); err == nil {
				t.Fatal("unsafe transcript was accepted")
			}
		})
	}
}

func TestTranscriptCodecBoundsAndDeterministicCompaction(t *testing.T) {
	value := TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{
		transcriptEntry(1, TranscriptKindUser, "critical user constraint", false),
		{Sequence: 2, PresentationTurn: 1, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(2), Text: "old final", AssistantState: AssistantStateFinal, ModelCommitted: true, PresentationOnly: true},
		{Sequence: 3, PresentationTurn: 2, Kind: TranscriptKindFileNotice, CreatedAt: transcriptTimestamp(3), Notice: &TypedNoticeV1{Code: "file_outcome_unknown", Outcome: NoticeOutcomeUnknown, Message: "must remain"}},
		{Sequence: 4, PresentationTurn: 2, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(4), Text: "old stopped", AssistantState: AssistantStateStopped, PresentationOnly: true},
		{Sequence: 5, PresentationTurn: 2, Kind: TranscriptKindTool, CreatedAt: transcriptTimestamp(5), PresentationOnly: true, Tools: []TerminalToolActivityV1{{Name: "read_file", State: ToolStateCompleted, Summary: "done"}}},
		{Sequence: 6, Kind: TranscriptKindSessionNotice, CreatedAt: transcriptTimestamp(6), Notice: &TypedNoticeV1{Code: "session_interrupted", Outcome: NoticeOutcomeInterrupted, Message: "must remain"}},
	}}
	limits := Limits{TranscriptEntries: 4}
	first, err := CompactTranscript(value, limits)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompactTranscript(value, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("compaction is not deterministic")
	}
	if len(first.Entries) != 4 || first.Entries[1].Notice == nil || first.Entries[1].Notice.Code != transcriptCompactedCode || first.Entries[1].Notice.Count != 3 {
		t.Fatalf("compacted transcript=%+v", first.Entries)
	}
	if first.Entries[0].Text != "critical user constraint" || first.Entries[2].Notice == nil || first.Entries[2].Notice.Outcome != NoticeOutcomeUnknown || first.Entries[3].Notice == nil || first.Entries[3].Notice.Outcome != NoticeOutcomeInterrupted {
		t.Fatalf("critical entries were lost: %+v", first.Entries)
	}
	encoded, err := EncodeTranscript(value, limits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTranscript(encoded, limits)
	if err != nil || !reflect.DeepEqual(decoded, first) {
		t.Fatalf("compacted round trip=%+v err=%v", decoded, err)
	}

	criticalOnly := TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{
		transcriptEntry(1, TranscriptKindUser, "one", false),
		transcriptEntry(2, TranscriptKindUser, "two", false),
	}}
	if _, err := EncodeTranscript(criticalOnly, Limits{TranscriptEntries: 1}); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("critical-only compaction error=%v", err)
	}

	tooLarge := TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{transcriptEntry(1, TranscriptKindUser, strings.Repeat("x", 128), false)}}
	if _, err := EncodeTranscript(tooLarge, Limits{TranscriptEntryBytes: 96}); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("per-entry bound error=%v", err)
	}
	if _, err := DecodeTranscript([]byte(`{"schema_version":1,"entries":[]}`), Limits{TranscriptBytes: 16}); err == nil {
		t.Fatal("total byte bound was not enforced")
	}
	if _, err := EncodeTranscript(TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{transcriptEntry(1, TranscriptKindUser, "one\ntwo", false)}}, Limits{TranscriptEntryLines: 1}); err == nil {
		t.Fatal("line bound was not enforced")
	}
}

func TestSessionRecordAuthenticatesAndClonesTranscript(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	original := TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{transcriptEntry(1, TranscriptKindUser, "transcript-secret", false)}}
	blob, err := EncodeTranscript(original, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "transcript", Checkpoint: []byte(`{"v":1}`), Transcript: blob})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(store.rootPath, recordName(record.StorageID)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("transcript-secret")) {
		t.Fatal("transcript plaintext leaked outside the authenticated record")
	}

	blob[0] ^= 0x01
	record.Transcript[0] ^= 0x01
	loaded, err := handle.Load()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTranscript(loaded.Record.Transcript, Limits{})
	if err != nil || decoded.Entries[0].Text != "transcript-secret" {
		t.Fatalf("create transcript clone=%+v err=%v", decoded, err)
	}

	updatedValue := TranscriptV1{SchemaVersion: transcriptSchemaVersion, Entries: []TranscriptEntryV1{
		transcriptEntry(1, TranscriptKindUser, "transcript-secret", false),
		{Sequence: 2, PresentationTurn: 1, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(2), Text: "saved final", AssistantState: AssistantStateFinal, ModelCommitted: true},
	}}
	updatedBlob, err := EncodeTranscript(updatedValue, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	candidate := loaded.Record
	candidate.Transcript = updatedBlob
	saved, err := handle.Save(t.Context(), loaded.Record.RecordRevision, candidate)
	if err != nil {
		t.Fatal(err)
	}
	updatedBlob[0] ^= 0x01
	saved.Transcript[0] ^= 0x01
	reloaded, err := handle.Load()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = DecodeTranscript(reloaded.Record.Transcript, Limits{})
	if err != nil || len(decoded.Entries) != 2 || decoded.Entries[1].Text != "saved final" {
		t.Fatalf("save transcript clone=%+v err=%v", decoded, err)
	}

	badCandidate := reloaded.Record
	badCandidate.Transcript = []byte(`{"schema_version":1,"entries":[],"raw_tool_result":"secret"}`)
	if _, err := handle.Save(t.Context(), reloaded.Record.RecordRevision, badCandidate); err == nil {
		t.Fatal("invalid transcript was saved")
	}
	afterRejectedSave, err := handle.Load()
	if err != nil || afterRejectedSave.Record.RecordRevision != reloaded.Record.RecordRevision {
		t.Fatalf("rejected save changed record: %+v err=%v", afterRejectedSave.Record, err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.deleteFile(indexName); err != nil {
		t.Fatal(err)
	}
	summaries, err := store.List(t.Context())
	if err != nil || len(summaries) != 1 || summaries[0].SessionID != reloaded.Record.SessionID {
		t.Fatalf("index rebuild=%+v err=%v", summaries, err)
	}
	reopened, restored, err := store.OpenSession(t.Context(), reloaded.Record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := DecodeTranscript(restored.Record.Transcript, Limits{}); err != nil {
		t.Fatalf("restored transcript=%v", err)
	}
}

func TestSessionRecordRejectsInvalidTranscriptBeforePublication(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	if _, _, err := store.Create(t.Context(), CreateInput{
		Title: "invalid", Checkpoint: []byte(`{"v":1}`),
		Transcript: []byte(`{"schema_version":1,"entries":[],"provider_raw_body":"secret"}`),
	}); err == nil {
		t.Fatal("invalid transcript was accepted")
	}
	summaries, err := store.List(t.Context())
	if err != nil || len(summaries) != 0 {
		t.Fatalf("invalid transcript published a session: %+v err=%v", summaries, err)
	}

	handle, record, err := store.Create(t.Context(), CreateInput{Title: "empty", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	decoded, err := DecodeTranscript(record.Transcript, Limits{})
	if err != nil || len(decoded.Entries) != 0 {
		t.Fatalf("default transcript=%+v err=%v", decoded, err)
	}
}

func transcriptEntry(sequence uint64, kind, text string, presentationOnly bool) TranscriptEntryV1 {
	return TranscriptEntryV1{
		Sequence: sequence, PresentationTurn: sequence, Kind: kind, CreatedAt: transcriptTimestamp(sequence),
		Text: text, PresentationOnly: presentationOnly,
	}
}

func transcriptTimestamp(sequence uint64) time.Time {
	return time.Date(2026, 9, 2, 0, 0, int(sequence), 0, time.UTC)
}

func transcriptUserJSON(text, stamp string) []byte {
	encodedText, _ := json.Marshal(text)
	return []byte(`{"schema_version":1,"entries":[{"sequence":1,"presentation_turn":1,"kind":"user","created_at":"` + stamp + `","text":` + string(encodedText) + `,"model_committed":false,"presentation_only":false}]}`)
}
