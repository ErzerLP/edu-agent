package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestSessionCheckpointRoundTripPreservesRecallAndResetsAuthorization(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "A graph has vertices and edges."}}}}
	session := newTestSession(t, model, &fakeServer{})
	defer session.Close()
	if _, err := session.Send(t.Context(), "What is a graph?"); err != nil {
		t.Fatal(err)
	}
	if err := session.SetReasoningEffort(modelclient.ReasoningEffortHigh); err != nil {
		t.Fatal(err)
	}
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}

	observationID := "obs_0000000000000001"
	reflectionID := "ref_0000000000000001"
	runtime := session.contextRuntime
	runtime.mu.Lock()
	if len(runtime.ledger.SourceOrder) == 0 {
		runtime.mu.Unlock()
		t.Fatal("missing source ledger")
	}
	sourceID := runtime.ledger.SourceOrder[0]
	createdAt := time.Date(2026, 9, 2, 6, 1, 0, 0, time.UTC)
	runtime.ledger.Observations[observationID] = Observation{
		ID: observationID, Content: "The user is learning graph basics.", CreatedAt: createdAt,
		Relevance: RelevanceHigh, Kind: ObservationUserIntent, SourceEntryIDs: []string{sourceID},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent, TokenEstimate: 9,
	}
	runtime.ledger.ObservationOrder = append(runtime.ledger.ObservationOrder, observationID)
	runtime.ledger.Reflections[reflectionID] = Reflection{
		ID: reflectionID, Content: "Keep explanations introductory.", Kind: ReflectionUserIntent,
		Support:   []CoverageEdge{{ObservationID: observationID, Fidelity: CoverageExact}},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent, CreatedAt: createdAt, TokenEstimate: 7,
	}
	runtime.ledger.ReflectionOrder = append(runtime.ledger.ReflectionOrder, reflectionID)
	runtime.usedIDs[observationID] = struct{}{}
	runtime.usedIDs[reflectionID] = struct{}{}
	runtime.ledger.CoverageIndex = 0
	runtime.ledger.CoverageWatermark = sourceID
	runtime.mu.Unlock()

	beforeObservation := session.contextRuntime.recallMemory(observationID)
	beforeReflection := session.contextRuntime.recallMemory(reflectionID)
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSessionCheckpoint(encoded)
	if err != nil {
		t.Fatal(err)
	}

	restoredModel := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "Continue."}}}}
	restored := newTestSession(t, restoredModel, &fakeServer{})
	defer restored.Close()
	if err := restored.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreCheckpoint(decoded); err != nil {
		t.Fatal(err)
	}
	if restored.FileAuthorizationMode() != FileAuthorizationConfirm {
		t.Fatalf("authorization=%q", restored.FileAuthorizationMode())
	}
	if restored.ReasoningEffort() != modelclient.ReasoningEffortHigh {
		t.Fatalf("effort=%q", restored.ReasoningEffort())
	}
	if !reflect.DeepEqual(beforeObservation, restored.contextRuntime.recallMemory(observationID)) {
		t.Fatal("observation recall changed")
	}
	if !reflect.DeepEqual(beforeReflection, restored.contextRuntime.recallMemory(reflectionID)) {
		t.Fatal("reflection recall changed")
	}
	if !reflect.DeepEqual(session.messages, restored.messages) || !reflect.DeepEqual(session.turnOrder, restored.turnOrder) {
		t.Fatal("conversation state changed")
	}
	if _, err := restored.Send(t.Context(), "Continue"); err != nil {
		t.Fatal(err)
	}
	if restored.turnSequence != checkpoint.TurnSequence+1 {
		t.Fatalf("turn sequence=%d", restored.turnSequence)
	}
}

