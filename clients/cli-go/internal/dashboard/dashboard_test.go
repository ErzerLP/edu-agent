package dashboard

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestMainMenuCommandsAndShortcuts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want []string
	}{
		{key: "l", want: []string{"learn"}},
		{key: "v", want: []string{"progress"}},
		{key: "r", want: []string{"route"}},
		{key: "e", want: []string{"evidence"}},
		{key: "w", want: []string{"reviews"}},
		{key: "d", want: []string{"device", "status"}},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			updated, _ := newModel(Snapshot{LocalState: LocalStatePaired}).Update(key(test.key))
			got := updated.(model)
			if !reflect.DeepEqual(got.command, test.want) {
				t.Fatalf("command = %#v, want %#v", got.command, test.want)
			}
		})
	}
}

func TestAgentShortcutRequiresCompleteLocalModelSetup(t *testing.T) {
	t.Parallel()

	unconfigured := newModel(Snapshot{LocalState: LocalStatePaired})
	if view := unconfigured.View(); !strings.Contains(view, "配置AI助手") || strings.Contains(view, "AI学习助手") {
		t.Fatalf("unconfigured main menu = %q", view)
	}
	updated, command := unconfigured.Update(key("a"))
	setup := updated.(model)
	if command != nil || len(setup.command) != 0 || setup.screen != screenAgentProvider {
		t.Fatalf("unconfigured shortcut = screen %d command %#v tea command %v", setup.screen, setup.command, command)
	}
	if !strings.Contains(setup.View(), "只有在表单中按Enter保存，配置才会生效") {
		t.Fatalf("provider save guidance missing: %q", setup.View())
	}

	missingKey := Snapshot{
		LocalState: LocalStatePaired, AgentProvider: "deepseek", AgentBaseURL: "https://api.deepseek.com/v1",
		AgentModel: "deepseek-chat", AgentContextWindow: 32768, AgentTimeout: "90s", AgentMaxToolRounds: 6,
	}
	updated, command = newModel(missingKey).Update(key("a"))
	keyForm := updated.(model)
	if command != nil || len(keyForm.command) != 0 || keyForm.screen != screenAgentKey {
		t.Fatalf("missing-key shortcut = screen %d command %#v tea command %v", keyForm.screen, keyForm.command, command)
	}

	missingKey.AgentKeyBackendUnavailable = true
	unavailable := newModel(missingKey)
	if view := unavailable.View(); !strings.Contains(view, "修复AI助手配置") {
		t.Fatalf("unavailable-key main menu = %q", view)
	}
	updated, command = unavailable.Update(key("a"))
	settings := updated.(model)
	if command != nil || len(settings.command) != 0 || settings.screen != screenAgentSettings || !strings.Contains(settings.View(), "系统钥匙串不可用") {
		t.Fatalf("unavailable-key shortcut = screen %d command %#v view %q", settings.screen, settings.command, settings.View())
	}

	ollama := Snapshot{
		LocalState: LocalStatePaired, AgentProvider: "ollama", AgentBaseURL: "http://127.0.0.1:11434/v1",
		AgentModel: "qwen2.5:7b", AgentContextWindow: 32768, AgentTimeout: "90s", AgentMaxToolRounds: 6,
	}
	updated, _ = newModel(ollama).Update(key("a"))
	if got := updated.(model).command; !reflect.DeepEqual(got, []string{"agent"}) {
		t.Fatalf("optional-key agent command = %#v", got)
	}
}

