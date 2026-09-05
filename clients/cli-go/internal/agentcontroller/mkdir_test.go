package agentcontroller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestControllerMkdirCompletedAndPartialSurviveResume(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "partial"}[partial], func(t *testing.T) {
			storeRoot, root := t.TempDir(), t.TempDir()
			secrets := &controllerSecretBackend{}
			path := "a/b"
			if partial {
				path = "a/" + strings.Repeat("x", 256) + "/last"
			}
			raw, _ := json.Marshal(map[string]any{"path": path, "parents": true})
			model := &scriptedControllerModel{responses: []modelclient.Response{
				{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "mkdir-call", Type: "function", Function: modelclient.ToolFunction{Name: "mkdir", Arguments: string(raw)}}}}},
				{Message: modelclient.Message{Role: "assistant", Content: "已创建目录"}},
			}}
			server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
			provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			deps := controllerDependencies(controllerStore(t, storeRoot, secrets), model, server, root, provider)
			deps.LoopOptions.Workspace = executor
			c, err := Start(t.Context(), deps, false)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := c.Send(t.Context(), "创建目录")
			if err != nil || pending.PendingFileMutation == nil {
				c.abort()
				t.Fatalf("pending=%+v %v", pending, err)
			}
			if _, err = c.ResolveFileMutation(t.Context(), "mkdir-call", agentloop.FileMutationApprove); err != nil {
				c.abort()
				t.Fatal(err)
			}
			c.mu.Lock()
			receipts := append([]agentsession.FileReceipt(nil), c.record.FileReceipts...)
			c.mu.Unlock()
			expectedOutcome, created := agentsession.NoticeOutcomeCompleted, 2
			if partial {
				expectedOutcome, created = agentsession.NoticeOutcomeUnknown, 1
			}
			if len(receipts) != 1 || receipts[0].Effect.Operation != "mkdir" || receipts[0].Effect.Target.Path != path || receipts[0].Effect.Directories.Created != created || receipts[0].Outcome != expectedOutcome {
				c.abort()
				t.Fatalf("receipts=%+v", receipts)
			}
			id := c.SessionID()
			if err = c.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			resumed.mu.Lock()
			saved := append([]agentsession.FileReceipt(nil), resumed.record.FileReceipts...)
			resumed.mu.Unlock()
			if len(saved) != 1 || saved[0] != receipts[0] {
				t.Fatalf("saved=%+v", saved)
			}
			if _, err = os.Stat(filepath.Join(root, "a")); err != nil {
				t.Fatal("partial directory deleted", err)
			}
			if partial {
				unknown := resumed.UnknownOutcomes()
				if len(unknown) != 1 || !strings.Contains(unknown[0].Label, "已知创建 1 层：a") || !strings.Contains(unknown[0].Label, "不会自动重试") {
					t.Fatalf("unknown=%+v", unknown)
				}
			}
		})
	}
}
func TestControllerMkdirWALCrashOnlyDescribesPossiblePaths(t *testing.T) {
	storeRoot, root := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	c, err := Start(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
		c.abort()
		t.Fatal(err)
	}
	effect := fileeffects.New("mkdir", "", "possible/child", "directory")
	effect.Directories = fileeffects.DirectoryChain{Anchor: ".", Count: 2}
	if err = c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: "crash", Effect: effect}); err != nil {
		c.abort()
		t.Fatal(err)
	}
	id := c.SessionID()
	c.abort()
	resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if _, err = os.Stat(filepath.Join(root, "possible")); !os.IsNotExist(err) {
		t.Fatalf("resume replayed mkdir: %v", err)
	}
	unknown := resumed.UnknownOutcomes()
	if len(unknown) != 1 || !strings.Contains(unknown[0].Label, "possible/child") || !strings.Contains(unknown[0].Label, "不代表未发生") {
		t.Fatalf("unknown=%+v", unknown)
	}
}
