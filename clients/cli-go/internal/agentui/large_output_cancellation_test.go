package agentui

import "testing"

func TestLargeOutputEscapeKeepsVisibleWorkAndRejectsLateEvents(t *testing.T) {
	TestAgentUIEscapeStopsTurnPreservesVisibleWorkAndRejectsLateEvents(t)
}