func TestMenuNavigationAndFormsReturnExistingArgv(t *testing.T) {
	t.Parallel()
	modelValue := newModel(Snapshot{})
	updated, _ := modelValue.Update(key("j"))
	if got := updated.(model).cursor; got != 1 {
		t.Fatalf("j cursor = %d, want 1", got)
	}
	updated, _ = updated.(model).Update(key("k"))
	if got := updated.(model).cursor; got != 0 {
		t.Fatalf("k cursor = %d, want 0", got)
	}

	updated, _ = newModel(Snapshot{LocalState: LocalStatePaired}).Update(key("g"))
	goal := updated.(model)
	goal.inputs[0].SetValue("Learn graph theory")
	updated, _ = goal.Update(key("enter"))
	if got := updated.(model).command; !reflect.DeepEqual(got, []string{"goal", "set", "--", "Learn graph theory"}) {
		t.Fatalf("goal command = %#v", got)
	}

	updated, _ = newModel(Snapshot{LocalState: LocalStatePaired}).Update(key("i"))
	importModel := updated.(model)
	importModel.inputs[0].SetValue("notes/course")
	updated, _ = importModel.Update(key("enter"))
	if got := updated.(model).command; !reflect.DeepEqual(got, []string{"knowledge", "import", "--", "notes/course"}) {
		t.Fatalf("import command = %#v", got)
	}
}

func TestSettingsNeverContainCredentialValueAndRePairIsExplicit(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{ServerURL: "https://example.test", Timeout: "12s", Color: "auto", DeviceName: "Laptop", LocalState: LocalStatePaired}
	modelValue := newModel(snapshot)
	updated, _ := modelValue.Update(key("s"))
	settings := updated.(model)
	view := settings.View()
	if settings.screen != screenSettings || !containsAll(view, "https://example.test", "12s", "输出颜色：自动", "本地状态：已配对") {
		t.Fatalf("settings view = %q", view)
	}
	updated, _ = settings.Update(key("p"))
	confirm := updated.(model)
	if confirm.screen != screenRePair || len(confirm.command) != 0 || !containsAll(confirm.View(), "安全注销旧设备", "仅清除本地配对", "旧服务器不可用") {
		t.Fatalf("re-pair state=%d command=%#v view=%q", confirm.screen, confirm.command, confirm.View())
	}
	updated, _ = confirm.Update(key("enter"))
	confirm = updated.(model)
	if confirm.screen != screenRePair || len(confirm.command) != 0 {
		t.Fatalf("enter unexpectedly confirmed re-pair: state=%d command=%#v", confirm.screen, confirm.command)
	}
	updated, _ = confirm.Update(key("y"))
	if got := updated.(model).command; !reflect.DeepEqual(got, []string{"logout"}) {
		t.Fatalf("re-pair command = %#v", got)
	}

	updated, _ = newModel(snapshot).Update(key("s"))
	updated, _ = updated.(model).Update(key("p"))
	updated, _ = updated.(model).Update(key("f"))
	if got := updated.(model).command; !reflect.DeepEqual(got, []string{"device", "forget-local"}) {
		t.Fatalf("local recovery command = %#v", got)
	}
}

func TestDashboardRePairActionsFitMinimumTerminal(t *testing.T) {
	t.Parallel()
	base := newModel(Snapshot{LocalState: LocalStatePaired})
	updated, _ := base.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	updated, _ = updated.(model).Update(key("s"))
	updated, _ = updated.(model).Update(key("p"))
	view := updated.(model).View()
	if lines := strings.Count(view, "\n") + 1; lines > minimumHeight {
		t.Fatalf("re-pair view uses %d lines at minimum height %d:\n%s", lines, minimumHeight, view)
	}
	for _, line := range strings.Split(view, "\n") {
		if width := lipgloss.Width(line); width > minimumWidth {
			t.Fatalf("re-pair line width = %d, want <= %d: %q", width, minimumWidth, line)
		}
	}
	for _, want := range []string{"[Y]", "[F]", "[N]"} {
		if !strings.Contains(view, want) {
			t.Fatalf("re-pair action %q missing at minimum terminal:\n%s", want, view)
		}
	}
}

