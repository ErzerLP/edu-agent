package command

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelsecret"
)

type memoryModelSecretStore struct {
	value   string
	binding string
	present bool
	loadErr error
	saves   int
	deletes int
}

func (s *memoryModelSecretStore) Load(binding string) (string, error) {
	if s.loadErr != nil {
		return "", s.loadErr
	}
	if !s.present || s.binding != binding {
		return "", modelsecret.ErrNotFound
	}
	return s.value, nil
}
func (s *memoryModelSecretStore) Save(binding, value string) error {
	s.value, s.binding, s.present, s.saves = value, binding, true, s.saves+1
	return nil
}
func (s *memoryModelSecretStore) Delete(binding string) error {
	if s.present && s.binding == binding {
		s.value, s.binding, s.present = "", "", false
	}
	s.deletes++
	return nil
}

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

func TestModelConfigurationWorksBeforePairingAndNeverPrintsKey(t *testing.T) {
	store := &memoryConfigStore{}
	secrets := &memoryModelSecretStore{}
	terminal := &fakeTerminal{lines: []string{""}}
	app, out, errOut := newTestApp(store, &memoryCredentialStore{}, terminal)
	app.ModelSecrets = secrets

	if exit := app.Run(t.Context(), []string{"model", "preset", "deepseek"}); exit != ExitOK {
		t.Fatalf("preset exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if !store.present || store.value.Agent == nil || store.value.Agent.Provider != "deepseek" || store.value.Agent.ReasoningEffort != config.DefaultAgentReasoningEffort {
		t.Fatalf("config=%+v", store.value)
	}
	out.Reset()
	if exit := app.Run(t.Context(), []string{"model", "set", "--base-url", "https://model.example/v1", "--model", "teacher-model", "--context-window", "65536", "--context-compaction", "recent-only", "--reasoning-effort", "high", "--timeout", "2m", "--max-tool-rounds", "0"}); exit != ExitOK {
		t.Fatalf("set exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if got := store.value.Agent; got.BaseURL != "https://model.example/v1" || got.Model != "teacher-model" || got.ContextWindow != 65536 || got.ContextCompaction != "recent-only" || got.ReasoningEffort != "high" || got.Timeout != "2m" || got.MaxToolRounds != 0 {
		t.Fatalf("agent config=%+v", got)
	}

	const secret = "private-key-value"
	out.Reset()
	app.Dashboard = &fakeDashboard{results: []fakeDashboardResult{{modelKey: secret}, {quit: true}}}
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm-256color" }
	if exit := app.Run(t.Context(), nil); exit != ExitOK || !secrets.present || secrets.binding != modelsecret.Binding(store.value.Agent.Provider, store.value.Agent.BaseURL) {
		t.Fatalf("dashboard key save exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if secrets.value != secret || secrets.saves != 1 {
		t.Fatalf("dashboard secret store=%+v", secrets)
	}

	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"__agent-key-save", "--", "attacker-value"}); exit != ExitInput || secrets.saves != 1 || secrets.value != secret {
		t.Fatalf("external key save exit=%d saves=%d value=%q err=%q", exit, secrets.saves, secrets.value, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"model", "show"}); exit != ExitOK {
		t.Fatalf("show exit=%d err=%q", exit, errOut.String())
	}
	if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "已存入系统钥匙串") || !strings.Contains(out.String(), "上下文压缩：recent-only") || !strings.Contains(out.String(), "默认推理强度：high") || !strings.Contains(out.String(), "无响应超时：2m") || !strings.Contains(out.String(), "最大工具轮数：不限制") {
		t.Fatalf("secret leaked or status missing: %q", out.String())
	}
	deletesBefore := secrets.deletes
	out.Reset()
	if exit := app.Run(t.Context(), []string{"model", "key", "delete", "--confirmed"}); exit != ExitOK || secrets.present || secrets.deletes != deletesBefore+1 {
		t.Fatalf("delete exit=%d out=%q err=%q store=%+v", exit, out.String(), errOut.String(), secrets)
	}
}

