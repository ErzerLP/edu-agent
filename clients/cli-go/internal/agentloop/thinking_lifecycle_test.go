package agentloop

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestThinkingLifecycleAfterTools(t *testing.T) {
	for _, tc := range []struct {
		name            string
		rounds          int
		modelErr        error
		limit           int
		cancelAfterTool bool
	}{
		{name: "final answer", rounds: 1},
		{name: "multiple tool rounds", rounds: 3},
		{name: "model failure", rounds: 1, modelErr: errors.New("provider unavailable")},
		{name: "model timeout", rounds: 1, modelErr: context.DeadlineExceeded},
		{name: "round limit", rounds: 1, limit: 1},
		{name: "cancel before next request", rounds: 1, cancelAfterTool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &scriptedStreamingModel{}
			for i := 0; i < tc.rounds; i++ {
				id := fmt.Sprintf("call-%d", i)
				model.steps = append(model.steps, func(_ context.Context, _ modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
					if err := observe(modelclient.StreamEvent{Kind: modelclient.StreamEventResponseStarted}); err != nil {
						return modelclient.Response{}, err
					}
					return modelclient.Response{Message: toolMessage(id, "search_knowledge", `{"query":"图论"}`)}, nil
				})
			}
			model.steps = append(model.steps, func(_ context.Context, _ modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
				if err := observe(modelclient.StreamEvent{Kind: modelclient.StreamEventResponseStarted}); err != nil {
					return modelclient.Response{}, err
				}
				return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "回答完成"}}, tc.modelErr
			})
			server := &fakeServer{}
			session := newTestSession(t, model, server)
			if tc.limit > 0 {
				session.options.MaxToolRounds = tc.limit
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var activities []Activity
			ctx = WithActivityReporter(ctx, func(a Activity) {
				activities = append(activities, a)
				if tc.cancelAfterTool && a.Kind == ActivityTool && a.Event.Status == EventSucceeded {
					cancel()
				}
			})
			result, err := session.Send(ctx, "解释图论")
			wantError := tc.modelErr != nil || tc.limit > 0 || tc.cancelAfterTool
			if (err != nil) != wantError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.modelErr != nil && !errors.Is(err, tc.modelErr) {
				t.Fatalf("model error changed: %v", err)
			}
			if tc.cancelAfterTool && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error changed: %v", err)
			}
			if !wantError && result.Text != "回答完成" {
				t.Fatalf("result=%+v", result)
			}
			wantRequests := tc.rounds + 1
			if tc.limit > 0 || tc.cancelAfterTool {
				wantRequests = tc.rounds
			}
			if len(model.snapshotRequests()) != wantRequests || server.retrieveCalls != tc.rounds {
				t.Fatalf("requests=%d tools=%d", len(model.snapshotRequests()), server.retrieveCalls)
			}

			latest := map[string]Activity{}
			phases := map[string][]ActivityPhase{}
			var ids []string
			maxRunning := 0
			for _, a := range activities {
				if a.Kind != ActivityThinking {
					continue
				}
				if a.Event.ID == "turn-stop" {
					if !tc.cancelAfterTool || (a.Phase != ActivityStopping && a.Phase != ActivityStopped) {
						t.Fatalf("unexpected cancellation activity: %+v", a)
					}
					continue // Separate, existing turn cancellation lifecycle, not a model request.
				}
				if _, exists := latest[a.Event.ID]; !exists {
					ids = append(ids, a.Event.ID)
				}
				latest[a.Event.ID] = a
				phases[a.Event.ID] = append(phases[a.Event.ID], a.Phase)
				running := 0
				for _, current := range latest {
					if current.Event.Status == EventRunning {
						running++
					}
				}
				maxRunning = max(maxRunning, running)
			}
			remaining := []string{}
			for id, a := range latest {
				if a.Event.Status == EventRunning {
					remaining = append(remaining, id)
				}
			}
			if maxRunning > 1 || len(remaining) > 0 {
				t.Errorf("max simultaneous running thinking=%d; unsettled thinking=%v", maxRunning, remaining)
			}
			if len(ids) != wantRequests {
				t.Errorf("thinking IDs=%v; want %d request IDs", ids, wantRequests)
			}
			for i, id := range ids {
				if !strings.HasPrefix(id, "thinking-") {
					t.Errorf("orphan activity ID %q", id)
					continue
				}
				wantPhases := []ActivityPhase{ActivityPreparingContext, ActivityWaitingModel, ActivityReceivingStream, ActivityValidatingResponse}
				if i > 0 {
					wantPhases = append([]ActivityPhase{ActivityContinuingAfterTool}, wantPhases...)
				}
				wantStatus := EventSucceeded
				if i < tc.rounds {
					wantPhases = append(wantPhases, ActivityAssemblingTools)
				} else if tc.modelErr != nil {
					wantStatus = EventFailed
				} else {
					wantPhases = append(wantPhases, ActivityValidatingResponse)
				}
				if !reflect.DeepEqual(phases[id], wantPhases) || latest[id].Event.Status != wantStatus {
					t.Errorf("%s phases=%v status=%s; want phases=%v status=%s", id, phases[id], latest[id].Event.Status, wantPhases, wantStatus)
				}
			}
		})
	}
}