func TestPairedSettingsReturnPreferenceCommands(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{Timeout: "12s", Color: "never", LocalState: LocalStatePaired}

	updated, _ := newModel(snapshot).Update(key("s"))
	updated, _ = updated.(model).Update(key("t"))
	timeout := updated.(model)
	timeout.inputs[0].SetValue("45s")
	updated, _ = timeout.Update(key("enter"))
	if got, want := updated.(model).command, []string{"config", "set", "--timeout", "45s"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("timeout command = %#v, want %#v", got, want)
	}

	updated, _ = newModel(snapshot).Update(key("s"))
	updated, _ = updated.(model).Update(key("c"))
	updated, _ = updated.(model).Update(key("a"))
	if got, want := updated.(model).command, []string{"config", "set", "--color", "auto"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("color command = %#v, want %#v", got, want)
	}
}

func TestLocalPreferenceCommandsWorkBeforePairing(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{Timeout: "12s", Color: "never", LocalState: LocalStateUnpaired}

	updated, _ := newModel(snapshot).Update(key("s"))
	settings := updated.(model)
	if view := settings.View(); !containsAll(view, "客户端请求超时", "命令输出颜色") || strings.Contains(view, "设备与服务状态") {
		t.Fatalf("unpaired settings=%q", view)
	}
	updated, _ = settings.Update(key("t"))
	timeout := updated.(model)
	timeout.inputs[0].SetValue("45s")
	updated, _ = timeout.Update(key("enter"))
	if got, want := updated.(model).command, []string{"config", "set", "--timeout", "45s"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("timeout command = %#v, want %#v", got, want)
	}
}

func TestIncompleteLocalStateOffersRecovery(t *testing.T) {
	t.Parallel()
	modelValue := newModel(Snapshot{LocalState: LocalStateIncomplete})
	if view := modelValue.View(); !containsAll(view, "修复本地配对状态", "配置AI助手", "退出") || containsAll(view, "继续结构化学习") {
		t.Fatalf("incomplete main view = %q", view)
	}
	items := modelValue.items()
	if len(items) != 3 {
		t.Fatalf("incomplete items = %#v", items)
	}
	for _, item := range items {
		if item.key == "s" || item.title == "设置" {
			t.Fatalf("incomplete main menu contains redundant settings item: %#v", items)
		}
	}
	for _, blockedKey := range []string{"s", "l", "i", "g", "v", "r", "e", "w", "d"} {
		updated, _ := modelValue.Update(key(blockedKey))
		got := updated.(model)
		if len(got.command) != 0 || got.screen != screenMain || got.quit {
			t.Fatalf("incomplete key %q dispatched state=%d command=%#v quit=%t", blockedKey, got.screen, got.command, got.quit)
		}
	}

	updated, _ := modelValue.Update(key("p"))
	if got, want := updated.(model).command, []string{"device", "forget-local"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("main recovery command = %#v, want %#v", got, want)
	}
}

func TestNarrowTerminalKeepsEveryRenderedLineWithinWidth(t *testing.T) {
	t.Parallel()
	updated, _ := newModel(Snapshot{}).Update(tea.WindowSizeMsg{Width: 24, Height: 20})
	view := updated.(model).View()
	for index, line := range strings.Split(view, "\n") {
		if width := lipgloss.Width(line); width > 24 {
			t.Fatalf("line %d width=%d: %q", index, width, line)
		}
	}
	if strings.Contains(view, "恢复服务端教学状态机中的当前会话") {
		t.Fatal("narrow terminal rendered optional descriptions")
	}
}

func TestSmallTerminalShowsBoundedResizeStateAndIgnoresActions(t *testing.T) {
	t.Parallel()
	updated, _ := newModel(Snapshot{}).Update(tea.WindowSizeMsg{Width: 8, Height: 2})
	value := updated.(model)
	view := value.View()
	lines := strings.Split(view, "\n")
	if len(lines) > 2 {
		t.Fatalf("rendered %d lines for height 2: %q", len(lines), view)
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width > 8 {
			t.Fatalf("line %d width=%d: %q", index, width, line)
		}
	}
	updated, _ = value.Update(key("enter"))
	if got := updated.(model).command; len(got) != 0 {
		t.Fatalf("small terminal dispatched command %#v", got)
	}
}

