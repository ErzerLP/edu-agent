package command

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestFileArtifactResourceOptions(t *testing.T) {
	args, options, err := parseFileResultOptions([]string{"--file-diff-limit=4096", "--file-patch-limit", "8192", "--artifact-memory-limit=16384", "--artifact-max-records=7", "--workspace", "--file-diff-limit", "--no-save"})
	if err != nil || !reflect.DeepEqual(args, []string{"--workspace", "--file-diff-limit", "--no-save"}) || options.DiffBytes != 4096 || options.PatchBytes != 8192 || options.Artifacts.MaxArtifactBytes != 4096 || options.Artifacts.MemoryBytes != 16384 || options.Artifacts.MaxRecords != 7 {
		t.Fatalf("%v %+v %v", args, options, err)
	}
	_, roundTrip, err := parseFileResultOptions(options.arguments())
	if err != nil || !reflect.DeepEqual(roundTrip, options) {
		t.Fatal("resource settings lost on resume/new transition")
	}
	for _, args := range [][]string{{"--file-diff-limit=0"}, {"--file-diff-limit=1073741825"}, {"--file-patch-limit=-1"}, {"--artifact-memory-limit=1073741825"}, {"--artifact-max-records=8193"}, {"--artifact-max-records=0"}, {"--artifact-memory-limit"}, {"--file-diff-limit=1", "--file-diff-limit=2"}} {
		if _, _, err := parseFileResultOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

type filePatchBudgetModel struct{ result map[string]any }

func (m *filePatchBudgetModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	last := r.Messages[len(r.Messages)-1]
	if last.Role == "user" {
		raw, _ := json.Marshal(map[string]any{"patch": "*** Begin Patch\n*** Add File: patch.txt\n+CLI_PATCH_BODY\n*** End Patch", "expected_hashes": map[string]string{}})
		return modelclient.Response{Message: toolMessageForPatch(string(raw))}, nil
	}
	if last.Role == "tool" {
		if err := json.Unmarshal([]byte(last.Content), &m.result); err != nil {
			return modelclient.Response{}, err
		}
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}
func toolMessageForPatch(args string) modelclient.Message {
	return modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "cli-patch", Type: "function", Function: modelclient.ToolFunction{Name: "apply_patch", Arguments: args}}}}
}

func TestFilePatchLaunchResourceBudgets(t *testing.T) {
	for _, test := range []struct{ name, flag, code string }{{"success", "", ""}, {"diff", "--file-diff-limit=16", "diff_too_large"}, {"batch", "--file-patch-limit=1", "patch_too_large"}, {"memory", "--artifact-memory-limit=16", "artifact_limit"}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			model := &filePatchBudgetModel{}
			cs, credentials := pairedStores(config.DefaultServerURL, "server-token")
			preset := config.DefaultAgentConfig("ollama")
			cs.value.Agent = &preset
			app, _, errOut := newTestApp(cs, credentials, &fakeTerminal{})
			app.ModelSecrets = &memoryModelSecretStore{}
			app.AgentUI = fileEditBudgetUI{}
			app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
			app.InputIsTTY = func() bool { return true }
			app.OutputIsTTY = func() bool { return true }
			app.Getenv = func(string) string { return "xterm" }
			args := []string{"agent", "--no-save", "--workspace", root}
			if test.flag != "" {
				args = append(args, test.flag)
			}
			if exit := app.Run(t.Context(), args); exit != ExitOK {
				t.Fatalf("exit=%d %s", exit, errOut.String())
			}
			data, err := os.ReadFile(filepath.Join(root, "patch.txt"))
			if test.code == "" {
				if err != nil || string(data) != "CLI_PATCH_BODY\n" || model.result["receipt_id"] == "" {
					t.Fatalf("result=%+v data=%q err=%v", model.result, data, err)
				}
			} else if !os.IsNotExist(err) || model.result["error"] != test.code {
				t.Fatalf("budget bypassed: %+v %q %v", model.result, data, err)
			}
		})
	}
}

func TestFileArtifactHelpContract(t *testing.T) {
	for _, text := range []string{"--file-diff-limit", "--file-patch-limit", "--artifact-memory-limit", "--artifact-max-records", "apply_patch", "F6", "完整diff"} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatalf("help missing %s", text)
		}
	}
}
