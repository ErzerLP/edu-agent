package agentui

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
)

func TestLargeOutputTranscriptDisplayAndRestore(t *testing.T) {
	text := strings.Repeat("long answer line\n", 8193) + "FINAL-MARKER"
	entries := []transcriptEntry{}
	for i := 0; i < 8193; i++ {
		entries = upsertAssistantDelta(entries, 1, "long answer line\n")
	}
	entries = upsertAssistantDelta(entries, 1, "FINAL-MARKER")
	entries = finalizeAssistant(entries, 1, text)
	if len(entries) != 1 || entries[0].text != text || entries[0].streaming {
		t.Fatal("long display was truncated")
	}
	rendered := renderTranscriptEntry(entries[0], 80, false)
	if !strings.Contains(rendered, "FINAL-MARKER") || strings.Count(rendered, "long answer line") != 8193 {
		t.Fatal("long multiline rendering truncated")
	}
	provider := &pickerConversation{fakeConversation: &fakeConversation{}, transcript: agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{{Kind: agentsession.TranscriptKindAssistant, Text: text, PresentationTurn: 1, AssistantState: agentsession.AssistantStateFinal, ModelCommitted: true}}}}
	restored := durableTranscriptEntries(provider)
	if len(restored) != 1 || restored[0].text != text {
		t.Fatal("restored display lost text")
	}
	degraded := renderContextEvent(agentloop.ContextEvent{Kind: agentloop.ContextEventDegraded, Code: "context_history_projected"}, 80)
	if !strings.Contains(degraded, "带来源") || !strings.Contains(degraded, "工具") {
		t.Fatal("projection degradation not visible")
	}
}