func TestFormsProtectLeadingDashValuesFromFlagParsing(t *testing.T) {
	t.Parallel()
	updated, _ := newModel(Snapshot{LocalState: LocalStatePaired}).Update(key("g"))
	goal := updated.(model)
	goal.inputs[0].SetValue("- learn algebra")
	updated, _ = goal.Update(key("enter"))
	if got, want := updated.(model).command, []string{"goal", "set", "--", "- learn algebra"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("goal command = %#v, want %#v", got, want)
	}

	updated, _ = newModel(Snapshot{LocalState: LocalStatePaired}).Update(key("i"))
	knowledge := updated.(model)
	knowledge.inputs[0].SetValue("-notes.md")
	updated, _ = knowledge.Update(key("enter"))
	if got, want := updated.(model).command, []string{"knowledge", "import", "--", "-notes.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("import command = %#v, want %#v", got, want)
	}
}

func TestAgentSettingsCommandsHideSecretsAndRequireConfirmation(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{
		LocalState: LocalStatePaired, AgentProvider: "deepseek", AgentBaseURL: "https://api.deepseek.com/v1", AgentModel: "deepseek-chat",
		AgentContextWindow: 32768, AgentContextCompaction: "auto", AgentTimeout: "90s", AgentMaxToolRounds: 6, AgentKeyConfigured: true,
	}
	updated, _ := newModel(snapshot).Update(key("s"))
	updated, _ = updated.(model).Update(key("a"))
	settings := updated.(model)
	if settings.screen != screenAgentSettings || !containsAll(settings.View(), "AI助手与模型", "DeepSeek", "上下文压缩：auto", "API Key：已存入系统钥匙串") {
		t.Fatalf("agent settings=%q", settings.View())
	}

	updated, _ = settings.Update(key("m"))
	form := updated.(model)
	form.inputs[0].SetValue("https://model.example/v1")
	form.inputs[1].SetValue("teacher-model")
	form.inputs[2].SetValue("65536")
	form.inputs[3].SetValue("recent-only")
	form.inputs[4].SetValue("2m")
	form.inputs[5].SetValue("8")
	updated, _ = form.Update(key("enter"))
	want := []string{"model", "set", "--provider", "deepseek", "--base-url", "https://model.example/v1", "--model", "teacher-model", "--context-window", "65536", "--context-compaction", "recent-only", "--timeout", "2m", "--max-tool-rounds", "8"}
	if got := updated.(model).command; !reflect.DeepEqual(got, want) {
		t.Fatalf("model command=%#v want=%#v", got, want)
	}

	updated, _ = newModel(snapshot).Update(key("s"))
	updated, _ = updated.(model).Update(key("a"))
	updated, _ = updated.(model).Update(key("u"))
	keyForm := updated.(model)
	keyForm.inputs[0].SetValue("secret-value")
	if strings.Contains(keyForm.View(), "secret-value") {
		t.Fatal("API key rendered in clear text")
	}
	updated, _ = keyForm.Update(key("enter"))
	keyAction := updated.(model)
	if keyAction.modelKey != "secret-value" || len(keyAction.command) != 0 {
		t.Fatalf("key action exposed command=%#v modelKeyPresent=%t", keyAction.command, keyAction.modelKey != "")
	}

	updated, _ = newModel(snapshot).Update(key("s"))
	updated, _ = updated.(model).Update(key("a"))
	updated, _ = updated.(model).Update(key("x"))
	confirm := updated.(model)
	updated, _ = confirm.Update(key("enter"))
	if got := updated.(model).command; len(got) != 0 {
		t.Fatalf("enter confirmed destructive action: %#v", got)
	}
	updated, _ = confirm.Update(key("y"))
	if got, want := updated.(model).command, []string{"model", "key", "delete", "--confirmed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delete command=%#v want=%#v", got, want)
	}
}

