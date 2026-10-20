package agentloop

import (
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestToolStepsRecordedMutationDoesNotConsumeExecution(t *testing.T) {
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{Tool: workspace.ToolRestoreArchive, Operation: "restore_archive", Path: ".edu-agent-archive/item", DestinationPath: "restored"}}
	executor := &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}, prepared: map[string]*workspace.PreparedMutation{workspace.ToolRestoreArchive: prepared}}
	m := &fakeModel{responses: []modelclient.Response{
		{Message: stepCalls(workspace.ToolRead, workspace.ToolRead, workspace.ToolRestoreArchive, workspace.ToolRead, workspace.ToolRead)},
		{Message: modelclient.Message{Role: "assistant", Content: "已读取三次，旧恢复调用未重放。"}},
	}}
	s := newDurableTestSession(t, m, &fakeServer{}, executor, &durabilitySink{fileErr: ErrLocalCallRecorded})
	defer s.Close()
	s.options.MaxToolSteps = 3
	r, err := s.Send(t.Context(), "核对归档并读取文件")
	if err != nil || r.PendingFileMutation == nil || len(executor.calls) != 2 {
		t.Fatalf("missing mutation approval: result=%+v err=%v reads=%v", r, err, executor.calls)
	}
	r, err = s.ResolveFileMutation(t.Context(), r.PendingFileMutation.CallID, FileMutationApprove)
	if err != nil || !r.PausedToolSteps || len(executor.calls) != 3 || len(executor.commitCalls) != 0 {
		t.Fatalf("blocked mutation consumed a tool execution: result=%+v err=%v reads=%v commits=%v", r, err, executor.calls, executor.commitCalls)
	}
}

func TestToolStepsLocalWALRejectionDoesNotConsumeExecution(t *testing.T) {
	for _, tool := range []string{"shell", "task"} {
		t.Run(tool, func(t *testing.T) {
			executor := &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}}
			m := &fakeModel{}
			s := newLocalExecutionSession(t, m, func(options *Options) {
				options.Workspace = executor
				options.Durability = &localExecutionSink{fail: true}
				options.MaxToolSteps = 3
			})
			calls := stepCalls(workspace.ToolRead, workspace.ToolRead, tool, workspace.ToolRead, workspace.ToolRead)
			if tool == "shell" {
				calls.ToolCalls[2] = localCall("blocked-shell", "shell", map[string]any{"command": "true"})
			} else {
				started, err := s.options.LocalExec.Start(t.Context(), s.options.LocalExecOwner, "setup-task", localexec.StartArgs{Command: "exec cat", CWD: s.options.LocalExecCWD, Stdin: true})
				if err != nil {
					t.Fatal(err)
				}
				calls.ToolCalls[2] = localCall("blocked-input", "task", map[string]any{"action": "input", "task_id": started.TaskID, "content": "must not be sent"})
			}
			m.responses = []modelclient.Response{{Message: calls}, {Message: modelclient.Message{Role: "assistant", Content: "已读取三次，本地写入尚未执行。"}}}
			r, err := s.Send(t.Context(), "核对本地任务并读取文件")
			if err != nil || !r.PausedToolSteps || len(executor.calls) != 3 {
				t.Fatalf("blocked local call consumed a tool execution: result=%+v err=%v reads=%v", r, err, executor.calls)
			}
			if tool == "shell" && len(s.options.LocalExec.List(s.options.LocalExecOwner)) != 0 {
				t.Fatal("WAL rejection started a shell")
			}
		})
	}
}