func TestCheckpointExcludesSystemPromptsAndCanonicalizesToolCalls(t *testing.T) {
	session := newTestSession(t, &fakeModel{}, &fakeServer{})
	defer session.Close()
	session.appendMu.Lock()
	session.messages[0].Content = "old-system-secret"
	session.appendMu.Unlock()
	turnID, err := session.startTurn()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.appendTurnMessage(turnID, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
		ID: "tool-call", Type: "function", Function: modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{"secret":"must-not-persist"}`},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.appendSessionToolResult("get_learning_progress", "tool-call", map[string]any{"active": false}); err != nil {
		t.Fatal(err)
	}
	session.finishSuccessfulTurn()

	for _, message := range session.messages {
		for _, call := range message.ToolCalls {
			if call.Function.Arguments != `{}` {
				t.Fatalf("completed in-memory tool arguments=%q", call.Function.Arguments)
			}
		}
	}
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range checkpoint.Messages {
		if message.Role == "system" {
			t.Fatal("system message persisted")
		}
		for _, call := range message.ToolCalls {
			if call.Function.Arguments != `{}` {
				t.Fatalf("checkpoint tool arguments=%q", call.Function.Arguments)
			}
		}
	}
	encoded, err := EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "old-system-secret") || strings.Contains(string(encoded), "must-not-persist") {
		t.Fatalf("forbidden prompt or arguments persisted: %s", encoded)
	}

	restored := newTestSession(t, &fakeModel{}, &fakeServer{})
	defer restored.Close()
	freshSystemCount := len(restored.messages)
	if err := restored.RestoreCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(restored.messages) != freshSystemCount+len(checkpoint.Messages) || restored.messages[0].Role != "system" || restored.messages[0].Content != systemPrompt {
		t.Fatalf("restored messages=%+v", restored.messages)
	}
	for index := freshSystemCount; index < len(restored.messages); index++ {
		if restored.messages[index].Role == "system" {
			t.Fatal("historical system message appended")
		}
	}

	invalid, err := DecodeSessionCheckpoint(encoded)
	if err != nil {
		t.Fatal(err)
	}
	invalid.Messages[0].ToolCalls[0].Function.Arguments = `{"secret":true}`
	if _, err := EncodeSessionCheckpoint(invalid); err == nil {
		t.Fatal("raw tool arguments were accepted")
	}
	invalid, err = DecodeSessionCheckpoint(encoded)
	if err != nil {
		t.Fatal(err)
	}
	invalid.Messages = invalid.Messages[:1]
	invalid.MessageTurnIDs = invalid.MessageTurnIDs[:1]
	invalid.ToolHistory = nil
	if _, err := EncodeSessionCheckpoint(invalid); err == nil {
		t.Fatal("orphan tool call was accepted")
	}
	invalid, err = DecodeSessionCheckpoint(encoded)
	if err != nil {
		t.Fatal(err)
	}
	invalid.Messages = append([]modelclient.Message{{Role: "system", Content: "injected"}}, invalid.Messages...)
	invalid.MessageTurnIDs = append([]string{""}, invalid.MessageTurnIDs...)
	if _, err := EncodeSessionCheckpoint(invalid); err == nil {
		t.Fatal("system message was accepted")
	}
}

func TestCheckpointRecomputesTransientContextAndPreservesAllocatedIDs(t *testing.T) {
	session := newTestSession(t, &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "stable"}}}}, &fakeServer{})
	defer session.Close()
	if _, err := session.Send(t.Context(), "remember this"); err != nil {
		t.Fatal(err)
	}
	runtime := session.contextRuntime
	prunedSourceID := "src_9999999999999998"
	prunedObservationID := "obs_9999999999999998"
	runtime.mu.Lock()
	sourceID := runtime.ledger.SourceOrder[0]
	observationID := "obs_0000000000000001"
	reflectionID := "ref_0000000000000001"
	createdAt := time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	runtime.ledger.Observations[observationID] = Observation{ID: observationID, Content: "durable observation", CreatedAt: createdAt, Relevance: RelevanceHigh, Kind: ObservationUserIntent, SourceEntryIDs: []string{sourceID}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent, TokenEstimate: 99991}
	runtime.ledger.ObservationOrder = append(runtime.ledger.ObservationOrder, observationID)
	runtime.ledger.Reflections[reflectionID] = Reflection{ID: reflectionID, Content: "durable reflection", Kind: ReflectionUserIntent, Support: []CoverageEdge{{ObservationID: observationID, Fidelity: CoverageExact}}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent, CreatedAt: createdAt, TokenEstimate: 99992}
	runtime.ledger.ReflectionOrder = append(runtime.ledger.ReflectionOrder, reflectionID)
	runtime.usedIDs[observationID] = struct{}{}
	runtime.usedIDs[reflectionID] = struct{}{}
	runtime.usedIDs[prunedSourceID] = struct{}{}
	runtime.usedIDs[prunedObservationID] = struct{}{}
	firstSource := runtime.ledger.Sources[sourceID]
	firstSource.TokenEstimate = 99993
	runtime.ledger.Sources[sourceID] = firstSource
	runtime.observerFailures = 5
	runtime.observerBlockedUntil = 40
	runtime.reflectorBlockedUntil = 30
	runtime.softPressure = true
	runtime.status = ContextStatus{CurrentTokens: 12345, CachePromptTokens: 99, CacheReadTokens: 88, CacheHitRateAvailable: true, Mode: runtime.mode, Phase: "stale"}
	runtime.mu.Unlock()

	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token_estimate", "observer_failures", "observer_blocked_until", "reflector_blocked_until", "soft_pressure", "cache_prompt_tokens", `"status"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("transient field %q persisted: %s", forbidden, encoded)
		}
	}
	withTokenEstimate := strings.Replace(string(encoded), `"retention":`, `"token_estimate":1,"retention":`, 1)
	if _, err := DecodeSessionCheckpoint([]byte(withTokenEstimate)); err == nil {
		t.Fatal("persisted token estimate was accepted")
	}

	restored := newTestSession(t, &fakeModel{}, &fakeServer{})
	defer restored.Close()
	if err := restored.RestoreCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	restoredRuntime := restored.contextRuntime
	restoredRuntime.mu.Lock()
	if _, ok := restoredRuntime.usedIDs[prunedSourceID]; !ok {
		restoredRuntime.mu.Unlock()
		t.Fatal("pruned source ID was reusable after restore")
	}
	if _, ok := restoredRuntime.usedIDs[prunedObservationID]; !ok {
		restoredRuntime.mu.Unlock()
		t.Fatal("pruned observation ID was reusable after restore")
	}
	restoredSource := restoredRuntime.ledger.Sources[sourceID]
	restoredObservation := restoredRuntime.ledger.Observations[observationID]
	restoredReflection := restoredRuntime.ledger.Reflections[reflectionID]
	if restoredSource.TokenEstimate != restored.estimator.EstimateText(restoredSource.RecallText) || restoredObservation.TokenEstimate != restored.estimator.EstimateText(restoredObservation.Content) || restoredReflection.TokenEstimate != restored.estimator.EstimateText(restoredReflection.Content) {
		restoredRuntime.mu.Unlock()
		t.Fatalf("token estimates were not recomputed: source=%d observation=%d reflection=%d", restoredSource.TokenEstimate, restoredObservation.TokenEstimate, restoredReflection.TokenEstimate)
	}
	if restoredRuntime.observerFailures != 0 || restoredRuntime.observerBlockedUntil != 0 || restoredRuntime.reflectorBlockedUntil != 0 || restoredRuntime.softPressure || restoredRuntime.status.CurrentTokens != 0 || restoredRuntime.status.CachePromptTokens != 0 || restoredRuntime.status.Phase != "idle" {
		restoredRuntime.mu.Unlock()
		t.Fatalf("transient context restored: failures=%d observer=%d reflector=%d pressure=%t status=%+v", restoredRuntime.observerFailures, restoredRuntime.observerBlockedUntil, restoredRuntime.reflectorBlockedUntil, restoredRuntime.softPressure, restoredRuntime.status)
	}
	attempts := 0
	restoredRuntime.idSource = func(prefix string) (string, error) {
		attempts++
		if attempts == 1 {
			return prunedSourceID, nil
		}
		return "src_9999999999999999", nil
	}
	restoredRuntime.mu.Unlock()
	allocated, err := restoredRuntime.appendSource(sourceDraft{TurnID: "turn-future", Kind: SourceUser, RecallText: "future", Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent})
	if err != nil || allocated != "src_9999999999999999" || attempts != 2 {
		t.Fatalf("allocated=%q attempts=%d err=%v", allocated, attempts, err)
	}

	invalid, err := DecodeSessionCheckpoint(encoded)
	if err != nil {
		t.Fatal(err)
	}
	filtered := invalid.Context.AllocatedIDs[:0]
	for _, id := range invalid.Context.AllocatedIDs {
		if id != sourceID {
			filtered = append(filtered, id)
		}
	}
	invalid.Context.AllocatedIDs = filtered
	if _, err := EncodeSessionCheckpoint(invalid); err == nil {
		t.Fatal("active context ID missing from allocated set was accepted")
	}
}

