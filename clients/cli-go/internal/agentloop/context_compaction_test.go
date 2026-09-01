package agentloop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type compactionTestModel struct {
	mu            sync.Mutex
	requests      []modelclient.Request
	normalCalls   int
	observerCalls int
	reflectCalls  int
}

func (m *compactionTestModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, cloneRequest(request))
	m.mu.Unlock()
	if len(request.Tools) == 1 && request.Tools[0].Function.Name == observerToolName {
		m.mu.Lock()
		m.observerCalls++
		m.mu.Unlock()
		sources := observerInputSources(request.Messages[len(request.Messages)-1].Content)
		if len(sources) == 0 {
			return modelclient.Response{}, errors.New("observer request omitted sources")
		}
		selected := sources[0]
		for _, source := range sources {
			if source.kind == SourceUser {
				selected = source
				break
			}
		}
		args := observerRecordArgs{
			CoversUpToID: sources[len(sources)-1].id,
			Observations: []observerObservationArg{{
				Content: "用户要求后续回答始终先给结论", Relevance: RelevanceCritical,
				Kind: ObservationUserConstraint, SourceEntryIDs: []string{selected.id},
				Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
				Supersedes: []string{}, SupersessionReason: "",
			}},
		}
		data, _ := json.Marshal(args)
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
			ID: "observer-record", Type: "function", Function: modelclient.ToolFunction{Name: observerToolName, Arguments: string(data)},
		}}}}, nil
	}
	if len(request.Tools) == 1 && request.Tools[0].Function.Name == reflectorToolName {
		m.mu.Lock()
		m.reflectCalls++
		m.mu.Unlock()
		return modelclient.Response{Message: modelclient.Message{Role: "assistant"}}, nil
	}
	m.mu.Lock()
	m.normalCalls++
	call := m.normalCalls
	m.mu.Unlock()
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: fmt.Sprintf("正常回答%d", call)}}, nil
}

func (m *compactionTestModel) requestSnapshot() []modelclient.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]modelclient.Request, len(m.requests))
	for index, request := range m.requests {
		result[index] = cloneRequest(request)
	}
	return result
}

type observerInputSource struct {
	id   string
	kind SourceKind
}

func observerInputSources(input string) []observerInputSource {
	var result []observerInputSource
	for _, line := range strings.Split(input, "\n") {
		var value struct {
			RecordType string     `json:"record_type"`
			ID         string     `json:"id"`
			Kind       SourceKind `json:"kind"`
		}
		if json.Unmarshal([]byte(line), &value) == nil && value.RecordType == "source" && value.ID != "" {
			result = append(result, observerInputSource{id: value.ID, kind: value.Kind})
		}
	}
	return result
}

func cloneRequest(request modelclient.Request) modelclient.Request {
	result := request
	result.Messages = make([]modelclient.Message, len(request.Messages))
	for index, message := range request.Messages {
		result.Messages[index] = cloneModelMessage(message)
	}
	result.Tools = append([]modelclient.Tool(nil), request.Tools...)
	return result
}

