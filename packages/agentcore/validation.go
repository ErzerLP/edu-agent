package agentcore

import (
	"errors"

	"github.com/edu-agent/edu-agent/packages/agentcore/agentlimits"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

// ValidateModelMessage 保留原 CLI 消息资源边界；具体 JSON 参数由工具适配器严格解码。
func ValidateModelMessage(message modelclient.Message) error {
	textLimit := 64 << 10
	if message.Role == "assistant" {
		textLimit = agentlimits.MaxAssistantTextBytes
	}
	if len(message.Content) > textLimit {
		return errors.New("模型回答超过客户端安全上限")
	}
	totalArguments := 0
	seenCallIDs := make(map[string]struct{}, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if call.ID == "" || call.Type != "function" || call.Function.Name == "" {
			return errors.New("模型工具调用身份无效")
		}
		if _, duplicate := seenCallIDs[call.ID]; duplicate {
			return errors.New("模型工具调用ID重复")
		}
		seenCallIDs[call.ID] = struct{}{}
		if len(call.Function.Arguments) > agentlimits.ToolArgumentsBytes(call.Function.Name) {
			return errors.New("模型工具参数超过客户端安全上限")
		}
		totalArguments += len(call.Function.Arguments)
	}
	if totalArguments > agentlimits.MaxToolCallArgumentsTotal {
		return errors.New("模型单轮工具参数总量超过安全上限")
	}
	return nil
}
