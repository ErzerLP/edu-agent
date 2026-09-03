# 教学 Agent 系统完整规格

## 产品目标

系统是单用户自托管的通用个人学习助理。用户导入任意领域的 Markdown 知识，系统建立结构化知识地图，通过诊断、路线、讲解、主动回忆、评估和复习帮助用户学习，并在多个客户端之间维持可信、可解释、可纠正的连续状态。

项目最初服务于技术面试学习，但课程范围不是硬编码约束。Go 是主要实现语言，不限制系统教授的内容。

## 交付边界

**第一个可运行里程碑 M1。** M1 必须独立部署和使用。它包含 Go 模块化单体服务、PostgreSQL、在线 Go CLI、一次性配对、设备令牌、PostgreSQL canonical Markdown revision、确定性 Markdown 树索引、带 revision 引用的检索、可信最小教学闭环、Learning Event Log、Learner Model、基础查询投影、固定兼容版本的 Nocturne Memory sidecar，以及明确的 OpenAI-compatible profile。

**后续 Supervisor 波次。** M1 之后分别交付离线操作队列与同步裁决、Fast Note Sync/Obsidian 发布与显式导入、MCP transport，以及 Agent 知识维护与风险审批。

**当前 change 之外。** Rust CLI、Web UI、PDF/网页内置解析、多用户和高级个性化调度不属于当前 change 的承诺。

## 仓库与服务端架构

```text
edu-agent/
├── server/
│   ├── cmd/edu-agentd/
│   ├── internal/
│   │   ├── app/
│   │   ├── identity/
│   │   ├── knowledge/
│   │   ├── learning/
│   │   ├── tutoring/
│   │   ├── integrations/
│   │   │   ├── llm/
│   │   │   ├── nocturne/
│   │   │   └── notesync/
│   │   ├── transport/
│   │   │   ├── httpapi/
│   │   │   └── mcp/
│   │   └── platform/
│   │       ├── postgres/
│   │       ├── outbox/
│   │       ├── config/
│   │       └── observability/
│   ├── api/openapi.yaml
│   └── migrations/
├── clients/
│   └── cli-go/
├── deploy/compose.yaml
├── contracttests/
└── docs/
```

代码按业务能力分包，不采用全局 controller/service/repository 横向分层。调用方向是 `transport -> application/domain -> ports -> adapters`，端口接口由消费方定义。HTTP、MCP 和 CLI 使用相同应用用例，transport 只做协议转换、认证、校验和错误映射。

`identity`、`knowledge`、`learning` 和 `tutoring` 分别拥有自己的业务数据，不存在拥有所有实体的通用 Repository。同步是各 aggregate 的应用协议，不是拥有跨模块业务数据的独立领域。M1 是一个 Go 进程，不引入内部网络 RPC、服务发现、分布式事务或消息中间件。

## 数据所有权

Go/PostgreSQL 的 `knowledge` 模块拥有 canonical Markdown、revision、节点身份和 lineage。文件与 Obsidian 只是导入或发布视图。

Go/PostgreSQL 的 `learning` 模块拥有目标、路线、Activity、Attempt、Assessment、Evidence 和复习状态。Go/PostgreSQL 的 `tutoring` 模块拥有教学 focus 与状态机，这些状态不依赖某个模型会话上下文。Go/PostgreSQL 的 `identity` 模块拥有设备、令牌与授权。

Nocturne Memory 拥有经过准入的个人偏好、长期背景和非权威摘要。它不得保存目标、掌握度、答题、路线、误区结论、复习队列或同步状态的权威副本。Go 服务只保存 Nocturne 写入意图、外部引用、状态、回执和删除 generation，不复制其长期记忆正文作为第二权威源。

Go/PostgreSQL Outbox 拥有外部写入意图与回执，采用至少一次投递、幂等、可审计语义。

## 知识文档与树索引

### Canonical revision

