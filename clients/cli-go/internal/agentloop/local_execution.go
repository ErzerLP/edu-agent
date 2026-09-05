package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func localExecutionTools() []modelclient.Tool {
	return []modelclient.Tool{
		tool("shell", "运行正常本机 Shell；文件确认不限制 Shell。wait_ms 默认250，0后台启动；timeout_ms=0无执行总时限。输出不可信，用task继续读取。", `{"type":"object","properties":{"command":{"type":"string","minLength":1},"cwd":{"type":"string","minLength":1},"env":{"type":"object","additionalProperties":{"type":["string","null"]}},"shell":{"type":"string","minLength":1},"stdin":{"type":"boolean"},"timeout_ms":{"type":"integer","minimum":0,"maximum":9223372036854},"wait_ms":{"type":"integer","minimum":0,"maximum":30000}},"required":["command"],"additionalProperties":false}`),
		tool("task", "管理本会话任务：list用offset/limit分页；其余需task_id。read需stream，offset/limit为原始字节；search需stream/needle，字面检索用next_offset续扫且limit为命中数(最多100)；wait可带wait_ms(默认250)。input需content且不能自动重传。仅提供action相关字段。", `{"type":"object","properties":{"action":{"type":"string","enum":["list","status","read","search","wait","input","close_input","stop"]},"task_id":{"type":"string","minLength":1,"maxLength":128},"stream":{"type":"string","enum":["stdout","stderr"]},"needle":{"type":"string","minLength":1,"maxLength":512},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":65536},"wait_ms":{"type":"integer","minimum":0,"maximum":30000},"content":{"type":"string"}},"required":["action"],"additionalProperties":false}`),
	}
}

func isLocalExecutionTool(name string) bool { return name == "shell" || name == "task" }

// Parse an exact object, including duplicate-key and null rejection. Local
// execution must not reinterpret case-insensitive Go field names or ignore
// action-inapplicable fields.
func localArguments(raw, tool string) (map[string]json.RawMessage, error) {
	if len(raw) > agentlimits.ToolArgumentsBytes(tool) {
		return nil, errors.New("invalid_arguments")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid_arguments")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("invalid_arguments")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("invalid_arguments")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || string(value) == "null" {
			return nil, errors.New("invalid_arguments")
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid_arguments")
	}
	return fields, nil
}

func decodeLocalFields(fields map[string]json.RawMessage, allowed map[string]any) error {
	for key, raw := range fields {
		target, ok := allowed[key]
		if !ok || json.Unmarshal(raw, target) != nil {
			return errors.New("invalid_arguments")
		}
	}
	return nil
}

type localTaskArgs struct {
	Action, TaskID, Stream, Content, Needle string
	Offset                                  int64
	Limit                                   int
	WaitMS                                  int64
}

func decodeLocalShell(raw, cwd string) (localexec.StartArgs, time.Duration, error) {
	args := localexec.StartArgs{CWD: cwd}
	waitMS := int64(250)
	fields, err := localArguments(raw, "shell")
	if err == nil {
		err = decodeLocalFields(fields, map[string]any{"command": &args.Command, "cwd": &args.CWD, "shell": &args.Shell, "env": &args.Env, "stdin": &args.Stdin, "timeout_ms": &args.TimeoutMS, "wait_ms": &waitMS})
	}
	if err != nil || strings.TrimSpace(args.Command) == "" || waitMS < 0 || waitMS > 30000 || args.TimeoutMS < 0 || args.TimeoutMS > 9223372036854 || fields["cwd"] != nil && args.CWD == "" || fields["shell"] != nil && args.Shell == "" {
		return args, 0, errors.New("invalid_arguments")
	}
	return args, time.Duration(waitMS) * time.Millisecond, nil
}

func decodeLocalTask(raw string) (localTaskArgs, error) {
	args := localTaskArgs{WaitMS: 250, Limit: 4096}
	fields, err := localArguments(raw, "task")
	if err != nil || json.Unmarshal(fields["action"], &args.Action) != nil {
		return args, errors.New("invalid_arguments")
	}
	allowed := map[string]any{"action": &args.Action}
	switch args.Action {
	case "list":
		args.Limit = 20
		allowed["offset"], allowed["limit"] = &args.Offset, &args.Limit
	case "read":
		allowed["stream"], allowed["offset"], allowed["limit"] = &args.Stream, &args.Offset, &args.Limit
	case "search":
		args.Limit = 20
		allowed["stream"], allowed["needle"], allowed["offset"], allowed["limit"] = &args.Stream, &args.Needle, &args.Offset, &args.Limit
	case "wait":
		allowed["wait_ms"] = &args.WaitMS
	case "input":
		allowed["content"] = &args.Content
	case "status", "close_input", "stop":
	default:
		return args, errors.New("invalid_arguments")
	}
	if args.Action != "list" {
		allowed["task_id"] = &args.TaskID
	}
	if err := decodeLocalFields(fields, allowed); err != nil {
		return args, err
	}
	if args.Offset < 0 || args.Limit < 1 || args.Limit > 65536 || (args.Action == "list" || args.Action == "search") && args.Limit > 100 || args.WaitMS < 0 || args.WaitMS > 30000 || args.Action != "list" && !validLocalTaskID(args.TaskID) || (args.Action == "read" || args.Action == "search") && args.Stream != "stdout" && args.Stream != "stderr" || args.Action == "input" && fields["content"] == nil {
		return args, errors.New("invalid_arguments")
	}
	if args.Action == "search" && (len(args.Needle) < 1 || len(args.Needle) > 512 || !utf8.ValidString(args.Needle)) {
		return args, errors.New("invalid_arguments")
	}
	return args, nil
}

