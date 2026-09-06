package agentcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type queryLimitModel struct {
	raw     string
	calls   int
	results []map[string]any
}

func (m *queryLimitModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(r) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"query test"}`}}, nil
	}
	last := r.Messages[len(r.Messages)-1]
	if last.Role == "user" {
		m.calls++
		raw := m.raw
		if raw == "" {
			raw = "{}"
		}
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: fmt.Sprintf("budget-query-%d", m.calls), Type: "function", Function: modelclient.ToolFunction{Name: "list", Arguments: raw}}}}}, nil
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
func TestQueryPaginationCurrentBudgetsAcrossResumeAndSwitch(t *testing.T) {
	f := outputFixture(t, false)
	owner, provider := f.controller.SessionID(), f.controller.provider
	f.controller.loopOptions.WorkspaceQueryMemoryBytes = 8192
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	model := &queryLimitModel{}
	dependencies := controllerDependencies(f.openStore(t), model, f.server, f.workspace, provider)
	dependencies.LoopOptions.ContextWindow = 32768
	dependencies.LoopOptions.WorkspaceQueryMemoryBytes = 128
	dependencies.LoopOptions.WorkspaceQueryEntries = 7
	dependencies.LoopOptions.WorkspaceQueryRecords = 1
	c, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	check := func() {
		t.Helper()
		before := len(model.results)
		if _, err := c.Send(t.Context(), "list"); err != nil {
			t.Fatal(err)
		}
		if len(model.results) != before+1 || model.results[before]["error"] != workspace.CodeQueryCapacity {
			t.Fatalf("current budget lost: %+v", model.results)
		}
	}
	check()
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(leaseSummary(t, c, owner)), SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestQueryPaginationCursorExpiresAcrossSwitchAndRestore(t *testing.T) {
	f := outputFixture(t, false)
	owner, provider := f.controller.SessionID(), f.controller.provider
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	model := &queryLimitModel{}
	open := func() *Controller {
		t.Helper()
		deps := controllerDependencies(f.openStore(t), model, f.server, f.workspace, provider)
		deps.LoopOptions.ContextWindow = 32768
		c, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	if _, err := c.Send(t.Context(), "list"); err != nil {
		t.Fatal(err)
	}
	cursor, ok := model.results[len(model.results)-1]["cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("no initial query: %+v", model.results)
	}
	raw, _ := json.Marshal(map[string]any{"cursor": cursor})
	model.raw = string(raw)
	check := func() {
		t.Helper()
		if _, err := c.Send(t.Context(), "continue"); err != nil {
			t.Fatal(err)
		}
		if result := model.results[len(model.results)-1]; result["error"] != workspace.CodeCursorExpired {
			t.Fatalf("resurrected cursor: %+v", result)
		}
	}
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(leaseSummary(t, c, owner)), SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	check()
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	c = open()
	defer c.Close()
	check()
}