func TestModelSetSupportsUnlimitedToolRoundsAndHasNoFixedMaximum(t *testing.T) {
	t.Parallel()
	store := &memoryConfigStore{}
	app, out, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})

	if exit := app.Run(t.Context(), []string{"model", "set", "--provider", "custom", "--max-tool-rounds", "0"}); exit != ExitOK {
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

func TestDashboardModelSetupPersistsBeforeAgentLaunch(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	terminal := &fakeTerminal{lines: []string{""}}
	dashboardRunner := &fakeDashboard{results: []fakeDashboardResult{
		{args: []string{"model", "set", "--provider", "custom", "--base-url", "http://127.0.0.1:1234/v1", "--model", "local-model", "--context-window", "32768", "--timeout", "1m", "--max-tool-rounds", "8"}},
		{args: []string{"agent"}},
		{quit: true},
	}}
	agentUI := &fakeAgentUI{}
	app, out, errOut := newTestApp(configStore, credentialStore, terminal)
	app.Dashboard = dashboardRunner
	app.AgentUI = agentUI
	app.ModelSecrets = &memoryModelSecretStore{}
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return fixedAgentModel{}, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }

	if exit := app.Run(t.Context(), nil); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if configStore.value.Agent == nil || configStore.value.Agent.Provider != "custom" || configStore.value.Agent.Model != "local-model" {
		t.Fatalf("persisted agent config=%+v", configStore.value.Agent)
	}
	if len(dashboardRunner.snapshots) != 3 || dashboardRunner.snapshots[1].AgentProvider != "custom" {
		t.Fatalf("dashboard snapshots=%+v", dashboardRunner.snapshots)
	}
	if agentUI.calls != 1 || agentUI.modelName != "local-model" {
		t.Fatalf("agent UI calls=%d model=%q", agentUI.calls, agentUI.modelName)
	}
	if terminal.readLineCalls != 1 {
		t.Fatalf("return prompt calls=%d, want only the model-save prompt", terminal.readLineCalls)
	}
}

func TestDashboardSnapshotReportsUnavailableModelCredentialBackend(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig("deepseek")
	preset.ReasoningEffort = "xhigh"
	configStore.value.Agent = &preset
	app, _, _ := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{loadErr: modelsecret.ErrUnavailable}

	snapshot := app.dashboardSnapshot()
	if snapshot.AgentKeyConfigured || !snapshot.AgentKeyBackendUnavailable || snapshot.AgentReasoningEffort != "xhigh" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestAgentLaunchRequiresTTYPairingAndModelCredential(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig("deepseek")
	configStore.value.Agent = &preset
	secrets := &memoryModelSecretStore{}
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = secrets
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm-256color" }
	app.AgentUI = &fakeAgentUI{}
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) {
		return fixedAgentModel{}, nil
	}

	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitAuth || !strings.Contains(errOut.String(), "model_key_unavailable") {
		t.Fatalf("missing-key exit=%d err=%q", exit, errOut.String())
	}

	secrets.value, secrets.binding, secrets.present = "provider-key", modelsecret.Binding(preset.Provider, preset.BaseURL), true
	runner := &fakeAgentUI{}
	app.AgentUI = runner
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK || runner.calls != 1 || runner.conversation == nil || runner.modelName != preset.Model {
		t.Fatalf("launch exit=%d calls=%d model=%q err=%q", exit, runner.calls, runner.modelName, errOut.String())
	}
	if _, err := runner.conversation.Send(t.Context(), "界面退出后不应继续追加"); !errors.Is(err, agentloop.ErrSessionClosed) {
		t.Fatalf("runAgent did not close session: %v", err)
	}

	app.OutputIsTTY = func() bool { return false }
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitInput || !strings.Contains(errOut.String(), "not_a_terminal") {
		t.Fatalf("non-tty exit=%d err=%q", exit, errOut.String())
	}
}

