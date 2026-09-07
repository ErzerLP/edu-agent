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
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestRecursiveCopyCLIOptions(t *testing.T) {
	defaults := fileCopyOptions{
		Bytes:              workspace.DefaultCopyBytes,
		PlanBytes:          workspace.DefaultCopyPlanBytes,
		Entries:            workspace.DefaultCopyEntries,
		JournalMemoryBytes: localartifact.DefaultMemoryBytes,
		JournalRecords:     localartifact.DefaultMaxRecords,
	}
	remaining, got, err := parseFileCopyOptions(nil)
	if err != nil || len(remaining) != 0 || got != defaults {
		t.Fatalf("defaults: remaining=%v options=%+v err=%v", remaining, got, err)
	}

	input := []string{
		"--file-copy-limit=8192",
		"--file-copy-plan-limit", "16384",
		"--file-copy-entry-limit=7",
		"--file-copy-journal-limit", "32768",
		"--file-copy-max-records=2",
		"--workspace", "--file-copy-entry-limit",
		"--", "--file-copy-limit", "literal",
	}
	remaining, got, err = parseFileCopyOptions(input)
	want := fileCopyOptions{Bytes: 8192, PlanBytes: 16384, Entries: 7, JournalMemoryBytes: 32768, JournalRecords: 2}
	wantRemaining := []string{"--workspace", "--file-copy-entry-limit", "--", "--file-copy-limit", "literal"}
	if err != nil || got != want || !reflect.DeepEqual(remaining, wantRemaining) {
		t.Fatalf("mixed options: remaining=%v options=%+v err=%v", remaining, got, err)
	}
	if roundtrip, again, err := parseFileCopyOptions(got.arguments()); err != nil || len(roundtrip) != 0 || again != got {
		t.Fatalf("arguments roundtrip: remaining=%v options=%+v err=%v", roundtrip, again, err)
	}

	maxInt := int64(^uint(0) >> 1)
	invalid := []struct {
		name string
		args []string
	}{
		{name: "missing", args: []string{"--file-copy-limit"}},
		{name: "non_numeric", args: []string{"--file-copy-limit=not-a-number"}},
		{name: "zero", args: []string{"--file-copy-limit=0"}},
		{name: "negative", args: []string{"--file-copy-limit=-1"}},
		{name: "overflow", args: []string{"--file-copy-limit=999999999999999999999"}},
		{name: "platform_max_int", args: []string{"--file-copy-limit=" + strconv.FormatInt(maxInt, 10)}},
		{name: "duplicate", args: []string{"--file-copy-limit=1", "--file-copy-limit=2"}},
		{name: "plan_ceiling", args: []string{"--file-copy-plan-limit=1073741825"}},
		{name: "entry_ceiling", args: []string{"--file-copy-entry-limit=1000001"}},
		{name: "journal_ceiling", args: []string{"--file-copy-journal-limit=1073741825"}},
		{name: "record_ceiling", args: []string{"--file-copy-max-records=8193"}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseFileCopyOptions(test.args); err == nil {
				t.Fatalf("accepted %v", test.args)
			}
		})
	}

	for _, text := range []string{
		"--file-copy-limit",
		"默认1073741824",
		"平台maxInt-1",
		"--file-copy-plan-limit",
		"默认67108864",
		"--file-copy-entry-limit",
		"默认100000",
		"--file-copy-journal-limit",
		"默认268435456",
		"--file-copy-max-records",
		"默认256",
		"复制追加日志",
		"递归目录",
	} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatalf("missing help %q", text)
		}
	}
	if strings.Contains(agentHelpText, "32MiB") || strings.Contains(agentHelpText, "仅支持普通文件") {
		t.Fatalf("help retained obsolete copy limitation: %q", agentHelpText)
	}
}

type recursiveCopyCLIModel struct {
	copyArguments string
	result        map[string]any
}

