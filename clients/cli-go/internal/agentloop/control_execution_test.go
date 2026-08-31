package agentloop

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type streamStep func(context.Context, modelclient.Request, func(modelclient.StreamEvent) error) (modelclient.Response, error)

type scriptedStreamingModel struct {
	mu       sync.Mutex
	steps    []streamStep
	requests []modelclient.Request
}

func (m *scriptedStreamingModel) Complete(context.Context, modelclient.Request) (modelclient.Response, error) {
	return modelclient.Response{}, errors.New("unexpected non-streaming request")
}

func (m *scriptedStreamingModel) Stream(ctx context.Context, request modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
	m.mu.Lock()
	if len(m.steps) == 0 {
		m.mu.Unlock()
		return modelclient.Response{}, errors.New("streaming model has no response")
	}
	step := m.steps[0]
	m.steps = m.steps[1:]
	m.requests = append(m.requests, cloneControlRequest(request))
	m.mu.Unlock()
	return step(ctx, request, observe)
}

func (m *scriptedStreamingModel) snapshotRequests() []modelclient.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	requests := make([]modelclient.Request, len(m.requests))
	for index := range m.requests {
		requests[index] = cloneControlRequest(m.requests[index])
	}
	return requests
}

type completeStep func(context.Context, modelclient.Request) (modelclient.Response, error)

type scriptedCompleteModel struct {
	mu       sync.Mutex
	steps    []completeStep
	requests []modelclient.Request
}

func (m *scriptedCompleteModel) Complete(ctx context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	if len(m.steps) == 0 {
		m.mu.Unlock()
		return modelclient.Response{}, errors.New("complete model has no response")
	}
	step := m.steps[0]
	m.steps = m.steps[1:]
	m.requests = append(m.requests, cloneControlRequest(request))
	m.mu.Unlock()
	return step(ctx, request)
}

func (m *scriptedCompleteModel) snapshotRequests() []modelclient.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	requests := make([]modelclient.Request, len(m.requests))
	for index := range m.requests {
		requests[index] = cloneControlRequest(m.requests[index])
	}
	return requests
}

func cloneControlRequest(request modelclient.Request) modelclient.Request {
	clone := request
	clone.Messages = make([]modelclient.Message, len(request.Messages))
	for index := range request.Messages {
		clone.Messages[index] = cloneModelMessage(request.Messages[index])
	}
	clone.Tools = append([]modelclient.Tool(nil), request.Tools...)
	return clone
}

func TestStreamingModelPublishesDeltasAndCommitsOnlyCompleteAssistant(t *testing.T) {
	deltaPublished := make(chan struct{})
	release := make(chan struct{})
	model := &scriptedStreamingModel{steps: []streamStep{
		func(ctx context.Context, _ modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
			if err := observe(modelclient.StreamEvent{Kind: modelclient.StreamEventResponseStarted}); err != nil {
				return modelclient.Response{}, err
			}
			if err := observe(modelclient.StreamEvent{Kind: modelclient.StreamEventTextDelta, Text: "即时增量"}); err != nil {
				return modelclient.Response{}, err
			}
			close(deltaPublished)
			select {
			case <-release:
			case <-ctx.Done():
				return modelclient.Response{}, ctx.Err()
			}
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "完整回答"}}, nil
		},
	}}
	session := newTestSession(t, model, &fakeServer{})
	activityCh := make(chan Activity, 16)
	ctx := WithActivityReporter(t.Context(), func(activity Activity) { activityCh <- activity })
	resultCh := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := session.Send(ctx, "测试流式回答")
		resultCh <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-deltaPublished:
	case <-time.After(time.Second):
		t.Fatal("text delta was not published promptly")
	}
	foundDelta := false
	for !foundDelta {
		select {
		case activity := <-activityCh:
			foundDelta = activity.Kind == ActivityTextDelta && activity.Delta == "即时增量"
		case <-time.After(time.Second):
			t.Fatal("visible text delta activity was not observed")
		}
	}
	session.appendMu.Lock()
	for _, message := range session.messages {
		if message.Role == "assistant" && message.Content != "" {
			session.appendMu.Unlock()
			t.Fatalf("assistant committed before stream completion: %+v", message)
		}
	}
	session.appendMu.Unlock()
	close(release)
	outcome := <-resultCh
	if outcome.err != nil || outcome.result.Text != "完整回答" {
		t.Fatalf("result=%+v err=%v", outcome.result, outcome.err)
	}
	session.appendMu.Lock()
	last := session.messages[len(session.messages)-1]
	session.appendMu.Unlock()
	if last.Role != "assistant" || last.Content != "完整回答" {
		t.Fatalf("committed assistant=%+v", last)
	}
}

