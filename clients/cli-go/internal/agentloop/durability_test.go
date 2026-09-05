package agentloop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type durabilitySink struct {
	beginErr      error
	preferenceErr error
	fileErr       error
	begins        int
	intents       []DirtyIntent
	preferences   []PreferenceWriteAhead
	files         []FileWriteAhead
}

func (s *durabilitySink) BeginTurn(_ context.Context, intent DirtyIntent) error {
	s.begins++
	s.intents = append(s.intents, intent)
	return s.beginErr
}
func (s *durabilitySink) BeforePreferenceWrite(_ context.Context, receipt PreferenceWriteAhead) error {
	s.preferences = append(s.preferences, receipt)
	return s.preferenceErr
}
func (s *durabilitySink) BeforeFilePublication(_ context.Context, receipt FileWriteAhead) error {
	s.files = append(s.files, receipt)
	return s.fileErr
}

func (s *durabilitySink) AfterFilePublication(_ context.Context, _ string, _ workspace.Result) error {
	return nil
}

func newDurableTestSession(t *testing.T, model Model, server Server, executor workspace.Executor, sink DurabilitySink) *Session {
	t.Helper()
	sequence := 0
	session, err := New(model, server, Options{
		ContextWindow: 32768, MaxToolRounds: 8, Workspace: executor, Durability: sink,
		ModelTimeout: time.Second, ToolTimeout: time.Second,
		Now: func() time.Time { return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) {
			sequence++
			return fmt.Sprintf("70000000-0000-4000-8000-%012d", sequence), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestDurabilityBeginTurnFailsClosedBeforeModelRequest(t *testing.T) {
	sink := &durabilitySink{beginErr: errors.New("disk unavailable")}
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "must not run"}}}}
	session := newDurableTestSession(t, model, &fakeServer{}, nil, sink)
	defer session.Close()

	if _, err := session.Send(t.Context(), "hello"); err == nil || sink.begins != 1 || len(sink.intents) != 1 ||
		sink.intents[0].TurnSequence != 1 || sink.intents[0].OperationClass != "agent-turn" || sink.intents[0].MayHaveSideEffect || len(model.requests) != 0 {
		t.Fatalf("err=%v begins=%d intents=%+v requests=%d", err, sink.begins, sink.intents, len(model.requests))
	}
}

func TestPreferenceWriteAheadFailsClosedBeforeServerSideEffect(t *testing.T) {
	sink := &durabilitySink{preferenceErr: errors.New("wal failed")}
	model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("preference-call", "remember_preference", `{"content":"偏好图示","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`)}}}
	server := &fakeServer{}
	session := newDurableTestSession(t, model, server, nil, sink)
	defer session.Close()

	result, err := session.Send(t.Context(), "请长期记住我偏好图示")
	if err != nil || result.Pending == nil {
		t.Fatalf("pending result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); err == nil {
		t.Fatal("preference write succeeded despite WAL failure")
	}
	if len(sink.preferences) != 1 || sink.preferences[0].CreateOperationID == "" || sink.preferences[0].AdmitOperationID == "" || sink.preferences[0].RejectOperationID == "" || server.createCalls != 0 {
		t.Fatalf("receipts=%+v createCalls=%d", sink.preferences, server.createCalls)
	}
}