func TestSessionAutoCompactionInjectsCommittedMemoryAfterRawTrim(t *testing.T) {
	model := &compactionTestModel{}
	uuidCalls := 0
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 8192, MaxToolRounds: 2, ContextCompaction: ContextCompactionAuto,
		Now: func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) {
			uuidCalls++
			return "must-not-be-used-for-context", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.contextRuntime.observeAfterTokens = 1
	session.contextRuntime.observerChunkTokens = 8192
	session.hotRawTokenLimit = 1

	if _, err := session.Send(t.Context(), "关键约束：后续回答始终先给结论"); err != nil {
		t.Fatal(err)
	}
	waitForRuntime(t, session.contextRuntime, func(runtime *ContextRuntime) bool {
		return runtime.ledger.SuccessfulObserverRuns >= 1 && !runtime.consolidationRunning
	})
	for _, input := range []string{"第2轮补充", "第3轮补充", "第4轮检查"} {
		if _, err := session.Send(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		waitForRuntime(t, session.contextRuntime, func(runtime *ContextRuntime) bool {
			return !runtime.consolidationRunning
		})
	}
	if uuidCalls != 0 {
		t.Fatalf("context IDs consumed Options.NewUUID %d times", uuidCalls)
	}

	var fourth modelclient.Request
	requests := model.requestSnapshot()
	for _, request := range requests {
		if len(request.Tools) == 1 {
			if request.MaxTokens > 2048 || request.MaxTokens <= 0 {
				t.Fatalf("internal request MaxTokens=%d", request.MaxTokens)
			}
			if request.Tools[0].Function.Name != observerToolName && request.Tools[0].Function.Name != reflectorToolName {
				t.Fatalf("internal request exposed unexpected tool %q", request.Tools[0].Function.Name)
			}
			if request.Tools[0].Function.Name == reflectorToolName && strings.Contains(request.Messages[len(request.Messages)-1].Content, `"record_type":"source"`) {
				t.Fatal("reflector received raw source evidence")
			}
		}
		if requestContains(request, "第4轮检查") && len(request.Tools) == len(Tools()) {
			fourth = request
		}
	}
	if len(fourth.Messages) == 0 {
		t.Fatal("did not capture the fourth production request")
	}
	memoryFound, oldRawFound := false, false
	for _, message := range fourth.Messages {
		if message.Role == "system" && strings.Contains(message.Content, "用户要求后续回答始终先给结论") &&
			strings.Contains(message.Content, "服务端状态可能已变化") && strings.Contains(message.Content, "obs_") {
			memoryFound = true
		}
		if message.Role == "user" && strings.Contains(message.Content, "关键约束") {
			oldRawFound = true
		}
	}
	if !memoryFound || oldRawFound {
		t.Fatalf("committed memory/raw trim mismatch: memory=%t oldRaw=%t messages=%+v", memoryFound, oldRawFound, fourth.Messages)
	}

	session.contextRuntime.mu.Lock()
	defer session.contextRuntime.mu.Unlock()
	warmFound := false
	for _, sourceID := range session.contextRuntime.ledger.SourceOrder {
		source := session.contextRuntime.ledger.Sources[sourceID]
		if source.TurnID == "turn-1" {
			warmFound = warmFound || source.Retention == RetentionWarm && !source.HasModelMessage && source.SourceAvailable
		}
	}
	if !warmFound {
		t.Fatal("trimmed raw turn did not become bounded warm evidence")
	}
}

func TestContextOpaqueIDsAreIndependentRandom96BitValuesWithCollisionChecks(t *testing.T) {
	generated, err := defaultContextIDSource("src_")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(generated, "src_"))
	if err != nil || len(decoded) != 12 || !validOpaqueID(generated, "src_") {
		t.Fatalf("opaque ID=%q decoded=%d err=%v", generated, len(decoded), err)
	}

	calls := 0
	idSource := func(prefix string) (string, error) {
		calls++
		if calls <= 2 {
			return prefix + "0000000000000001", nil
		}
		return prefix + "0000000000000002", nil
	}
	estimator := NewTokenEstimator()
	runtime := newContextRuntime(&singleResponseModel{}, Options{
		ContextWindow: 8192, ContextCompaction: ContextCompactionAuto, Now: time.Now, ContextIDSource: idSource,
	}, estimator)
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	first, err := runtime.appendSource(sourceDraft{
		TurnID: "turn-1", Kind: SourceUser, ModelMessage: modelclient.Message{Role: "user", Content: "api_key=abcdefghijk"},
		RecallText: "api_key=abcdefghijk", Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.appendSource(sourceDraft{
		TurnID: "turn-2", Kind: SourceTool,
		ModelMessage: modelclient.Message{Role: "tool", Content: "safe", ToolCalls: []modelclient.ToolCall{{Function: modelclient.ToolFunction{Arguments: `{"secret":"raw"}`}}}},
		RecallText:   "safe", Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || calls != 3 {
		t.Fatalf("collision was not retried: first=%q second=%q calls=%d", first, second, calls)
	}
	runtime.mu.Lock()
	captured := runtime.ledger.Sources[first]
	toolSource := runtime.ledger.Sources[second]
	runtime.mu.Unlock()
	if strings.Contains(captured.RecallText, "abcdefghijk") || strings.Contains(captured.ModelMessage.Content, "abcdefghijk") ||
		!strings.Contains(captured.RecallText, "[REDACTED]") || len(toolSource.ModelMessage.ToolCalls) != 0 {
		t.Fatalf("source capture retained credentials or tool arguments: user=%+v tool=%+v", captured, toolSource)
	}
}

func TestContextEvidenceSanitizesAssignmentJSONAndBearerCredentials(t *testing.T) {
	input := "api_key=abcdefghijk Authorization: Bearer abcdefghijklmnop \"device_token\": \"qrstuvwxyz123456\" bearer zyxwvutsrqponmlk"
	sanitized := sanitizeContextEvidence(input)
	for _, secret := range []string{"abcdefghijk", "abcdefghijklmnop", "qrstuvwxyz123456", "zyxwvutsrqponmlk"} {
		if strings.Contains(sanitized, secret) {
			t.Fatalf("sanitized evidence retained credential %q: %s", secret, sanitized)
		}
	}
	if strings.Count(sanitized, "[REDACTED]") != 4 || looksLikeUnredactedSecret(sanitized) {
		t.Fatalf("sanitized evidence=%q", sanitized)
	}
}

func TestObserverEmptyAndInvalidOutputDoNotAdvanceCoverage(t *testing.T) {
	tests := []struct {
		name     string
		response modelclient.Response
		failure  bool
	}{
		{
			name:     "empty",
			response: modelclient.Response{Message: modelclient.Message{Role: "assistant"}},
		},
		{
			name:    "invalid source",
			failure: true,
			response: modelclient.Response{Message: modelclient.Message{
				Role: "assistant",
				ToolCalls: []modelclient.ToolCall{{
					ID: "bad", Type: "function",
					Function: modelclient.ToolFunction{
						Name:      observerToolName,
						Arguments: `{"covers_up_to_id":"src_AAAAAAAAAAAAAAAA","observations":[{"content":"约束","relevance":"critical","kind":"user_constraint","source_entry_ids":["src_BBBBBBBBBBBBBBBB"],"authority":"session_statement","freshness":"session_current","supersedes_observation_ids":[],"supersession_reason":""}]}`,
					},
				}},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &singleResponseModel{response: test.response}
			runtime := newTestRuntime(model)
			defer func() { runtime.beginClose(); runtime.waitAndClear() }()
			if _, err := runtime.appendSource(sourceDraft{
				TurnID: "turn-1", Kind: SourceUser, ModelMessage: modelclient.Message{Role: "user", Content: "关键约束"},
				RecallText: "关键约束", Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
			}); err != nil {
				t.Fatal(err)
			}
			runtime.observeAfterTokens = 1
			runtime.triggerConsolidation()
			waitForRuntime(t, runtime, func(runtime *ContextRuntime) bool { return !runtime.consolidationRunning })
			runtime.mu.Lock()
			coverage := runtime.ledger.CoverageIndex
			sources := len(runtime.ledger.Sources)
			failures := runtime.observerFailures
			blocked := runtime.observerBlockedUntil
			runtime.mu.Unlock()
			if coverage != -1 || sources != 1 || blocked <= 1 || test.failure != (failures > 0) {
				t.Fatalf("coverage=%d sources=%d failures=%d blocked=%d", coverage, sources, failures, blocked)
			}
		})
	}
}

func TestObserverAndReflectorRejectUnsafeOrUnboundedProtocol(t *testing.T) {
	source := SourceEntry{
		ID: "src_0000000000000001", Kind: SourceUser, Authority: AuthoritySessionStatement,
		Freshness: FreshnessSessionCurrent, RecallText: "约束", SourceAvailable: true,
	}
	snapshot := observerSnapshot{
		Sources: []SourceEntry{source}, SourcePositions: map[string]int{source.ID: 0},
		ActiveObservations: []Observation{{ID: "obs_0000000000000001", Content: "旧约束"}},
	}
	validArgs, _ := json.Marshal(observerRecordArgs{
		CoversUpToID: source.ID,
		Observations: []observerObservationArg{{
			Content: "用户要求先给结论", Relevance: RelevanceCritical, Kind: ObservationUserConstraint,
			SourceEntryIDs: []string{source.ID}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
			Supersedes: []string{}, SupersessionReason: "",
		}},
	})
	validCall := modelclient.ToolCall{ID: "record", Type: "function", Function: modelclient.ToolFunction{Name: observerToolName, Arguments: string(validArgs)}}
	fiveCalls := make([]modelclient.ToolCall, 5)
	for index := range fiveCalls {
		fiveCalls[index] = validCall
	}
	observerCases := []modelclient.Message{
		{Role: "assistant", Content: "不用工具"},
		{Role: "assistant", ToolCalls: fiveCalls},
		{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "bad", Type: "function", Function: modelclient.ToolFunction{Name: observerToolName, Arguments: strings.Replace(string(validArgs), "用户要求先给结论", "用户要求\\n先给结论", 1)}}}},
		{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "bad", Type: "function", Function: modelclient.ToolFunction{Name: observerToolName, Arguments: strings.Replace(string(validArgs), `"critical"`, `"urgent"`, 1)}}}},
		{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "bad", Type: "function", Function: modelclient.ToolFunction{Name: observerToolName, Arguments: strings.Replace(string(validArgs), `"content"`, `"id":"obs_supplied","content"`, 1)}}}},
	}
	for index, message := range observerCases {
		if _, err := validateObserverResponse(message, snapshot); err == nil {
			t.Fatalf("observer case %d accepted unsafe protocol", index)
		}
	}

	active := Observation{
		ID: "obs_0000000000000001", Content: "用户约束", Kind: ObservationUserConstraint,
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}
	reflectionArgs := marshalReflectorArguments(reflectorRecordArgs{Reflections: []reflectorReflectionArg{{
		Content: "耐久约束", Kind: ReflectionUserConstraint,
		Support:   []reflectorCoverageArg{{ObservationID: active.ID, Fidelity: CoverageFidelity("complete")}},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}})
	reflectionCall := modelclient.ToolCall{ID: "reflect", Type: "function", Function: modelclient.ToolFunction{Name: reflectorToolName, Arguments: reflectionArgs}}
	if _, err := validateReflectorResponse(modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{reflectionCall}}, reflectorSnapshot{Observations: []Observation{active}}); err == nil {
		t.Fatal("reflector accepted fidelity outside partial|exact")
	}
	reflectionCall.Function.Arguments = strings.Replace(reflectionCall.Function.Arguments, `"complete"`, `"partial"`, 1)
	if _, err := validateReflectorResponse(modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{reflectionCall, reflectionCall}}, reflectorSnapshot{Observations: []Observation{active}}); err == nil {
		t.Fatal("reflector accepted more than one record call")
	}
}