func TestFinalAnswerCommitLinearizesWithCancellation(t *testing.T) {
	t.Run("cancel wins before commit", func(t *testing.T) {
		model := &scriptedCompleteModel{steps: []completeStep{func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "不应提交"}}, nil
		}}}
		session := newTestSession(t, model, &fakeServer{})
		commitPending := make(chan struct{})
		releaseReporter := make(chan struct{})
		var once sync.Once
		ctx, cancel := context.WithCancel(t.Context())
		ctx = WithActivityReporter(ctx, func(activity Activity) {
			if activity.Phase == ActivityValidatingResponse && activity.Event.Status == EventRunning && activity.Event.Summary == "正在校验模型响应" {
				once.Do(func() { close(commitPending) })
				<-releaseReporter
			}
		})
		resultCh := make(chan error, 1)
		go func() { _, err := session.Send(ctx, "取消提交"); resultCh <- err }()
		select {
		case <-commitPending:
		case <-time.After(time.Second):
			t.Fatal("response did not reach pre-commit barrier")
		}
		cancel()
		close(releaseReporter)
		if err := <-resultCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel-before-commit err=%v", err)
		}
		assertNoActiveTurnFragments(t, session)
	})

	t.Run("commit wins before late cancel", func(t *testing.T) {
		model := &scriptedCompleteModel{steps: []completeStep{func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已经提交"}}, nil
		}}}
		session := newTestSession(t, model, &fakeServer{})
		ctx, cancel := context.WithCancel(t.Context())
		ctx = WithActivityReporter(ctx, func(activity Activity) {
			if activity.Phase == ActivityValidatingResponse && activity.Event.Status == EventSucceeded && activity.Event.Summary == "已完成回答组织" {
				cancel()
			}
		})
		result, err := session.Send(ctx, "完成提交")
		if err != nil || result.Text != "已经提交" {
			t.Fatalf("late cancellation overrode committed answer: result=%+v err=%v", result, err)
		}
		session.appendMu.Lock()
		defer session.appendMu.Unlock()
		if session.activeTurnID != "" || len(session.messages) < 3 || session.messages[len(session.messages)-1].Content != "已经提交" {
			t.Fatalf("committed state active=%q messages=%+v", session.activeTurnID, session.messages)
		}
	})
}

func TestActivityReporterPanicDoesNotAbortSession(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "正常完成"}}}}
	session := newTestSession(t, model, &fakeServer{})
	ctx := WithActivityReporter(t.Context(), func(Activity) { panic("broken reporter") })
	result, err := session.Send(ctx, "忽略报告器故障")
	if err != nil || result.Text != "正常完成" {
		t.Fatalf("reporter panic escaped: result=%+v err=%v", result, err)
	}
	PublishActivity(ctx, Activity{Kind: ActivityThinking, Phase: ActivityWaitingModel})
}

func TestStreamingProtocolAndCancellationRollbackBeforeNextSend(t *testing.T) {
	for _, test := range []struct {
		name string
		step streamStep
	}{
		{
			name: "protocol failure",
			step: func(_ context.Context, _ modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
				if err := observe(modelclient.StreamEvent{Kind: modelclient.StreamEventTextDelta, Text: "半截协议回答"}); err != nil {
					return modelclient.Response{}, err
				}
				return modelclient.Response{}, errors.New("stream protocol failed")
			},
		},
		{
			name: "cancelled",
			step: func(ctx context.Context, _ modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
				if err := observe(modelclient.StreamEvent{Kind: modelclient.StreamEventTextDelta, Text: "半截取消回答"}); err != nil {
					return modelclient.Response{}, err
				}
				<-ctx.Done()
				return modelclient.Response{}, ctx.Err()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := &scriptedStreamingModel{steps: []streamStep{
				test.step,
				func(_ context.Context, _ modelclient.Request, _ func(modelclient.StreamEvent) error) (modelclient.Response, error) {
					return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "下一轮正常"}}, nil
				},
			}}
			session := newTestSession(t, model, &fakeServer{})
			ctx := t.Context()
			cancel := func() {}
			if test.name == "cancelled" {
				var cancelFunc context.CancelFunc
				ctx, cancelFunc = context.WithCancel(t.Context())
				cancel = cancelFunc
				go func() {
					time.Sleep(10 * time.Millisecond)
					cancelFunc()
				}()
			}
			_, firstErr := session.Send(ctx, "不应残留的用户轮次")
			cancel()
			if firstErr == nil {
				t.Fatal("failed stream unexpectedly succeeded")
			}
			if test.name == "cancelled" && !errors.Is(firstErr, context.Canceled) {
				t.Fatalf("cancellation error=%v", firstErr)
			}
			result, err := session.Send(t.Context(), "新的用户轮次")
			if err != nil || result.Text != "下一轮正常" {
				t.Fatalf("next result=%+v err=%v", result, err)
			}
			requests := model.snapshotRequests()
			if len(requests) != 2 {
				t.Fatalf("requests=%d", len(requests))
			}
			for _, message := range requests[1].Messages {
				if strings.Contains(message.Content, "不应残留") || strings.Contains(message.Content, "半截") || message.Role == "tool" {
					t.Fatalf("rolled-back fragment leaked into next request: %+v", requests[1].Messages)
				}
			}
		})
	}
}

