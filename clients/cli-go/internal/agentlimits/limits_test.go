package agentlimits

import "testing"

func TestValidToolRoundsAllowsUnlimitedAndHasNoFixedMaximum(t *testing.T) {
	t.Parallel()
	for _, value := range []int{0, 1, 60, 1_000_000} {
		if !ValidToolRounds(value) {
			t.Fatalf("ValidToolRounds(%d)=false", value)
		}
	}
	if ValidToolRounds(-1) {
		t.Fatal("ValidToolRounds(-1)=true")
	}
}