func TestSessionCheckpointRejectsUnsafeControlAndBidiText(t *testing.T) {
	session := newTestSession(t, &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "stable"}}}}, &fakeServer{})
	defer session.Close()
	if _, err := session.Send(t.Context(), "safe"); err != nil {
		t.Fatal(err)
	}
	stable, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"terminal\x1b[31m", "bidi\u202eevil"} {
		invalid := stable
		invalid.Messages = append([]modelclient.Message(nil), stable.Messages...)
		invalid.Messages[0] = cloneModelMessage(stable.Messages[0])
		invalid.Messages[0].Content = unsafe
		if _, err := EncodeSessionCheckpoint(invalid); err == nil {
			t.Fatalf("unsafe checkpoint text accepted: %q", unsafe)
		}
	}
}

func TestSessionCheckpointRejectsUnstableAndInvalidWithoutMutation(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "stable"}}}}
	session := newTestSession(t, model, &fakeServer{})
	defer session.Close()
	if _, err := session.Send(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	stable, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}

	session.appendMu.Lock()
	session.pendingKind = pendingQuestion
	session.appendMu.Unlock()
	if _, err := session.ExportCheckpoint(); !errors.Is(err, ErrCheckpointUnstable) {
		t.Fatalf("unstable error=%v", err)
	}
	session.appendMu.Lock()
	session.clearPendingLocked()
	session.appendMu.Unlock()

	fresh := newTestSession(t, &fakeModel{}, &fakeServer{})
	defer fresh.Close()
	originalMessages := append([]modelclient.Message(nil), fresh.messages...)
	invalid := stable
	invalid.MessageTurnIDs = invalid.MessageTurnIDs[:len(invalid.MessageTurnIDs)-1]
	if err := fresh.RestoreCheckpoint(invalid); err == nil {
		t.Fatal("invalid checkpoint restored")
	}
	if !reflect.DeepEqual(originalMessages, fresh.messages) || len(fresh.turnOrder) != 0 {
		t.Fatal("invalid restore mutated fresh session")
	}

	raw, err := json.Marshal(stable)
	if err != nil {
		t.Fatal(err)
	}
	validRaw := append([]byte(nil), raw...)
	var object map[string]any
	if err := json.Unmarshal(validRaw, &object); err != nil {
		t.Fatal(err)
	}
	object["unknown_field"] = true
	unknownRaw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSessionCheckpoint(unknownRaw); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("unknown field error=%v", err)
	}

	trailingRaw := append(append([]byte(nil), validRaw...), []byte(` {}`)...)
	if _, err := DecodeSessionCheckpoint(trailingRaw); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("trailing JSON error=%v", err)
	}

	invalidUTF8 := append([]byte(nil), validRaw...)
	contentAt := bytes.Index(invalidUTF8, []byte(`"content":"first"`))
	if contentAt < 0 {
		t.Fatalf("checkpoint fixture did not contain user content: %s", validRaw)
	}
	invalidUTF8[contentAt+len(`"content":"`)] = 0xff
	if _, err := DecodeSessionCheckpoint(invalidUTF8); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("invalid UTF-8 error=%v", err)
	}

	object = nil
	if err := json.Unmarshal(validRaw, &object); err != nil {
		t.Fatal(err)
	}
	object["schema_version"] = SessionCheckpointSchemaVersion + 1
	futureRaw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSessionCheckpoint(futureRaw); !errors.Is(err, ErrCheckpointVersionUnsupported) {
		t.Fatalf("future checkpoint schema error=%v", err)
	}
}