type cancellationServer struct {
	*fakeServer
	retrieveStarted chan struct{}
	once            sync.Once
}

func (s *cancellationServer) RetrieveKnowledge(ctx context.Context, _ api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error) {
	s.retrieveCalls++
	s.once.Do(func() { close(s.retrieveStarted) })
	<-ctx.Done()
	return api.KnowledgeRetrievalResult{}, ctx.Err()
}

func TestCancellationStopsFirstModelReadToolAndPostToolModel(t *testing.T) {
	t.Run("first model", func(t *testing.T) {
		started := make(chan struct{})
		model := &scriptedCompleteModel{steps: []completeStep{
			func(ctx context.Context, _ modelclient.Request) (modelclient.Response, error) {
				close(started)
				<-ctx.Done()
				return modelclient.Response{}, ctx.Err()
			},
		}}
		session := newTestSession(t, model, &fakeServer{})
		ctx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan error, 1)
		go func() { _, err := session.Send(ctx, "取消第一次模型"); resultCh <- err }()
		<-started
		cancel()
		if err := <-resultCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		assertNoActiveTurnFragments(t, session)
	})

	t.Run("read tool and siblings", func(t *testing.T) {
		model := &scriptedCompleteModel{steps: []completeStep{
			func(context.Context, modelclient.Request) (modelclient.Response, error) {
				return modelclient.Response{Message: toolCallsMessage(
					modelclient.ToolCall{ID: "read-1", Type: "function", Function: modelclient.ToolFunction{Name: "search_knowledge", Arguments: `{"query":"图论"}`}},
					modelclient.ToolCall{ID: "read-2", Type: "function", Function: modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{}`}},
				)}, nil
			},
		}}
		base := &fakeServer{}
		server := &cancellationServer{fakeServer: base, retrieveStarted: make(chan struct{})}
		session := newTestSession(t, model, server)
		ctx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan error, 1)
		go func() { _, err := session.Send(ctx, "取消读工具"); resultCh <- err }()
		<-server.retrieveStarted
		cancel()
		if err := <-resultCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		if base.currentCalls != 0 || len(model.snapshotRequests()) != 1 {
			t.Fatalf("sibling or next model executed: current=%d requests=%d", base.currentCalls, len(model.snapshotRequests()))
		}
		assertNoActiveTurnFragments(t, session)
	})

	t.Run("second model after tool", func(t *testing.T) {
		secondStarted := make(chan struct{})
		model := &scriptedCompleteModel{steps: []completeStep{
			func(context.Context, modelclient.Request) (modelclient.Response, error) {
				return modelclient.Response{Message: toolMessage("read-1", "get_learning_progress", `{}`)}, nil
			},
			func(ctx context.Context, _ modelclient.Request) (modelclient.Response, error) {
				close(secondStarted)
				<-ctx.Done()
				return modelclient.Response{}, ctx.Err()
			},
		}}
		session := newTestSession(t, model, &fakeServer{})
		ctx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan error, 1)
		go func() { _, err := session.Send(ctx, "工具后取消"); resultCh <- err }()
		<-secondStarted
		cancel()
		if err := <-resultCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		if len(model.snapshotRequests()) != 2 {
			t.Fatalf("requests=%d", len(model.snapshotRequests()))
		}
		assertNoActiveTurnFragments(t, session)
	})
}

func TestReasoningEffortFreezesCurrentRequestAndChangesNext(t *testing.T) {
	firstStarted := make(chan modelclient.Request, 1)
	releaseFirst := make(chan struct{})
	model := &scriptedCompleteModel{steps: []completeStep{
		func(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
			firstStarted <- cloneControlRequest(request)
			<-releaseFirst
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "第一轮"}}, nil
		},
		func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "第二轮"}}, nil
		},
	}}
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 32768, MaxToolRounds: 8, ReasoningEffort: modelclient.ReasoningEffortLow,
		Now: time.Now, NewUUID: func() (string, error) { return "60000000-0000-4000-8000-000000000001", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan error, 1)
	go func() { _, sendErr := session.Send(t.Context(), "第一轮"); resultCh <- sendErr }()
	firstRequest := <-firstStarted
	if err := session.SetReasoningEffort(modelclient.ReasoningEffortHigh); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
	if firstRequest.ReasoningEffort != modelclient.ReasoningEffortLow {
		t.Fatalf("current request effort=%q", firstRequest.ReasoningEffort)
	}
	if _, err := session.Send(t.Context(), "第二轮"); err != nil {
		t.Fatal(err)
	}
	requests := model.snapshotRequests()
	if len(requests) != 2 || requests[1].ReasoningEffort != modelclient.ReasoningEffortHigh {
		t.Fatalf("requests=%+v", requests)
	}
	if err := session.SetReasoningEffort("turbo"); err == nil || session.ReasoningEffort() != modelclient.ReasoningEffortHigh {
		t.Fatalf("invalid effort err=%v current=%q", err, session.ReasoningEffort())
	}
}

func TestReasoningActivityTracksFrozenEffortAcrossToolContinuation(t *testing.T) {
	firstStarted := make(chan modelclient.Request, 1)
	releaseFirst := make(chan struct{})
	model := &scriptedCompleteModel{steps: []completeStep{
		func(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
			firstStarted <- cloneControlRequest(request)
			<-releaseFirst
			return modelclient.Response{Message: toolMessage("progress", "get_learning_progress", `{}`)}, nil
		},
		func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已结合工具完成"}}, nil
		},
	}}
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 32768, MaxToolRounds: 8, ReasoningEffort: modelclient.ReasoningEffortLow,
		Now: time.Now, NewUUID: func() (string, error) { return "60000000-0000-4000-8000-000000000001", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	activities := make([]Activity, 0, 16)
	ctx := WithActivityReporter(t.Context(), func(activity Activity) {
		mu.Lock()
		activities = append(activities, activity)
		mu.Unlock()
	})
	resultCh := make(chan error, 1)
	go func() { _, sendErr := session.Send(ctx, "工具后切换推理"); resultCh <- sendErr }()
	firstRequest := <-firstStarted
	if err := session.SetReasoningEffort(modelclient.ReasoningEffortHigh); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
	requests := model.snapshotRequests()
	if firstRequest.ReasoningEffort != modelclient.ReasoningEffortLow || len(requests) != 2 || requests[1].ReasoningEffort != modelclient.ReasoningEffortHigh {
		t.Fatalf("requests=%+v first=%q", requests, firstRequest.ReasoningEffort)
	}
	mu.Lock()
	defer mu.Unlock()
	var waiting []modelclient.ReasoningEffort
	for _, activity := range activities {
		if activity.Phase == ActivityWaitingModel {
			waiting = append(waiting, activity.ReasoningEffort)
		}
	}
	if !reflect.DeepEqual(waiting, []modelclient.ReasoningEffort{modelclient.ReasoningEffortLow, modelclient.ReasoningEffortHigh}) {
		t.Fatalf("waiting efforts=%+v activities=%+v", waiting, activities)
	}
}

func TestActivityTimestampsUseStableTurnAndEventIdentity(t *testing.T) {
	current := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	session, err := New(&fakeModel{}, &fakeServer{}, Options{
		ContextWindow: 32768, MaxToolRounds: 8,
		ModelTimeout: 45 * time.Second, ToolTimeout: 12 * time.Second,
		Now:     func() time.Time { current = current.Add(time.Second); return current },
		NewUUID: func() (string, error) { return "60000000-0000-4000-8000-000000000001", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var activities []Activity
	ctx1 := WithActivityReporter(withActivityTurn(t.Context(), "turn-a"), func(activity Activity) { activities = append(activities, activity) })
	session.publishActivity(ctx1, Activity{Kind: ActivityThinking, Event: Event{ID: "same", Status: EventRunning}, Phase: ActivityWaitingModel})
	session.publishActivity(ctx1, Activity{Kind: ActivityThinking, Event: Event{ID: "same", Status: EventSucceeded}, Phase: ActivityValidatingResponse})
	session.publishActivity(ctx1, Activity{Kind: ActivityTool, Event: Event{ID: "same", Tool: "tool-a", Status: EventRunning}, Phase: ActivityExecutingTool})
	ctx2 := WithActivityReporter(withActivityTurn(t.Context(), "turn-b"), func(activity Activity) { activities = append(activities, activity) })
	session.publishActivity(ctx2, Activity{Kind: ActivityThinking, Event: Event{ID: "same", Status: EventRunning}, Phase: ActivityWaitingModel})
	if len(activities) != 4 || !activities[0].StartedAt.Equal(activities[1].StartedAt) {
		t.Fatalf("same event did not retain start: %+v", activities)
	}
	if activities[0].StartedAt.Equal(activities[2].StartedAt) || activities[0].StartedAt.Equal(activities[3].StartedAt) {
		t.Fatalf("different kind or turn collided: %+v", activities)
	}
	if activities[0].TimeoutBudget != 45*time.Second || activities[1].TimeoutBudget != 0 || activities[2].TimeoutBudget != 12*time.Second || activities[3].TimeoutBudget != 45*time.Second {
		t.Fatalf("activity timeout budgets=%+v", activities)
	}
}

func TestDiscardIncompleteTurnRemovesOnlyCurrentMessagesSourcesAndToolHistory(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", Content: "前一轮完成"}},
		{Message: toolMessage("current-tool", "get_learning_progress", `{}`)},
	}, err: errors.New("followup model failed")}
	session := newTestSession(t, model, &fakeServer{})
	if _, err := session.Send(t.Context(), "前一轮"); err != nil {
		t.Fatal(err)
	}
	session.appendMu.Lock()
	previousMessages := make([]modelclient.Message, len(session.messages))
	for index := range session.messages {
		previousMessages[index] = cloneModelMessage(session.messages[index])
	}
	previousTurnIDs := append([]string(nil), session.messageTurnIDs...)
	session.appendMu.Unlock()

	if _, err := session.Send(t.Context(), "应回滚的当前轮"); err == nil {
		t.Fatal("followup model failure was not returned")
	}
	session.appendMu.Lock()
	if !reflect.DeepEqual(session.messages, previousMessages) || !reflect.DeepEqual(session.messageTurnIDs, previousTurnIDs) {
		t.Fatalf("previous turn changed or current fragments survived: messages=%+v turnIDs=%+v", session.messages, session.messageTurnIDs)
	}
	if _, exists := session.toolHistory["current-tool"]; exists {
		t.Fatal("current tool history survived rollback")
	}
	if session.activeTurnID != "" || session.currentTurnID != "" {
		t.Fatalf("turn remained active: active=%q current=%q", session.activeTurnID, session.currentTurnID)
	}
	session.appendMu.Unlock()
	session.contextRuntime.mu.Lock()
	defer session.contextRuntime.mu.Unlock()
	for _, sourceID := range session.contextRuntime.ledger.SourceOrder {
		if source := session.contextRuntime.ledger.Sources[sourceID]; source.TurnID != "turn-1" {
			t.Fatalf("incomplete source survived: %+v", source)
		}
	}
}

func assertNoActiveTurnFragments(t *testing.T, session *Session) {
	t.Helper()
	session.appendMu.Lock()
	defer session.appendMu.Unlock()
	if session.activeTurnID != "" || session.currentTurnID != "" {
		t.Fatalf("turn remained active: active=%q current=%q", session.activeTurnID, session.currentTurnID)
	}
	for _, message := range session.messages {
		if message.Role != "system" {
			t.Fatalf("cancelled turn message survived: %+v", session.messages)
		}
	}
	if len(session.toolHistory) != 0 {
		t.Fatalf("cancelled tool history survived: %+v", session.toolHistory)
	}
}

func toolCallsMessage(calls ...modelclient.ToolCall) modelclient.Message {
	return modelclient.Message{Role: "assistant", ToolCalls: calls}
}
