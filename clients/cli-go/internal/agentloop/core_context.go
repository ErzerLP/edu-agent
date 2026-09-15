package agentloop

import (
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	core "github.com/edu-agent/edu-agent/packages/agentcore"
)

// 使用类型别名保持检查点、界面和内部测试的协议兼容。
type TokenEstimator = core.TokenEstimator
type ConservativeTokenEstimator = core.ConservativeTokenEstimator
type ContextPlanner = core.ContextPlanner

func NewTokenEstimator() *ConservativeTokenEstimator { return core.NewTokenEstimator() }
func divideRoundUp(value, divisor int) int           { return core.DivideRoundUp(value, divisor) }
func percentRoundUp(value, percent int) int          { return core.PercentRoundUp(value, percent) }
func clampInt(value, minimum, maximum int) int       { return core.ClampInt(value, minimum, maximum) }
func splitSystemMessages(messages []modelclient.Message) ([]modelclient.Message, []modelclient.Message) {
	return core.SplitSystemMessages(messages)
}