func TestCoverageWatermarkIsMonotonic(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	ids := appendRuntimeSources(t, runtime, 3, "turn")
	snapshot := observerSnapshot{Sources: []SourceEntry{}, SourcePositions: map[string]int{}, SourceCount: 3}
	runtime.mu.Lock()
	for _, id := range ids {
		snapshot.Sources = append(snapshot.Sources, cloneSourceEntry(runtime.ledger.Sources[id]))
		snapshot.SourcePositions[id] = runtime.ledger.SourceIndex[id]
	}
	runtime.mu.Unlock()
	first := observerResult{CoversUpToID: ids[1], Observations: []observationDraft{{
		Content: "用户有一个明确约束", Relevance: RelevanceCritical, Kind: ObservationUserConstraint,
		SourceEntryIDs: []string{ids[0]}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}
	if !runtime.commitObserverResult(snapshot, first) {
		t.Fatal("initial observer result was not committed")
	}
	if runtime.commitObserverResult(snapshot, observerResult{CoversUpToID: ids[0], Observations: []observationDraft{{
		Content: "倒退覆盖", Relevance: RelevanceHigh, Kind: ObservationUserIntent,
		SourceEntryIDs: []string{ids[0]}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("coverage watermark moved backwards")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.ledger.CoverageWatermark != ids[1] || runtime.ledger.CoverageIndex != 1 {
		t.Fatalf("coverage=%q index=%d", runtime.ledger.CoverageWatermark, runtime.ledger.CoverageIndex)
	}
}

func TestReflectionExactPrunesButPartialAndLowValueDoNotPruneCriticalObservation(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	ids := appendRuntimeSources(t, runtime, 1, "turn-1")
	snapshot := observerSnapshot{SourcePositions: map[string]int{ids[0]: 0}, SourceCount: 1}
	runtime.mu.Lock()
	snapshot.Sources = []SourceEntry{cloneSourceEntry(runtime.ledger.Sources[ids[0]])}
	runtime.mu.Unlock()
	if !runtime.commitObserverResult(snapshot, observerResult{CoversUpToID: ids[0], Observations: []observationDraft{{
		Content: "用户要求所有回答先给结论", Relevance: RelevanceLow, Kind: ObservationUserConstraint,
		SourceEntryIDs: ids, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("observation commit failed")
	}
	runtime.markTurnsWarm([]string{"turn-1"})
	runtime.mu.Lock()
	observationID := runtime.ledger.ObservationOrder[0]
	activeAfterExpiry := runtime.observationActiveLocked(observationID)
	partialSnapshot := reflectorSnapshot{Observations: runtime.activeObservationsLocked(), ObserverRuns: runtime.ledger.SuccessfulObserverRuns}
	runtime.mu.Unlock()
	if !activeAfterExpiry {
		t.Fatal("critical observation was removed by low-value expiry")
	}
	if !runtime.commitReflectorResult(partialSnapshot, reflectorResult{Reflections: []reflectionDraft{{
		Content: "用户偏好简洁结构", Kind: ReflectionUserConstraint,
		Support:   []CoverageEdge{{ObservationID: observationID, Fidelity: CoveragePartial}},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("partial reflection commit failed")
	}
	runtime.mu.Lock()
	activeAfterPartial := runtime.observationActiveLocked(observationID)
	exactSnapshot := reflectorSnapshot{Observations: runtime.activeObservationsLocked(), Reflections: runtime.reflectionsLocked(), ObserverRuns: runtime.ledger.SuccessfulObserverRuns}
	runtime.mu.Unlock()
	if !activeAfterPartial {
		t.Fatal("partial coverage pruned an observation")
	}
	if !runtime.commitReflectorResult(exactSnapshot, reflectorResult{Reflections: []reflectionDraft{{
		Content: "用户明确要求所有后续回答必须先给结论", Kind: ReflectionUserConstraint,
		Support:   []CoverageEdge{{ObservationID: observationID, Fidelity: CoverageExact}},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("exact reflection commit failed")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.observationActiveLocked(observationID) || runtime.ledger.Tombstones[observationID].Reason != DropExactCoverage {
		t.Fatalf("exact coverage did not deterministically prune observation: %+v", runtime.ledger.Tombstones[observationID])
	}
	for _, reflectionID := range runtime.ledger.ReflectionOrder {
		if !strings.HasPrefix(reflectionID, "ref_") {
			t.Fatalf("reflection ID=%q", reflectionID)
		}
	}
}

func TestDeterministicPruningUsesDuplicateSupersessionAndNewerSnapshotRules(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	sessionIDs := appendRuntimeSources(t, runtime, 2, "turn-session")
	firstSnapshot := runtimeObserverSnapshot(t, runtime, sessionIDs[:1])
	duplicate := observationDraft{
		Content: "用户最初要求详细解释", Relevance: RelevanceHigh, Kind: ObservationUserConstraint,
		SourceEntryIDs: []string{sessionIDs[0]}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}
	if !runtime.commitObserverResult(firstSnapshot, observerResult{CoversUpToID: sessionIDs[0], Observations: []observationDraft{duplicate, duplicate}}) {
		t.Fatal("duplicate observer batch did not commit")
	}
	runtime.mu.Lock()
	if len(runtime.ledger.ObservationOrder) != 1 {
		runtime.mu.Unlock()
		t.Fatalf("duplicate identity created %d observations", len(runtime.ledger.ObservationOrder))
	}
	olderObservationID := runtime.ledger.ObservationOrder[0]
	runtime.mu.Unlock()
	secondSnapshot := runtimeObserverSnapshot(t, runtime, sessionIDs[1:])
	if !runtime.commitObserverResult(secondSnapshot, observerResult{CoversUpToID: sessionIDs[1], Observations: []observationDraft{{
		Content: "用户更正为只要简洁结论", Relevance: RelevanceCritical, Kind: ObservationCorrection,
		SourceEntryIDs: []string{sessionIDs[1]}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
		Supersedes: []string{olderObservationID}, SupersessionReason: "用户明确更正",
	}}}) {
		t.Fatal("superseding observation did not commit")
	}
	runtime.mu.Lock()
	if runtime.ledger.Tombstones[olderObservationID].Reason != DropSuperseded {
		runtime.mu.Unlock()
		t.Fatalf("supersession tombstone=%+v", runtime.ledger.Tombstones[olderObservationID])
	}
	runtime.mu.Unlock()

	toolIDs := make([]string, 0, 2)
	for version := int64(1); version <= 2; version++ {
		id, err := runtime.appendSource(sourceDraft{
			TurnID: fmt.Sprintf("turn-tool-%d", version), Kind: SourceTool,
			ModelMessage: modelclient.Message{Role: "tool", Content: fmt.Sprintf("route version %d", version)},
			RecallText:   fmt.Sprintf("route version %d", version), Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
			ServerReference: &ServerReference{Tool: "get_learning_route", Entity: "route_revision", EntityID: "route-current", Version: version},
		})
		if err != nil {
			t.Fatal(err)
		}
		toolIDs = append(toolIDs, id)
	}
	toolSnapshot := runtimeObserverSnapshot(t, runtime, toolIDs)
	if !runtime.commitObserverResult(toolSnapshot, observerResult{CoversUpToID: toolIDs[1], Observations: []observationDraft{
		{Content: "路线历史快照版本1", Relevance: RelevanceMedium, Kind: ObservationToolSnapshot, SourceEntryIDs: []string{toolIDs[0]}, Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical},
		{Content: "路线历史快照版本2", Relevance: RelevanceMedium, Kind: ObservationToolSnapshot, SourceEntryIDs: []string{toolIDs[1]}, Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical},
	}}) {
		t.Fatal("versioned snapshots did not commit")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	var oldSnapshotID, newSnapshotID string
	for _, observationID := range runtime.ledger.ObservationOrder {
		observation := runtime.ledger.Observations[observationID]
		if observation.Content == "路线历史快照版本1" {
			oldSnapshotID = observationID
		}
		if observation.Content == "路线历史快照版本2" {
			newSnapshotID = observationID
		}
	}
	if runtime.ledger.Tombstones[oldSnapshotID].Reason != DropNewerSnapshot || !runtime.observationActiveLocked(newSnapshotID) {
		t.Fatalf("newer snapshot pruning old=%+v newActive=%t", runtime.ledger.Tombstones[oldSnapshotID], runtime.observationActiveLocked(newSnapshotID))
	}
}

func TestWarmEvidenceLimitReclaimsOnlySafeOldestBodiesAndRetainsMetadata(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	runtime.warmEvidenceLimit = 25
	ids := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		id, err := runtime.appendSource(sourceDraft{
			TurnID: fmt.Sprintf("turn-%d", index), Kind: SourceUser,
			ModelMessage: modelclient.Message{Role: "user", Content: strings.Repeat(string(rune('a'+index)), 20)},
			RecallText:   strings.Repeat(string(rune('a'+index)), 20),
			Authority:    AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	runtime.mu.Lock()
	snapshot := observerSnapshot{Sources: make([]SourceEntry, 0, len(ids)), SourcePositions: make(map[string]int), SourceCount: len(ids)}
	for _, id := range ids {
		snapshot.Sources = append(snapshot.Sources, cloneSourceEntry(runtime.ledger.Sources[id]))
		snapshot.SourcePositions[id] = runtime.ledger.SourceIndex[id]
	}
	runtime.mu.Unlock()
	if !runtime.commitObserverResult(snapshot, observerResult{CoversUpToID: ids[2], Observations: []observationDraft{{
		Content: "不能丢失的用户约束", Relevance: RelevanceCritical, Kind: ObservationUserConstraint,
		SourceEntryIDs: []string{ids[0]}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("failed to seed unsafe warm evidence")
	}
	runtime.markTurnsWarm([]string{"turn-0", "turn-1", "turn-2"})
	runtime.mu.Lock()
	first, second, third := runtime.ledger.Sources[ids[0]], runtime.ledger.Sources[ids[1]], runtime.ledger.Sources[ids[2]]
	runtime.mu.Unlock()
	if !first.SourceAvailable || second.SourceAvailable || third.SourceAvailable || first.ContentHash == "" || second.ContentHash == "" ||
		first.Retention != RetentionWarm || second.Retention != RetentionMetadata || third.Retention != RetentionMetadata ||
		first.HasModelMessage || second.HasModelMessage || third.HasModelMessage {
		t.Fatalf("unsafe evidence was reclaimed or safe order changed: first=%+v second=%+v third=%+v", first, second, third)
	}

	bounded := newTestRuntime(&singleResponseModel{})
	defer func() { bounded.beginClose(); bounded.waitAndClear() }()
	largeID, err := bounded.appendSource(sourceDraft{
		TurnID: "turn-large", Kind: SourceAssistant, ModelMessage: modelclient.Message{Role: "assistant", Content: strings.Repeat("x", 20<<10)},
		RecallText: strings.Repeat("x", 20<<10), Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	bounded.mu.Lock()
	large := bounded.ledger.Sources[largeID]
	bounded.mu.Unlock()
	if len(large.RecallText) != maxContextSourceRecallBytes {
		t.Fatalf("recall length=%d", len(large.RecallText))
	}
}

func TestServerSnapshotMemoryIsHistoricalAndRequiresRefresh(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	projection := projectToolResult("get_learning_route", map[string]any{
		"route_revision_id": "route-revision-2", "revision": 2, "steps": []any{},
	})
	id, err := runtime.appendSource(sourceDraft{
		TurnID: "turn-1", Kind: SourceTool, ModelMessage: modelclient.Message{Role: "tool", ToolCallID: "call-route", Content: projection.Live},
		RecallText: projection.Recall, Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
		ServerReference: projection.ServerReference,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	source := runtime.ledger.Sources[id]
	runtime.mu.Unlock()
	if source.ServerReference == nil || source.ServerReference.Version != 2 || source.Authority != AuthorityServerSnapshot || source.Freshness != FreshnessHistorical {
		t.Fatalf("server source=%+v", source)
	}
	snapshot := observerSnapshot{Sources: []SourceEntry{source}, SourcePositions: map[string]int{id: 0}, SourceCount: 1}
	if !runtime.commitObserverResult(snapshot, observerResult{CoversUpToID: id, Observations: []observationDraft{{
		Content: "路线 revision 2 是历史服务端快照", Relevance: RelevanceHigh, Kind: ObservationToolSnapshot,
		SourceEntryIDs: []string{id}, Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
	}}}) {
		t.Fatal("server observation commit failed")
	}
	memory := runtime.memoryProjection()
	joined := memory.Instruction + "\n" + strings.Join(memory.Items, "\n")
	if !strings.Contains(joined, "authority=server_snapshot") || !strings.Contains(joined, "historical_snapshot") ||
		!strings.Contains(joined, "服务端状态可能已变化") || !strings.Contains(joined, "重新调用对应读取工具") {
		t.Fatalf("server snapshot memory=%s", joined)
	}
}

func TestContentRedactedInvalidatesAllServerBodiesAndDerivedMemory(t *testing.T) {
	session := newTestSession(t, &singleResponseModel{}, &fakeServer{})
	defer session.Close()
	if _, err := session.startTurn(); err != nil {
		t.Fatal(err)
	}
	if err := session.appendToolResult("get_learning_route", "call-route-old", map[string]any{
		"route_revision_id": "route-secret", "revision": 1, "generation": "4", "steps": []any{"SECRET_ROUTE_BODY"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.appendToolResult("list_long_term_preferences", "call-pref-old", map[string]any{
		"generation": "4", "items": []map[string]any{{"revision": 2, "content": "SECRET_PREFERENCE_BODY"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.appendCapturedMessage(session.currentTurnID,
		modelclient.Message{Role: "assistant", Content: "SECRET_ASSISTANT_DERIVED"},
		"SECRET_ASSISTANT_DERIVED", SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
		t.Fatal(err)
	}
	turn := session.turns[session.currentTurnID]
	oldSourceIDs := append([]string(nil), turn.SourceIDs...)
	snapshot := runtimeObserverSnapshot(t, session.contextRuntime, oldSourceIDs)
	if !session.contextRuntime.commitObserverResult(snapshot, observerResult{
		CoversUpToID: oldSourceIDs[len(oldSourceIDs)-1],
		Observations: []observationDraft{
			{Content: "SECRET_ROUTE_OBSERVATION", Relevance: RelevanceHigh, Kind: ObservationToolSnapshot, SourceEntryIDs: []string{oldSourceIDs[0]}, Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical},
			{Content: "SECRET_PREFERENCE_OBSERVATION", Relevance: RelevanceHigh, Kind: ObservationToolSnapshot, SourceEntryIDs: []string{oldSourceIDs[1]}, Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical},
		},
	}) {
		t.Fatal("failed to seed server observations")
	}
	session.contextRuntime.mu.Lock()
	observationIDs := append([]string(nil), session.contextRuntime.ledger.ObservationOrder...)
	reflector := reflectorSnapshot{Observations: session.contextRuntime.activeObservationsLocked(), ObserverRuns: session.contextRuntime.ledger.SuccessfulObserverRuns}
	session.contextRuntime.mu.Unlock()
	if !session.contextRuntime.commitReflectorResult(reflector, reflectorResult{Reflections: []reflectionDraft{{
		Content: "SECRET_SERVER_REFLECTION", Kind: ReflectionServerState,
		Support:   []CoverageEdge{{ObservationID: observationIDs[0], Fidelity: CoveragePartial}, {ObservationID: observationIDs[1], Fidelity: CoveragePartial}},
		Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
	}}}) {
		t.Fatal("failed to seed server reflection")
	}

	if err := session.appendToolResult("get_learning_route", "call-redacted", map[string]any{
		"error": "route unavailable", "code": "content_redacted",
	}); err != nil {
		t.Fatal(err)
	}

	for _, message := range session.messages {
		if message.ToolCallID == "call-route-old" || message.ToolCallID == "call-pref-old" {
			if message.Content != invalidatedToolResultJSON || !json.Valid([]byte(message.Content)) {
				t.Fatalf("raw tool body was not replaced: %+v", message)
			}
		}
		if strings.Contains(message.Content, "SECRET_") {
			t.Fatalf("raw messages retained invalidated body: %+v", message)
		}
	}
	for _, callID := range []string{"call-route-old", "call-pref-old"} {
		if session.toolHistory[callID] != invalidatedToolResultJSON {
			t.Fatalf("history[%s]=%q", callID, session.toolHistory[callID])
		}
	}

	session.contextRuntime.mu.Lock()
	for _, sourceID := range oldSourceIDs {
		source := session.contextRuntime.ledger.Sources[sourceID]
		if source.Freshness != FreshnessInvalidated || source.SourceAvailable || source.RecallText != "" || source.HasModelMessage ||
			source.ModelMessage.Content != "" || source.ContentHash == "" {
			session.contextRuntime.mu.Unlock()
			t.Fatalf("invalidated source lost metadata or retained body: %+v", source)
		}
		if source.Kind == SourceTool && source.ServerReference == nil {
			session.contextRuntime.mu.Unlock()
			t.Fatalf("invalidated tool source lost server reference: %+v", source)
		}
	}
	for _, observationID := range observationIDs {
		if session.contextRuntime.ledger.Observations[observationID].Freshness != FreshnessInvalidated {
			session.contextRuntime.mu.Unlock()
			t.Fatalf("observation %s remained fresh", observationID)
		}
	}
	reflectionID := session.contextRuntime.ledger.ReflectionOrder[0]
	if session.contextRuntime.ledger.Reflections[reflectionID].Freshness != FreshnessInvalidated ||
		len(session.contextRuntime.activeObservationsLocked()) != 0 || len(session.contextRuntime.reflectionsLocked()) != 0 {
		session.contextRuntime.mu.Unlock()
		t.Fatal("invalidated derived memory remained active")
	}
	session.contextRuntime.observeAfterTokens = 1
	observer, ok := session.contextRuntime.observerSnapshotLocked()
	session.contextRuntime.mu.Unlock()
	for _, source := range observer.Sources {
		if source.Freshness == FreshnessInvalidated || strings.Contains(source.RecallText, "SECRET_") {
			t.Fatalf("observer snapshot retained invalidated evidence: %+v", source)
		}
	}
	if !ok {
		t.Fatal("redaction result should remain observable as a safe failure")
	}
	memory := session.contextRuntime.memoryProjection()
	if strings.Contains(strings.Join(memory.Items, "\n"), "SECRET_") {
		t.Fatalf("memory projection restored invalidated content: %+v", memory)
	}
}

func TestPreferenceMemoryGenerationAndPrivacyDegradationInvalidateOldBodies(t *testing.T) {
	for _, test := range []struct {
		name            string
		newPage         api.MemoryExportPage
		wantMemoryGen   int64
		wantPrivacyCode string
	}{
		{
			name: "redacted item advances memory generation",
			newPage: api.MemoryExportPage{
				Items: []api.MemoryExportItem{{
					Record:        api.MemoryRecord{LogicalMemoryID: "memory-1", CandidateID: "candidate-1", Revision: 2},
					ContentStatus: "redacted", Content: "REDACTED_BODY_MUST_NOT_RETURN",
				}},
				ReadGeneration: api.MemoryGenerationStamp{LearnerGeneration: 7, MemoryGeneration: 11},
				ReasonCodes:    []string{},
			},
			wantMemoryGen:   11,
			wantPrivacyCode: "content_redacted",
		},
		{
			name: "privacy degradation at same generation",
			newPage: api.MemoryExportPage{
				Items:          []api.MemoryExportItem{},
				ReadGeneration: api.MemoryGenerationStamp{LearnerGeneration: 7, MemoryGeneration: 10},
				Degraded:       true,
				ReasonCodes:    []string{"privacy_clear_in_progress"},
			},
			wantMemoryGen:   10,
			wantPrivacyCode: "privacy_clear_in_progress",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &fakeServer{exportResult: api.MemoryExportPage{
				Items: []api.MemoryExportItem{{
					Record:        api.MemoryRecord{LogicalMemoryID: "memory-1", CandidateID: "candidate-1", Revision: 2},
					ContentStatus: "available", Content: "PREFERENCE_SECRET_BODY",
				}},
				ReadGeneration: api.MemoryGenerationStamp{LearnerGeneration: 7, MemoryGeneration: 10},
				ReasonCodes:    []string{},
			}}
			session := newTestSession(t, &singleResponseModel{}, server)
			defer session.Close()
			if _, err := session.startTurn(); err != nil {
				t.Fatal(err)
			}

			oldValue, _ := session.executeReadTool(t.Context(), modelclient.ToolCall{Function: modelclient.ToolFunction{
				Name: "list_long_term_preferences", Arguments: `{}`,
			}})
			oldJSON, _ := json.Marshal(oldValue)
			if !strings.Contains(string(oldJSON), `"learner_generation":7`) || !strings.Contains(string(oldJSON), `"memory_generation":10`) {
				t.Fatalf("old preference result lost generation pair: %s", oldJSON)
			}
			if err := session.appendToolResult("list_long_term_preferences", "call-pref-old", oldValue); err != nil {
				t.Fatal(err)
			}
			if err := session.appendCapturedMessage(session.currentTurnID,
				modelclient.Message{Role: "assistant", Content: "PREFERENCE_SECRET_ASSISTANT"},
				"PREFERENCE_SECRET_ASSISTANT", SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
				t.Fatal(err)
			}
			oldSourceIDs := append([]string(nil), session.turns[session.currentTurnID].SourceIDs...)
			snapshot := runtimeObserverSnapshot(t, session.contextRuntime, oldSourceIDs)
			if !session.contextRuntime.commitObserverResult(snapshot, observerResult{
				CoversUpToID: oldSourceIDs[len(oldSourceIDs)-1],
				Observations: []observationDraft{{
					Content: "PREFERENCE_SECRET_OBSERVATION", Relevance: RelevanceHigh, Kind: ObservationToolSnapshot,
					SourceEntryIDs: []string{oldSourceIDs[0]}, Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
				}},
			}) {
				t.Fatal("failed to seed preference observation")
			}
			session.contextRuntime.mu.Lock()
			observationID := session.contextRuntime.ledger.ObservationOrder[0]
			reflector := reflectorSnapshot{Observations: session.contextRuntime.activeObservationsLocked(), ObserverRuns: session.contextRuntime.ledger.SuccessfulObserverRuns}
			session.contextRuntime.mu.Unlock()
			if !session.contextRuntime.commitReflectorResult(reflector, reflectorResult{Reflections: []reflectionDraft{{
				Content: "PREFERENCE_SECRET_REFLECTION", Kind: ReflectionServerState,
				Support:   []CoverageEdge{{ObservationID: observationID, Fidelity: CoveragePartial}},
				Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
			}}}) {
				t.Fatal("failed to seed preference reflection")
			}

			server.exportResult = test.newPage
			newValue, _ := session.executeReadTool(t.Context(), modelclient.ToolCall{Function: modelclient.ToolFunction{
				Name: "list_long_term_preferences", Arguments: `{}`,
			}})
			newJSON, _ := json.Marshal(newValue)
			if !strings.Contains(string(newJSON), `"privacy_invalidated":true`) || !strings.Contains(string(newJSON), test.wantPrivacyCode) ||
				strings.Contains(string(newJSON), "REDACTED_BODY_MUST_NOT_RETURN") || strings.Contains(string(newJSON), "PREFERENCE_SECRET") {
				t.Fatalf("privacy-degraded result leaked or omitted signal: %s", newJSON)
			}
			if err := session.appendToolResult("list_long_term_preferences", "call-pref-new", newValue); err != nil {
				t.Fatal(err)
			}

			if session.toolHistory["call-pref-old"] != invalidatedToolResultJSON {
				t.Fatalf("old preference history survived: %q", session.toolHistory["call-pref-old"])
			}
			newReference := session.toolReferences["call-pref-new"]
			if newReference == nil || newReference.LearnerGeneration != 7 || newReference.MemoryGeneration != test.wantMemoryGen {
				t.Fatalf("new preference reference=%+v", newReference)
			}
			for index, message := range session.messages {
				if strings.Contains(message.Content, "PREFERENCE_SECRET") {
					t.Fatalf("message %d retained invalidated preference body: %+v", index, message)
				}
			}
			session.contextRuntime.mu.Lock()
			oldSource := session.contextRuntime.ledger.Sources[oldSourceIDs[0]]
			observation := session.contextRuntime.ledger.Observations[observationID]
			reflectionID := session.contextRuntime.ledger.ReflectionOrder[0]
			reflection := session.contextRuntime.ledger.Reflections[reflectionID]
			session.contextRuntime.mu.Unlock()
			if oldSource.Freshness != FreshnessInvalidated || oldSource.SourceAvailable || observation.Freshness != FreshnessInvalidated || reflection.Freshness != FreshnessInvalidated {
				t.Fatalf("privacy invalidation incomplete source=%+v observation=%+v reflection=%+v", oldSource, observation, reflection)
			}
			recalled := session.contextRuntime.recallMemory(observationID)
			recalledJSON, _ := json.Marshal(recalled)
			if toolResultCode(recalled) != ContextSourceUnavailable || strings.Contains(string(recalledJSON), "PREFERENCE_SECRET") {
				t.Fatalf("invalidated preference recall=%s", recalledJSON)
			}
		})
	}
}

func TestContentRedactedReplacesRawToolBodiesWithoutLedger(t *testing.T) {
	for _, mode := range []string{ContextCompactionRecentOnly, ContextCompactionOff} {
		t.Run(mode, func(t *testing.T) {
			session, err := New(&singleResponseModel{}, &fakeServer{}, Options{
				ContextWindow: 8192, MaxToolRounds: 2, ContextCompaction: mode,
				Now: time.Now, NewUUID: func() (string, error) { return "unused", nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if _, err := session.startTurn(); err != nil {
				t.Fatal(err)
			}
			if err := session.appendToolResult("get_due_reviews", "call-old", map[string]any{
				"generation": "3", "due_before": "2026-09-01T00:00:00Z", "items": []any{"SECRET_WITHOUT_LEDGER"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := session.appendCapturedMessage(session.currentTurnID,
				modelclient.Message{Role: "assistant", Content: "SECRET_ASSISTANT_WITHOUT_LEDGER"},
				"SECRET_ASSISTANT_WITHOUT_LEDGER", SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
				t.Fatal(err)
			}
			if err := session.appendToolResult("get_due_reviews", "call-redacted", map[string]any{
				"error": "unavailable", "code": "content_redacted",
			}); err != nil {
				t.Fatal(err)
			}
			if session.toolHistory["call-old"] != invalidatedToolResultJSON {
				t.Fatalf("history retained redacted body: %q", session.toolHistory["call-old"])
			}
			for index, message := range session.messages {
				if message.ToolCallID == "call-old" && message.Content != invalidatedToolResultJSON {
					t.Fatalf("raw tool body retained in mode %s: %+v", mode, message)
				}
				if message.Role == "assistant" && session.messageTurnIDs[index] == session.currentTurnID && message.Content != invalidatedAssistantText {
					t.Fatalf("derived assistant body retained in mode %s: %+v", mode, message)
				}
			}
		})
	}
}

func TestHigherGenerationInvalidatesOnlyOlderMatchingIdentity(t *testing.T) {
	session := newTestSession(t, &singleResponseModel{}, &fakeServer{})
	defer session.Close()
	if _, err := session.startTurn(); err != nil {
		t.Fatal(err)
	}
	appendResult := func(tool, callID string, value any) string {
		t.Helper()
		if err := session.appendToolResult(tool, callID, value); err != nil {
			t.Fatal(err)
		}
		turn := session.turns[session.currentTurnID]
		return turn.SourceIDs[len(turn.SourceIDs)-1]
	}
	oldDue := appendResult("get_due_reviews", "call-due-5", map[string]any{"generation": "5", "due_before": "2026-09-01T00:00:00Z", "items": []any{"OLD_DUE_BODY"}})
	otherIdentity := appendResult("list_long_term_preferences", "call-pref-5", map[string]any{"generation": "5", "items": []map[string]any{{"revision": 1, "content": "OTHER_BODY"}}})
	newDue := appendResult("get_due_reviews", "call-due-6", map[string]any{"generation": "6", "due_before": "2026-09-02T00:00:00Z", "items": []any{"NEW_DUE_BODY"}})

	session.contextRuntime.mu.Lock()
	oldSource := session.contextRuntime.ledger.Sources[oldDue]
	otherSource := session.contextRuntime.ledger.Sources[otherIdentity]
	newSource := session.contextRuntime.ledger.Sources[newDue]
	session.contextRuntime.mu.Unlock()
	if oldSource.Freshness != FreshnessInvalidated || oldSource.SourceAvailable ||
		otherSource.Freshness == FreshnessInvalidated || !otherSource.SourceAvailable ||
		newSource.Freshness == FreshnessInvalidated || !newSource.SourceAvailable {
		t.Fatalf("generation invalidation mismatch old=%+v other=%+v new=%+v", oldSource, otherSource, newSource)
	}
	if session.toolHistory["call-due-5"] != invalidatedToolResultJSON || !strings.Contains(session.toolHistory["call-due-6"], "NEW_DUE_BODY") {
		t.Fatalf("generation history mismatch: %+v", session.toolHistory)
	}

	lowerDue := appendResult("get_due_reviews", "call-due-lower", map[string]any{"generation": "5", "due_before": "2026-09-03T00:00:00Z", "items": []any{"LOWER_DUE_BODY"}})
	session.contextRuntime.mu.Lock()
	newSource = session.contextRuntime.ledger.Sources[newDue]
	lowerSource := session.contextRuntime.ledger.Sources[lowerDue]
	session.contextRuntime.mu.Unlock()
	if newSource.Freshness == FreshnessInvalidated || !newSource.SourceAvailable ||
		lowerSource.Freshness != FreshnessInvalidated || lowerSource.SourceAvailable || lowerSource.RecallText != "" {
		t.Fatalf("lower generation re-entered newer context: new=%+v lower=%+v", newSource, lowerSource)
	}
	if session.toolHistory["call-due-lower"] != staleServerGenerationToolResultJSON {
		t.Fatalf("lower generation history was not fenced: %q", session.toolHistory["call-due-lower"])
	}
	for _, message := range session.messages {
		if message.ToolCallID == "call-due-lower" && message.Content != staleServerGenerationToolResultJSON {
			t.Fatalf("lower generation raw result was not fenced: %+v", message)
		}
	}
}

type cancellationIgnoringConsolidationModel struct {
	started   chan struct{}
	release   chan struct{}
	completed chan struct{}
	once      sync.Once
}

func (m *cancellationIgnoringConsolidationModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if len(request.Tools) == 1 && request.Tools[0].Function.Name == observerToolName {
		m.once.Do(func() { close(m.started) })
		<-m.release
		sources := observerInputSources(request.Messages[len(request.Messages)-1].Content)
		args, _ := json.Marshal(observerRecordArgs{
			CoversUpToID: sources[len(sources)-1].id,
			Observations: []observerObservationArg{{
				Content: "LATE_OBSERVATION", Relevance: RelevanceHigh, Kind: ObservationUserIntent,
				SourceEntryIDs: []string{sources[0].id}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
				Supersedes: []string{}, SupersessionReason: "",
			}},
		})
		close(m.completed)
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
			ID: "late-observer", Type: "function", Function: modelclient.ToolFunction{Name: observerToolName, Arguments: string(args)},
		}}}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "正常回答"}}, nil
}

func TestSessionCloseIsBoundedWhenProviderIgnoresCancellation(t *testing.T) {
	model := &cancellationIgnoringConsolidationModel{started: make(chan struct{}), release: make(chan struct{}), completed: make(chan struct{})}
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 8192, MaxToolRounds: 2, ContextCompaction: ContextCompactionAuto,
		Now: time.Now, NewUUID: func() (string, error) { return "unused", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	session.contextRuntime.observeAfterTokens = 1
	session.contextRuntime.closeWait = 25 * time.Millisecond
	if _, err := session.Send(t.Context(), "触发忽略取消的后台整理"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("observer did not start")
	}
	started := time.Now()
	session.Close()
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Close exceeded configured boundary: %v", elapsed)
	}
	close(model.release)
	select {
	case <-model.completed:
	case <-time.After(time.Second):
		t.Fatal("late provider response did not return")
	}
	waitForRuntime(t, session.contextRuntime, func(runtime *ContextRuntime) bool { return !runtime.consolidationRunning })
	session.contextRuntime.mu.Lock()
	defer session.contextRuntime.mu.Unlock()
	if !session.contextRuntime.closed || len(session.contextRuntime.ledger.Sources) != 0 ||
		len(session.contextRuntime.ledger.Observations) != 0 || len(session.contextRuntime.ledger.Reflections) != 0 {
		t.Fatalf("late worker committed after close: %+v", session.contextRuntime.ledger)
	}
}

func TestRecentOnlyAndOffDoNotCallInternalConsolidationModel(t *testing.T) {
	for _, mode := range []string{ContextCompactionRecentOnly, ContextCompactionOff} {
		t.Run(mode, func(t *testing.T) {
			model := &compactionTestModel{}
			session, err := New(model, &fakeServer{}, Options{
				ContextWindow: 8192, MaxToolRounds: 2, ContextCompaction: mode, Now: time.Now,
				NewUUID: func() (string, error) { return "unused", nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 4; index++ {
				if _, err := session.Send(t.Context(), fmt.Sprintf("第%d轮", index)); err != nil {
					t.Fatal(err)
				}
			}
			session.Close()
			model.mu.Lock()
			observerCalls, reflectorCalls := model.observerCalls, model.reflectCalls
			model.mu.Unlock()
			if observerCalls != 0 || reflectorCalls != 0 {
				t.Fatalf("mode=%s observer=%d reflector=%d", mode, observerCalls, reflectorCalls)
			}
		})
	}
}

type blockingConsolidationModel struct {
	started chan struct{}
	once    sync.Once
	normal  atomic.Int32
}

func (m *blockingConsolidationModel) Complete(ctx context.Context, request modelclient.Request) (modelclient.Response, error) {
	if len(request.Tools) == 1 && request.Tools[0].Function.Name == observerToolName {
		m.once.Do(func() { close(m.started) })
		<-ctx.Done()
		return modelclient.Response{}, ctx.Err()
	}
	m.normal.Add(1)
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "正常回答"}}, nil
}

func TestSessionWorkerDoesNotBlockNextTurnAndCloseCancels(t *testing.T) {
	model := &blockingConsolidationModel{started: make(chan struct{})}
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 8192, MaxToolRounds: 2, ContextCompaction: ContextCompactionAuto,
		Now: time.Now, NewUUID: func() (string, error) { return "unused", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	session.contextRuntime.observeAfterTokens = 1
	if _, err := session.Send(t.Context(), "第一轮触发后台整理"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("observer did not start")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, sendErr := session.Send(context.Background(), "第二轮不等待后台")
		secondDone <- sendErr
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("next turn blocked on observer")
	}
	closed := make(chan struct{})
	go func() {
		session.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel and wait for observer")
	}
	if _, err := session.Send(t.Context(), "关闭后追加"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed session append err=%v", err)
	}
	session.contextRuntime.mu.Lock()
	defer session.contextRuntime.mu.Unlock()
	if len(session.contextRuntime.ledger.Sources) != 0 || session.contextRuntime.consolidationRunning {
		t.Fatalf("closed runtime retained ledger or worker: %+v", session.contextRuntime.ledger)
	}
}

func TestRecallSessionMemoryExactIDStatesAndPrivacy(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	sourceID, err := runtime.appendSource(sourceDraft{
		TurnID: "turn-1", Kind: SourceUser,
		ModelMessage: modelclient.Message{Role: "user", Content: "约束 api_key=SECRETSECRET"},
		RecallText:   "约束 api_key=SECRETSECRET", Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtimeObserverSnapshot(t, runtime, []string{sourceID})
	if !runtime.commitObserverResult(snapshot, observerResult{CoversUpToID: sourceID, Observations: []observationDraft{{
		Content: "用户要求先给结论", Relevance: RelevanceCritical, Kind: ObservationUserConstraint,
		SourceEntryIDs: []string{sourceID}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("observation commit failed")
	}
	runtime.mu.Lock()
	observationID := runtime.ledger.ObservationOrder[0]
	runtime.mu.Unlock()

	active := runtime.recallMemory(observationID)
	activeJSON, _ := json.Marshal(active)
	if active["status"] != "active" || toolResultCode(active) != "" || !json.Valid(activeJSON) ||
		strings.Contains(string(activeJSON), "SECRETSECRET") || !strings.Contains(string(activeJSON), "[REDACTED]") ||
		strings.Contains(string(activeJSON), "raw tool arguments") || strings.Contains(string(activeJSON), "observerSystemPrompt") {
		t.Fatalf("active recall=%s", activeJSON)
	}

	runtime.mu.Lock()
	runtime.tombstoneLocked(observationID, DropExactCoverage)
	runtime.mu.Unlock()
	dropped := runtime.recallMemory(observationID)
	if dropped["status"] != "dropped" || dropped["tombstone_reason"] != DropExactCoverage {
		t.Fatalf("dropped recall=%+v", dropped)
	}

	runtime.mu.Lock()
	reflectionID, err := runtime.allocateIDLocked("ref_")
	if err != nil {
		runtime.mu.Unlock()
		t.Fatal(err)
	}
	runtime.ledger.Reflections[reflectionID] = Reflection{
		ID: reflectionID, Content: "耐久约束", Kind: ReflectionUserConstraint,
		Support:   []CoverageEdge{{ObservationID: observationID, Fidelity: CoverageExact}},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent, CreatedAt: time.Now(),
	}
	runtime.ledger.ReflectionOrder = append(runtime.ledger.ReflectionOrder, reflectionID)
	runtime.mu.Unlock()
	reflection := runtime.recallMemory(reflectionID)
	reflectionJSON, _ := json.Marshal(reflection)
	if reflection["status"] != "active" || !strings.Contains(string(reflectionJSON), `"fidelity":"exact"`) ||
		!strings.Contains(string(reflectionJSON), `"status":"dropped"`) {
		t.Fatalf("reflection recall=%s", reflectionJSON)
	}

	runtime.mu.Lock()
	source := runtime.ledger.Sources[sourceID]
	source.SourceAvailable = false
	source.RecallText = ""
	source.Retention = RetentionMetadata
	runtime.ledger.Sources[sourceID] = source
	runtime.mu.Unlock()
	unavailable := runtime.recallMemory(observationID)
	if unavailable["status"] != "dropped" || toolResultCode(unavailable) != ContextSourceUnavailable {
		t.Fatalf("unavailable recall=%+v", unavailable)
	}

	runtime.mu.Lock()
	observation := runtime.ledger.Observations[observationID]
	observation.Freshness = FreshnessInvalidated
	runtime.ledger.Observations[observationID] = observation
	runtime.mu.Unlock()
	invalidated := runtime.recallMemory(observationID)
	if invalidated["status"] != "invalidated" || toolResultCode(invalidated) != ContextSourceUnavailable {
		t.Fatalf("invalidated recall=%+v", invalidated)
	}
	runtime.mu.Lock()
	reflected := runtime.ledger.Reflections[reflectionID]
	reflected.Freshness = FreshnessInvalidated
	runtime.ledger.Reflections[reflectionID] = reflected
	runtime.mu.Unlock()
	invalidatedReflection := runtime.recallMemory(reflectionID)
	invalidatedReflectionJSON, _ := json.Marshal(invalidatedReflection)
	if invalidatedReflection["status"] != "invalidated" || toolResultCode(invalidatedReflection) != ContextSourceUnavailable ||
		strings.Contains(string(invalidatedReflectionJSON), "耐久约束") || strings.Contains(string(invalidatedReflectionJSON), "用户要求先给结论") || strings.Contains(string(invalidatedReflectionJSON), "recall_text") {
		t.Fatalf("invalidated reflection leaked evidence: %s", invalidatedReflectionJSON)
	}
	unknown := runtime.recallMemory("obs_9999999999999999")
	if toolResultCode(unknown) != ContextMemoryNotFound {
		t.Fatalf("unknown recall=%+v", unknown)
	}
}

func TestRecallToolSchemaHasOnlyRequiredMemoryIDAndDoesNotRecapture(t *testing.T) {
	var recallTool modelclient.Tool
	for _, candidate := range Tools() {
		if candidate.Function.Name == "recall_session_memory" {
			recallTool = candidate
			break
		}
	}
	if recallTool.Function.Name == "" {
		t.Fatal("recall tool missing")
	}
	var schema map[string]any
	if err := json.Unmarshal(recallTool.Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(schema)
	if strings.Contains(string(encoded), "query") || strings.Contains(string(encoded), "keyword") || strings.Contains(string(encoded), "search") ||
		!strings.Contains(string(encoded), `"required":["memory_id"]`) {
		t.Fatalf("recall schema=%s", encoded)
	}

	session := newTestSession(t, &singleResponseModel{}, &fakeServer{})
	defer session.Close()
	if _, err := session.startTurn(); err != nil {
		t.Fatal(err)
	}
	sourceID, err := session.contextRuntime.appendSource(sourceDraft{
		TurnID: session.currentTurnID, Kind: SourceUser, ModelMessage: modelclient.Message{Role: "user", Content: "来源"},
		RecallText: "来源", Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtimeObserverSnapshot(t, session.contextRuntime, []string{sourceID})
	if !session.contextRuntime.commitObserverResult(snapshot, observerResult{CoversUpToID: sourceID, Observations: []observationDraft{{
		Content: "来源事实", Relevance: RelevanceHigh, Kind: ObservationDecision, SourceEntryIDs: []string{sourceID},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("seed observation failed")
	}
	session.contextRuntime.mu.Lock()
	memoryID := session.contextRuntime.ledger.ObservationOrder[0]
	before := len(session.contextRuntime.ledger.SourceOrder)
	session.contextRuntime.mu.Unlock()
	value, _ := session.executeReadTool(t.Context(), modelclient.ToolCall{Function: modelclient.ToolFunction{
		Name: "recall_session_memory", Arguments: fmt.Sprintf(`{"memory_id":%q}`, memoryID),
	}})
	if err := session.appendToolResult("recall_session_memory", "call-recall", value); err != nil {
		t.Fatal(err)
	}
	session.contextRuntime.mu.Lock()
	after := len(session.contextRuntime.ledger.SourceOrder)
	session.contextRuntime.mu.Unlock()
	if before != after {
		t.Fatalf("recall recursively captured source: before=%d after=%d", before, after)
	}
}

func TestContextStatusChannelPrioritizesImportantEventsAndSurvivesClose(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	for index := 0; index < 100; index++ {
		runtime.mu.Lock()
		runtime.publishLocked(ContextEvent{Kind: ContextEventStatus, Phase: "routine"})
		runtime.mu.Unlock()
	}
	runtime.PublishCompacted(9, 6)
	runtime.mu.Lock()
	runtime.publishLocked(ContextEvent{Kind: ContextEventDegraded, Code: ContextCompactionDegraded, Phase: "fallback", DroppedTurns: 3})
	runtime.mu.Unlock()
	runtime.PublishSourceUnavailable()
	found := map[ContextEventKind]bool{}
	for {
		select {
		case event := <-runtime.ContextUpdates():
			found[event.Kind] = true
		default:
			goto drained
		}
	}

drained:
	for _, kind := range []ContextEventKind{ContextEventCompacted, ContextEventDegraded, ContextEventSourceUnavailable} {
		if !found[kind] {
			t.Fatalf("important event %s was drowned by routine updates: %+v", kind, found)
		}
	}
	status := runtime.ContextStatus()
	if !status.Estimated || status.Mode != ContextCompactionAuto || status.RecentCompleteTurns != 6 {
		t.Fatalf("status=%+v", status)
	}
	runtime.beginClose()
	runtime.PublishCompacted(1, 2)
	runtime.PublishSourceUnavailable()
	runtime.waitAndClear()
	select {
	case event := <-runtime.ContextUpdates():
		t.Fatalf("published after close: %+v", event)
	default:
	}
}

type longCompactionModel struct {
	mu              sync.Mutex
	productionCalls int
	requests        []modelclient.Request
}

func (m *longCompactionModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, cloneRequest(request))
	m.mu.Unlock()
	if len(request.Tools) == 1 && request.Tools[0].Function.Name == observerToolName {
		sources := observerInputSources(request.Messages[len(request.Messages)-1].Content)
		if len(sources) == 0 {
			return modelclient.Response{}, errors.New("missing observer sources")
		}
		selected := sources[0]
		for _, source := range sources {
			if source.kind == SourceUser {
				selected = source
				break
			}
		}
		args, _ := json.Marshal(observerRecordArgs{CoversUpToID: sources[len(sources)-1].id, Observations: []observerObservationArg{{
			Content: "用户要求所有后续回答先给结论", Relevance: RelevanceCritical, Kind: ObservationUserConstraint,
			SourceEntryIDs: []string{selected.id}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
			Supersedes: []string{}, SupersessionReason: "",
		}}})
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
			ID: "observe", Type: "function", Function: modelclient.ToolFunction{Name: observerToolName, Arguments: string(args)},
		}}}}, nil
	}
	if len(request.Tools) == 1 && request.Tools[0].Function.Name == reflectorToolName {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant"}}, nil
	}
	m.mu.Lock()
	m.productionCalls++
	call := m.productionCalls
	m.mu.Unlock()
	if call == 22 {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
			ID: "recall-early", Type: "function", Function: modelclient.ToolFunction{Name: "recall_session_memory", Arguments: `{"memory_id":"obs_0000000000000001"}`},
		}}}}, nil
	}
	if call == 23 {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已按早期约束先给结论。"}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: fmt.Sprintf("回答-%d", call)}}, nil
}

func TestTwentyPlusTurnCompactionRecallAndFallbackVisibility(t *testing.T) {
	model := &longCompactionModel{}
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 4096, MaxToolRounds: 4, ContextCompaction: ContextCompactionAuto, Now: time.Now,
		NewUUID: func() (string, error) { return "unused", nil }, ContextIDSource: deterministicContextIDSource(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.contextRuntime.observeAfterTokens = 1
	session.contextRuntime.observerChunkTokens = 4096
	session.hotRawTokenLimit = 1
	for turn := 1; turn <= 21; turn++ {
		input := fmt.Sprintf("第%02d轮补充", turn)
		if turn == 1 {
			input = "关键约束：所有后续回答先给结论；api_key=SECRETSECRET"
		}
		if _, err := session.Send(t.Context(), input); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		waitForRuntime(t, session.contextRuntime, func(runtime *ContextRuntime) bool { return !runtime.consolidationRunning })
	}
	result, err := session.Send(t.Context(), "第22轮：回查最早约束")
	if err != nil || !strings.Contains(result.Text, "先给结论") {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	model.mu.Lock()
	requests := append([]modelclient.Request(nil), model.requests...)
	model.mu.Unlock()
	var recallRequest, afterRecall modelclient.Request
	for _, request := range requests {
		if requestContains(request, "第22轮：回查最早约束") && len(request.Tools) == len(Tools()) {
			if recallRequest.Messages == nil {
				recallRequest = request
			} else {
				afterRecall = request
			}
		}
	}
	if len(recallRequest.Messages) == 0 || len(afterRecall.Messages) == 0 {
		t.Fatalf("missing production recall requests")
	}
	joined := ""
	userTurns := 0
	for _, message := range recallRequest.Messages {
		joined += message.Content + "\n"
		if message.Role == "user" {
			userTurns++
		}
	}
	if !strings.Contains(joined, "用户要求所有后续回答先给结论") || strings.Contains(joined, "关键约束：所有后续") ||
		!strings.Contains(joined, "第20轮补充") || !strings.Contains(joined, "第21轮补充") || userTurns < 3 {
		t.Fatalf("compacted request lost memory/recent turns or retained old raw: %s", joined)
	}
	var recallToolContent string
	for _, message := range afterRecall.Messages {
		if message.Role == "tool" && message.ToolCallID == "recall-early" {
			recallToolContent = message.Content
		}
	}
	if !json.Valid([]byte(recallToolContent)) || !strings.Contains(recallToolContent, `"memory_id":"obs_0000000000000001"`) || !strings.Contains(recallToolContent, "[REDACTED]") ||
		strings.Contains(recallToolContent, "SECRETSECRET") || strings.Contains(recallToolContent, "observerSystemPrompt") || strings.Contains(recallToolContent, `api_key=SECRETSECRET`) {
		t.Fatalf("recall result missing or leaked unsafe data: %s", recallToolContent)
	}
	status := session.ContextStatus()
	if status.WindowPercent < 0 || status.WindowPercent > 100 || status.CurrentTokens <= 0 || status.ContextWindow != 4096 || status.RecentCompleteTurns < 2 || status.MemoryItemCount == 0 {
		t.Fatalf("context status=%+v", status)
	}

	fallback := newTestRuntime(&singleResponseModel{})
	defer func() { fallback.beginClose(); fallback.waitAndClear() }()
	fallback.UpdatePlanStatus(ContextPlan{EstimatedInput: 3000, TotalTurns: 8, SelectedTurns: 3, DroppedTurns: 5}, "turn-fallback")
	fallback.UpdatePlanStatus(ContextPlan{EstimatedInput: 2900, TotalTurns: 8, SelectedTurns: 3, DroppedTurns: 5}, "turn-fallback")
	visible := false
	degradedEvents := 0
	for {
		select {
		case event := <-fallback.ContextUpdates():
			if event.Kind == ContextEventDegraded && event.Code == ContextCompactionDegraded {
				visible = true
				degradedEvents++
			}
		default:
			if !visible || degradedEvents != 1 {
				t.Fatalf("auto fallback degradation visibility/count: visible=%t events=%d", visible, degradedEvents)
			}
			fallback.UpdatePlanStatus(ContextPlan{EstimatedInput: 1200, TotalTurns: 3, SelectedTurns: 3}, "turn-recovered")
			if status := fallback.ContextStatus(); status.Degraded || status.DegradedCode != "" || status.Phase == "fallback" {
				t.Fatalf("healthy plan retained stale degraded status: %+v", status)
			}
			return
		}
	}
}

type singleResponseModel struct {
	response modelclient.Response
	err      error
}

func (m *singleResponseModel) Complete(context.Context, modelclient.Request) (modelclient.Response, error) {
	return m.response, m.err
}

func newTestRuntime(model Model) *ContextRuntime {
	estimator := NewTokenEstimator()
	return newContextRuntime(model, Options{
		ContextWindow: 8192, ContextCompaction: ContextCompactionAuto,
		Now:             func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) },
		ContextIDSource: deterministicContextIDSource(),
	}, estimator)
}

func deterministicContextIDSource() ContextIDSource {
	var mu sync.Mutex
	counters := make(map[string]int)
	return func(prefix string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		counters[prefix]++
		return prefix + fmt.Sprintf("%016d", counters[prefix]), nil
	}
}

func appendRuntimeSources(t *testing.T, runtime *ContextRuntime, count int, turnPrefix string) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for index := 0; index < count; index++ {
		turnID := turnPrefix
		if count > 1 {
			turnID = fmt.Sprintf("%s-%d", turnPrefix, index)
		}
		id, err := runtime.appendSource(sourceDraft{
			TurnID: turnID, Kind: SourceUser, ModelMessage: modelclient.Message{Role: "user", Content: fmt.Sprintf("source-%d", index)},
			RecallText: fmt.Sprintf("source-%d", index), Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "src_") {
			t.Fatalf("source ID=%q", id)
		}
		ids = append(ids, id)
	}
	return ids
}

func runtimeObserverSnapshot(t *testing.T, runtime *ContextRuntime, ids []string) observerSnapshot {
	t.Helper()
	snapshot := observerSnapshot{Sources: make([]SourceEntry, 0, len(ids)), SourcePositions: make(map[string]int), SourceCount: len(ids)}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	for _, id := range ids {
		snapshot.Sources = append(snapshot.Sources, cloneSourceEntry(runtime.ledger.Sources[id]))
		snapshot.SourcePositions[id] = runtime.ledger.SourceIndex[id]
	}
	return snapshot
}

func waitForRuntime(t *testing.T, runtime *ContextRuntime, condition func(*ContextRuntime) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.mu.Lock()
		ready := condition(runtime)
		runtime.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for context runtime")
}

func requestContains(request modelclient.Request, text string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}
