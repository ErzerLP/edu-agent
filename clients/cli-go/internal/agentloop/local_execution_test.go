package agentloop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type localExecutionModel func(context.Context, modelclient.Request) (modelclient.Response, error)

func (f localExecutionModel) Complete(ctx context.Context, request modelclient.Request) (modelclient.Response, error) {
	return f(ctx, request)
}

func localCall(id, name string, args any) modelclient.ToolCall {
	data, _ := json.Marshal(args)
	return toolMessage(id, name, string(data)).ToolCalls[0]
}
func localResponse(call modelclient.ToolCall) modelclient.Response {
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{call}}}
}
func localFinal() modelclient.Response {
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "done"}}
}

func newLocalExecutionSession(t *testing.T, model Model, configure func(*Options)) *Session {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local shell requires Linux/macOS")
	}
	t.Setenv("SHELL", "/bin/sh")
	manager := localexec.New(localexec.Options{StopGrace: 20 * time.Millisecond})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("manager cleanup: %v", err)
		}
	})
	options := Options{ContextWindow: 32768, MaxToolRounds: 0, LocalExec: manager, LocalExecOwner: "test-owner", LocalExecCWD: t.TempDir(), ToolTimeout: time.Second, ModelTimeout: time.Second, NewUUID: func() (string, error) { return "70000000-0000-4000-8000-000000000001", nil }}
	if configure != nil {
		configure(&options)
	}
	session, err := New(model, &fakeServer{}, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	return session
}

