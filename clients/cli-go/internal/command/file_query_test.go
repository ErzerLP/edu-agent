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

func TestQueryPaginationResourceOptions(t *testing.T) {
	args, options, err := parseFileQueryOptions([]string{"--file-query-memory-limit=8192", "--file-query-entry-limit", "7", "--file-query-max-records=2", "--workspace", "--file-query-entry-limit", "--no-save"})
	if err != nil || options.MemoryBytes != 8192 || options.Entries != 7 || options.Records != 2 || !reflect.DeepEqual(args, []string{"--workspace", "--file-query-entry-limit", "--no-save"}) {
		t.Fatalf("%v %+v %v", args, options, err)
	}
	_, again, err := parseFileQueryOptions(options.arguments())
	if err != nil || again != options {
		t.Fatal("lost resume/new budgets")
	}
	for _, args := range [][]string{{"--file-query-memory-limit=0"}, {"--file-query-memory-limit=1073741825"}, {"--file-query-entry-limit=1000001"}, {"--file-query-max-records=65"}, {"--file-query-entry-limit=-1"}, {"--file-query-max-records"}, {"--file-query-max-records=1", "--file-query-max-records=2"}} {
		if _, _, err := parseFileQueryOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, text := range []string{"--file-query-memory-limit", "--file-query-entry-limit", "--file-query-max-records", "next_cursor", "scan_complete", "cursor_stale"} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatalf("missing help %s", text)
		}
	}
}

type queryBudgetModel struct{ result map[string]any }

func (m *queryBudgetModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	last := r.Messages[len(r.Messages)-1]
	if last.Role == "user" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "cli-query", Type: "function", Function: modelclient.ToolFunction{Name: "list", Arguments: "{}"}}}}}, nil
	}
	if last.Role == "tool" {
		if err := json.Unmarshal([]byte(last.Content), &m.result); err != nil {
			return modelclient.Response{}, err
		}
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}
func TestQueryPaginationLaunchBudgets(t *testing.T) {
	for _, flag := range []string{"", "--file-query-memory-limit=128", "--file-query-entry-limit=1"} {
		t.Run(flag, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			model := &queryBudgetModel{}
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
			if flag != "" {
				args = append(args, flag)
			}
			if exit := app.Run(t.Context(), args); exit != ExitOK {
				t.Fatalf("exit=%d %s", exit, errOut.String())
			}
			if flag == "" {
				if model.result["scan_complete"] != true || model.result["returned"] != float64(1) {
					t.Fatalf("%+v", model.result)
				}
			} else if model.result["error"] != "query_capacity" && model.result["scan_error"] != "query_capacity" {
				t.Fatalf("budget bypassed: %+v", model.result)
			}
		})
	}
}