导入 Markdown 文件或目录时，服务创建不可变 `KnowledgeRevision` 和文档 revision。PostgreSQL 保存 canonical Markdown、内容哈希、父 revision、来源、创建者和时间。导出使用 YAML frontmatter 与不影响 Obsidian 渲染的隐藏节点标记保存文档和节点身份。

再导入时优先匹配显式身份。缺失身份时才使用路径和标题祖先进行候选匹配，不确定结果必须审阅。

### 确定性索引

Markdown AST 决定标题层级、正文范围、节点身份映射和源引用。LLM 可以生成节点摘要、候选先修关系和检索选择，但这些都是可失效、可重建的派生工件，不能改变节点身份。同一 canonical revision 的全量重建必须得到相同节点树。

移动或改名且语义未变时保持节点身份。重大改写、拆分或合并创建新的 node revision 与 `NodeLineage`。旧学习证据保持绑定原 revision，不自动转移。Agent 可以提出折扣或 provisional 迁移建议，批准后才生效。

### 检索

检索先在文档或主题层选择候选，再逐层在节点摘要和结构上推理，最后读取正文。结果必须包含查询上下文版本、每层候选、选择理由、被选节点、`knowledge_revision_id`、`node_revision_id`、正文范围，以及降级或截断标记。

验收使用固定语料和期望候选，不以模型生成文案是否相同作为判断标准。

## 教学编排

### 显式状态机

M1 状态机如下：

```text
Idle -> GoalReady -> Diagnostic -> RouteActive
RouteActive -> ActivityIssued -> AwaitingResponse -> Evaluating
Evaluating -> Feedback -> AdvanceOrReview -> RouteActive|Completed
RouteActive|ActivityIssued|AwaitingResponse
  -> FocusSuspended -> FreeQuestion -> FreeAnswer -> FocusResumed
  -> FocusFrame.saved_state
```

`FocusFrame` 保存进入自由问答前的 `saved_state`、`route_revision_id`、`route_step_id`、`focus_node_revision_id` 和 Activity/Attempt 上下文。恢复时在一个事务中回到该保存状态和原上下文，除非用户显式结束当前 Activity 或切换目标。

LLM 只产生讲解、题目、评估和路线调整 proposal。确定性编排器验证状态转移、revision 引用、结构化 schema 和写入权限。

### Activity 与评估

Activity 发给用户前冻结目标 node revision、知识 revision、题目正文、题目类型、rubric revision、难度、允许帮助、route revision 和 policy version。

用户提交形成 Attempt。模型评估形成不可变 `AssessmentArtifact`，其中保存逐 rubric 项结论、引用的答案片段、知识引用、模型标识、模型参数、提示 revision、重试信息和风险标志。

模型工件不能直接修改 Learner Model。确定性客观题可以自动接纳。满足版本化结构化 rubric、引用完整且无风险标志的高置信评估可以自动接纳。低置信、知识引用不足、结构不完整或存在争议的开放式评估保持 `provisional`。用户可以确认、覆盖或作废 Assessment，系统保存原因和操作者。

阅读、讲解、用户提问和模型回答只记录 exposure，不直接增加掌握状态。自由问答默认不计分。用户或 Agent 只有显式执行“转为测验”并冻结新的 Activity/rubric 后，后续回答才可能形成 Evidence。

## 学习记录、投影与复习

### 记录模型

核心实体包括 `LearningSession`、`FocusFrame`、`GoalRevision`、`RouteRevision`、`RouteStep`、`Activity`、`Attempt`、`AssessmentArtifact`、`AcceptedEvidence`、`MisconceptionHypothesis`、`MasteryProjection`、`ReviewSchedule`、`LearningEvent` 和 `ProjectionCheckpoint`。

误区是带证据、置信状态和反证能力的 hypothesis，不是不可修改的用户事实。

### 事件与命令

客户端提交 operation 或 observation。服务端在一个事务中使用 `(device_id, operation_id)` 写入 Inbox 并永久去重，检查 aggregate、expected version、Activity 和知识 revision，生成服务端 canonical Learning Event，更新需要 read-your-writes 的核心投影，并写入必要 Outbox。