func TestPreferenceWriteAheadAdvancesTypedStagesWithoutChangingIDs(t *testing.T) {
	sink := &durabilitySink{}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("preference-call", "remember_preference", `{"content":"偏好图示","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已保存"}},
	}}
	server := &fakeServer{}
	session := newDurableTestSession(t, model, server, nil, sink)
	defer session.Close()
	result, err := session.Send(t.Context(), "请长期记住我偏好图示")
	if err != nil || result.Pending == nil {
		t.Fatalf("pending result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); err != nil {
		t.Fatal(err)
	}
	if len(sink.preferences) != 3 {
		t.Fatalf("write-ahead stages=%+v", sink.preferences)
	}
	create, admit, completed := sink.preferences[0], sink.preferences[1], sink.preferences[2]
	if create.Stage != PreferenceStageCreate || admit.Stage != PreferenceStageAdmit || completed.Stage != PreferenceStageAdmit ||
		create.CreateOperationID == "" || create.AdmitOperationID == "" || create.RejectOperationID == "" ||
		create.CreateOperationID != admit.CreateOperationID || create.AdmitOperationID != admit.AdmitOperationID || create.RejectOperationID != admit.RejectOperationID ||
		create.Payload.Content != "偏好图示" || create.Payload.Reason != "用户明确要求" || create.Payload.ValidUntil.IsZero() ||
		admit.CandidateID == "" || admit.CandidateRevision < 1 || completed.StableCode != "preference_saved" || completed.Outcome != PreferenceOutcomeCompleted {
		t.Fatalf("invalid typed write-ahead transitions: %+v", sink.preferences)
	}
}

func TestPreferenceWriteAheadPersistsCompensatingRejectBeforeAndAfterEffect(t *testing.T) {
	sink := &durabilitySink{}
	model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("preference-call", "remember_preference", `{"content":"偏好图示","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`)}}}
	server := &fakeServer{decisionErrors: []error{&api.APIError{Status: 403, Code: "admission_forbidden"}, nil}}
	session := newDurableTestSession(t, model, server, nil, sink)
	defer session.Close()
	result, err := session.Send(t.Context(), "请长期记住我偏好图示")
	if err != nil || result.Pending == nil {
		t.Fatalf("pending result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); err == nil || errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("compensated rejection error=%v", err)
	}
	if len(sink.preferences) != 4 || len(server.decisionRequests) != 2 {
		t.Fatalf("write-ahead=%+v decisions=%+v", sink.preferences, server.decisionRequests)
	}
	create, admit, beforeReject, afterReject := sink.preferences[0], sink.preferences[1], sink.preferences[2], sink.preferences[3]
	if create.Stage != PreferenceStageCreate || admit.Stage != PreferenceStageAdmit || beforeReject.Stage != PreferenceStageReject || afterReject.Stage != PreferenceStageReject ||
		beforeReject.Outcome != "" || afterReject.Outcome != PreferenceOutcomeRejected || afterReject.StableCode != "admission_forbidden" ||
		create.CreateOperationID != afterReject.CreateOperationID || create.AdmitOperationID != afterReject.AdmitOperationID ||
		create.RejectOperationID == "" || create.RejectOperationID != afterReject.RejectOperationID ||
		server.decisionRequests[1].OperationID != create.RejectOperationID || server.decisionRequests[1].Decision != "reject" {
		t.Fatalf("invalid compensation durability state: write-ahead=%+v decisions=%+v", sink.preferences, server.decisionRequests)
	}
}

func TestFileWriteAheadFailsClosedBeforePublication(t *testing.T) {
	sink := &durabilitySink{fileErr: errors.New("wal failed")}
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{Tool: workspace.ToolWrite, Operation: "create", Path: "notes.md"}}
	executor := &fakeWorkspaceExecutor{
		status:   workspace.Status{Available: true, Label: "workspace"},
		prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
	}
	model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"create","content":"hello"}`)}}}
	session := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
	defer session.Close()

	result, err := session.Send(t.Context(), "写入笔记")
	if err != nil || result.PendingFileMutation == nil {
		t.Fatalf("pending result=%+v err=%v", result, err)
	}
	if _, err := session.ResolveFileMutation(t.Context(), "write-call", FileMutationApprove); err == nil {
		t.Fatal("file publication succeeded despite WAL failure")
	}
	if len(sink.files) != 1 || sink.files[0].Effect.Target.Path != "notes.md" || len(executor.commitCalls) != 0 {
		t.Fatalf("receipts=%+v commits=%v", sink.files, executor.commitCalls)
	}
}

func TestCancelledFileEffectCheckpointRoundTripPreservesReceiptHistoryWithoutPending(t *testing.T) {
	const contentHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, test := range []struct {
		name    string
		unknown bool
	}{
		{name: "completed publication", unknown: false},
		{name: "unknown publication", unknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
				Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md", PreviewKind: "content", Preview: "hello",
			}}
			baseExecutor := &fakeWorkspaceExecutor{
				status:   workspace.Status{Available: true, Label: "project"},
				prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
			}
			started := make(chan struct{})
			var model Model
			var executor workspace.Executor
			if test.unknown {
				executor = &unknownOnCancelWorkspaceExecutor{fakeWorkspaceExecutor: baseExecutor, started: started}
				model = &fakeModel{responses: []modelclient.Response{{Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"create","content":"hello"}`)}}}
			} else {
				baseExecutor.commitResults = []workspace.Result{{
					Value:   map[string]any{"path": "notes.md", "content_hash": contentHash, "complete": true, "publication_outcome": string(workspace.PublicationCompleted)},
					Summary: "已创建 notes.md", Reference: &workspace.Reference{Path: "notes.md", ContentHash: contentHash, Kind: "file"}, Publication: workspace.PublicationCompleted,
				}}
				executor = baseExecutor
				model = &cancelAfterMutationModel{followupStarted: started}
			}
			session := newDurableTestSession(t, model, &fakeServer{}, executor, &durabilitySink{})
			defer session.Close()
			if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				result Result
				err    error
			}
			finished := make(chan outcome, 1)
			go func() {
				result, err := session.Send(ctx, "创建文件")
				finished <- outcome{result: result, err: err}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("side-effect turn did not reach its cancellation boundary")
			}
			cancel()
			select {
			case completed := <-finished:
				if completed.err != nil {
					t.Fatalf("cancelled side-effect turn error=%v result=%+v", completed.err, completed.result)
				}
				if test.unknown && !strings.Contains(completed.result.Text, "无法确认") {
					t.Fatalf("unknown side-effect fallback=%q", completed.result.Text)
				}
				if !test.unknown && !strings.Contains(completed.result.Text, "文件修改已完成") {
					t.Fatalf("completed side-effect fallback=%q", completed.result.Text)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled side-effect turn did not finish")
			}

			sink := session.options.Durability.(*durabilitySink)
			if sink.begins != 1 || len(sink.files) != 1 || sink.files[0].ToolCallID != "write-call" || sink.files[0].Effect.Operation != "write_create" || sink.files[0].Effect.Target.Path != "notes.md" {
				t.Fatalf("durability receipt=%+v begins=%d", sink.files, sink.begins)
			}
			checkpoint := exportAndDecodeDurabilityCheckpoint(t, session)
			if len(checkpoint.Turns) != 1 {
				t.Fatalf("checkpoint turns=%+v", checkpoint.Turns)
			}
			turn := checkpoint.Turns[0]
			if turn.ID != "turn-1" || !turn.Completed || turn.FileEffectCallID != "write-call" || turn.FileEffectUnknown != test.unknown || turn.OutcomeUnknown != test.unknown || turn.Protected != test.unknown {
				t.Fatalf("side-effect checkpoint turn=%+v", turn)
			}
			if len(checkpoint.ToolHistory) != 1 || checkpoint.ToolHistory[0].Key != "write-call" || (test.unknown && !strings.Contains(checkpoint.ToolHistory[0].Value, workspace.CodeOutcomeUnknown)) {
				t.Fatalf("side-effect tool history=%+v", checkpoint.ToolHistory)
			}
			if len(checkpoint.WorkspaceReferences) != 1 || checkpoint.WorkspaceReferences[0].Key != "write-call" || checkpoint.WorkspaceReferences[0].Value.Path != "notes.md" {
				t.Fatalf("side-effect workspace history=%+v", checkpoint.WorkspaceReferences)
			}
			foundTool, foundFallback := false, false
			for _, message := range checkpoint.Messages {
				if message.Role == "tool" && message.ToolCallID == "write-call" {
					foundTool = true
				}
				if message.Role == "assistant" && (strings.Contains(message.Content, "文件修改已完成") || strings.Contains(message.Content, "无法确认")) {
					foundFallback = true
				}
				for _, call := range message.ToolCalls {
					if call.ID == "write-call" && call.Function.Arguments != `{}` {
						t.Fatalf("raw file mutation arguments persisted: %q", call.Function.Arguments)
					}
				}
			}
			if !foundTool || !foundFallback {
				t.Fatalf("side-effect history missing tool=%t fallback=%t messages=%+v", foundTool, foundFallback, checkpoint.Messages)
			}

			restoredExecutor := &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}}
			restoredModel := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "恢复后继续"}}}}
			restored := newDurableTestSession(t, restoredModel, &fakeServer{}, restoredExecutor, nil)
			defer restored.Close()
			if err := restored.RestoreCheckpoint(checkpoint); err != nil {
				t.Fatal(err)
			}
			assertDurabilitySessionIdle(t, restored)
			if restored.FileAuthorizationMode() != FileAuthorizationConfirm || len(restoredModel.requests) != 0 || len(restoredExecutor.calls) != 0 || len(restoredExecutor.commitCalls) != 0 {
				t.Fatalf("restore replayed side effect or authorization: mode=%q modelRequests=%d calls=%+v commits=%+v", restored.FileAuthorizationMode(), len(restoredModel.requests), restoredExecutor.calls, restoredExecutor.commitCalls)
			}
			restoredTurn := restored.turns["turn-1"]
			if restoredTurn == nil || restoredTurn.FileEffectCallID != "write-call" || restoredTurn.FileEffectUnknown != test.unknown || restoredTurn.OutcomeUnknown != test.unknown || restoredTurn.Protected != test.unknown {
				t.Fatalf("restored side-effect turn=%+v", restoredTurn)
			}
			if restored.toolHistory["write-call"] == "" || restored.workspaceReferences["write-call"] == nil || restored.workspaceReferences["write-call"].Path != "notes.md" {
				t.Fatalf("restored side-effect history tool=%q reference=%+v", restored.toolHistory["write-call"], restored.workspaceReferences["write-call"])
			}
			if _, err := restored.Send(t.Context(), "恢复后继续"); err != nil {
				t.Fatal(err)
			}
			if len(restoredModel.requests) != 1 || len(restoredExecutor.commitCalls) != 0 {
				t.Fatalf("restored continuation replayed mutation: requests=%d commits=%+v", len(restoredModel.requests), restoredExecutor.commitCalls)
			}
		})
	}
}