func TestAgentLaunchSelectsWorkspaceAndDegradesInvalidRoots(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig("ollama")
	configStore.value.Agent = &preset
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return fixedAgentModel{}, nil }
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
	preset := config.DefaultAgentConfig("ollama")
	preset.ContextWindow = 4096
	preset.ContextCompaction = "off"
	configStore.value.Agent = &preset
	model := &sequenceAgentModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: strings.Repeat("x", 20<<10)}}}}
	runner := &contextPassingAgentUI{}
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.AgentUI = runner
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
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

func TestOllamaAgentAllowsMissingAPIKeyAndModelTestIsBounded(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig("ollama")
	configStore.value.Agent = &preset
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{value: "legacy-cloud-key", binding: modelsecret.Binding("openai", config.DefaultAgentConfig("openai").BaseURL), present: true}
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }
	runner := &fakeAgentUI{}
	app.AgentUI = runner
	var receivedKey string
	app.NewModel = func(_ config.AgentConfig, key string) (agentloop.Model, error) {
		receivedKey = key
		return fixedAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "连接正常"}}}, nil
	}
	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK || receivedKey != "" || runner.calls != 1 {
		t.Fatalf("agent exit=%d key=%q calls=%d err=%q", exit, receivedKey, runner.calls, errOut.String())
	}
	out.Reset()
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitOK || !strings.Contains(out.String(), "模型连接正常") {
		t.Fatalf("test exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
}

func TestModelTestWorksBeforePairing(t *testing.T) {
	t.Parallel()
	preset := config.DefaultAgentConfig("ollama")
	store := &memoryConfigStore{value: config.Config{Timeout: "30s", Color: "never", Agent: &preset}, present: true}
	app, out, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) {
		return fixedAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "连接正常"}}}, nil
	}
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitOK || !strings.Contains(out.String(), "模型连接正常") {
		t.Fatalf("test exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
}

func TestModelCredentialsAreScopedToProviderEndpoint(t *testing.T) {
	t.Parallel()
	openAI := config.DefaultAgentConfig("openai")
	store := &memoryConfigStore{value: config.Config{Timeout: "30s", Color: "never", Agent: &openAI}, present: true}
	secrets := &memoryModelSecretStore{value: "openai-key", binding: modelsecret.Binding(openAI.Provider, openAI.BaseURL), present: true}
	app, out, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	app.ModelSecrets = secrets

	if exit := app.Run(t.Context(), []string{"model", "set", "--model", "gpt-4.1"}); exit != ExitOK || !secrets.present {
		t.Fatalf("model-only exit=%d secret=%+v out=%q err=%q", exit, secrets, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"model", "set", "--base-url", "https://gateway.example/v1"}); exit != ExitOK {
		t.Fatalf("base-url exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	app.NewModel = func(_ config.AgentConfig, _ string) (agentloop.Model, error) {
		t.Fatal("model constructor called with an unbound credential")
		return nil, nil
	}
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitAuth || !strings.Contains(errOut.String(), "model_key_unavailable") {
		t.Fatalf("unbound test exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}

	deepSeek := config.DefaultAgentConfig("deepseek")
	store.value.Agent = &deepSeek
	secrets.value, secrets.binding, secrets.present = "deepseek-key", modelsecret.Binding(deepSeek.Provider, deepSeek.BaseURL), true
	var receivedKey string
	app.NewModel = func(_ config.AgentConfig, key string) (agentloop.Model, error) {
		receivedKey = key
		return fixedAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "连接正常"}}}, nil
	}
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitOK || receivedKey != "deepseek-key" {
		t.Fatalf("bound test exit=%d key=%q out=%q err=%q", exit, receivedKey, out.String(), errOut.String())
	}
}

func TestModelSetAcceptsProviderAndRejectsUnknown(t *testing.T) {
	t.Parallel()
	store := &memoryConfigStore{}
	app, _, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	args := []string{"model", "set", "--provider", "custom", "--base-url", "http://127.0.0.1:9000/v1", "--model", "local", "--context-window", "8192", "--timeout", "45s", "--max-tool-rounds", "4"}
	if exit := app.Run(t.Context(), args); exit != ExitOK {
		t.Fatalf("set exit=%d err=%q", exit, errOut.String())
	}
	if store.value.Agent == nil || store.value.Agent.Provider != "custom" || store.value.Agent.BaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("agent config=%+v", store.value.Agent)
	}
	if exit := app.Run(t.Context(), []string{"model", "set", "--provider", "unknown"}); exit != ExitInput {
		t.Fatalf("unknown provider exit=%d", exit)
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
	preset := config.DefaultAgentConfig("ollama")
	preset.ReasoningEffort = "xhigh"
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	model := &requestCapturingAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "连接正常"}}}
	runner := &singleSendAgentUI{}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.AgentUI = runner
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
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