客户端不能生成评分、掌握更新、路线 revision 或其他权威事件。离线 `occurred_at` 是不可信展示信息，服务端同时记录可信 `received_at`。

### 重放与纠错

“可重建”只指由 Learning Event Log 派生的学习投影，不包括 Nocturne 内容或模型生成摘要。重建输入包含事件 schema version、严格 event sequence、不可变知识/node revision、reducer、接纳 policy、复习 policy、Assessment 和 Evidence 引用。

Projection 保存 checkpoint、`as_of_event_seq` 和版本。schema 变化必须提供 upcaster 或明确停止支持旧版本。业务纠错通过 `AssessmentOverridden`、`EvidenceInvalidated`、`EventRedacted` 等补偿事件完成。派生投影不能被直接手工改值。

### 掌握与复习

M1 使用 `unseen / learning / provisional / retained`，并同时展示有效证据数量、类型、帮助程度、最近时间和不确定性。单次 Assessment 不能直接建立 `retained`，至少需要跨时间的主动回忆证据。

M1 使用版本化固定间隔策略，默认阶梯为可配置的 `1d / 3d / 7d / 14d / 30d`。`ReviewPresented` 不等于成功，只有后续 accepted evidence 才能推进复习状态。系统不在缺少校准数据时声称精确预测遗忘。未来策略必须保留版本以支持历史解释和重放。

## Nocturne Memory 集成

### 部署与契约

Nocturne 是 M1 正式 sidecar，使用固定版本和镜像 digest。它可以与 Go 服务共用 PostgreSQL 实例，但必须使用独立数据库、账号和迁移生命周期，禁止跨库读取业务表。Go 服务通过受认证的适配器调用固定 REST/MCP capability，并运行契约测试。上游升级前必须重新验证 capability matrix。Nocturne Dashboard 可以部署，但不是教学业务管理界面。

### 记忆准入

生命周期是 `MemoryCandidate -> admitted|rejected -> MemoryRecord -> superseded|deleted`。每个 Candidate 包含来源事件、提议内容、敏感级别、有效期、候选 URI、提议者和理由。

用户明确陈述、稳定、非敏感的交互偏好或时间约束可以按版本化规则自动 admitted。模型推断、敏感信息、长期背景和生成摘要必须先审阅。生成摘要标记为非权威模型工件，不得伪装成用户陈述。原始聊天、完整答题、掌握度、误区结论、路线和复习队列禁止写入 Nocturne。

### Outbox 与降级

外部写入使用至少一次 Outbox。每条消息包含幂等键、实体 revision 和 generation。`applied` 表示固定契约已确认，`queued` 表示教学事务成功且长期记忆写入等待重试，`rejected` 表示策略或永久契约错误且不再自动重试。

旧 revision 或旧 generation 的消息不得覆盖新值或复活已删除记忆。超过重试预算的消息进入死信并可人工重放。

## 隐私清除

隐私清除和业务纠错是不同操作。清除时先建立 tombstone/generation barrier，再取消相关未执行 Outbox/Inbox payload，清除或加密销毁活动数据库中的可识别正文和引用，清除索引、缓存和查询投影中的内容，在 Nocturne 中解除所有路径并清理相关 orphan 与历史版本，重新查询验证后生成逐存储位置的删除回执。

删除回执记录备份最长 30 天的最终不可恢复截止时间。事件只保留最小审计元数据，正文使用可 redaction 的 payload 引用。重放遇到 redacted 事件时按版本化 no-op 或补偿语义处理。系统不能承诺清除已发送给外部模型供应商的数据，也不能在备份截止时间前宣称备份已彻底清除。

## API、CLI 与未来 Web

### HTTP 与模型 profile

OpenAPI 是 HTTP 契约。M1 的服务端教学模型 profile 明确约定 Chat Completions 风格消息、结构化 JSON 输出、错误语义、超时和最低上下文要求；原生 JSON Schema、流式输出和工具调用继续通过 capability 协商。交互式客户端 Agent 的独立 OpenAI-compatible 适配器支持有界 SSE Chat Completions 流式文本与工具调用，但不能据此宣称服务端教学模型或任意兼容端点都支持流式。fake model server 用于确定性测试。

