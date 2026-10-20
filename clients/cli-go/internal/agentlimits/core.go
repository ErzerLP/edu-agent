// Package agentlimits 保留 CLI 的资源配置入口，数值与校验共享同一实现。
package agentlimits

import core "github.com/edu-agent/edu-agent/packages/agentcore/agentlimits"

const (
	DefaultContextWindow          = core.DefaultContextWindow
	MaxOutputTokens               = core.MaxOutputTokens
	MaxAssistantTextBytes         = core.MaxAssistantTextBytes
	MaxToolCallArgumentsBytes     = core.MaxToolCallArgumentsBytes
	MaxFileMutationArgumentsBytes = core.MaxFileMutationArgumentsBytes
	MaxToolCallArgumentsTotal     = core.MaxToolCallArgumentsTotal
)

func ValidMaxTokens(value int) bool      { return core.ValidMaxTokens(value) }
func ToolArgumentsBytes(name string) int { return core.ToolArgumentsBytes(name) }
