package agentlimits

import "testing"

func TestLargeContextLimits(t *testing.T) {
	if DefaultContextWindow != 272000 || MaxOutputTokens != 128000 || MaxAssistantTextBytes != 1<<20 {
		t.Fatal("default contract changed")
	}
	for _, value := range []int{-1, 0, 128001} {
		if ValidMaxTokens(value) {
			t.Fatalf("accepted %d", value)
		}
	}
	for _, value := range []int{1, 512, 64000, 128000} {
		if !ValidMaxTokens(value) {
			t.Fatalf("rejected %d", value)
		}
	}
}