### Go CLI

M1 Go CLI 支持配对、设备状态、注销、导入知识、设置目标、开始或继续教学、回答、查看 rubric 评估、确认或覆盖 provisional 结果、自由提问、显式转为测验、恢复 focus，以及查看路线、证据和复习。

交互式客户端 Agent 在单次进程内维护有界、来源可追踪的会话 Observation 与 Reflection，用于在模型窗口受限时保留用户意图、约束、纠正、已完成结果和未解决事项。该会话记忆不落盘、不自动进入 Nocturne，也不成为 Knowledge、Learning、Review 或长期偏好的权威副本；服务端快照必须携带 revision/generation 并在当前状态相关操作前重新读取。压缩预算必须包含系统提示、工具 Schema、当前完整工具调用组、最近至少两个已完成完整轮次、输出预留和安全余量；工具调用组不得拆分，无法同时保留当前轮次与这两个最近轮次时必须拒绝请求而不是静默丢弃，降级到最近完整轮次时必须向用户可见。精确会话记忆可以按 opaque ID 回查，但不提供模糊聊天搜索，也不暴露隐藏推理、原始工具参数或凭据。只有经用户明确确认并通过既有 Memory admission 的偏好才可成为长期记忆。

普通 Agent turn 使用独立于 TUI 生命周期的可取消 context 和唯一 turn identity。模型分析、只读工具或工具结果后的继续生成正在运行时，`Esc` 只停止当前普通 turn，不退出 TUI；界面立即进入“正在停止”，停止接收该 turn 的后续可见增量，并在 worker 确认退出后恢复输入。取消必须传播到模型 HTTP、当前只读工具和剩余工具列表，不得继续下一次模型请求。取消或协议失败的 turn 必须原子移除不完整的用户/assistant/tool 模型消息、工具调用组和会话来源，迟到事件不得污染后续 turn；用户问题、已完成工具摘要和已经显示的部分回答可以留在 transcript，并明确标记为“本轮已停止”，但部分回答不得作为完整 assistant 消息进入模型历史。`Ctrl+C`/`Ctrl+Q` 继续退出整个 Agent。

Agent 工具面增加只读、本地执行的 `ask_user_question`。一次工具调用只包含一个稳定 question ID、短 header、问题文本、`single` 或 `multiple` 模式，以及 2–4 个带稳定 option ID、标签和一句说明的选项；客户端固定追加“自定义输入”入口，模型不得把它作为普通选项伪造。所有字段在展示前执行未知字段拒绝、ID 唯一、UTF-8、显示宽度、字节上限和终端/双向控制字符清理；工具说明禁止询问密码、API key、token、私钥、恢复码或把回答描述成外部写入授权。同一用户 turn 同时最多一个 pending interaction，累计最多四次问询暂停。

模型调用 `ask_user_question` 时，Agent 保存原 assistant tool-call 序列、暂停位置、turn ID 和已完成事件，不执行该调用后的兄弟工具，也不发起下一次模型请求。TUI 状态切换为“等待你的选择”，用独占底部面板替换普通 composer/footer，同时保留 transcript 可用 `PgUp/PgDn` 查看。选项显示为 `1.`–`4.`；`↑/↓` 和数字键移动焦点，默认高亮不算提交。单选用 `Enter` 提交当前项；多选用 `Space` 勾选/取消，至少一项后 `Enter` 提交稳定顺序的 option ID 数组。“自定义输入”在单选中替代普通选项，在多选中可与普通选项并存，使用有界多行编辑器并以 `Shift+Enter` 换行。resize、滚动和模式切换不得丢失草稿、焦点或选择。

