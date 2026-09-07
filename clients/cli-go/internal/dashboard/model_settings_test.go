package dashboard

import (
	"strings"
	"testing"
)

func TestModelSettingsFormForwardsUncappedValues(t *testing.T) {
	value := newModel(Snapshot{})
	value.agentProviderDraft = "ollama"
	value.open(screenAgentConfig)
	value.inputs[2].SetValue("2000000")
	value.inputs[4].SetValue("30m")
	value.inputs[6].SetValue("256000")
	if strings.Contains(strings.Join(value.inputLabels, " "), "1-128000") {
		t.Fatal("form still advertises the old cap")
	}
	updated, _ := value.Update(key("enter"))
	command := strings.Join(updated.(model).command, " ")
	for _, wanted := range []string{"--context-window 2000000", "--timeout 30m", "--max-tokens 256000"} {
		if !strings.Contains(command, wanted) {
			t.Fatalf("form clamped %s: %s", wanted, command)
		}
	}
}
