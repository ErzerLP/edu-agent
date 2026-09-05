package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestLargeFileEditOptionsAreIndependent(t *testing.T) {
	args, readBytes, err := parseFileReadOptions([]string{"--file-read-limit=16", "--file-edit-limit", "32", "--workspace", "--file-edit-limit", "--no-save"})
	if err != nil {
		t.Fatal(err)
	}
	args, editBytes, err := parseFileEditOptions(args)
	if err != nil || readBytes != 16 || editBytes != 32 || !reflect.DeepEqual(args, []string{"--workspace", "--file-edit-limit", "--no-save"}) {
		t.Fatalf("%v %d %d %v", args, readBytes, editBytes, err)
	}
	_, editBytes, err = parseFileEditOptions(nil)
	if err != nil || editBytes != workspace.DefaultEditFileBytes {
		t.Fatal("invalid edit default")
	}
	for _, args := range [][]string{{"--file-edit-limit"}, {"--file-edit-limit="}, {"--file-edit-limit=0"}, {"--file-edit-limit=-1"}, {"--file-edit-limit=1MiB"}, {"--file-edit-limit=999999999999999999999"}, {"--file-edit-limit=1", "--file-edit-limit=2"}} {
		if _, _, err := parseFileEditOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, text := range []string{"--file-edit-limit", "局部edit", "完整diff"} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatalf("help missing %q", text)
		}
	}
}

type fileEditBudgetUI struct{}

func (fileEditBudgetUI) Run(ctx context.Context, c agentui.Conversation, _ string) error {
	result, err := c.Send(ctx, "edit")
	if err == nil && result.PendingFileMutation != nil {
		_, err = c.ResolveFileMutation(ctx, result.PendingFileMutation.CallID, agentloop.FileMutationApprove)
	}
	return err
}

type fileEditBudgetModel struct {
	hash   string
	result map[string]any
}

func (m *fileEditBudgetModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	last := r.Messages[len(r.Messages)-1]
	if last.Role == "user" {
		raw, _ := json.Marshal(map[string]any{"path": "large.txt", "expected_hash": m.hash, "edits": []map[string]string{{"old_text": "CLI_EDIT_OLD", "new_text": "CLI_EDIT_NEW"}}})
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "edit-budget", Type: "function", Function: modelclient.ToolFunction{Name: "edit", Arguments: string(raw)}}}}}, nil
	}
	if last.Role == "tool" {
		if err := json.Unmarshal([]byte(last.Content), &m.result); err != nil {
			return modelclient.Response{}, err
		}
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestLargeFileEditLaunchHonorsProcessingBudget(t *testing.T) {
	original := strings.Repeat("x", (1<<20)+1) + "\nCLI_EDIT_OLD\n"
	for _, limit := range []int64{128, 2 << 20} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "large.txt")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(original))
			model := &fileEditBudgetModel{hash: "sha256:" + hex.EncodeToString(digest[:])}
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
			if exit := app.Run(t.Context(), []string{"agent", "--no-save", "--workspace", root, "--file-read-limit=64", "--file-edit-limit=" + strconv.FormatInt(limit, 10)}); exit != ExitOK {
				t.Fatalf("exit=%d %s", exit, errOut.String())
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if limit == 128 {
				if string(data) != original || model.result["error"] != workspace.CodeFileTooLarge {
					t.Fatalf("budget bypassed: %+v", model.result)
				}
			} else if string(data) != strings.Replace(original, "CLI_EDIT_OLD", "CLI_EDIT_NEW", 1) {
				t.Fatal("edit incorrectly used read/write budget")
			}
		})
	}
}