提交问询后，客户端生成绑定原 tool call ID 的结构化结果：`answered` 包含 selected option IDs 和可选 custom text，`cancelled` 表示用户按 `Esc` 取消当前问题，`unavailable` 表示交互 UI 未实际展示或失败。`Esc` 取消问题并把 `cancelled` tool result 返回模型，让 Agent 换一种问法、使用安全默认或结束；它不停止整个 turn，`Ctrl+C` 才中止 turn。回答作为当前 Session 的用户决定进入有界 context source，不是服务端快照、不带 ServerReference、不自动进入 Nocturne，也不授权其他工具。结果写入后只从原暂停位置继续尚未执行的工具，再进入下一次模型请求；重复提交、非法 option ID 或迟到选择必须被拒绝。

长期偏好确认复用同一选择器布局和键盘 reducer，但保持专用、类型安全的 resolver。写入前固定显示完整候选内容、理由、类别、敏感度和稳定性，并提供“保存为长期记忆”“仅本次会话”“不采用”三项：保存进入既有 create/admit 和稳定 operation ID 路径；仅本次会话不发起服务端写入，但把决定作为 Session 当前偏好回传模型；不采用不写入且明确要求 Agent 不把该候选当作偏好。写入开始后若结果可能未知，选择器切换为 retry-only 核对状态，不显示“仅本次会话”或“不采用”，`Esc` 不把结果未知伪装成取消，也不丢弃 operation ID。若 create 已确认产生 `pending_review` 候选而 admit 返回确定性失败，客户端必须用第三个独立、稳定的 operation ID 显式 reject 该候选；只有服务端确认 rejected 后才可恢复本地三选，补偿拒绝结果未知时继续 retry-only 并复用原 create/admit/reject IDs。通用问询即使返回名为 `save` 或 `yes` 的 option ID，也不得进入长期偏好 resolver。退出 Agent 仍取消安全可取消的请求，并遵守结果未知状态的既有保护。

交互式 Agent 的前台模型请求使用严格、有界的 OpenAI-compatible SSE Chat Completions。客户端增量渲染最终回答文本，并按 tool-call index、ID、函数名和 arguments delta 原子组装工具调用；只有在流正常结束、UTF-8/JSON/finish reason/工具数量和全部边界通过校验后，才执行工具或把完整 assistant 消息提交到 Session。模型配置中的 timeout 对 SSE 表示可配置的连续无响应时间：从请求发出后开始计算，每次成功读取任意响应 body 字节时重置，包括 SSE 心跳、隐藏推理、工具增量和跨分片内容；响应头本身不算模型输出，持续有响应的流不受该值形成的固定总时长限制。连续静默达到 timeout 必须取消 HTTP 并返回 `context_deadline_exceeded`；非流式请求继续将同一配置作为整次请求 deadline。TUI 在第一个合法 SSE 事件前保持“等待模型响应”，不能仅因收到 HTTP 200 或 SSE 响应头就显示“正在接收模型输出”。响应上限、单次 delta、累计文本、工具数、参数长度、事件行、错误帧、缺失 `[DONE]`/终止标志和异常 EOF 都失败关闭。端点在任何有效增量前明确拒绝 `stream_options` 时，客户端最多重试一次不带该字段的 SSE；该尝试若再明确拒绝 `stream`，才最多执行一次可见的非流式兼容降级。收到角色、文本、工具或隐藏推理等任何有效增量后不得自动重试，避免重复模型调用。`finish_reason=length` 与 `content_filter` 必须返回独立稳定错误，已经显示的文本标记为未完成且不提交到 Session；未知 finish reason 失败关闭。降级请求仍可由 `Esc` 取消。后台 Observer/Reflector 可以继续使用其有界非流式协议，不向 TUI 暴露内部输出。

供应商返回的隐藏 reasoning trace、`reasoning_content` 或等价字段不显示、不记录到 transcript、不进入 Source/Observation/Reflection、日志或长期记忆。仅当兼容端点要求在同一在途工具调用组中回传不透明 continuation 字段时，客户端可以在严格大小上限内暂存到该 turn 的私有协议状态，并在完成、取消或失败时销毁；该内容不能成为用户可展开的“思考详情”。

