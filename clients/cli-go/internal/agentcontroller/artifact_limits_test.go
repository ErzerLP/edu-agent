package agentcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type artifactLimitModel struct {
	calls   int
	results []map[string]any
}

func (m *artifactLimitModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(r) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"budget test"}`}}, nil
	}
	last := r.Messages[len(r.Messages)-1]
	if last.Role == "user" {
		m.calls++
		raw, _ := json.Marshal(map[string]any{"patch": "*** Begin Patch\n*** Add File: budget.txt\n+budget data\n*** End Patch", "expected_hashes": map[string]string{}})
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: fmt.Sprintf("budget-patch-%d", m.calls), Type: "function", Function: modelclient.ToolFunction{Name: "apply_patch", Arguments: string(raw)}}}}}, nil
	}
	if last.Role == "tool" {
		var result map[string]any
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
			return modelclient.Response{}, err
		}
		m.results = append(m.results, result)
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestFileArtifactCurrentBudgetsAcrossResumeAndSwitch(t *testing.T) {
	f := outputFixture(t, false)
	owner := f.controller.SessionID()
	provider := f.controller.provider
	f.controller.loopOptions.WorkspaceDiffBytes = 2048
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	model := &artifactLimitModel{}
	dependencies := controllerDependencies(f.openStore(t), model, f.server, f.workspace, provider)
	dependencies.LoopOptions.ContextWindow = 32768
	dependencies.LoopOptions.WorkspaceDiffBytes = 16
	dependencies.LoopOptions.WorkspacePatchBytes = 128
	dependencies.LoopOptions.ArtifactOptions = localartifact.Options{MaxArtifactBytes: 16, MemoryBytes: 1024, MaxRecords: 4}
	c, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	check := func() {
		t.Helper()
		before := len(model.results)
		if _, err := c.Send(t.Context(), "patch"); err != nil {
			t.Fatal(err)
		}
		if len(model.results) != before+1 || model.results[before]["error"] != workspace.CodeDiffTooLarge {
			t.Fatalf("current diff budget lost: %+v", model.results)
		}
		if _, err := c.artifacts.Put(t.Context(), c.artifactOwner, "diff", make([]byte, 17)); err == nil {
			t.Fatal("current artifact budget lost")
		}
	}
	check()
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	check()
	summary := leaseSummary(t, c, owner)
	if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(summary), SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	check()
}
