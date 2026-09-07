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

type archivePurgeCLIModel struct {
	arguments  string
	result     map[string]any
	registered bool
}

func (m *archivePurgeCLIModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	for _, tool := range r.Tools {
		if tool.Function.Name == "purge_archive" {
			m.registered = true
		}
	}
	last := r.Messages[len(r.Messages)-1]
	if last.Role == "user" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "cli-purge", Type: "function", Function: modelclient.ToolFunction{Name: "purge_archive", Arguments: m.arguments}}}}}, nil
	}
	if last.Role == "tool" {
		if err := json.Unmarshal([]byte(last.Content), &m.result); err != nil {
			return modelclient.Response{}, err
		}
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestArchivePurgeCLISharedBudgetsAndLargeFile(t *testing.T) {
	for _, mode := range []string{"large_file", "entry_limit", "plan_limit", "journal_limit"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := ".edu-agent-archive/selected"
			if err := os.MkdirAll(filepath.Join(root, path, "empty"), 0700); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(filepath.Join(root, path, "large.bin"), os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			const size = int64(33<<20) + 17
			if err := file.Truncate(size); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			w, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			version := w.Execute(t.Context(), "stat", `{"path":"`+path+`"}`).Value.(map[string]any)["entry_version"].(string)
			_ = w.Close()
			raw, _ := json.Marshal(map[string]any{"path": path, "expected_version": version})
			model := &archivePurgeCLIModel{arguments: string(raw)}
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
			args := []string{"agent", "--no-save", "--workspace", root, "--file-copy-limit", "1"}
			want := "file_too_large"
			switch mode {
			case "entry_limit":
				args = append(args, "--file-copy-entry-limit", "1")
			case "plan_limit":
				args = append(args, "--file-copy-plan-limit", "1")
			case "journal_limit":
				args = append(args, "--file-copy-journal-limit", "1")
				want = "file_batch_limit"
			}
			if exit := app.Run(t.Context(), args); exit != ExitOK {
				t.Fatal(exit, errOut.String())
			}
			if !model.registered || model.result == nil {
				t.Fatal("purge route missing", model)
			}
			if mode == "large_file" {
				if model.result["publication_outcome"] != "completed" || model.result["logical_bytes_removed"] != float64(size) || model.result["physical_bytes_reclaimed"] != "unknown" {
					t.Fatal(model.result)
				}
				if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
					t.Fatal("large entry not removed", err)
				}
			} else {
				if model.result["code"] != want {
					t.Fatal(mode, model.result)
				}
				if info, err := os.Stat(filepath.Join(root, path, "large.bin")); err != nil || info.Size() != size {
					t.Fatal("budget silently narrowed deletion", info, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, ".edu-agent-archive")); err != nil {
				t.Fatal("archive root removed", err)
			}
		})
	}
	for _, want := range []string{"purge_archive", "YOLO仍须明确批准", "不可撤销", "物理释放量未知", "--file-copy-limit不限制清理大文件", "复制/归档清理", "不恢复批准或自动续执行"} {
		if !strings.Contains(agentHelpText, want) {
			t.Fatal("missing help", want)
		}
	}
}
