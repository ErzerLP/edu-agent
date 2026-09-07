package agentui

import (
	"context"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestThinkingLifecycleUpdatesCurrentTranscriptEntry(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			value := newModel(t.Context(), &fakeConversation{}, "model")
			value.entries = nil // Isolate this turn from startup notices.
			value.busy, value.activeCancelable = true, true
			value.activeTurnID = 1
			previous := agentloop.Activity{Kind: agentloop.ActivityThinking, Phase: agentloop.ActivityAssemblingTools, Event: agentloop.Event{ID: "thinking-1", Summary: "已确定下一步工具操作", Status: agentloop.EventSucceeded}}
			value.handleActivity(1, previous)
			value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool, Event: agentloop.Event{ID: "call-1", Tool: "list", Summary: "返回 1 项", Status: agentloop.EventSucceeded}})
			for _, phase := range []agentloop.ActivityPhase{agentloop.ActivityContinuingAfterTool, agentloop.ActivityPreparingContext, agentloop.ActivityWaitingModel, agentloop.ActivityReceivingStream, agentloop.ActivityValidatingResponse} {
				value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityThinking, Phase: phase, Event: agentloop.Event{ID: "thinking-2", Summary: "当前请求", Status: agentloop.EventRunning}})
				if len(value.entries) != 3 || value.entries[2].activity.Phase != phase || value.entries[2].activity.Event.ID != "thinking-2" {
					t.Fatalf("phase %s appended rather than updated current thinking: %+v", phase, value.entries)
				}
				if value.entries[0].activity != previous {
					t.Fatal("previous completed thinking was overwritten")
				}
			}
			if outcome == "cancel" || outcome == "timeout" {
				completionError := context.DeadlineExceeded
				if outcome == "cancel" {
					value.stopping = true
					completionError = context.Canceled
				}
				value.finishTurn(agentloop.Result{}, completionError)
			} else {
				status := agentloop.EventSucceeded
				if outcome == "failure" {
					status = agentloop.EventFailed
				}
				value.handleActivity(1, agentloop.Activity{Kind: agentloop.ActivityThinking, Phase: agentloop.ActivityValidatingResponse, Event: agentloop.Event{ID: "thinking-2", Summary: "当前请求已结束", Status: status}})
				if len(value.entries) != 3 || value.entries[2].activity.Event.Status != status {
					t.Fatalf("terminal activity not updated in place: %+v", value.entries)
				}
			}
			count := 0
			for _, entry := range value.entries {
				if entry.kind != entryThinking {
					continue
				}
				count++
				if entry.activity.Event.Status == agentloop.EventRunning {
					t.Fatalf("running thinking survived %s: %+v", outcome, entry)
				}
				if outcome == "cancel" && entry.activity.Event.ID == "thinking-2" && entry.activity.Phase != agentloop.ActivityStopped {
					t.Fatalf("cancellation lost stopped phase: %+v", entry)
				}
			}
			if count != 2 {
				t.Fatalf("thinking entries=%d, want two independent model requests", count)
			}
		})
	}
}
