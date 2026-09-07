package agentui

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestLiveReasoningQueuedDeltasPreserveOrderAndBoundaries(t *testing.T) {
	stream := &turnStream{activities: make(chan agentloop.Activity, 128), completion: make(chan turnMsg, 1), wake: make(chan struct{}, 1)}
	stream.activities <- liveThinking("one", agentloop.EventRunning)
	for range 60 {
		stream.activities <- agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "one"}, Delta: "你"}
	}
	stream.activities <- agentloop.Activity{Kind: agentloop.ActivityResponseProgress, Event: agentloop.Event{ID: "one"}}
	for range 60 {
		stream.activities <- agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "one"}, Delta: "好"}
	}
	stream.activities <- liveThinking("one", agentloop.EventSucceeded)
	stream.activities <- liveThinking("two", agentloop.EventRunning)
	stream.activities <- agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "two"}, Delta: "second"}
	stream.activities <- liveThinking("two", agentloop.EventSucceeded)
	stream.completion <- turnMsg{done: true}
	var got []agentloop.Activity
	for range 20 {
		msg := waitTurnCmdForGeneration(t.Context(), 1, 1, turnSend, stream)().(turnMsg)
		if msg.done {
			break
		}
		if msg.activity != nil {
			got = append(got, *msg.activity)
		}
	}
	if len(got) != 8 || got[0].Event.Status != agentloop.EventRunning || got[1].Delta != strings.Repeat("你", 60) || got[2].Kind != agentloop.ActivityResponseProgress || got[3].Delta != strings.Repeat("好", 60) || got[4].Event.Status != agentloop.EventSucceeded || got[5].Event.ID != "two" || got[6].Delta != "second" || got[7].Event.Status != agentloop.EventSucceeded {
		t.Fatal("coalescing lost/reordered body or lifecycle")
	}
}

func TestLiveReasoningCoalescedBatchDoesNotExceedBudget(t *testing.T) {
	stream := &turnStream{activities: make(chan agentloop.Activity, 4)}
	for _, text := range []string{strings.Repeat("a", 32<<10), strings.Repeat("b", 32<<10), "tail"} {
		stream.activities <- agentloop.Activity{Kind: agentloop.ActivityReasoningDelta, Event: agentloop.Event{ID: "one"}, Delta: text}
	}
	stream.activities <- liveThinking("one", agentloop.EventSucceeded)
	first, ok := stream.popActivity()
	if !ok || len(first.Delta) != 64<<10 {
		t.Fatal("unbounded or incomplete first batch")
	}
	next, ok := stream.popActivity()
	if !ok || next.Delta != "tail" {
		t.Fatal("tail was lost")
	}
	end, ok := stream.popActivity()
	if !ok || end.Event.Status != agentloop.EventSucceeded {
		t.Fatal("terminal event was lost")
	}
}