本地 Agent 模型设置增加默认推理强度，合法值为 `auto`、`none`、`minimal`、`low`、`medium`、`high`、`xhigh` 和 `max`。旧配置缺少字段时规范化为 `auto`；`auto` 完全省略 `reasoning_effort`，保持现有端点默认行为。设置页保存以后新 Agent Session 的默认值；Agent TUI 使用 `F3` 打开推理强度对话框，对话框切换只覆盖当前 Session，不改写持久化默认值。每次模型请求开始时冻结当前档位；在途请求期间切换只对下一次模型请求生效，状态栏显示当前值和待生效值。具体模型只支持子集时，显式档位被明确拒绝必须返回稳定的 `reasoning_effort_unsupported` 恢复提示，不静默降级、不把普通 HTTP 400 全部误分类，也不把 UI 选择误标为已生效。

Agent TUI 顶部只保留产品标识；有界多行输入区按内容增长，底部状态与帮助区持续显示运行状态、估算上下文、模型、推理强度和响应式键位提示。宽终端在右侧显示有界学习状态栏：Agent 区显示公开运行状态，学习区通过既有认证 `CurrentSession` 读取权威目标、会话状态、路线位置、当前 Activity 和估算活跃时间；启动、完整 turn 结束或显式刷新时重新读取，失败不阻断对话，窄终端则完全折叠且不缩窄 transcript 到合同下限以下。侧栏不显示 opaque ID、凭据、隐藏推理或原始工具参数，不持久化学习副本，也不把展示快照注入模型上下文。压缩、降级与来源不可用事件仍以不含正文的结构化 transcript 卡片呈现。

`Ctrl+O` 是统一执行详情开关。收起时只显示简洁的思考阶段和工具摘要；展开时显示客户端可证明的阶段名称、运行/完成/停止/失败状态、开始后的经过时间、工具显示名、有界进度和稳定错误码。它不显示模型隐藏 chain-of-thought、推理 token、工具参数、检索原文、opaque activity ID、凭据或原始供应商正文。传统终端没有可靠独立 `Ctrl+0` 编码，因此正式帮助与验收只承诺 `Ctrl+O`。

模型请求、上下文准备、SSE 接收、工具组装、工具执行、响应校验和工具结果后的继续分析均有真实阶段状态。运行中使用有界 tick 原地更新时间，不持续追加重复卡片；达到慢响应阈值时显示已运行时间、当前无响应超时配置和 `Esc` 停止提示，不能把无响应 timeout 误示为整轮总预算，也不能把静态 spinner 当作唯一反馈。网络、SSH 或供应商仍可能慢，但用户可以区分正在等待、正在接收、正在执行工具、正在停止和已经失败。

长期偏好读取继续通过既有认证 Memory 边界，维持最多二十项、category/sensitivity/stability/valid-until、generation/revision、redaction 和内容上限。候选元数据读取使用可取消的有界并发并保持原始结果顺序，执行详情原地显示 `已处理/总数`；任一请求取消后不启动剩余工作。部分元数据不可用继续以既有 degraded/reason code 表达，不把隐私失效内容重新注入上下文。

CLI 默认使用低颜色、无显眼学习标识的文本模式并支持快速清屏。清屏只清除应用当前显示，不承诺清除终端 scrollback、Shell 历史或系统日志。

### 查询投影

面向 CLI 和未来 Web 的查询返回时间线、会话、当前及历史路线、知识节点状态、最近有效证据、误区 hypothesis、今日与未来复习、`as_of_event_seq`、projection version、knowledge revision，以及 rebuilding、degraded 和 incomplete 标记。

统计时间只能标记为估计值，不能把不可信离线设备时钟当作精确学习时长。

## 后续波次契约

### 离线同步

离线客户端只保存服务端预签发 Activity 和 client operation/observation。每项包含 `operation_id`、`device_id`、`device_seq`、`aggregate_id`、`expected_version`、`activity_revision`、`occurred_at` 和 payload schema version。