func (m *recursiveCopyCLIModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
			ID: "cli-recursive-copy", Type: "function", Function: modelclient.ToolFunction{Name: "copy", Arguments: m.copyArguments},
		}}}}, nil
	}
	if last.Role == "tool" {
		if err := json.Unmarshal([]byte(last.Content), &m.result); err != nil {
			return modelclient.Response{}, err
		}
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestRecursiveCopyCLILaunchHonorsBudgets(t *testing.T) {
	for _, test := range []struct {
		name       string
		flag       string
		wantTarget bool
	}{
		{name: "default", wantTarget: true},
		{name: "byte_budget", flag: "--file-copy-limit=8"},
		{name: "entry_budget", flag: "--file-copy-entry-limit=1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			if err := os.MkdirAll(filepath.Join(source, "empty"), 0700); err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{
				"first.bin":  []byte("recursive-copy-first"),
				"second.bin": []byte("second\x00binary\xff"),
			}
			for name, body := range files {
				if err := os.WriteFile(filepath.Join(source, name), body, 0600); err != nil {
					t.Fatal(err)
				}
			}

			statWorkspace, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			stat := statWorkspace.Execute(t.Context(), workspace.ToolStat, `{"path":"source"}`)
			if err := statWorkspace.Close(); err != nil {
				t.Fatal(err)
			}
			statValue, ok := stat.Value.(map[string]any)
			if !ok {
				t.Fatalf("stat value=%#v", stat.Value)
			}
			version, ok := statValue["entry_version"].(string)
			if !ok || version == "" {
				t.Fatalf("stat entry version=%#v", statValue["entry_version"])
			}
			copyArguments, err := json.Marshal(map[string]string{
				"source":           "source",
				"destination":      "target",
				"expected_version": version,
			})
			if err != nil {
				t.Fatal(err)
			}
			model := &recursiveCopyCLIModel{copyArguments: string(copyArguments)}
			configStore, credentials := pairedStores(config.DefaultServerURL, "server-token")
			preset := config.DefaultAgentConfig("ollama")
			configStore.value.Agent = &preset
			app, _, errOut := newTestApp(configStore, credentials, &fakeTerminal{})
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
				t.Fatalf("exit=%d err=%s result=%+v", exit, errOut.String(), model.result)
			}
			if model.result == nil {
				t.Fatal("model did not receive copy result")
			}

			if test.wantTarget {
				targetInfo, err := os.Stat(filepath.Join(root, "target"))
				if err != nil || !targetInfo.IsDir() {
					t.Fatalf("target root: info=%+v err=%v", targetInfo, err)
				}
				var copiedBytes int64
				for name, want := range files {
					path := filepath.Join(root, "target", name)
					got, err := os.ReadFile(path)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("target %s: got=%v err=%v", name, got, err)
					}
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					copiedBytes += info.Size()
				}
				emptyInfo, err := os.Stat(filepath.Join(root, "target", "empty"))
				if err != nil || !emptyInfo.IsDir() {
					t.Fatalf("empty target directory: info=%+v err=%v", emptyInfo, err)
				}
				var expectedBytes int64
				for _, body := range files {
					expectedBytes += int64(len(body))
				}
				if copiedBytes != expectedBytes {
					t.Fatalf("copied bytes=%d want=%d", copiedBytes, expectedBytes)
				}
				for _, key := range []string{"plan_id", "batch_id"} {
					value, ok := model.result[key].(string)
					if !ok || value == "" {
						t.Fatalf("missing %s in copy result: %+v", key, model.result)
					}
				}
				if model.result["complete"] != true {
					t.Fatalf("copy not complete: %+v", model.result)
				}
			} else {
				if _, err := os.Stat(filepath.Join(root, "target")); !os.IsNotExist(err) {
					t.Fatalf("budget failure created target: err=%v result=%+v", err, model.result)
				}
				if model.result["error"] != workspace.CodeFileTooLarge || model.result["code"] != workspace.CodeFileTooLarge {
					t.Fatalf("budget failure code=%+v", model.result)
				}
			}
		})
	}
}
