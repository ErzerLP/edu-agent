package agentloop

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func Tools() []modelclient.Tool {
	return []modelclient.Tool{
		tool("search_knowledge", "从服务端知识库检索与用户问题相关的权威片段。", `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":2000,"description":"要检索的中文问题或主题"}},"required":["query"],"additionalProperties":false}`),
		tool("get_learning_progress", "读取当前学习会话、目标、状态和工作项；尚未开始学习时返回合法空状态。", emptySchema),
		tool("get_learning_route", "读取当前学习会话绑定的路线步骤；步骤较多时用offset继续读取。", `{"type":"object","properties":{"offset":{"type":"integer","minimum":0,"description":"从第几个路线步骤开始，默认0"}},"additionalProperties":false}`),
		tool("get_due_reviews", "读取当前已经到期的复习任务；响应有更多数据时用cursor继续读取。", cursorSchema),
		tool("list_long_term_preferences", "读取服务端已接纳的长期学习偏好、时间约束和个人背景；响应有更多数据时用cursor继续读取。", cursorSchema),
		tool("recall_session_memory", "按当前会话中的明确 opaque memory ID 回查被压缩证据；不支持关键词、query 或语义搜索。", `{"type":"object","properties":{"memory_id":{"type":"string","pattern":"^(obs_|ref_)[A-Za-z0-9_-]{16}$"}},"required":["memory_id"],"additionalProperties":false}`),
		tool("ask_user_question", "当继续学习确实需要用户在少量明确选项中做当前会话决定时暂停当前轮次并提问；支持single或multiple，客户端固定提供有界自定义回答。问题与选项必须简短，并遵守字段声明的终端显示宽度。不得询问密码、API Key、令牌、私钥、恢复码或助记词。普通问询答案只属于当前会话，不构成长期记忆、外部写入、删除或发布授权。", `{"type":"object","properties":{"question_id":{"type":"string","minLength":1,"maxLength":64},"header":{"type":"string","minLength":1,"maxLength":48,"description":"短标题，最多36个终端显示列"},"question":{"type":"string","minLength":1,"maxLength":160,"description":"问题文本，最多72个终端显示列"},"mode":{"type":"string","enum":["single","multiple"]},"options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":64},"label":{"type":"string","minLength":1,"maxLength":48,"description":"短选项标签，最多32个终端显示列"},"description":{"type":"string","minLength":1,"maxLength":120,"description":"一句简短说明，最多60个终端显示列"}},"required":["id","label","description"],"additionalProperties":false}}},"required":["question_id","header","question","mode","options"],"additionalProperties":false}`),
		tool("remember_preference", "保存一个长期偏好。仅在用户明确要求长期记住时调用；客户端会让用户选择长期保存、仅当前会话使用或拒绝。", `{"type":"object","properties":{"content":{"type":"string","description":"简洁、独立、可长期理解的偏好内容"},"reason":{"type":"string","description":"为什么该信息值得长期保存"},"category":{"type":"string","enum":["interaction_preference","time_constraint","personal_context"]},"sensitivity":{"type":"string","enum":["non_sensitive","sensitive"]},"stability":{"type":"string","enum":["stable","transient"]}},"required":["content","reason","category","sensitivity","stability"],"additionalProperties":false}`),
	}
}

const (
	emptySchema  = `{"type":"object","properties":{},"additionalProperties":false}`
	cursorSchema = `{"type":"object","properties":{"cursor":{"type":"string","minLength":1,"maxLength":4096,"description":"上一页返回的next_cursor；首页省略"}},"additionalProperties":false}`
)

func tool(name, description, schema string) modelclient.Tool {
	return modelclient.Tool{Type: "function", Function: modelclient.ToolDefinition{
		Name: name, Description: description, Parameters: json.RawMessage(schema),
	}}
}