func localLastResult(t *testing.T, request modelclient.Request) map[string]any {
	t.Helper()
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == "tool" {
			var value map[string]any
			if err := json.Unmarshal([]byte(request.Messages[i].Content), &value); err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatal("no tool result")
	return nil
}
func localCheckpoint(t *testing.T, s *Session) []byte {
	t.Helper()
	checkpoint, err := s.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSessionCheckpoint(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLocalExecutionSmallContextKeepsCompleteToolSet(t *testing.T) {
	calls := 0
	s := newLocalExecutionSession(t, localExecutionModel(func(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
		calls++
		names := make(map[string]bool)
		for _, definition := range request.Tools {
			names[definition.Function.Name] = true
		}
		for _, name := range []string{"shell", "task", "write", "edit", "archive", "ask_user_question", "remember_preference"} {
			if !names[name] {
				t.Fatalf("small context hid %s", name)
			}
		}
		if calls == 1 {
			return localResponse(localCall("small-shell", "shell", map[string]any{"command": "printf small", "wait_ms": 1000})), nil
		}
		return localFinal(), nil
	}), func(options *Options) {
		options.ContextWindow = 4096
		options.ContextCompaction = ContextCompactionOff
		var err error
		options.Workspace, err = workspace.Open(options.LocalExecCWD)
		if err != nil {
			t.Fatal(err)
		}
		options.WorkspaceStatus = options.Workspace.Status()
	})
	system, _ := splitSystemMessages(s.messages)
	fixed := s.estimator.EstimateRequest(modelclient.Request{Messages: system, Tools: s.tools()})
	if result, err := s.Send(t.Context(), "hello"); err != nil || calls != 2 || result.Text != "done" {
		t.Fatalf("fixed tokens=%d, model calls=%d, result=%+v, send error=%v", fixed, calls, result, err)
	}
}

func TestLocalExecutionModelLoopAndMetadataOnlyCheckpoint(t *testing.T) {
	step, taskID := 0, ""
	model := localExecutionModel(func(_ context.Context, req modelclient.Request) (modelclient.Response, error) {
		step++
		switch step {
		case 1:
			names := map[string]bool{}
			for _, tool := range req.Tools {
				names[tool.Function.Name] = true
			}
			if !names["shell"] || !names["task"] || names["artifact"] {
				t.Fatalf("tools: %v", names)
			}
			return localResponse(localCall("shell-1", "shell", map[string]any{"command": `printf '\377\033\000'; printf diagnostic >&2; exit 7`, "env": map[string]string{"PRIVATE_LOCAL_ENV": "not-for-history"}, "wait_ms": 1000})), nil
		case 2:
			result := localLastResult(t, req)
			taskID, _ = result["task_id"].(string)
			if taskID == "" || result["state"] != "exited" || result["exit_code"] != float64(7) || result["availability"] != "memory_only" {
				t.Fatalf("shell: %v", result)
			}
			for _, message := range req.Messages {
				for _, call := range message.ToolCalls {
					if isLocalExecutionTool(call.Function.Name) && call.Function.Arguments != `{}` {
						t.Fatal("raw executable arguments sent back to model")
					}
				}
			}
			return localResponse(localCall("read-1", "task", map[string]any{"action": "read", "task_id": taskID, "stream": "stdout"})), nil
		case 3:
			result := localLastResult(t, req)
			page := result["stdout"].(map[string]any)
			decoded, err := base64.StdEncoding.DecodeString(page["data"].(string))
			if err != nil || !bytes.Equal(decoded, []byte{255, 27, 0}) || page["encoding"] != "base64" || page["next_offset"] != float64(3) {
				t.Fatalf("page %v err %v", page, err)
			}
			return localFinal(), nil
		default:
			return modelclient.Response{}, errors.New("unexpected model call")
		}
	})
	s := newLocalExecutionSession(t, model, nil)
	if s.WorkspaceStatus().Available {
		t.Fatal("test needs unavailable workspace")
	}
	result, err := s.Send(t.Context(), "run the task")
	if err != nil || result.Text != "done" || step != 3 {
		t.Fatalf("result %+v err %v steps %d", result, err, step)
	}
	data := localCheckpoint(t, s)
	for _, secret := range []string{"PRIVATE_LOCAL_ENV", "not-for-history", "printf", "diagnostic", "base64"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("checkpoint leaked %q", secret)
		}
	}
	if !bytes.Contains(data, []byte(taskID)) || len(s.toolReferences) != 0 {
		t.Fatal("missing identity or server authority assigned")
	}
	for _, source := range s.contextRuntime.ledger.Sources {
		if source.Kind == SourceTool && (source.Authority != AuthoritySessionStatement || source.ServerReference != nil || strings.Contains(source.RecallText, `"data"`)) {
			t.Fatalf("source: %+v", source)
		}
	}
}

func TestLocalExecutionStrictArgumentsAndNilManager(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"command":"true","command":"false"}`, `{"Command":"true"}`, `{"command":"true","stdin":null}`, `{"command":"true","cwd":""}`, `{"command":"true","wait_ms":30001}`, `{"command":"true","wait_ms":1.1}`, `{"command":"true","timeout_ms":9223372036855}`, `{"command":"true","extra":true}`, `{"command":"true"} {}`} {
		if _, _, err := decodeLocalShell(raw, "/tmp"); err == nil {
			t.Errorf("accepted shell %s", raw)
		}
	}
	for _, raw := range []string{`{"action":"list","task_id":"x"}`, `{"action":"status","task_id":"x","wait_ms":0}`, `{"action":"list","limit":101}`, `{"action":"read","task_id":"x"}`, `{"action":"read","task_id":"x","stream":"stdout","offset":-1}`, `{"action":"input","task_id":"x"}`, `{"action":"stop","task_id":"x","content":"bad"}`, `{"action":"status","task_id":"x\u001b"}`, `{"action":"input","task_id":"x","content":null}`, `{"action":"status","Task_id":"x"}`} {
		if _, err := decodeLocalTask(raw); err == nil {
			t.Errorf("accepted task %s", raw)
		}
	}
	if _, err := decodeLocalTask(`{"action":"input","task_id":"UpperCASE-1","content":""}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeLocalShell(`{"command":"true","env":{"REMOVE":null},"wait_ms":0}`, "/tmp"); err != nil {
		t.Fatal(err)
	}
	oversized := `{"action":"input","task_id":"x","content":"` + strings.Repeat("a", 64<<10) + `"}`
	if _, err := decodeLocalTask(oversized); err == nil {
		t.Fatal("accepted oversized JSON")
	}
	s := newLocalExecutionSession(t, &fakeModel{responses: []modelclient.Response{localFinal()}}, func(o *Options) { o.LocalExec = nil })
	for _, tool := range s.tools() {
		if isLocalExecutionTool(tool.Function.Name) {
			t.Fatal("nil manager exposed tools")
		}
	}
}

func TestLocalExecutionWaitCancellationAndExecutionTimeout(t *testing.T) {
	s := newLocalExecutionSession(t, &fakeModel{}, func(o *Options) { o.ToolTimeout = time.Millisecond })
	if _, err := s.startTurn(); err != nil {
		t.Fatal(err)
	}
	background := s.executeLocalTool(t.Context(), localCall("background", "shell", map[string]any{"command": "sleep 10", "wait_ms": 5}))
	if background.Snapshot == nil || background.Snapshot.State != "running" {
		t.Fatalf("short wait killed task: %+v", background)
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	waited := s.executeLocalTool(cancelCtx, localCall("wait", "task", map[string]any{"action": "wait", "task_id": background.Snapshot.TaskID, "wait_ms": 30000}))
	cancel()
	if waited.Code != "wait_canceled" || waited.Snapshot.State != "running" {
		t.Fatalf("existing wait: %+v", waited)
	}
	timed := s.executeLocalTool(t.Context(), localCall("timeout", "shell", map[string]any{"command": "sleep 10", "timeout_ms": 20, "wait_ms": 1000}))
	if timed.Snapshot == nil || timed.Snapshot.State != "timed_out" {
		t.Fatalf("timeout: %+v", timed)
	}
	foregroundCtx, stop := context.WithTimeout(t.Context(), 30*time.Millisecond)
	foreground := s.executeLocalTool(foregroundCtx, localCall("foreground", "shell", map[string]any{"command": "sleep 10", "wait_ms": 30000}))
	stop()
	if foreground.Snapshot == nil || foreground.Snapshot.State != "canceled" {
		t.Fatalf("foreground: %+v", foreground)
	}
	status, _ := s.options.LocalExec.Status(s.options.LocalExecOwner, background.Snapshot.TaskID)
	if status.State != "running" {
		t.Fatalf("unrelated task stopped: %+v", status)
	}
	s.Close()
	status, _ = s.options.LocalExec.Status(s.options.LocalExecOwner, background.Snapshot.TaskID)
	if status.State != "running" {
		t.Fatal("Session.Close closed shared manager")
	}
}

func TestLocalExecutionFailureAndCancellationKeepFacts(t *testing.T) {
	for _, mode := range []string{"model_failure", "cancel_followup", "duplicate_call", "source_failure"} {
		t.Run(mode, func(t *testing.T) {
			step := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var s *Session
			model := localExecutionModel(func(_ context.Context, _ modelclient.Request) (modelclient.Response, error) {
				step++
				if step == 1 {
					return localResponse(localCall("start", "shell", map[string]any{"command": "sleep 10 # private-command", "env": map[string]string{"PRIVATE_ENV": "secret-environment"}, "wait_ms": 0})), nil
				}
				if mode == "cancel_followup" {
					cancel()
				}
				if mode == "duplicate_call" {
					return localResponse(localCall("start", "shell", map[string]any{"command": "echo should-not-run", "wait_ms": 0})), nil
				}
				return modelclient.Response{}, errors.New("model failed")
			})
			configure := func(o *Options) {
				if mode == "source_failure" {
					allocations := 0
					o.ContextIDSource = func(prefix string) (string, error) {
						allocations++
						if allocations == 1 {
							return prefix + "0123456789abcdef", nil
						}
						return "", errors.New("source unavailable")
					}
				}
			}
			s = newLocalExecutionSession(t, model, configure)
			result, err := s.Send(ctx, "start a background task")
			if err != nil || !strings.Contains(result.Text, "不会自动重跑") {
				t.Fatalf("fallback %+v err %v", result, err)
			}
			tasks := s.options.LocalExec.List(s.options.LocalExecOwner)
			if len(tasks) != 1 || tasks[0].State != "running" {
				t.Fatalf("tasks %+v", tasks)
			}
			data := localCheckpoint(t, s)
			for _, secret := range []string{"private-command", "PRIVATE_ENV", "secret-environment", "should-not-run"} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatalf("leaked %s", secret)
				}
			}
			if !bytes.Contains(data, []byte(tasks[0].TaskID)) {
				t.Fatal("lost task")
			}
		})
	}
}