func TestSessionCheckpointRejectsProductionActiveTurnWithoutMutation(t *testing.T) {
	started := make(chan struct{})
	model := &scriptedCompleteModel{steps: []completeStep{
		func(ctx context.Context, _ modelclient.Request) (modelclient.Response, error) {
			close(started)
			<-ctx.Done()
			return modelclient.Response{}, ctx.Err()
		},
	}}
	session := newTestSession(t, model, &fakeServer{})
	defer session.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		_, err := session.Send(ctx, "阻塞中的当前轮次")
		resultCh <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("production turn did not reach the model")
	}

	assertCheckpointExportUnstablePreservesState(t, session, SessionSwitchState{ActiveTurn: true})

	cancel()
	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled active turn error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled active turn did not finish")
	}
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointHasNoTransientTurnFragments(t, checkpoint)
}

func TestSessionCheckpointRejectsProductionPendingInteractionsWithoutMutation(t *testing.T) {
	t.Run("question", func(t *testing.T) {
		model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("question-call", "ask_user_question", `{"question_id":"question","header":"学习方向","question":"先从哪一部分开始？","mode":"single","options":[{"id":"graph","label":"图","description":"先学习图结构"},{"id":"tree","label":"树","description":"先学习树结构"}]}`)}}}
		session := newTestSession(t, model, &fakeServer{})
		defer session.Close()
		result, err := session.Send(t.Context(), "请帮我选择学习方向")
		if err != nil || result.PendingQuestion == nil {
			t.Fatalf("pending question result=%+v err=%v", result, err)
		}
		assertCheckpointExportUnstablePreservesState(t, session, SessionSwitchState{ActiveTurn: true, PendingQuestion: true})
	})

	t.Run("preference", func(t *testing.T) {
		model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("preference-call", "remember_preference", `{"content":"偏好图示","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`)}}}
		session := newTestSession(t, model, &fakeServer{})
		defer session.Close()
		result, err := session.Send(t.Context(), "请长期记住我偏好图示")
		if err != nil || result.Pending == nil {
			t.Fatalf("pending preference result=%+v err=%v", result, err)
		}
		assertCheckpointExportUnstablePreservesState(t, session, SessionSwitchState{ActiveTurn: true, PendingPreference: true})
	})

	t.Run("file mutation", func(t *testing.T) {
		prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
			Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md", PreviewKind: "content", Preview: "hello",
		}}
		executor := &fakeWorkspaceExecutor{
			status:   workspace.Status{Available: true, Label: "project"},
			prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
		}
		model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"create","content":"hello"}`)}}}
		session := newWorkspaceTestSession(t, model, executor)
		defer session.Close()
		result, err := session.Send(t.Context(), "写入笔记")
		if err != nil || result.PendingFileMutation == nil {
			t.Fatalf("pending file mutation result=%+v err=%v", result, err)
		}
		assertCheckpointExportUnstablePreservesState(t, session, SessionSwitchState{ActiveTurn: true, PendingFileMutation: true})
	})
}

func TestSessionCheckpointCancelledOrdinaryTurnRoundTripOmitsFragments(t *testing.T) {
	t.Run("partial streaming assistant", func(t *testing.T) {
		deltaPublished := make(chan struct{})
		model := &scriptedStreamingModel{steps: []streamStep{
			func(ctx context.Context, _ modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
				if err := observe(modelclient.StreamEvent{Kind: modelclient.StreamEventTextDelta, Text: "半截助手回答"}); err != nil {
					return modelclient.Response{}, err
				}
				close(deltaPublished)
				<-ctx.Done()
				return modelclient.Response{}, ctx.Err()
			},
		}}
		session := newTestSession(t, model, &fakeServer{})
		defer session.Close()
		ctx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan error, 1)
		go func() {
			_, err := session.Send(ctx, "取消半截回答")
			resultCh <- err
		}()
		select {
		case <-deltaPublished:
		case <-time.After(time.Second):
			t.Fatal("stream did not publish the partial assistant delta")
		}
		cancel()
		select {
		case err := <-resultCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled streaming turn error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled streaming turn did not finish")
		}
		checkpoint := exportAndDecodeCheckpoint(t, session)
		assertCheckpointHasNoTransientTurnFragments(t, checkpoint)

		restoredModel := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "取消后继续"}}}}
		restored := newTestSession(t, restoredModel, &fakeServer{})
		defer restored.Close()
		if err := restored.RestoreCheckpoint(checkpoint); err != nil {
			t.Fatal(err)
		}
		assertSessionCheckpointIdle(t, restored)
		if _, err := restored.Send(t.Context(), "取消后继续"); err != nil {
			t.Fatal(err)
		}
		if len(restoredModel.requests) != 1 {
			t.Fatalf("restored requests=%d", len(restoredModel.requests))
		}
		for _, message := range restoredModel.requests[0].Messages {
			if strings.Contains(message.Content, "取消半截回答") || strings.Contains(message.Content, "半截助手回答") || message.Role == "tool" {
				t.Fatalf("cancelled fragments leaked after restore: %+v", restoredModel.requests[0].Messages)
			}
		}
	})

	t.Run("partial tool turn", func(t *testing.T) {
		followupStarted := make(chan struct{})
		model := &scriptedCompleteModel{steps: []completeStep{
			func(context.Context, modelclient.Request) (modelclient.Response, error) {
				return modelclient.Response{Message: toolMessage("progress-call", "get_learning_progress", `{}`)}, nil
			},
			func(ctx context.Context, _ modelclient.Request) (modelclient.Response, error) {
				close(followupStarted)
				<-ctx.Done()
				return modelclient.Response{}, ctx.Err()
			},
		}}
		session := newTestSession(t, model, &fakeServer{})
		defer session.Close()
		ctx, cancel := context.WithCancel(t.Context())
		resultCh := make(chan error, 1)
		go func() {
			_, err := session.Send(ctx, "工具后取消")
			resultCh <- err
		}()
		select {
		case <-followupStarted:
		case <-time.After(time.Second):
			t.Fatal("post-tool model did not start")
		}
		cancel()
		select {
		case err := <-resultCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled post-tool turn error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled post-tool turn did not finish")
		}
		checkpoint := exportAndDecodeCheckpoint(t, session)
		assertCheckpointHasNoTransientTurnFragments(t, checkpoint)

		restoredModel := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "工具后继续"}}}}
		restored := newTestSession(t, restoredModel, &fakeServer{})
		defer restored.Close()
		if err := restored.RestoreCheckpoint(checkpoint); err != nil {
			t.Fatal(err)
		}
		assertSessionCheckpointIdle(t, restored)
		if _, err := restored.Send(t.Context(), "工具后继续"); err != nil {
			t.Fatal(err)
		}
		for _, message := range restoredModel.requests[0].Messages {
			if strings.Contains(message.Content, "工具后取消") || message.Role == "tool" {
				t.Fatalf("cancelled tool fragments leaked after restore: %+v", restoredModel.requests[0].Messages)
			}
		}
	})
}

func TestSessionCheckpointRoundTripContinuesOrdinaryTurnEquivalently(t *testing.T) {
	first := modelclient.Message{Role: "assistant", Content: "第一轮完成"}
	next := modelclient.Message{Role: "assistant", Content: "下一轮完成"}
	originalModel := &fakeModel{responses: []modelclient.Response{{Message: first}, {Message: next}}}
	original := newTestSession(t, originalModel, &fakeServer{})
	defer original.Close()
	if _, err := original.Send(t.Context(), "第一轮问题"); err != nil {
		t.Fatal(err)
	}
	checkpoint := exportAndDecodeCheckpoint(t, original)

	restoredModel := &fakeModel{responses: []modelclient.Response{{Message: next}}}
	restored := newTestSession(t, restoredModel, &fakeServer{})
	defer restored.Close()
	if err := restored.RestoreCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Send(t.Context(), "下一轮问题"); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Send(t.Context(), "下一轮问题"); err != nil {
		t.Fatal(err)
	}
	if len(originalModel.requests) != 2 || len(restoredModel.requests) != 1 {
		t.Fatalf("original requests=%d restored requests=%d", len(originalModel.requests), len(restoredModel.requests))
	}
	if !reflect.DeepEqual(originalModel.requests[1], restoredModel.requests[0]) {
		t.Fatalf("continued request changed across checkpoint round trip:\noriginal=%+v\nrestored=%+v", originalModel.requests[1], restoredModel.requests[0])
	}
}

func assertCheckpointExportUnstablePreservesState(t *testing.T, session *Session, want SessionSwitchState) {
	t.Helper()
	before := session.SwitchState()
	if !reflect.DeepEqual(before, want) {
		t.Fatalf("unexpected pre-export switch state=%+v want=%+v", before, want)
	}
	rejected, err := session.ExportCheckpoint()
	if !errors.Is(err, ErrCheckpointUnstable) {
		t.Fatalf("unstable checkpoint error=%v", err)
	}
	if !reflect.DeepEqual(rejected, SessionCheckpoint{}) {
		t.Fatalf("failed export returned a checkpoint: %+v", rejected)
	}
	after := session.SwitchState()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed export mutated switch state: before=%+v after=%+v", before, after)
	}
}

func exportAndDecodeCheckpoint(t *testing.T, session *Session) SessionCheckpoint {
	t.Helper()
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSessionCheckpoint(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func assertCheckpointHasNoTransientTurnFragments(t *testing.T, checkpoint SessionCheckpoint) {
	t.Helper()
	if checkpoint.CurrentTurnID != "" || len(checkpoint.Turns) != 0 || len(checkpoint.Messages) != 0 || len(checkpoint.MessageTurnIDs) != 0 || len(checkpoint.ToolHistory) != 0 || len(checkpoint.ToolReferences) != 0 || len(checkpoint.WorkspaceReferences) != 0 || len(checkpoint.Context.Sources) != 0 {
		t.Fatalf("cancelled turn fragments survived checkpoint: %+v", checkpoint)
	}
}

func assertSessionCheckpointIdle(t *testing.T, session *Session) {
	t.Helper()
	state := session.SwitchState()
	if state.ActiveTurn || state.PendingQuestion || state.PendingPreference || state.PendingFileMutation || state.Resolving {
		t.Fatalf("restored session retained transient state=%+v", state)
	}
}
