package command

import (
	"context"
	"errors"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type fakeAgentUI struct {
	conversation agentui.Conversation
	modelName    string
	calls        int
}

func (u *fakeAgentUI) Run(_ context.Context, conversation agentui.Conversation, modelName string) error {
	u.conversation, u.modelName, u.calls = conversation, modelName, u.calls+1
	return nil
}

type contextPassingAgentUI struct {
	firstErr  error
	secondErr error
}

func (u *contextPassingAgentUI) Run(ctx context.Context, conversation agentui.Conversation, _ string) error {
	_, u.firstErr = conversation.Send(ctx, "第一轮")
	if u.firstErr == nil {
		_, u.secondErr = conversation.Send(ctx, "第二轮")
	}
	return nil
}

type sequenceAgentModel struct {
	responses []modelclient.Response
	calls     int
}

func (m *sequenceAgentModel) Complete(context.Context, modelclient.Request) (modelclient.Response, error) {
	m.calls++
	if len(m.responses) == 0 {
		return modelclient.Response{}, errors.New("unexpected model request")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

type fixedAgentModel struct {
	response modelclient.Response
	err      error
}

func (m fixedAgentModel) Complete(context.Context, modelclient.Request) (modelclient.Response, error) {
	return m.response, m.err
}

type requestCapturingAgentModel struct {
	requests []modelclient.Request
	response modelclient.Response
}

func (m *requestCapturingAgentModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.requests = append(m.requests, request)
	return m.response, nil
}

type singleSendAgentUI struct{ err error }

func (u *singleSendAgentUI) Run(ctx context.Context, conversation agentui.Conversation, _ string) error {
	_, u.err = conversation.Send(ctx, "测试默认推理强度")
	return nil
}

func TestModelSetSupportsUnlimitedToolRoundsAndHasNoFixedMaximum(t *testing.T) {
	t.Parallel()
	store := &memoryConfigStore{}
	app, out, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})

	if exit := app.Run(t.Context(), []string{"model", "set", "--max-tool-rounds", "0"}); exit != ExitOK {
		t.Fatalf("set unlimited exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if store.value.Agent == nil || store.value.Agent.MaxToolRounds != 0 {
		t.Fatalf("saved unlimited config=%+v", store.value.Agent)
	}

	out.Reset()
	if exit := app.Run(t.Context(), []string{"model", "set", "--max-tool-rounds", "1000000"}); exit != ExitOK {
		t.Fatalf("set large guard exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if store.value.Agent.MaxToolRounds != 1_000_000 {
		t.Fatalf("saved large guard=%+v", store.value.Agent)
	}

	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"model", "set", "--max-tool-rounds", "-1"}); exit != ExitInput {
		t.Fatalf("set negative exit=%d err=%q", exit, errOut.String())
	}
	if !strings.Contains(errOut.String(), "0表示不限制") || store.value.Agent.MaxToolRounds != 1_000_000 {
		t.Fatalf("negative err=%q saved=%+v", errOut.String(), store.value.Agent)
	}
}

func TestAgentLaunchSelectsWorkspaceAndDegradesInvalidRoots(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig()
	configStore.value.Agent = &preset
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})

	app.NewModel = func(*modelgateway.Model) (agentloop.Model, error) { return fixedAgentModel{}, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }

	startupRoot := t.TempDir()
	t.Chdir(startupRoot)
	runner := &fakeAgentUI{}
	app.AgentUI = runner
	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK {
		t.Fatalf("default workspace exit=%d err=%q", exit, errOut.String())
	}
	status := runner.conversation.WorkspaceStatus()
	if !status.Available || status.Label != filepath.Base(startupRoot) || runner.conversation.FileAuthorizationMode() != agentloop.FileAuthorizationConfirm {
		t.Fatalf("default workspace status=%+v fileMode=%q", status, runner.conversation.FileAuthorizationMode())
	}

	explicitRoot := t.TempDir()
	runner = &fakeAgentUI{}
	app.AgentUI = runner
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"agent", "--workspace", explicitRoot}); exit != ExitOK {
		t.Fatalf("explicit workspace exit=%d err=%q", exit, errOut.String())
	}
	status = runner.conversation.WorkspaceStatus()
	if !status.Available || status.Label != filepath.Base(explicitRoot) {
		t.Fatalf("explicit workspace status=%+v", status)
	}

	runner = &fakeAgentUI{}
	app.AgentUI = runner
	errOut.Reset()
	missingRoot := filepath.Join(t.TempDir(), "missing")
	if exit := app.Run(t.Context(), []string{"agent", "--workspace", missingRoot}); exit != ExitOK {
		t.Fatalf("unavailable workspace exit=%d err=%q", exit, errOut.String())
	}
	status = runner.conversation.WorkspaceStatus()
	if status.Available || status.Code != "workspace_unavailable" {
		t.Fatalf("unavailable workspace status=%+v", status)
	}

	calls := runner.calls
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"agent", "extra"}); exit != ExitInput || runner.calls != calls || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("invalid args exit=%d calls=%d err=%q", exit, runner.calls, errOut.String())
	}
}

