package agentloop

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

// Keep the added schema's fixed cost visible at the supported small-window
// boundary. The existing four-call test remains the behavioral budget gate.
func TestStatSchemaFixedBudgetCost(t *testing.T) {
	estimator := NewTokenEstimator()
	all := compactToolProse(append(Tools(), workspace.Definitions()...))
	withoutStat := make([]modelclient.Tool, 0, len(all)-1)
	for _, tool := range all {
		if tool.Function.Name != workspace.ToolStat {
			withoutStat = append(withoutStat, tool)
		}
	}
	messages := []modelclient.Message{{Role: "system", Content: systemPrompt}, {Role: "system", Content: workspaceSystemPrompt}}
	current := estimator.EstimateRequest(modelclient.Request{Messages: messages, Tools: all})
	without := estimator.EstimateRequest(modelclient.Request{Messages: messages, Tools: withoutStat})
	messages[1].Content = strings.Replace(workspaceSystemPrompt, "stat:metadata version, not content/subtree; hash=true:raw SHA256<=1MiB. ", "", 1)
	baseline := estimator.EstimateRequest(modelclient.Request{Messages: messages, Tools: withoutStat})
	maximumInput := 4096 - divideRoundUp(4096*5, 100) - 512
	t.Logf("4096-window maximumInput=%d fixed=%d without-stat fixed=%d stat schema delta=%d stat prompt delta=%d current-turn capacity=%d", maximumInput, current, baseline, current-without, without-baseline, maximumInput-current)
	if current > maximumInput {
		t.Fatal("stat schema alone exceeds fixed input budget")
	}
}