服务端分别返回操作是否存档和证据是否接纳。重复操作不重复计分。过期知识 revision 的 Attempt 可以保留审计，但默认 provisional。多设备完成同一 Activity 时按明确 policy 处理，不按设备时钟静默覆盖。

### Fast Note Sync/Obsidian

系统固定 Fast Note Sync 最低版本、测试版本和 API capability matrix。发布 Markdown 包含 source revision，Outbox 使用单调 revision，旧消息不能覆盖新笔记。Fast Note Sync 不是 canonical 知识存储或冲突协调器。

Obsidian 修改只通过显式导入进入系统。系统根据 base、local 和 remote revision 生成三方差异，冲突必须审阅。

### MCP

MCP 只包装现有应用用例，共用设备身份、授权、审计和错误语义。通用 Agent 不能绕过 Assessment 接纳、知识修改审批或 Memory Candidate 准入策略。

### Agent 知识维护

Agent 先生成带来源、base revision、diff、lineage 影响和风险等级的 proposal。新增和局部低风险修订可以按规则自动应用。移动、重大改写、拆分、合并和删除必须审批。应用后产生新 canonical revision，回退通过新的反向 revision 完成而不删除历史。与学习证据相关的 lineage 迁移必须独立审批。

## 安全与部署

服务默认监听 loopback。非 loopback 部署应由 HTTPS 反向代理终止 TLS。显式不安全开关仅供受信网络并产生持续告警。

首次配对码由服务器本地管理命令生成，具有高熵、短 TTL、一次性和尝试限流。设备令牌使用高熵随机值，只在签发时返回，数据库只保存哈希。令牌包含设备身份和作用域，可以单独吊销。

Nocturne、Fast Note Sync 和模型凭据只保存在服务端 secret/config provider 中，不返回客户端、不进入日志或导出。HTTP 与 MCP 在进入应用用例前执行统一认证、授权、速率限制和审计。

## 验证策略

领域与应用规则使用单元测试。PostgreSQL 使用真实数据库集成测试，不维护 SQLite 生产方言。模型使用 fake server 验证 schema 错误、低置信、超时、重试和模型升级回归。

客户端 Agent additionally 使用 fake OpenAI-compatible SSE server 覆盖：文本和 UTF-8 跨 chunk、工具 ID/name/arguments delta、多个工具、finish reason、usage、隐藏 reasoning 字段隔离、事件与响应上限、错误帧、缺失终止、异常 EOF、慢响应、可配置无响应超时、总流时长超过 timeout 但任意响应字节持续续期、响应头不提前进入接收阶段、取消、取消后的 turn rollback、最终提交与取消线性化、`stream_options` 单次移除重试、端点明确不支持 `stream` 时的一次可见非流式降级、`length`/`content_filter` 未完成错误，以及显式 reasoning effort 的支持/拒绝。Agent Loop 精确覆盖 `ask_user_question` 严格 schema、独占暂停、原 tool call ID 结果、后续兄弟工具续跑、单/多选、custom answer、cancelled/unavailable、每 turn 问询上限、Session authority、偏好 pending candidate 补偿拒绝和与 preference resolver 的权限隔离。TUI fake conversation 覆盖 `Esc` 停止但不退出、迟到事件隔离、活动通道退出、Codex 风格底部选择面板、编号焦点、单选 Enter、多选 Space/Enter、自定义输入、resize/窄终端、问询 Esc 取消并续跑、长期偏好三项决定及 outcome-unknown retry-only、`F3` 会话强度对话框、`Ctrl+O` 执行详情、阶段耗时、无响应超时提示和暂停滚动。修改过的 goroutine、channel、pending interaction、Session turn 状态和有界长期偏好并发运行定向 race。

知识树比较增量更新与全量重建，学习投影比较增量 reducer 与从零重放。Inbox/Outbox、设备认证、Nocturne、删除 fence、Fast Note Sync 和离线同步执行重复、乱序、并发与故障注入测试。外部依赖按固定版本运行契约测试，升级不自动视为兼容。
