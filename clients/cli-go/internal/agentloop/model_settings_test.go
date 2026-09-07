package agentloop

import (
	"math/big"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestModelSettingsLargeBudgetArithmetic(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, value := range []int{0, 1, 99, 100, 101, 4096, 272000, 2000000, maxInt - 1, maxInt} {
		for percent := 0; percent <= 100; percent++ {
			want := new(big.Int).Mul(big.NewInt(int64(value)), big.NewInt(int64(percent)))
			want.Add(want, big.NewInt(99)).Div(want, big.NewInt(100))
			if got := percentRoundUp(value, percent); int64(got) != want.Int64() {
				t.Fatalf("ceil(%d * %d%%)=%d want=%s", value, percent, got, want)
			}
		}
	}
	if divideRoundUp(maxInt, 100) != maxInt/100+1 {
		t.Fatal("integer rounding overflowed")
	}
	for _, mode := range []string{ContextCompactionAuto, ContextCompactionRecentOnly, ContextCompactionOff} {
		for _, window := range []int{2000000, maxInt} {
			for _, output := range []int{256000, maxInt} {
				planner := ContextPlanner{ContextWindow: window, MaxTokens: output, Mode: mode, Estimator: NewTokenEstimator()}
				plan, err := planner.Plan([]modelclient.Message{{Role: "user", Content: "hello"}}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				// Subtractions also keep the assertion free of overflow.
				if plan.SafetyMargin != percentRoundUp(window, 5) || plan.ReservedOutput <= 0 || plan.ReservedOutput > output || plan.EstimatedInput > window-plan.SafetyMargin-plan.ReservedOutput {
					t.Fatalf("invalid budget: mode=%s window=%d output=%d plan=%+v", mode, window, output, plan)
				}
				if window == 2000000 && output == 256000 && plan.Request.MaxTokens != output {
					t.Fatal("output was clamped to the old ceiling")
				}
			}
		}
	}
	m := &fakeModel{}
	session, err := New(m, &fakeServer{}, Options{ContextWindow: maxInt, MaxTokens: maxInt, NewUUID: func() (string, error) { return "10000000-0000-4000-8000-000000000001", nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	if session.hotRawTokenLimit != percentRoundUp(maxInt, 55) || session.currentToolResultBudget != 2048 || session.contextRuntime.observeAfterTokens != 8000 || session.contextRuntime.observerChunkTokens != 8192 {
		t.Fatal("runtime percentage-derived budgets overflowed")
	}
}
