package agentui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func liveThinking(id string, status agentloop.EventStatus) agentloop.Activity {
	return agentloop.Activity{Kind: agentloop.ActivityThinking, Phase: agentloop.ActivityReceivingStream, Event: agentloop.Event{ID: id, Summary: "正在接收模型响应", Status: status}}
}

func TestLiveReasoningToggleWorkerCompletionAndSessionSwitch(t *testing.T) {
	const marker = "private-ui-reasoning"
	for _, failure := range []error{nil, context.Canceled, context.DeadlineExceeded} {
		activities := []agentloop.Activity{liveThinking("thinking-1", agentloop.EventRunning), {Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "thinking-1"}, Delta: marker + "\x1b[2J"}}
		if failure == nil {
			activities = append(activities, liveThinking("thinking-1", agentloop.EventSucceeded))
		}
		conversation := &fakeConversation{activities: activities, result: agentloop.Result{Text: "public answer"}, sendErr: failure}
		value := newModel(t.Context(), conversation, "model")
		value.input.SetValue("hello")
		updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
		value = runTurn(t, updated.(model), command)
		render := func() string {
			var b strings.Builder
			for _, e := range value.entries {
				b.WriteString(renderTranscriptEntry(e, 100, value.toolsExpanded))
			}
			return b.String()
		}
		if value.toolsExpanded || strings.Contains(render(), marker) {
			t.Fatal("reasoning visible by default")
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
		value = updated.(model)
		if !value.toolsExpanded || !strings.Contains(render(), marker) || strings.ContainsRune(render(), '\x1b') {
			t.Fatal("Ctrl+O did not safely reveal reasoning")
		}
		var oldSize int
		for _, entry := range value.entries {
			if entry.kind == entryThinking {
				if entry.activity.Event.Status == agentloop.EventRunning {
					t.Fatal("worker left thinking running")
				}
				oldSize = entry.reasoning.text.Len()
			}
			encoded, _ := json.Marshal(entry.activity)
			if strings.Contains(string(encoded), marker) || strings.Contains(entry.text, marker) {
				t.Fatal("reasoning mixed into activity metadata/assistant")
			}
		}
		// Terminal entries reject late deltas even before turn/generation filtering.
		value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "thinking-1"}, Delta: "LATE"})
		for _, e := range value.entries {
			if e.reasoning != nil && e.reasoning.text.Len() != oldSize {
				t.Fatal("late delta reopened settled reasoning")
			}
		}
		updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
		value = updated.(model)
		if strings.Contains(render(), marker) {
			t.Fatal("collapse left text visible")
		}
		oldGeneration := value.generation
		value.resetAfterSessionSwap(oldGeneration + 1)
		late := agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "thinking-1"}, Delta: marker}
		updated, _ = value.Update(turnMsg{generation: oldGeneration, turnID: 1, activity: &late})
		value = updated.(model)
		if value.toolsExpanded || strings.Contains(render(), marker) {
			t.Fatal("reasoning survived session swap")
		}
		for _, e := range value.entries {
			if e.reasoning != nil {
				t.Fatal("old reasoning retained after swap")
			}
		}
	}
}

func TestLiveReasoningBoundedCacheAndNoGapConcatenation(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	value.entries = nil
	value.handleActivity(1, liveThinking("old", agentloop.EventRunning))
	value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "old"}, Delta: strings.Repeat("a", liveReasoningBytes-8)})
	value.handleActivity(1, liveThinking("old", agentloop.EventSucceeded))
	value.handleActivity(2, liveThinking("new", agentloop.EventRunning))
	value.handleActivity(2, agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "new"}, Delta: strings.Repeat("你", liveReasoningBytes/3+10)})
	value.handleActivity(2, agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "new"}, Delta: "AFTER-GAP"})
	old, current := value.entries[0].reasoning, value.entries[1].reasoning
	if old.text.Len() != 0 || !old.evicted || !current.truncated || current.text.Len() > liveReasoningBytes || !utf8.ValidString(current.text.String()) || strings.Contains(current.text.String(), "AFTER-GAP") {
		t.Fatal("cache bound or prefix contract violated")
	}
	if !strings.Contains(renderThinkingEntry(value.entries[0], 80, true), "回收") || !strings.Contains(renderThinkingEntry(value.entries[1], 80, true), "截断") {
		t.Fatal("retention loss undisclosed")
	}
}

func TestLiveReasoningTransportProgressDoesNotClaimValidatedResponse(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	value.entries = nil
	thinking := liveThinking("one", agentloop.EventRunning)
	thinking.Phase = agentloop.ActivityWaitingModel
	value.handleActivity(1, thinking)
	now := time.Now()
	value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityResponseProgress, Event: agentloop.Event{ID: "one"}, ReceivedAt: now})
	if len(value.entries) != 1 || value.entries[0].activity.Phase != agentloop.ActivityWaitingModel || !value.entries[0].lastResponseAt.Equal(now) {
		t.Fatal("transport activity created card or claimed validated content")
	}
	shown := renderThinkingEntry(value.entries[0], 100, true)
	if !strings.Contains(shown, "含心跳") || !strings.Contains(shown, "尚未提供") {
		t.Fatal("missing honest no-reasoning/progress state")
	}
	value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "unknown"}, Delta: "wrong"})
	if len(value.entries) != 1 || value.entries[0].reasoning != nil {
		t.Fatal("unknown request created reasoning")
	}
}

func TestLiveReasoningToggleWhileRunningPreservesScroll(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	value.entries = nil
	value.busy, value.activeCancelable, value.activeTurnID = true, true, 1
	value.handleActivity(1, liveThinking("running", agentloop.EventRunning))
	value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "running"}, Delta: strings.Repeat("推理正在逐步返回\n", 100)})
	value.refreshTranscript(true)
	updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	value = updated.(model)
	if !value.toolsExpanded || !value.busy || len(value.entries) != 1 || !strings.Contains(value.viewport.View(), "推理正在逐步返回") {
		t.Fatal("running Ctrl+O did not reveal the current card")
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	value = updated.(model)
	if value.follow {
		t.Fatal("scrolling did not pause follow")
	}
	offset := value.viewport.YOffset
	delta := agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "running"}, Delta: "新推理\n"}
	updated, _ = value.Update(turnMsg{generation: value.generation, turnID: 1, activity: &delta, stream: &turnStream{}})
	value = updated.(model)
	if value.follow || value.viewport.YOffset != offset || !value.hasNewContent {
		t.Fatal("reasoning update stole scroll position")
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	value = updated.(model)
	if value.toolsExpanded || !value.busy || strings.Contains(value.viewport.View(), "推理正在逐步返回") {
		t.Fatal("collapse modified active operation or left body visible")
	}
}