func TestModelTestPreservesUnsupportedReasoningCode(t *testing.T) {
	t.Parallel()
	preset := config.DefaultAgentConfig("ollama")
	preset.ReasoningEffort = "max"
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) {
		return fixedAgentModel{err: &modelclient.ClientError{
			Code: modelclient.ErrorCodeReasoningEffortUnsupported, Message: "reasoning_effort unsupported",
		}}, nil
	}

	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitUnavailable {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
	if !strings.Contains(errOut.String(), string(modelclient.ErrorCodeReasoningEffortUnsupported)) || !strings.Contains(errOut.String(), "auto或none") {
		t.Fatalf("stable recovery guidance missing: %q", errOut.String())
	}
}

func TestCustomModelAllowsNoAPIKey(t *testing.T) {
	t.Parallel()
	preset := config.DefaultAgentConfig("custom")
	preset.BaseURL = "http://127.0.0.1:9000/v1"
	preset.Model = "local-model"
	store := &memoryConfigStore{value: config.Config{Timeout: "30s", Color: "never", Agent: &preset}, present: true}
	app, out, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{loadErr: modelsecret.ErrUnavailable}
	var receivedKey string
	app.NewModel = func(_ config.AgentConfig, key string) (agentloop.Model, error) {
		receivedKey = key
		return fixedAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "连接正常"}}}, nil
	}
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitOK || receivedKey != "" || !strings.Contains(out.String(), "模型连接正常") {
		t.Fatalf("test exit=%d key=%q out=%q err=%q", exit, receivedKey, out.String(), errOut.String())
	}
}

func TestCustomRemoteModelFailsClosedWithoutAPIKey(t *testing.T) {
	t.Parallel()
	preset := config.DefaultAgentConfig("custom")
	preset.BaseURL = "https://model-gateway.example.test/v1"
	preset.Model = "remote-model"
	store := &memoryConfigStore{value: config.Config{Timeout: "30s", Color: "never", Agent: &preset}, present: true}
	app, _, errOut := newTestApp(store, &memoryCredentialStore{}, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{loadErr: modelsecret.ErrUnavailable}
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) {
		panic("remote custom endpoint must not start without an accessible key")
	}
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitAuth || !strings.Contains(errOut.String(), "model_key_unavailable") {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
}

func TestModelTestMapsProviderFailureWithoutLeakingDetails(t *testing.T) {
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig("openai")
	configStore.value.Agent = &preset
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{value: "provider-key", binding: modelsecret.Binding(preset.Provider, preset.BaseURL), present: true}
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) {
		return fixedAgentModel{err: errors.New("provider response contained sensitive detail")}, nil
	}
	if exit := app.Run(t.Context(), []string{"model", "test"}); exit != ExitUnavailable || !strings.Contains(errOut.String(), "model_unavailable") || strings.Contains(errOut.String(), "sensitive detail") {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
}
