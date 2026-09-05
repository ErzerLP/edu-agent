package agentcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type fileReadResumeModel struct {
	calls   int
	results []map[string]any
}

func (m *fileReadResumeModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"read"}`}}, nil
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" {
		m.calls++
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: fmt.Sprintf("read-%d", m.calls), Type: "function", Function: modelclient.ToolFunction{Name: "read", Arguments: `{"path":"file","offset":2}`}}}}}, nil
	}
	if last.Role == "tool" {
		var value map[string]any
		if err := json.Unmarshal([]byte(last.Content), &value); err != nil {
			return modelclient.Response{}, err
		}
		m.results = append(m.results, value)
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestLargeFileReadClientBudgetSurvivesResumeAndSwitch(t *testing.T) {
	f := outputFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.workspace, "file"), []byte(strings.Repeat("x", 1500)+"\ntail\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner := f.controller.SessionID()
	// The previous process's resource setting is not a persisted permission.
	f.controller.loopOptions.WorkspaceReadFileBytes = 4096
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	model := &fileReadResumeModel{}
	dependencies := controllerDependencies(f.openStore(t), model, f.server, f.workspace, f.controller.provider)
	dependencies.LoopOptions.ContextWindow = 32768
	dependencies.LoopOptions.WorkspaceReadFileBytes = 128
	c, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	check := func() {
		t.Helper()
		if _, err := c.Send(t.Context(), "read"); err != nil {
			t.Fatal(err)
		}
		value := model.results[len(model.results)-1]
		if value["error"] != workspace.CodeFileTooLarge || value["read_byte_limit"] != float64(128) {
			t.Fatalf("current client budget lost: %+v", value)
		}
	}
	check()
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	summary := leaseSummary(t, c, c.SessionID())
	if _, err := c.RenameSession(t.Context(), summary.SessionID, "read new", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	check()
	summary = leaseSummary(t, c, owner)
	if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(summary), SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	check()
}
