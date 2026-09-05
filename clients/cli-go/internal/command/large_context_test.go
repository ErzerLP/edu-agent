package command

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestLargeContextCLIConfigurationAndLaunch(t *testing.T) {
	store := &memoryConfigStore{}
	app, out, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	if exit := app.Run(t.Context(), []string{"model", "preset", "ollama"}); exit != ExitOK {
		t.Fatal(errOut.String())
	}
	if store.value.Agent.ContextWindow != 272000 || store.value.Agent.MaxTokens != 128000 {
		t.Fatalf("defaults=%+v", store.value.Agent)
	}
	if exit := app.Run(t.Context(), []string{"model", "show"}); exit != ExitOK || !strings.Contains(out.String(), "272000") || !strings.Contains(out.String(), "128000") {
		t.Fatalf("show: %s %s", out.String(), errOut.String())
	}
	for _, arg := range []string{"0", "-1", "128001"} {
		before := *store.value.Agent
		if exit := app.Run(t.Context(), []string{"model", "set", "--max-tokens", arg}); exit != ExitInput {
			t.Fatalf("accepted %s", arg)
		}
		if *store.value.Agent != before {
			t.Fatal("rejected flag changed config")
		}
	}
	if exit := app.Run(t.Context(), []string{"model", "set", "--max-tokens", "64000", "--context-window", "32768"}); exit != ExitOK {
		t.Fatal(errOut.String())
	}
	if store.value.Agent.MaxTokens != 64000 || store.value.Agent.ContextWindow != 32768 {
		t.Fatal("explicit limits not saved")
	}
	// Missing new field in an old explicit configuration defaults without resetting it.
	store.value.Agent.MaxTokens = 0
	if err := store.value.Validate(); err != nil {
		t.Fatal(err)
	}
	if store.value.Agent.MaxTokens != 128000 || store.value.Agent.ContextWindow != 32768 {
		t.Fatal("old window overridden")
	}

	for _, limit := range []int{128000, 64000} {
		preset := config.DefaultAgentConfig("ollama")
		preset.MaxTokens = limit
		configs, credentials := pairedStores(config.DefaultServerURL, "test-token")
		configs.value.Agent = &preset
		model := &requestCapturingAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "hello"}}}
		runner := &singleSendAgentUI{}
		app, _, errOut := newTestApp(configs, credentials, &fakeTerminal{})
		app.ModelSecrets = &memoryModelSecretStore{}
		app.AgentUI = runner
		app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
		app.InputIsTTY = func() bool { return true }
		app.OutputIsTTY = func() bool { return true }
		app.Getenv = func(string) string { return "xterm" }
		if exit := app.Run(t.Context(), []string{"agent", "--no-save"}); exit != ExitOK || runner.err != nil {
			t.Fatalf("launch=%d send=%v %s", exit, runner.err, errOut.String())
		}
		if len(model.requests) != 1 || model.requests[0].MaxTokens != limit {
			t.Fatalf("launch limit=%d requests=%d", limit, len(model.requests))
		}
	}
}