func TestLocalExecutionCumulativeInputAndReadback(t *testing.T) {
	step, id := 0, ""
	chunk := strings.Repeat("input-private-block-", 1900) // each JSON <64KiB; total >64KiB
	var outputPath string
	model := localExecutionModel(func(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
		step++
		switch step {
		case 1:
			return localResponse(localCall("start-input", "shell", map[string]any{"command": "cat > accumulated.txt", "stdin": true, "wait_ms": 0})), nil
		case 2:
			id = localLastResult(t, request)["task_id"].(string)
			return localResponse(localCall("input-1", "task", map[string]any{"action": "input", "task_id": id, "content": chunk})), nil
		case 3, 4:
			result := localLastResult(t, request)
			if result["written"] != float64(len(chunk)) || result["input_outcome"] != "written" {
				t.Fatalf("input result: %v", result)
			}
			if step == 3 {
				return localResponse(localCall("input-2", "task", map[string]any{"action": "input", "task_id": id, "content": chunk})), nil
			}
			return localResponse(localCall("close-input", "task", map[string]any{"action": "close_input", "task_id": id})), nil
		case 5:
			return localResponse(localCall("wait-input", "task", map[string]any{"action": "wait", "task_id": id, "wait_ms": 1000})), nil
		case 6:
			if result := localLastResult(t, request); result["state"] != "exited" || result["exit_code"] != float64(0) {
				t.Fatalf("end: %v", result)
			}
			return localFinal(), nil
		default:
			return modelclient.Response{}, errors.New("unexpected call")
		}
	})
	s := newLocalExecutionSession(t, model, func(o *Options) { outputPath = filepath.Join(o.LocalExecCWD, "accumulated.txt") })
	if _, err := s.Send(t.Context(), "write via multiple inputs"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil || string(data) != chunk+chunk || len(data) <= 65536 {
		t.Fatalf("readback bytes %d err %v", len(data), err)
	}
	if data := localCheckpoint(t, s); bytes.Contains(data, []byte("input-private-block")) || bytes.Contains(data, []byte("cat >")) {
		t.Fatal("checkpoint contains executable inputs")
	}
}

func TestLocalExecutionProjectionPreservesRawCursors(t *testing.T) {
	raw := append(bytes.Repeat([]byte("世界\x1b"), 500), 0xff)
	for _, stream := range []string{"stdout", "stderr"} {
		offset := int64(0)
		collected := []byte{}
		for iterations := 0; offset < int64(len(raw)) && iterations < 100; iterations++ {
			result := localToolResult{Action: "read", Snapshot: &localexec.Snapshot{TaskID: "UPPER-lower-1", State: "exited", OutputState: "complete"}, Pages: map[string]localexec.OutputPage{stream: {Data: raw[offset:], Offset: offset, NextOffset: int64(len(raw)), Received: int64(len(raw)), Retained: int64(len(raw))}}}
			var value map[string]any
			if err := json.Unmarshal([]byte(result.project(850, nil, false)), &value); err != nil {
				t.Fatal(err)
			}
			page := value[stream].(map[string]any)
			var body []byte
			if page["encoding"] == "base64" {
				body, _ = base64.StdEncoding.DecodeString(page["data"].(string))
			} else {
				body = []byte(page["data"].(string))
			}
			next := int64(page["next_offset"].(float64))
			if next != offset+int64(len(body)) || next <= offset {
				t.Fatalf("cursor skipped/stuck: %v", page)
			}
			collected = append(collected, body...)
			var history map[string]any
			json.Unmarshal([]byte(result.project(2048, nil, true)), &history)
			if hp := history[stream].(map[string]any); hp["next_offset"] != float64(offset) || hp["data"] != nil {
				t.Fatalf("history skipped bytes: %v", hp)
			}
			tiny := result.project(850, func(string) bool { return false }, false)
			var minimum map[string]any
			json.Unmarshal([]byte(tiny), &minimum)
			if minimum["task_id"] != "UPPER-lower-1" || minimum["state"] != "exited" || minimum[stream].(map[string]any)["next_offset"] != float64(offset) {
				t.Fatalf("minimum: %v", minimum)
			}
			offset = next
		}
		if !bytes.Equal(collected, raw) {
			t.Fatalf("%s lost bytes: %d/%d", stream, len(collected), len(raw))
		}
	}
	tasks := make([]localexec.Snapshot, 20)
	for i := range tasks {
		tasks[i] = localexec.Snapshot{TaskID: fmt.Sprintf("Unique-Task-%d", i), State: "running", OutputState: "collecting"}
	}
	result := localToolResult{Action: "list", Tasks: tasks, Offset: 5, Total: 25}
	var page map[string]any
	json.Unmarshal([]byte(result.project(850, nil, false)), &page)
	if count := len(page["tasks"].([]any)); count >= 20 || page["next_offset"] != float64(5+count) || page["more"] != true {
		t.Fatalf("list page: %v", page)
	}
}

type localExecutionSink struct {
	durabilitySink
	fail  bool
	local []LocalExecutionIntent
}

func (s *localExecutionSink) BeforeLocalExecution(_ context.Context, intent LocalExecutionIntent) error {
	s.local = append(s.local, intent)
	if s.fail {
		return errors.New("disk-private-error")
	}
	return nil
}

func TestLocalExecutionDurabilityAndUnavailableTask(t *testing.T) {
	s := newLocalExecutionSession(t, &fakeModel{}, nil)
	s.startTurn()
	sink := &localExecutionSink{fail: true}
	s.options.Durability = sink
	denied := s.executeLocalTool(t.Context(), localCall("denied", "shell", map[string]any{"command": "true"}))
	if denied.Code != "local_execution_not_saved" || len(s.options.LocalExec.List(s.options.LocalExecOwner)) != 0 {
		t.Fatalf("WAL did not fail closed: %+v", denied)
	}
	s.options.Durability = &durabilitySink{}
	if result := s.executeLocalTool(t.Context(), localCall("missing-interface", "shell", map[string]any{"command": "true"})); result.Code != "local_execution_not_saved" {
		t.Fatal("legacy sink silently accepted")
	}
	sink.fail = false
	s.options.Durability = sink
	started := s.executeLocalTool(t.Context(), localCall("started", "shell", map[string]any{"command": "cat", "stdin": true, "wait_ms": 0}))
	id := started.Snapshot.TaskID
	sink.fail = true
	input := s.executeLocalTool(t.Context(), localCall("denied-input", "task", map[string]any{"action": "input", "task_id": id, "content": "secret"}))
	if input.Code != "local_execution_not_saved" {
		t.Fatalf("input %+v", input)
	}
	stopped := s.executeLocalTool(t.Context(), localCall("stop-anyway", "task", map[string]any{"action": "stop", "task_id": id}))
	if !stopped.NotSaved || stopped.Snapshot.State != "canceled" {
		t.Fatalf("stop blocked on WAL: %+v", stopped)
	}
	last := sink.local[len(sink.local)-1]
	if last.Operation != "task_stop" || last.TaskID != id || last.ToolCallID != "stop-anyway" {
		t.Fatalf("intent %+v", last)
	}
	for _, action := range []string{"status", "wait", "stop", "read", "input", "close_input"} {
		args := map[string]any{"action": action, "task_id": "Old-Process-Task"}
		if action == "read" {
			args["stream"] = "stdout"
		}
		if action == "input" {
			args["content"] = "never-replay"
		}
		result := s.executeLocalTool(t.Context(), localCall("missing-"+action, "task", args)).value(0, 0, true, false)
		if result["error"] != "task_unavailable" || result["state"] != "unknown" || result["replay"] != false {
			t.Fatalf("missing task: %v", result)
		}
	}
}

func TestLocalExecutionPendingFileCancellationKeepsTask(t *testing.T) {
	shell := localCall("before-pending", "shell", map[string]any{"command": "sleep 10", "wait_ms": 0})
	mutation := localCall("pending-write", "write", map[string]any{"path": "note.txt", "mode": "create", "content": "pending"})
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{shell, mutation}}}}}
	s := newLocalExecutionSession(t, model, func(o *Options) {
		o.Workspace = &fakeWorkspaceExecutor{status: workspace.Status{Available: true}, prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: {Presentation: workspace.MutationPresentation{Tool: workspace.ToolWrite, Operation: "write_create", Path: "note.txt"}}}}
	})
	result, err := s.Send(t.Context(), "start and prepare a file")
	if err != nil || result.PendingFileMutation == nil {
		t.Fatalf("pending %+v err %v", result, err)
	}
	result, err = s.CancelPendingFileMutation("pending-write")
	if err != nil || !strings.Contains(result.Text, "不会自动重跑") {
		t.Fatalf("cancel %+v err %v", result, err)
	}
	data := localCheckpoint(t, s)
	if bytes.Contains(data, []byte("pending-write")) {
		t.Fatal("incomplete call persisted")
	}
	if tasks := s.options.LocalExec.List(s.options.LocalExecOwner); len(tasks) != 1 || tasks[0].State != "running" {
		t.Fatalf("tasks %+v", tasks)
	}
}

func TestLocalExecutionSmallContextKeepsLocator(t *testing.T) {
	step := 0
	model := localExecutionModel(func(_ context.Context, req modelclient.Request) (modelclient.Response, error) {
		step++
		if step == 1 {
			return localResponse(localCall("small", "shell", map[string]any{"command": "printf tiny", "wait_ms": 1000})), nil
		}
		value := localLastResult(t, req)
		if value["task_id"] == "" || value["state"] != "exited" || value["stdout"].(map[string]any)["next_offset"] == nil {
			t.Fatalf("lost locator: %v", value)
		}
		return localFinal(), nil
	})
	s := newLocalExecutionSession(t, model, func(o *Options) { o.ContextWindow = 4096 })
	result, err := s.Send(t.Context(), "run")
	if err != nil || result.Text != "done" || step != 2 {
		t.Fatalf("small result %+v err %v steps %d", result, err, step)
	}
}