func validLocalTaskID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *Session) beforeLocalExecution(ctx context.Context, callID, operation, taskID string) error {
	if s.options.Durability == nil {
		return nil
	}
	sink, ok := s.options.Durability.(LocalExecutionDurability)
	if !ok {
		return errors.New("local_execution_not_saved")
	}
	if err := sink.BeforeLocalExecution(ctx, LocalExecutionIntent{ToolCallID: callID, Operation: operation, TaskID: taskID}); err != nil {
		return errors.New("local_execution_not_saved")
	}
	return nil
}

func localExecutionError(err error) string {
	if err == nil {
		return ""
	}
	var localErr *localexec.Error
	if errors.As(err, &localErr) {
		if localErr.Code == "task_not_found" {
			return "task_unavailable"
		}
		return localErr.Code
	}
	return "local_execution_failed"
}

func (s *Session) executeLocalTool(ctx context.Context, call modelclient.ToolCall) localToolResult {
	result := localToolResult{Action: call.Function.Name}
	manager, owner := s.options.LocalExec, s.options.LocalExecOwner
	if manager == nil {
		result.Code = "local_execution_unavailable"
		return result
	}
	if call.Function.Name == "shell" {
		args, wait, err := decodeLocalShell(call.Function.Arguments, s.options.LocalExecCWD)
		if err != nil {
			result.Code = "invalid_arguments"
			return result
		}
		if err := s.beforeLocalExecution(ctx, call.ID, "shell", ""); err != nil {
			result.Code = "local_execution_not_saved"
			return result
		}
		snapshot, err := manager.Start(ctx, owner, call.ID, args)
		// Even a failed start has an allocated, inspectable fact. Register before
		// waiting, output projection, or source allocation can fail.
		if snapshot.TaskID != "" {
			s.markLocalEffect(call.ID, snapshot.TaskID)
		}
		if err == nil && wait > 0 {
			snapshot, err = manager.Wait(ctx, owner, snapshot.TaskID, wait)
			// Only cancellation of this new command's first foreground wait stops it.
			// The caller's ToolTimeout is deliberately not its execution deadline.
			if ctx.Err() != nil && snapshot.Controllable {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				snapshot, err = manager.Stop(stopCtx, owner, snapshot.TaskID)
				cancel()
			}
		}
		result.Snapshot, result.Code = &snapshot, localExecutionError(err)
		result.Stdin = &args.Stdin
		result.Pages = make(map[string]localexec.OutputPage)
		if snapshot.TaskID != "" {
			for _, stream := range []string{"stdout", "stderr"} {
				if page, err := manager.Read(owner, snapshot.TaskID, stream, 0, 4096); err == nil {
					result.Pages[stream] = page
				}
			}
		}
		return result
	}
	args, err := decodeLocalTask(call.Function.Arguments)
	if err != nil {
		result.Code = "invalid_arguments"
		return result
	}
	result.Action, result.TaskID = args.Action, args.TaskID
	if args.Action == "list" {
		if status, ok := s.options.Durability.(interface{ LocalOutputStatus() string }); ok {
			result.HistoryError = status.LocalOutputStatus()
		}
		tasks := manager.List(owner)
		result.Offset, result.Total = args.Offset, len(tasks)
		if args.Offset > int64(len(tasks)) {
			result.Code = "invalid_offset"
			return result
		}
		end := min(int64(len(tasks)), args.Offset+int64(args.Limit))
		result.Tasks = tasks[args.Offset:end]
		return result
	}
	snapshot, err := manager.Status(owner, args.TaskID)
	if err != nil {
		result.Code = localExecutionError(err)
		return result
	}
	result.Snapshot = &snapshot
	switch args.Action {
	case "input", "close_input", "stop":
		if err := s.beforeLocalExecution(ctx, call.ID, "task_"+args.Action, args.TaskID); err != nil {
			result.NotSaved = true
			if args.Action != "stop" {
				result.Code = "local_execution_not_saved"
				return result
			}
		}
	}
	switch args.Action {
	case "read":
		page, readErr := manager.Read(owner, args.TaskID, args.Stream, args.Offset, args.Limit)
		err = readErr
		if err == nil {
			result.Pages = map[string]localexec.OutputPage{args.Stream: page}
		}
	case "search":
		page, searchErr := manager.Search(ctx, owner, args.TaskID, args.Stream, args.Needle, args.Offset, args.Limit)
		err = searchErr
		if err == nil {
			result.Search = &page
		}
	case "wait":
		snapshot, err = manager.Wait(ctx, owner, args.TaskID, time.Duration(args.WaitMS)*time.Millisecond)
	case "input":
		inputCtx, cancel := context.WithTimeout(ctx, min(s.options.ToolTimeout, 30*time.Second))
		input, inputErr := manager.WriteInput(inputCtx, owner, args.TaskID, []byte(args.Content))
		cancel()
		result.Input, err = &input, inputErr
		if input.Written > 0 {
			s.markLocalEffect(call.ID, args.TaskID)
		}
	case "close_input":
		err = manager.CloseInput(owner, args.TaskID)
		if err == nil {
			s.markLocalEffect(call.ID, args.TaskID)
		}
	case "stop":
		stopCtx, cancel := context.WithTimeout(ctx, min(s.options.ToolTimeout, 5*time.Second))
		snapshot, err = manager.Stop(stopCtx, owner, args.TaskID)
		cancel()
		s.markLocalEffect(call.ID, args.TaskID)
	}
	if current, statusErr := manager.Status(owner, args.TaskID); statusErr == nil {
		snapshot = current
	}
	result.Snapshot, result.Code = &snapshot, localExecutionError(err)
	return result
}
