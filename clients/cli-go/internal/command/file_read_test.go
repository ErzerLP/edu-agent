package command

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestLargeFileReadOptions(t *testing.T) {
	args := []string{"--workspace", "--file-read-limit", "--task-max-running=2", "--file-read-limit", "2097152", "--no-save", "--", "--file-read-limit=1"}
	remaining, limit, err := parseFileReadOptions(args)
	want := []string{"--workspace", "--file-read-limit", "--task-max-running=2", "--no-save", "--", "--file-read-limit=1"}
	if err != nil || limit != 2097152 || !reflect.DeepEqual(remaining, want) {
		t.Fatalf("args=%v limit=%d err=%v", remaining, limit, err)
	}
	_, limit, err = parseFileReadOptions(nil)
	if err != nil || limit != workspace.DefaultReadFileBytes {
		t.Fatal("wrong default")
	}
	for _, args := range [][]string{{"--file-read-limit"}, {"--file-read-limit="}, {"--file-read-limit=0"}, {"--file-read-limit=-1"}, {"--file-read-limit=1MiB"}, {"--file-read-limit=999999999999999999999"}, {"--file-read-limit=1", "--file-read-limit", "2"}, {"--file-read-limit=" + strconv.FormatInt(int64(^uint(0)>>1), 10)}} {
		if _, _, err := parseFileReadOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, text := range []string{"--file-read-limit", "67108864", "完整文件hash", "全文读取"} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatalf("help missing %q", text)
		}
	}
}

type fileReadBudgetUI struct{}

func (fileReadBudgetUI) Run(ctx context.Context, conversation agentui.Conversation, _ string) error {
	_, err := conversation.Send(ctx, "inspect")
	return err
}

type fileReadBudgetModel struct {
	result      map[string]any
	schemaLimit float64
}

func (m *fileReadBudgetModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" {
		for _, tool := range request.Tools {
			if tool.Function.Name == "read" {
				var schema map[string]any
				if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
					return modelclient.Response{}, err
				}
				m.schemaLimit = schema["properties"].(map[string]any)["byte_offset"].(map[string]any)["maximum"].(float64)
			}
		}
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "read-budget", Type: "function", Function: modelclient.ToolFunction{Name: "read", Arguments: `{"path":"large.txt","offset":2}`}}}}}, nil
	}
	if last.Role == "tool" {
		if err := json.Unmarshal([]byte(last.Content), &m.result); err != nil {
			return modelclient.Response{}, err
		}
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestLargeFileReadLaunchPassesBudgetToRealTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("x", (1<<20)+1)+"\nCLI_TAIL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{128, 2 << 20} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			cs, credentials := pairedStores(config.DefaultServerURL, "server-token")
			preset := config.DefaultAgentConfig("ollama")
			cs.value.Agent = &preset
			app, _, errOut := newTestApp(cs, credentials, &fakeTerminal{})
			app.ModelSecrets = &memoryModelSecretStore{}
			app.AgentUI = fileReadBudgetUI{}
			model := &fileReadBudgetModel{}
			app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
			app.InputIsTTY = func() bool { return true }
			app.OutputIsTTY = func() bool { return true }
			app.Getenv = func(string) string { return "xterm" }
			if exit := app.Run(t.Context(), []string{"agent", "--no-save", "--workspace", root, "--file-read-limit=" + strconv.FormatInt(limit, 10)}); exit != ExitOK {
				t.Fatalf("exit=%d err=%s", exit, errOut.String())
			}
			if model.schemaLimit != float64(limit) {
				t.Fatalf("schema limit=%v", model.schemaLimit)
			}
			if limit == 128 {
				if model.result["error"] != workspace.CodeFileTooLarge || model.result["read_byte_limit"] != float64(limit) {
					t.Fatalf("budget ignored: %+v", model.result)
				}
			} else if model.result["content"] != "CLI_TAIL\n" {
				t.Fatalf("large read unavailable: %+v", model.result)
			}
		})
	}
}
