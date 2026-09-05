package dashboard

import (
	"strings"
	"testing"
)

func TestLargeContextSettingsFormAndDisplay(t *testing.T) {
	value := newModel(Snapshot{AgentProvider: "ollama", AgentContextWindow: 272000, AgentMaxTokens: 128000})
	value.open(screenAgentSettings)
	if !strings.Contains(value.View(), "272000") || !strings.Contains(value.View(), "128000") {
		t.Fatalf("view=%s", value.View())
	}
	value.agentProviderDraft = "ollama"
	value.open(screenAgentConfig)
	if len(value.inputs) != 7 || value.inputs[2].Value() != "272000" || value.inputs[6].Value() != "128000" {
		t.Fatal("wrong new form defaults")
	}
	value.inputs[6].SetValue("64000")
	updated, _ := value.Update(key("enter"))
	command := strings.Join(updated.(model).command, " ")
	if !strings.Contains(command, "--max-tokens 64000") || !strings.Contains(command, "--context-window 272000") {
		t.Fatalf("command=%s", command)
	}
}
