package agentsession

import (
	"errors"
	"strings"
	"testing"
)

func TestLargeOutputTranscriptRoundTripAndQuotas(t *testing.T) {
	limits := DefaultLimits()
	for name, text := range map[string]string{
		"many lines":       strings.Repeat("line\n", 30000),
		"one long line":    strings.Repeat("x", limits.TranscriptAssistantBytes),
		"escaped max body": strings.Repeat("<", limits.TranscriptAssistantBytes),
	} {
		t.Run(name, func(t *testing.T) {
			value := TranscriptV1{SchemaVersion: 1, Entries: []TranscriptEntryV1{{Sequence: 1, PresentationTurn: 1, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(1), Text: text, AssistantState: AssistantStateFinal, ModelCommitted: true}}}
			data, err := EncodeTranscript(value, limits)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeTranscript(data, limits)
			if err != nil || decoded.Entries[0].Text != text {
				t.Fatalf("round trip err=%v", err)
			}
			if _, err := EncodeTranscript(value, Limits{TranscriptBytes: 1024}); !errors.Is(err, ErrStoreFull) {
				t.Fatalf("quota failure=%v", err)
			}
			if _, err := EncodeTranscript(value, Limits{TranscriptAssistantJSONBytes: 1024}); !errors.Is(err, ErrStoreFull) {
				t.Fatalf("entry JSON quota=%v", err)
			}
		})
	}
	tooLarge := TranscriptV1{SchemaVersion: 1, Entries: []TranscriptEntryV1{{Sequence: 1, PresentationTurn: 1, Kind: TranscriptKindAssistant, CreatedAt: transcriptTimestamp(1), Text: strings.Repeat("x", limits.TranscriptAssistantBytes+1), AssistantState: AssistantStateFinal, ModelCommitted: true}}}
	if _, err := EncodeTranscript(tooLarge, limits); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("source quota err=%v", err)
	}
	// User/event limits, strict legacy DTOs and unknown versions are not broadened.
	if limits.TranscriptEntryBytes != 64<<10 || limits.TranscriptEntryLines != 1024 || limits.TranscriptEventBytes != 16<<10 || limits.AutoTitleMaxTokens != 96 {
		t.Fatal("unrelated limits changed")
	}
	for _, data := range []string{`{"schema_version":2,"entries":[]}`, `{"schema_version":1,"entries":[],"hidden_reasoning":"secret"}`} {
		if _, err := DecodeTranscript([]byte(data), limits); err == nil {
			t.Fatal("future or unknown field accepted")
		}
	}
}
