package command

import (
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

func TestLocalExecutionResourceOptions(t *testing.T) {
	args := []string{"--workspace", "--task-output-limit", "--task-max-records=30", "--task-max-running", "2", "--task-output-limit", "1024", "--task-total-output-limit=4096", "--no-save", "--", "--task-max-records=1"}
	remaining, options, err := parseLocalExecutionOptions(args)
	want := []string{"--workspace", "--task-output-limit", "--no-save", "--", "--task-max-records=1"}
	if err != nil || !reflect.DeepEqual(remaining, want) || options.MaxTasks != 30 || options.MaxConcurrent != 2 || options.OutputBytesPerTask != 1024 || options.OutputBytesTotal != 4096 {
		t.Fatalf("remaining=%v options=%+v err=%v", remaining, options, err)
	}
	_, defaults, err := parseLocalExecutionOptions(nil)
	if err != nil || defaults.MaxTasks != localexec.DefaultMaxTasks || defaults.MaxConcurrent != localexec.DefaultMaxConcurrent || defaults.OutputBytesPerTask != localexec.DefaultOutputBytesPerTask || defaults.OutputBytesTotal != localexec.DefaultOutputBytesTotal {
		t.Fatalf("defaults=%+v err=%v", defaults, err)
	}
}

func TestLocalExecutionResourceOptionsRejectInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		{"--task-output-limit"}, {"--task-output-limit="}, {"--task-output-limit", "0"},
		{"--task-output-limit=-1"}, {"--task-output-limit=1MiB"}, {"--task-output-limit=999999999999999999999999999"},
		{"--task-max-running=3", "--task-max-records=2"},
		{"--task-output-limit=1", "--task-output-limit", "2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, _, err := parseLocalExecutionOptions(args); err == nil {
				t.Fatalf("accepted %v", args)
			}
		})
	}
}

func TestLocalOutputSavedResourceOptions(t *testing.T) {
	remaining, options, err := parseLocalExecutionOptions([]string{"--task-saved-output-limit=4096", "--no-save"})
	if err != nil || options.SavedOutputBytesPerTask != 4096 || !reflect.DeepEqual(remaining, []string{"--no-save"}) {
		t.Fatalf("options=%+v remaining=%v err=%v", options, remaining, err)
	}
	_, defaults, err := parseLocalExecutionOptions(nil)
	if err != nil || defaults.SavedOutputBytesPerTask != localexec.DefaultSavedOutputBytesPerTask {
		t.Fatalf("defaults=%+v err=%v", defaults, err)
	}
	for _, args := range [][]string{{"--task-saved-output-limit=0"}, {"--task-saved-output-limit=-1"}, {"--task-saved-output-limit=1", "--task-saved-output-limit=2"}} {
		if _, _, err := parseLocalExecutionOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, text := range []string{"--task-saved-output-limit", "task search", "PTY 尚未交付"} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatalf("help missing %q", text)
		}
	}
}

func TestLocalExecutionHelpDisclosesTaskScope(t *testing.T) {
	for _, text := range []string{"--task-max-records", "--task-max-running", "--task-output-limit", "--task-total-output-limit", "Shell", "F5", "不受文件确认模式限制", "PTY 尚未交付"} {
		if !strings.Contains(agentHelpText, text) {
			t.Fatalf("help missing %q", text)
		}
	}
}
