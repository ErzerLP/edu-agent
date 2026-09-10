package agentloop

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func Tools() []modelclient.Tool {
	return []modelclient.Tool{
		tool("search_knowledge", "从服务端知识库检索与用户问题相关的权威片段。", `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":2000,"description":"要检索的中文问题或主题"}},"required":["query"],"additionalProperties":false}`),
		tool("get_learning_progress", "读取绑定学习区及可选目标的权威进度，使用 cursor 分页；不读取全局或选择最近会话。", progressSchema),
		tool("get_learning_route", "读取当前学习会话绑定的路线步骤；步骤较多时用offset继续读取。", `{"type":"object","properties":{"offset":{"type":"integer","minimum":0,"description":"从第几个路线步骤开始，默认0"}},"additionalProperties":false}`),
		tool("get_due_reviews", "读取绑定学习区及可选目标的到期复习，保留真实任务身份与来源。", progressSchema),
		tool("list_long_term_preferences", "读取服务端已接纳的长期学习偏好、时间约束和个人背景；响应有更多数据时用cursor继续读取。", cursorSchema),
		tool("recall_session_memory", "按当前会话中的明确 opaque memory ID 回查被压缩证据；不支持关键词、query 或语义搜索。", `{"type":"object","properties":{"memory_id":{"type":"string","pattern":"^(obs_|ref_)[A-Za-z0-9_-]{16}$"}},"required":["memory_id"],"additionalProperties":false}`),
		tool("ask_user_question", "必要会话决定时暂停提问；single/multiple，客户端固定有界自定义回答。问题/选项简短，遵守字段终端显示宽度；禁问秘密。普通问询答案仅属当前会话，不构成长期记忆、外部写入、删除或发布授权。", `{"type":"object","properties":{"question_id":{"type":"string","minLength":1,"maxLength":64},"header":{"type":"string","minLength":1,"maxLength":48,"description":"短标题，最多36个终端显示列"},"question":{"type":"string","minLength":1,"maxLength":160,"description":"问题文本，最多72个终端显示列"},"mode":{"type":"string","enum":["single","multiple"]},"options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":64},"label":{"type":"string","minLength":1,"maxLength":48,"description":"短选项标签，最多32个终端显示列"},"description":{"type":"string","minLength":1,"maxLength":120,"description":"一句简短说明，最多60个终端显示列"}},"required":["id","label","description"],"additionalProperties":false}}},"required":["question_id","header","question","mode","options"],"additionalProperties":false}`),
		tool("remember_preference", "保存一个长期偏好。仅在用户明确要求长期记住时调用；客户端会让用户选择长期保存、仅当前会话使用或拒绝。", `{"type":"object","properties":{"content":{"type":"string","description":"简洁、独立、可长期理解的偏好内容"},"reason":{"type":"string","description":"为什么该信息值得长期保存"},"category":{"type":"string","enum":["interaction_preference","time_constraint","personal_context"]},"sensitivity":{"type":"string","enum":["non_sensitive","sensitive"]},"stability":{"type":"string","enum":["stable","transient"]}},"required":["content","reason","category","sensitivity","stability"],"additionalProperties":false}`),
	}
}

func (s *Session) tools() []modelclient.Tool {
	result := Tools()
	if _, ok := s.server.(*agentcontext.Client); ok {
		// 将学习查询合并为一个入口，为 4096 窗口保留完整本地工具和结果预算。
		result = append([]modelclient.Tool{
			tool("learning_context", "读取绑定学习区与可选目标的正式上下文：goals 列表、goal 详情、plans 规划、progress 进度、reviews 到期复习、route 指定教学路线、search 冻结资料检索。cursor 分页，query 用于 search，offset 用于 route。无明确目标时不能选最近目标，使用正式流程选择。", `{"type":"object","properties":{"view":{"enum":["goals","goal","plans","progress","reviews","route","search"]},"cursor":{"type":"string"},"query":{"type":"string"},"offset":{"type":"integer","minimum":0}},"required":["view"],"additionalProperties":false}`),
			tool("open_learning_workflow", "打开正式页面：import 客户端扫描、预览和确认；goal 查看编辑目标；planning 生成编辑采用规划；select_context 选择区/目标/教学并进入独立聊天。参数不接受路径或批准，用户在页面明确选择。", `{"type":"object","properties":{"workflow":{"enum":["import","goal","planning","select_context"]}},"required":["workflow"],"additionalProperties":false}`),
		}, result[4:]...)
	}
	if s.workspace != nil && s.workspaceStatus.Available {
		result = append(result, s.workspace.Definitions()...)
	}
	if s.options.Artifacts != nil {
		result = append(result, artifactTool())
	}
	if s.options.LocalExec != nil {
		result = append(result, localExecutionTools()...)
	}
	if s.options.ContextWindow <= 8192 {
		result = compactToolProse(result)
	}
	return result
}

const (
	progressSchema = `{"type":"object","properties":{"cursor":{"type":"string"}},"additionalProperties":false}`
	emptySchema    = `{"type":"object","properties":{},"additionalProperties":false}`
	cursorSchema   = `{"type":"object","properties":{"cursor":{"type":"string","minLength":1,"maxLength":4096,"description":"上一页返回的next_cursor；首页省略"}},"additionalProperties":false}`
)

func tool(name, description, schema string) modelclient.Tool {
	return modelclient.Tool{Type: "function", Function: modelclient.ToolDefinition{
		Name: name, Description: description, Parameters: json.RawMessage(schema),
	}}
}