func TestAgentHelpShowsWorkspaceWithoutTTYOrDependencies(t *testing.T) {
	app, out, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
	runner := &fakeAgentUI{}
	app.AgentUI = runner
	app.InputIsTTY = func() bool { return false }
	app.OutputIsTTY = func() bool { return false }
	if exit := app.Run(t.Context(), []string{"agent", "--help"}); exit != ExitOK {
		t.Fatalf("help exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	for _, expected := range []string{"edu-agent agent [--workspace PATH]", "--workspace PATH", "F4", "YOLO", "list、read、search、write、edit"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("help missing %q: %s", expected, out.String())
		}
	}
	if runner.calls != 0 || errOut.Len() != 0 {
		t.Fatalf("help started UI or wrote stderr: calls=%d err=%q", runner.calls, errOut.String())
	}
}

func TestAgentLaunchPassesContextCompactionMode(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig()
	preset.ContextCompaction = "off"
	configStore.value.Agent = &preset
	model := &sequenceAgentModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: strings.Repeat("x", 200<<10)}}}}
	runner := &contextPassingAgentUI{}
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})

	app.AgentUI = runner
	app.NewModel = func(*modelgateway.Model) (agentloop.Model, error) { return model, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }

	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK {
		t.Fatalf("launch exit=%d err=%q", exit, errOut.String())
	}
	if runner.firstErr != nil || runner.secondErr == nil || !strings.Contains(runner.secondErr.Error(), agentloop.ContextBudgetInvalid) || model.calls != 1 {
		t.Fatalf("context mode was not passed: first=%v second=%v calls=%d", runner.firstErr, runner.secondErr, model.calls)
	}
}

func TestModelSetRejectsInvalidContextCompaction(t *testing.T) {
	t.Parallel()
	store := &memoryConfigStore{}
	app, _, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"model", "set", "--context-compaction", "rolling"}); exit != ExitInput {
		t.Fatalf("invalid mode exit=%d err=%q", exit, errOut.String())
	}
	if store.saveCalls != 0 {
		t.Fatalf("invalid mode was saved: %+v", store.value.Agent)
	}
}

func TestModelSetRejectsInvalidReasoningEffort(t *testing.T) {
	t.Parallel()
	store := &memoryConfigStore{}
	app, _, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"model", "set", "--reasoning-effort", "extreme"}); exit != ExitInput {
		t.Fatalf("invalid effort exit=%d err=%q", exit, errOut.String())
	}
	if store.saveCalls != 0 {
		t.Fatalf("invalid effort was saved: %+v", store.value.Agent)
	}
}

func TestAgentAndModelTestUseConfiguredReasoningEffort(t *testing.T) {
	preset := config.DefaultAgentConfig()
	preset.ReasoningEffort = "xhigh"
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	model := &requestCapturingAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "连接正常"}}}
	runner := &singleSendAgentUI{}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})

	app.AgentUI = runner
	app.NewModel = func(*modelgateway.Model) (agentloop.Model, error) { return model, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }

	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK || runner.err != nil {
		t.Fatalf("agent exit=%d send=%v err=%q", exit, runner.err, errOut.String())
	}
	if len(model.requests) != 1 || model.requests[0].ReasoningEffort != modelclient.ReasoningEffortXHigh {
		t.Fatalf("agent requests=%+v", model.requests)
	}

	model.requests = nil
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitOK {
		t.Fatalf("model test exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if len(model.requests) != 1 || model.requests[0].ReasoningEffort != modelclient.ReasoningEffortXHigh {
		t.Fatalf("model test requests=%+v", model.requests)
	}
}
