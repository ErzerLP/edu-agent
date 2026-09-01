package agentloop

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestObserverCommitRejectsWorkspaceSourceSupersededAfterSnapshot(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	oldReference := &WorkspaceReference{Path: "notes.md", ContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "file"}
	sourceID, err := runtime.appendSource(sourceDraft{
		TurnID: "turn-1", Kind: SourceTool,
		ModelMessage: modelclient.Message{Role: "tool", ToolCallID: "read-old", Content: `{"path":"notes.md"}`},
		RecallText:   "old workspace text", Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved,
		WorkspaceReference: oldReference,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtimeObserverSnapshot(t, runtime, []string{sourceID})
	runtime.supersedeWorkspaceEvidence(&WorkspaceReference{Path: "notes.md", ContentHash: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Kind: "file"})
	committed := runtime.commitObserverResult(snapshot, observerResult{CoversUpToID: sourceID, Observations: []observationDraft{{
		Content: "旧工作区内容", Relevance: RelevanceHigh, Kind: ObservationToolSnapshot,
		SourceEntryIDs: []string{sourceID}, Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved,
	}}})
	if committed {
		t.Fatal("observer committed a stale workspace_observed draft after source supersession")
	}
}

func TestReflectorCommitRejectsWorkspaceObservationSupersededAfterSnapshot(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	oldReference := &WorkspaceReference{Path: "notes.md", ContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "file"}
	sourceID, err := runtime.appendSource(sourceDraft{
		TurnID: "turn-1", Kind: SourceTool,
		ModelMessage: modelclient.Message{Role: "tool", ToolCallID: "read-old", Content: `{"path":"notes.md"}`},
		RecallText:   "old workspace text", Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved,
		WorkspaceReference: oldReference,
	})
	if err != nil {
		t.Fatal(err)
	}
	observerSnapshot := runtimeObserverSnapshot(t, runtime, []string{sourceID})
	if !runtime.commitObserverResult(observerSnapshot, observerResult{CoversUpToID: sourceID, Observations: []observationDraft{{
		Content: "旧工作区内容", Relevance: RelevanceHigh, Kind: ObservationToolSnapshot,
		SourceEntryIDs: []string{sourceID}, Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved,
	}}}) {
		t.Fatal("workspace observation was not committed")
	}
	runtime.mu.Lock()
	observations := runtime.activeObservationsLocked()
	snapshot := reflectorSnapshot{Observations: observations, Reflections: runtime.reflectionsLocked(), ObserverRuns: runtime.ledger.SuccessfulObserverRuns}
	observationID := observations[0].ID
	runtime.mu.Unlock()
	runtime.supersedeWorkspaceEvidence(&WorkspaceReference{Path: "notes.md", ContentHash: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Kind: "file"})
	if runtime.commitReflectorResult(snapshot, reflectorResult{Reflections: []reflectionDraft{{
		Content: "旧工作区状态", Kind: ReflectionWorkspaceState,
		Support:   []CoverageEdge{{ObservationID: observationID, Fidelity: CoverageExact}},
		Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved,
	}}}) {
		t.Fatal("reflector committed stale workspace_observed state after observation supersession")
	}
}

func TestWorkspaceObservationCannotSupersedeSessionObservation(t *testing.T) {
	source := SourceEntry{ID: "src_workspace", Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved}
	_, err := validateObservationArg(observerObservationArg{
		Content: "文件快照", Relevance: RelevanceHigh, Kind: ObservationToolSnapshot,
		SourceEntryIDs: []string{source.ID}, Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved,
		Supersedes: []string{"obs_user"}, SupersessionReason: "file said so",
	}, source.ID, map[string]int{source.ID: 0}, map[string]SourceEntry{source.ID: source}, map[string]struct{}{"obs_user": {}})
	if err == nil {
		t.Fatal("workspace evidence was allowed to supersede user/session semantics")
	}
}

func TestExactReflectionRemainsProjectedAndRecallableAfterCoveragePrune(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	sourceID := appendRuntimeSources(t, runtime, 1, "turn-1")[0]
	snapshot := runtimeObserverSnapshot(t, runtime, []string{sourceID})
	if !runtime.commitObserverResult(snapshot, observerResult{CoversUpToID: sourceID, Observations: []observationDraft{{
		Content: "用户要求先给结论", Relevance: RelevanceCritical, Kind: ObservationUserConstraint,
		SourceEntryIDs: []string{sourceID}, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("observation was not committed")
	}
	runtime.mu.Lock()
	observations := runtime.activeObservationsLocked()
	reflector := reflectorSnapshot{Observations: observations, ObserverRuns: runtime.ledger.SuccessfulObserverRuns}
	observationID := observations[0].ID
	runtime.mu.Unlock()
	if !runtime.commitReflectorResult(reflector, reflectorResult{Reflections: []reflectionDraft{{
		Content: "用户要求后续回答先给结论", Kind: ReflectionUserConstraint,
		Support:   []CoverageEdge{{ObservationID: observationID, Fidelity: CoverageExact}},
		Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	}}}) {
		t.Fatal("exact reflection was not committed")
	}
	runtime.mu.Lock()
	if tombstone := runtime.ledger.Tombstones[observationID]; tombstone.Reason != DropExactCoverage {
		runtime.mu.Unlock()
		t.Fatalf("observation tombstone=%+v", tombstone)
	}
	reflectionID := runtime.ledger.ReflectionOrder[len(runtime.ledger.ReflectionOrder)-1]
	runtime.mu.Unlock()
	projection := runtime.memoryProjection()
	if !strings.Contains(strings.Join(projection.Items, "\n"), reflectionID) {
		t.Fatalf("exact reflection disappeared from memory projection: %+v", projection.Items)
	}
	recalled := runtime.recallMemory(reflectionID)
	if recalled["content"] != "用户要求后续回答先给结论" || recalled["error"] != nil {
		t.Fatalf("recalled=%+v", recalled)
	}
}

func TestWorkspaceNewVersionSupersedesLedgerAndHotToolHistory(t *testing.T) {
	executor := &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}}
	session := newWorkspaceTestSession(t, &fakeModel{}, executor)
	defer session.Close()
	if _, err := session.startTurn(); err != nil {
		t.Fatal(err)
	}
	oldHash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newHash := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := session.appendWorkspaceToolResult(workspace.ToolRead, "read-old", workspace.Result{
		Value:   map[string]any{"path": "notes.md", "content": "old text", "content_hash": oldHash, "complete": true},
		Summary: "old", Reference: &workspace.Reference{Path: "notes.md", ContentHash: oldHash, Kind: "file"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.appendWorkspaceToolResult(workspace.ToolWrite, "write-new", workspace.Result{
		Value:   map[string]any{"path": "notes.md", "content_hash": newHash, "complete": true, "publication_outcome": "completed"},
		Summary: "new", Reference: &workspace.Reference{Path: "notes.md", ContentHash: newHash, Kind: "file"}, Publication: workspace.PublicationCompleted,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(session.toolHistory["read-old"], `"superseded":true`) || !strings.Contains(session.toolHistory["read-old"], newHash) {
		t.Fatalf("old tool history=%s", session.toolHistory["read-old"])
	}
	oldMessageSuperseded := false
	for _, message := range session.messages {
		if message.Role == "tool" && message.ToolCallID == "read-old" {
			oldMessageSuperseded = strings.Contains(message.Content, `"superseded":true`) && !strings.Contains(message.Content, "old text")
		}
	}
	if !oldMessageSuperseded {
		t.Fatalf("old hot tool message was not superseded: %+v", session.messages)
	}
	session.contextRuntime.mu.Lock()
	oldFreshness, newFreshness := FreshnessClass(""), FreshnessClass("")
	for _, sourceID := range session.contextRuntime.ledger.SourceOrder {
		source := session.contextRuntime.ledger.Sources[sourceID]
		switch source.ModelMessage.ToolCallID {
		case "read-old":
			oldFreshness = source.Freshness
		case "write-new":
			newFreshness = source.Freshness
		}
	}
	session.contextRuntime.mu.Unlock()
	if oldFreshness != FreshnessWorkspaceSuperseded || newFreshness != FreshnessWorkspaceObserved {
		t.Fatalf("freshness old=%q new=%q", oldFreshness, newFreshness)
	}
}

func TestWorkspaceUncertainResultInvalidatesPriorVersionWithoutInventingHash(t *testing.T) {
	executor := &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}}
	session := newWorkspaceTestSession(t, &fakeModel{}, executor)
	defer session.Close()
	if _, err := session.startTurn(); err != nil {
		t.Fatal(err)
	}
	oldHash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := session.appendWorkspaceToolResult(workspace.ToolRead, "read-old", workspace.Result{
		Value:   map[string]any{"path": "notes.md", "content": "old text", "content_hash": oldHash, "complete": true},
		Summary: "old", Reference: &workspace.Reference{Path: "notes.md", ContentHash: oldHash, Kind: "file"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.appendWorkspaceToolResult(workspace.ToolWrite, "write-unknown", workspace.Result{
		Value:   map[string]any{"path": "notes.md", "code": workspace.CodeOutcomeUnknown, "complete": false, "publication_outcome": "unknown"},
		Summary: "unknown", Reference: &workspace.Reference{Path: "notes.md", Kind: "file", InvalidateObserved: true}, Publication: workspace.PublicationUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(session.toolHistory["read-old"], `"requires_reread":true`) || strings.Contains(session.toolHistory["read-old"], "current_content_hash") {
		t.Fatalf("uncertain replacement=%s", session.toolHistory["read-old"])
	}
	session.contextRuntime.mu.Lock()
	defer session.contextRuntime.mu.Unlock()
	for _, sourceID := range session.contextRuntime.ledger.SourceOrder {
		source := session.contextRuntime.ledger.Sources[sourceID]
		if source.ModelMessage.ToolCallID == "read-old" && source.Freshness != FreshnessWorkspaceSuperseded {
			t.Fatalf("old source freshness=%q", source.Freshness)
		}
		if source.ModelMessage.ToolCallID == "write-unknown" && (source.Freshness != FreshnessWorkspaceSuperseded || source.WorkspaceReference.ContentHash != "") {
			t.Fatalf("unknown source=%+v", source)
		}
	}
}

func TestServerInvalidationPreservesWorkspaceSource(t *testing.T) {
	runtime := newTestRuntime(&singleResponseModel{})
	defer func() { runtime.beginClose(); runtime.waitAndClear() }()
	workspaceID, err := runtime.appendSource(sourceDraft{
		TurnID: "turn-workspace", Kind: SourceTool,
		ModelMessage: modelclient.Message{Role: "tool", ToolCallID: "workspace-call", Content: "workspace"}, RecallText: "workspace",
		Authority: AuthorityWorkspaceSnapshot, Freshness: FreshnessWorkspaceObserved,
		WorkspaceReference: &WorkspaceReference{Path: "notes.md", ContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "file"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.appendSource(sourceDraft{
		TurnID: "turn-server", Kind: SourceTool,
		ModelMessage: modelclient.Message{Role: "tool", ToolCallID: "server-call", Content: "server"}, RecallText: "server",
		Authority: AuthorityServerSnapshot, Freshness: FreshnessHistorical,
		ServerReference: &ServerReference{Tool: "current_session", Entity: "session", Generation: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.invalidateServerEvidenceForAppend(nil, true, false)
	runtime.mu.Lock()
	workspaceSource := runtime.ledger.Sources[workspaceID]
	runtime.mu.Unlock()
	if workspaceSource.Freshness != FreshnessWorkspaceObserved || !workspaceSource.SourceAvailable || workspaceSource.ModelMessage.ToolCallID != "workspace-call" {
		t.Fatalf("workspace source was damaged by server invalidation: %+v", workspaceSource)
	}
}