func exportAndDecodeDurabilityCheckpoint(t *testing.T, session *Session) SessionCheckpoint {
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

func assertDurabilitySessionIdle(t *testing.T, session *Session) {
	t.Helper()
	state := session.SwitchState()
	if state.ActiveTurn || state.PendingQuestion || state.PendingPreference || state.PendingFileMutation || state.Resolving {
		t.Fatalf("restored session retained transient state=%+v", state)
	}
}

type unknownOnCancelWorkspaceExecutor struct {
	*fakeWorkspaceExecutor
	started chan struct{}
}

func (w *unknownOnCancelWorkspaceExecutor) CommitMutation(ctx context.Context, prepared *workspace.PreparedMutation) workspace.Result {
	if prepared != nil {
		w.commitCalls = append(w.commitCalls, prepared.Presentation.Tool)
	}
	close(w.started)
	<-ctx.Done()
	return workspace.Result{
		Value: map[string]any{
			"path":                "notes.md",
			"code":                workspace.CodeOutcomeUnknown,
			"complete":            false,
			"publication_outcome": string(workspace.PublicationUnknown),
		},
		Summary: "文件发布结果无法确认", Reference: &workspace.Reference{Path: "notes.md", Kind: "file", InvalidateObserved: true}, Publication: workspace.PublicationUnknown,
	}
}
