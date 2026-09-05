package agentloop

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestPTYToolSchemasRejectIncompatibleFields(t *testing.T) {
	for _, raw := range []string{
		`{"command":"true","rows":20}`,
		`{"command":"true","pty":true,"rows":0}`,
		`{"command":"true","pty":true,"cols":4097}`,
		`{"command":"true","pty":true,"stdin":false}`,
		`{"command":"true","pty":null}`,
	} {
		if _, _, err := decodeLocalShell(raw, "/tmp"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{
		`{"action":"resize","task_id":"task_a","rows":20}`,
		`{"action":"resize","task_id":"task_a","rows":20,"cols":0}`,
		`{"action":"interrupt","task_id":"task_a","content":"x"}`,
		`{"action":"eof","task_id":"task_a","rows":20}`,
	} {
		if _, err := decodeLocalTask(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	args, _, err := decodeLocalShell(`{"command":"true","pty":true}`, "/tmp")
	if err != nil || !args.PTY || !args.Stdin {
		t.Fatal("PTY did not imply input")
	}
}

func TestPTYModelControlsRequireIntentAndHonorCanceledResize(t *testing.T) {
	s := newLocalExecutionSession(t, &fakeModel{}, nil)
	s.startTurn()
	sink := &localExecutionSink{}
	s.options.Durability = sink
	started := s.executeLocalTool(t.Context(), localCall("pty-intent-start", "shell", map[string]any{"command": "read line", "pty": true, "wait_ms": 0}))
	if started.Code != "" || started.Snapshot == nil {
		t.Fatalf("start=%+v", started)
	}
	id := started.Snapshot.TaskID
	sink.fail = true
	for _, action := range []string{"interrupt", "eof", "resize"} {
		args := map[string]any{"action": action, "task_id": id}
		if action == "resize" {
			args["rows"], args["cols"] = 66, 99
		}
		result := s.executeLocalTool(t.Context(), localCall("denied-"+action, "task", args))
		last := sink.local[len(sink.local)-1]
		if result.Code != "local_execution_not_saved" || last.Operation != "task_input" || last.TaskID != id {
			t.Fatalf("control bypassed intent: %+v %+v", result, last)
		}
	}
	sink.fail = false
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := s.executeLocalTool(ctx, localCall("canceled-resize", "task", map[string]any{"action": "resize", "task_id": id, "rows": 66, "cols": 99}))
	if result.Code != "resize_canceled" || result.Snapshot.Rows == 66 {
		t.Fatalf("canceled resize applied: %+v", result)
	}
	s.executeLocalTool(t.Context(), localCall("pty-intent-stop", "task", map[string]any{"action": "stop", "task_id": id}))
}

func TestPTYModelResizeInputAndMergedRead(t *testing.T) {
	step, taskID := 0, ""
	s := newLocalExecutionSession(t, localExecutionModel(func(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
		step++
		switch step {
		case 1:
			return localResponse(localCall("pty-start", "shell", map[string]any{"command": `test -t 0 && test -t 1 && test -t 2 || exit 8; read line; printf ':got=%s:' "$line"; stty size`, "pty": true, "rows": 20, "cols": 70, "wait_ms": 0})), nil
		case 2:
			result := localLastResult(t, request)
			taskID, _ = result["task_id"].(string)
			if result["output_mode"] != "pty_merged" || result["stdin_mode"] != "pty" {
				t.Fatalf("PTY identity lost: %+v", result)
			}
			if _, exists := result["stderr"]; exists {
				t.Fatal("separate stderr invented")
			}
			return localResponse(localCall("pty-resize", "task", map[string]any{"action": "resize", "task_id": taskID, "rows": 25, "cols": 90})), nil
		case 3:
			result := localLastResult(t, request)
			if result["rows"] != float64(25) || result["cols"] != float64(90) {
				t.Fatalf("resize=%+v", result)
			}
			return localResponse(localCall("pty-input", "task", map[string]any{"action": "input", "task_id": taskID, "content": "PTY_INPUT_901\n"})), nil
		case 4:
			return localResponse(localCall("pty-wait", "task", map[string]any{"action": "wait", "task_id": taskID, "wait_ms": 1000})), nil
		case 5:
			result := localLastResult(t, request)
			if result["state"] != "exited" || result["exit_code"] != float64(0) {
				t.Fatalf("exit=%+v", result)
			}
			return localResponse(localCall("pty-read", "task", map[string]any{"action": "read", "task_id": taskID, "stream": "stdout"})), nil
		case 6:
			result := localLastResult(t, request)
			page := result["stdout"].(map[string]any)
			text := []byte(page["data"].(string))
			if page["encoding"] == "base64" {
				var err error
				text, err = base64.StdEncoding.DecodeString(string(text))
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Contains(text, []byte(":got=PTY_INPUT_901:")) || !bytes.Contains(text, []byte("25 90")) {
				t.Fatalf("wrong terminal output: %q", text)
			}
		}
		return localFinal(), nil
	}), nil)
	result, err := s.Send(t.Context(), "run a terminal")
	if err != nil || result.Text != "done" || step != 6 {
		t.Fatalf("result=%+v err=%v steps=%d", result, err, step)
	}
	checkpoint := localCheckpoint(t, s)
	if bytes.Contains(checkpoint, []byte("PTY_INPUT_901")) || bytes.Contains(checkpoint, []byte("test -t 0")) {
		t.Fatal("raw terminal input/output entered stable history")
	}
}
