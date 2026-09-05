package agentloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func sizedArguments(t *testing.T, size int) string {
	t.Helper()
	base, err := json.Marshal(map[string]string{"value": "界\\\"\n"})
	if err != nil || size < len(base) {
		t.Fatalf("invalid fixture size=%d err=%v", size, err)
	}
	raw := string(base[:len(base)-2]) + strings.Repeat("a", size-len(base)) + `"}`
	if !json.Valid([]byte(raw)) || len(raw) != size {
		t.Fatal("invalid JSON size fixture")
	}
	return raw
}

func TestLargeArgumentsLiveAndCheckpointBoundaries(t *testing.T) {
	for _, name := range []string{"write", "edit", "read", "archive", "unknown"} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", name, extra), func(t *testing.T) {
				message := toolMessage("call", name, sizedArguments(t, agentlimits.ToolArgumentsBytes(name)+extra))
				for label, validate := range map[string]func(modelclient.Message) error{"live": validateModelMessage, "checkpoint": validateCheckpointMessage} {
					if err := validate(message); (err != nil) != (extra != 0) {
						t.Fatalf("%s extra=%d err=%v", label, extra, err)
					}
				}
			})
		}
	}
	message := toolMessage("first", "write", sizedArguments(t, 65536))
	message.ToolCalls = append(message.ToolCalls, toolMessage("second", "edit", sizedArguments(t, 65536)).ToolCalls...)
	for label, validate := range map[string]func(modelclient.Message) error{"live": validateModelMessage, "checkpoint": validateCheckpointMessage} {
		if err := validate(message); err != nil {
			t.Fatalf("%s exact aggregate rejected: %v", label, err)
		}
	}
	message.ToolCalls = append(message.ToolCalls, toolMessage("third", "read", `{}`).ToolCalls...)
	for label, validate := range map[string]func(modelclient.Message) error{"live": validateModelMessage, "checkpoint": validateCheckpointMessage} {
		if err := validate(message); err == nil {
			t.Fatalf("%s accepted oversized aggregate", label)
		}
	}
}

func newLargeArgumentsSession(t *testing.T, model Model, executor workspace.Executor) *Session {
	t.Helper()
	var id int
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 272000, ContextCompaction: ContextCompactionRecentOnly, Workspace: executor,
		NewUUID: func() (string, error) {
			id++
			return fmt.Sprintf("71000000-0000-4000-8000-%012d", id), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestLargeArgumentsAuthorizedHTTPWriteEditAndRestore(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			root := t.TempDir()
			original := strings.Repeat("line-data\n", 1000)
			updated := strings.Repeat("updated-data\n", 900)
			writeJSON, err := json.Marshal(map[string]any{"path": "large.md", "mode": "create", "content": original})
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(original))
			editJSON, err := json.Marshal(map[string]any{"path": "large.md", "expected_hash": "sha256:" + hex.EncodeToString(digest[:]), "edits": []map[string]string{{"old_text": original, "new_text": updated}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(writeJSON) <= 8192 || len(editJSON) <= 8192 {
				t.Fatal("fixtures do not exceed old argument limit")
			}
			responses := []modelclient.Message{
				toolMessage("write-large", "write", string(writeJSON)),
				{Role: "assistant", Content: "written"},
				toolMessage("edit-large", "edit", string(editJSON)),
				{Role: "assistant", Content: "edited"},
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(responses) {
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				message := responses[index]
				finish := "stop"
				if len(message.ToolCalls) > 0 {
					finish = "tool_calls"
				}
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				delta := map[string]any{"role": "assistant"}
				if len(message.ToolCalls) > 0 {
					call := message.ToolCalls[0]
					delta["tool_calls"] = []any{map[string]any{"index": 0, "id": call.ID, "type": "function", "function": call.Function}}
				} else {
					delta["content"] = message.Content
				}
				frame, encodeErr := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				if encodeErr != nil {
					t.Error(encodeErr)
					return
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
			}))
			defer server.Close()
			client, err := modelclient.New(server.URL, "fake", "", 10*time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			var model Model = client
			if !stream {
				model = largeContextCompleteOnly{client}
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			session := newLargeArgumentsSession(t, model, executor)
			defer session.Close()
			for index, id := range []string{"write-large", "edit-large"} {
				pending, err := session.Send(t.Context(), "按预览修改文件")
				if err != nil || pending.PendingFileMutation == nil {
					t.Fatalf("pending=%+v err=%v", pending, err)
				}
				before, readErr := os.ReadFile(filepath.Join(root, "large.md"))
				if index == 0 && !os.IsNotExist(readErr) || index == 1 && (readErr != nil || string(before) != original) {
					t.Fatal("file changed before approval")
				}
				if _, err := session.ResolveFileMutation(t.Context(), id, FileMutationApprove); err != nil {
					t.Fatal(err)
				}
			}
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
			restoreExecutor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			restoreModel := &fakeModel{}
			restored := newLargeArgumentsSession(t, restoreModel, restoreExecutor)
			defer restored.Close()
			if err := restored.RestoreCheckpoint(decoded); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "large.md"))
			if err != nil || string(data) != updated || len(restoreModel.requests) != 0 || calls.Load() != 4 {
				t.Fatalf("file/restore mismatch err=%v calls=%d", err, calls.Load())
			}
		})
	}
}
