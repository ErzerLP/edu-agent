package command

import (
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestModelSettingsCLIHasNoProductCeilings(t *testing.T) {
	configs, credentials := pairedStores(config.DefaultServerURL, "test-token")
	configs.value.Timeout = "30s"
	preset := config.DefaultAgentConfig("ollama")
	configs.value.Agent = &preset
	model := &requestCapturingAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "hello"}}}
	runner := &singleSendAgentUI{}
	app, out, errOut := newTestApp(configs, credentials, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.AgentUI = runner
	app.NewModel = func(got config.AgentConfig, _ string) (agentloop.Model, error) {
		if got.Timeout != "30m" || got.ContextWindow != 2000000 || got.MaxTokens != 256000 {
			t.Fatalf("model config clamped: %+v", got)
		}
		return model, nil
	}
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }
	if exit := app.Run(t.Context(), []string{"model", "set", "--context-window", "2000000", "--max-tokens", "256000", "--timeout", "30m"}); exit != ExitOK {
		t.Fatalf("set=%d %s", exit, errOut.String())
	}
	requestTimeout, modelTimeout, err := agentTimeouts(configs.value)
	if err != nil || requestTimeout != 30*time.Second || modelTimeout != 30*time.Minute {
		t.Fatalf("timeouts coupled: server=%v model=%v error=%v", requestTimeout, modelTimeout, err)
	}
	if exit := app.Run(t.Context(), []string{"model", "show"}); exit != ExitOK {
		t.Fatal(errOut.String())
	}
	for _, want := range []string{"2000000", "256000", "30m"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("show omitted %s: %s", want, out.String())
		}
	}
	before := *configs.value.Agent
	for _, args := range [][]string{{"--timeout", "0s"}, {"--timeout", "-1s"}, {"--timeout", "99999999999999999999h"}, {"--context-window", "0"}, {"--context-window", "4095"}, {"--context-window", "9223372036854775808"}, {"--max-tokens", "0"}, {"--max-tokens", "-1"}, {"--max-tokens", "9223372036854775808"}} {
		if exit := app.Run(t.Context(), append([]string{"model", "set"}, args...)); exit != ExitInput || *configs.value.Agent != before {
			t.Fatalf("invalid settings accepted/changed config: args=%v exit=%d", args, exit)
		}
	}
	if exit := app.Run(t.Context(), []string{"agent", "--no-save"}); exit != ExitOK || runner.err != nil {
		t.Fatalf("launch=%d send=%v %s", exit, runner.err, errOut.String())
	}
	if len(model.requests) != 1 || model.requests[0].MaxTokens != 256000 {
		t.Fatalf("actual request output was capped: %+v", model.requests)
	}
}
