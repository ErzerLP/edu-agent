package agentloop

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func Tools() []modelclient.Tool {
	return []modelclient.Tool{
		tool("search_knowledge", "从服务端知识库检索与用户问题相关的权威片段。", `{"type":"object","properties":{"query":{"type":"string","description":"要检索的中文问题或主题"}},"required":["query"],"additionalProperties":false}`),
		tool("get_learning_progress", "读取当前学习会话、目标、状态和工作项。", emptySchema),
		tool("get_learning_route", "读取当前学习路线及步骤。", emptySchema),
		tool("get_due_reviews", "读取当前已经到期的复习任务。", emptySchema),
		tool("list_long_term_preferences", "读取服务端已接纳的长期学习偏好、时间约束和个人背景。", emptySchema),
		tool("remember_preference", "提交一个长期偏好候选。仅在用户明确要求长期记住时调用；客户端会在执行前要求用户确认。", `{"type":"object","properties":{"content":{"type":"string","description":"简洁、独立、可长期理解的偏好内容"},"reason":{"type":"string","description":"为什么该信息值得长期保存"},"category":{"type":"string","enum":["interaction_preference","time_constraint","personal_context"]},"sensitivity":{"type":"string","enum":["non_sensitive","sensitive"]},"stability":{"type":"string","enum":["stable","transient"]}},"required":["content","reason","category","sensitivity","stability"],"additionalProperties":false}`),
	}
}

const emptySchema = `{"type":"object","properties":{},"additionalProperties":false}`

func tool(name, description, schema string) modelclient.Tool {
	return modelclient.Tool{Type: "function", Function: modelclient.ToolDefinition{
		Name: name, Description: description, Parameters: json.RawMessage(schema),
	}}
}