func TestAgentProviderSelectionContinuesIntoPrefilledForm(t *testing.T) {
	t.Parallel()
	updated, _ := newModel(Snapshot{LocalState: LocalStateUnpaired}).Update(key("a"))
	updated, _ = updated.(model).Update(key("p"))
	updated, command := updated.(model).Update(key("c"))
	form := updated.(model)
	if command != nil || form.screen != screenAgentConfig || form.agentProviderDraft != "custom" || form.snapshot.AgentProvider != "" {
		t.Fatalf("provider selection = screen %d draft %q persisted %q command %v", form.screen, form.agentProviderDraft, form.snapshot.AgentProvider, command)
	}
	if got := form.inputs[0].Value(); got != "http://127.0.0.1:1234/v1" {
		t.Fatalf("custom base URL = %q", got)
	}
	updated, _ = form.Update(key("enter"))
	got := updated.(model).command
	if len(got) < 4 || !reflect.DeepEqual(got[:4], []string{"model", "set", "--provider", "custom"}) {
		t.Fatalf("custom model command = %#v", got)
	}
}

func TestAgentProviderDraftIsDiscardedOnCancel(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{
		LocalState: LocalStatePaired, AgentProvider: "deepseek", AgentBaseURL: "https://api.deepseek.com/v1", AgentModel: "deepseek-chat",
		AgentContextWindow: 32768, AgentTimeout: "90s", AgentMaxToolRounds: 6, AgentKeyConfigured: true,
	}
	updated, _ := newModel(snapshot).Update(key("s"))
	updated, _ = updated.(model).Update(key("a"))
	updated, _ = updated.(model).Update(key("p"))
	updated, _ = updated.(model).Update(key("c"))
	form := updated.(model)
	if form.agentProviderDraft != "custom" || form.snapshot.AgentProvider != "deepseek" {
		t.Fatalf("draft=%q persisted=%q", form.agentProviderDraft, form.snapshot.AgentProvider)
	}
	updated, _ = form.Update(key("esc"))
	settings := updated.(model)
	if settings.screen != screenAgentSettings || settings.agentProviderDraft != "" || settings.snapshot.AgentProvider != "deepseek" || !strings.Contains(settings.View(), "DeepSeek") {
		t.Fatalf("cancelled provider draft leaked: screen=%d draft=%q persisted=%q view=%q", settings.screen, settings.agentProviderDraft, settings.snapshot.AgentProvider, settings.View())
	}
	updated, _ = settings.Update(key("t"))
	if got := updated.(model).command; !reflect.DeepEqual(got, []string{"model", "test"}) {
		t.Fatalf("model test command after cancel = %#v", got)
	}
}

func TestUnpairedMenuOffersPairingAndLocalModelSettingsOnly(t *testing.T) {
	t.Parallel()
	value := newModel(Snapshot{})
	view := value.View()
	if !containsAll(view, "配对设备", "配置AI助手", "退出") || strings.Contains(view, "AI学习助手") {
		t.Fatalf("unpaired view=%q", view)
	}
	for _, blockedKey := range []string{"l", "i", "g", "v", "r", "e", "w", "d"} {
		updated, _ := value.Update(key(blockedKey))
		if got := updated.(model); len(got.command) != 0 || got.quit {
			t.Fatalf("unpaired key %q dispatched %#v", blockedKey, got.command)
		}
	}
}

func TestUnpairedConnectionFormUsesPairFlags(t *testing.T) {
	t.Parallel()
	updated, _ := newModel(Snapshot{}).Update(key("p"))
	connection := updated.(model)
	if connection.screen != screenConnection {
		t.Fatalf("screen = %d", connection.screen)
	}
	connection.inputs[0].SetValue("https://agent.example")
	connection.inputs[1].SetValue("45s")
	updated, _ = connection.Update(key("enter"))
	want := []string{"pair", "--server", "https://agent.example", "--timeout", "45s"}
	if got := updated.(model).command; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}

func key(value string) tea.KeyMsg {
	if len(value) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
	}
	if value == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	if value == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
