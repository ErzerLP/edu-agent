package command

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type archiveRestoreCLIModel struct {
	arguments  string
	result     map[string]any
	registered bool
}

func (m *archiveRestoreCLIModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	for _, tool := range request.Tools {
		if tool.Function.Name == "restore_archive" {
			m.registered = true
		}
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "cli-restore", Type: "function", Function: modelclient.ToolFunction{Name: "restore_archive", Arguments: m.arguments}}}}}, nil
	}
	if last.Role == "tool" {
		if err := json.Unmarshal([]byte(last.Content), &m.result); err != nil {
			return modelclient.Response{}, err
		}
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestArchiveRestoreCLI(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "conflict"}[conflict], func(t *testing.T) {
			root := t.TempDir()
			source := ".edu-agent-archive/legacy-container/original.bin"
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, source)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, source), []byte("binary\x00\xff"), 0600); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if err := os.WriteFile(filepath.Join(root, "recovered"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			w, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			version := w.Execute(t.Context(), "stat", `{"path":"`+source+`"}`).Value.(map[string]any)["entry_version"].(string)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"source": source, "destination": "recovered", "expected_version": version})
			model := &archiveRestoreCLIModel{arguments: string(raw)}
			store, credentials := pairedStores(config.DefaultServerURL, "server-token")
			preset := config.DefaultAgentConfig("ollama")
			store.value.Agent = &preset
			app, _, errOut := newTestApp(store, credentials, &fakeTerminal{})
			app.ModelSecrets = &memoryModelSecretStore{}
			app.AgentUI = fileEditBudgetUI{}
			app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
			app.InputIsTTY = func() bool { return true }
			app.OutputIsTTY = func() bool { return true }
			app.Getenv = func(string) string { return "xterm" }
			if exit := app.Run(t.Context(), []string{"agent", "--no-save", "--workspace", root}); exit != ExitOK {
				t.Fatal(exit, errOut.String())
			}
			if !model.registered || model.result == nil {
				t.Fatal("missing restore route", model)
			}
			body, err := os.ReadFile(filepath.Join(root, "recovered"))
			if err != nil {
				t.Fatal(err)
			}
			if conflict {
				if string(body) != "keep" || model.result["code"] != workspace.CodeAlreadyExists {
					t.Fatal(body, model.result)
				}
			} else {
				if string(body) != "binary\x00\xff" || model.result["publication_outcome"] != "completed" {
					t.Fatal(body, model.result)
				}
				if _, err := os.Stat(filepath.Join(root, source)); !os.IsNotExist(err) {
					t.Fatal("source remains", err)
				}
			}
		})
	}
	for _, text := range []string{"restore_archive", "显式destination", "不从旧布局猜测原位置", "不合并", "无需末页", "不可重放身份"} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatal("missing help", text)
		}
	}
}
